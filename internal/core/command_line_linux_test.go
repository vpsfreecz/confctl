//go:build linux

package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/health"
)

type commandPromptOutput struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	prompts chan struct{}
}

func newCommandPromptOutput() *commandPromptOutput {
	return &commandPromptOutput{prompts: make(chan struct{}, 16)}
}

func (w *commandPromptOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if bytes.Contains(p, []byte("Continue?")) || bytes.Contains(p, []byte("Choose [")) {
		w.prompts <- struct{}{}
	}
	return n, err
}

func commandPromptPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	in, producer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = producer.Close() })
	return in, producer
}

func commandPromptWait(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt operation did not reach its observed boundary")
	}
}

func commandPromptJoin(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled prompt did not join while producer remained open")
		return nil
	}
}

func commandPromptStart(t *testing.T, cancel context.CancelFunc, in, producer *os.File, run func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
			return
		default:
		}
		// Only failure cleanup closes the test-owned borrowed endpoints to release
		// a regressed blocking read. Passing assertions require the producer open.
		_ = producer.Close()
		_ = in.Close()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("prompt worker survived test cleanup")
		}
	})
	return done
}

func TestCommandLineNativeOwnerCleanup(t *testing.T) {
	for _, partial := range []string{"", "y"} {
		t.Run(fmt.Sprintf("partial=%q", partial), func(t *testing.T) {
			in, producer := commandPromptPipe(t)
			if _, err := io.WriteString(producer, partial); err != nil {
				t.Fatal(err)
			}
			input := bufio.NewReader(in)
			native := nativeBasicLinuxOps()
			polling := make(chan struct{}, 1)
			var mu sync.Mutex
			closed := map[*os.File]int{}
			var read, write *os.File
			ops := inputWaitOps{
				Pipe: func() (*os.File, *os.File, error) {
					var err error
					read, write, err = native.Pipe()
					return read, write, err
				},
				Poll: func(in, wake *os.File) error {
					// Observe an actual poll with the producer still open. In the
					// partial case, Byte observes consumption of the prefix.
					if partial == "" {
						select {
						case polling <- struct{}{}:
						default:
						}
					}
					return native.Poll(in, wake)
				},
				Byte: func(input *bufio.Reader) (byte, error) {
					b, err := native.Byte(input)
					if partial != "" && b == 'y' {
						polling <- struct{}{}
					}
					return b, err
				},
				Wake: native.Wake,
				Close: func(f *os.File) error {
					mu.Lock()
					closed[f]++
					mu.Unlock()
					return native.Close(f)
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := commandPromptStart(t, cancel, in, producer, func() error {
				line, err := linuxGenDataLine(ctx, in, input, ops)
				if line != partial {
					return fmt.Errorf("partial line %q, want %q (read error: %v)", line, partial, err)
				}
				return err
			})
			commandPromptWait(t, polling)
			cancel()
			if err := commandPromptJoin(t, done); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			mu.Lock()
			if closed[read] != 1 || closed[write] != 1 || closed[in] != 0 || len(closed) != 2 {
				t.Error("owned endpoints did not close exactly once", closed)
			}
			mu.Unlock()
			// The line owner has returned after its notifier joined. Both
			// borrowed ends still work, through the same retained reader.
			if _, err := io.WriteString(producer, "next\n"); err != nil {
				t.Fatal(err)
			}
			if got, err := linuxCommandLine(in, input)(context.Background()); err != nil || got != "next\n" {
				t.Fatal(got, err)
			}
		})
	}
}

func TestCommandLineRetainedBufferAndEOF(t *testing.T) {
	input := bufio.NewReaderSize(strings.NewReader("y\nQ"), 16)
	retained, line := commandInput(input)
	if retained != input {
		t.Fatal("replaced the retained reader")
	}
	if err := testConnectionConfirm(context.Background(), line, io.Discard); err != nil {
		t.Fatal(err)
	}
	if suffix, err := input.ReadString('\n'); suffix != "Q" || err != io.EOF {
		t.Fatal("Basic suffix was stranded", suffix, err)
	}
	_, queued := commandInput(strings.NewReader("y\nr\nf"))
	if ok, err := deployConfirm(context.Background(), queued, io.Discard); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if got, err := deployChoice(context.Background(), queued, io.Discard, "ysar"); got != "r" || err != nil {
		t.Fatal(got, err)
	}
	if got, err := deployChoice(context.Background(), queued, io.Discard, "crfa"); got != "f" || err != nil {
		t.Fatal("valid partial EOF choice lost", got, err)
	}
	in, producer := commandPromptPipe(t)
	if _, err := io.WriteString(producer, "y\nQ"); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	retained, line = commandInput(in)
	if err := testConnectionConfirm(context.Background(), line, io.Discard); err != nil {
		t.Fatal(err)
	}
	if suffix, err := retained.ReadString('\n'); suffix != "Q" || err != io.EOF {
		t.Fatal("native confirmation stranded the pager suffix", suffix, err)
	}
}

func TestCommandPromptCancellationAndErrorPrecedence(t *testing.T) {
	readFailure := errors.New("reached line failure")
	writeFailure := errors.New("reached output failure")
	for _, kind := range []string{"ordinary", "deploy", "engine"} {
		for _, boundary := range []string{"before", "complete", "partial-eof", "read-error", "write-error"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reads := 0
				line := commandLine(func(context.Context) (string, error) {
					reads++
					cancel()
					switch boundary {
					case "read-error":
						return "y", readFailure
					case "partial-eof":
						return "y", io.EOF
					default:
						return "y\n", nil
					}
				})
				var out bytes.Buffer
				var writer io.Writer = &out
				want := error(context.Canceled)
				if boundary == "before" {
					cancel()
				}
				if boundary == "read-error" {
					want = readFailure
				}
				if boundary == "write-error" {
					writer = commandPromptFailWriter{cancel: cancel, err: writeFailure}
					want = writeFailure
				}
				var err error
				switch kind {
				case "ordinary":
					err = testConnectionConfirm(ctx, line, writer)
				case "deploy":
					_, err = deployChoice(ctx, line, writer, "yn")
				case "engine":
					var accepted bool
					accepted, err = (&Engine{}).confirm(ctx, line, writer, "", false)
					if accepted {
						t.Error("cancellation/error became permission")
					}
				}
				if err != want {
					t.Fatalf("got %v, want %v", err, want)
				}
				if (boundary == "before" || boundary == "write-error") && reads != 0 {
					t.Fatal("read reached after early refusal", reads)
				}
				if boundary == "before" && out.Len() != 0 {
					t.Fatal("prompt emitted after cancellation")
				}
			})
		}
	}
}

type commandPromptFailWriter struct {
	cancel context.CancelFunc
	err    error
}

func (w commandPromptFailWriter) Write([]byte) (int, error) { w.cancel(); return 0, w.err }

func TestCommandPromptEnginePolicies(t *testing.T) {
	for _, tc := range []struct {
		input                 string
		yes, always, accepted bool
		failure               string
	}{
		{"", true, false, true, ""},
		{"n\n", true, true, false, ""},
		{"\ninvalid\nY", false, false, true, ""},
		{"N", false, false, false, ""},
		{"", false, false, false, "end of file reached"},
	} {
		_, line := commandInput(strings.NewReader(tc.input))
		var out bytes.Buffer
		accepted, err := (&Engine{Yes: tc.yes}).confirm(context.Background(), line, &out, "message", tc.always)
		if accepted != tc.accepted || (err != nil) != (tc.failure != "") || err != nil && err.Error() != tc.failure {
			t.Fatal(tc, accepted, err)
		}
		if tc.yes && !tc.always && out.Len() != 0 {
			t.Fatal("--yes emitted or acquired a prompt")
		}
	}
}

func TestCommandPromptEngineUsesOwnContext(t *testing.T) {
	in, producer := commandPromptPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := commandPromptStart(t, cancel, in, producer, func() error {
		accepted, err := (&Engine{Context: ctx}).Confirm(in, "", false)
		if accepted {
			return errors.New("cancelled Engine.Confirm returned permission")
		}
		return err
	})
	if err := commandPromptJoin(t, done); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestCommandPromptPublicCallerCancellation(t *testing.T) {
	for _, family := range []string{"test-connection", "ssh", "cssh", "comparison", "health", "build", "status", "rm", "rotate", "gc", "deploy"} {
		t.Run(family, func(t *testing.T) {
			in, producer := commandPromptPipe(t)
			out := newCommandPromptOutput()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := connectionEngine(t, connectionMachine("node", "node", "fixture.invalid", true))
			e.Context = ctx
			var run func() error
			unexpected := func() { t.Error("effect reached after cancelled confirmation") }
			switch family {
			case "test-connection":
				run = func() error {
					return e.runTestConnection(connectionInvocation(t, "node"), in, out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) { unexpected(); return Result{}, nil }})
				}
			case "ssh":
				run = func() error {
					return e.runSSH(sshInvocation(t, "ssh", "node", "true"), in, out, out, sshDeps{Run: func(context.Context, string, []string, sshInput) (Result, error) { unexpected(); return Result{}, nil }})
				}
			case "cssh":
				run = func() error { return e.CSSH(sshInvocationForPrompt(t, "cssh", "node"), in, out) }
			case "comparison":
				_, d, _ := comparisonFixture(t, e.Inventory)
				input := bufio.NewReader(in)
				d.Line = linuxCommandLine(in, input)
				run = func() error { return e.compare(comparisonInvocation(t, "diff", "node"), input, out, d) }
			case "health":
				e = healthCLIEngine(t, healthMachineJSON("node", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
				e.Context = ctx
				run = func() error {
					_, err := e.healthCheck(healthInvocation(t, "node"), in, out, out, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) { unexpected(); return Result{}, nil })
					return err
				}
			case "build":
				run = func() error {
					return e.buildCLI(sshInvocationForPrompt(t, "build", "node"), in, out, io.Discard, buildDeps{})
				}
			case "status":
				run = func() error {
					return e.statusCLI(sshInvocationForPrompt(t, "status", "--generation", "none", "node"), in, out, io.Discard, buildDeps{})
				}
			case "rm", "rotate":
				var d generationMutationDeps
				e, d, _, _ = mutationFixture(t)
				e.Context = ctx
				d.Roots = func() (string, error) { unexpected(); return "", errors.New("unexpected roots") }
				inv := mutationInvocation(t, family, "node")
				run = func() error {
					if family == "rm" {
						return e.removeGenerations(inv, in, out, d)
					}
					return e.rotateGenerations(inv, in, out, d)
				}
			case "gc":
				run = func() error {
					return e.collectGarbage(sshInvocationForPrompt(t, "collect-garbage", "node"), in, out, gcDependencies{})
				}
			case "deploy":
				run = func() error {
					_, err := e.deployCommand(deployInvocation(t, "node"), in, out, out, deployCommandDeps{Machines: func() ([]Machine, error) { return e.Inventory, nil }, Prepare: func([]string) (int, error) { unexpected(); return 0, nil }})
					return err
				}
			}
			done := commandPromptStart(t, cancel, in, producer, run)
			commandPromptWait(t, out.prompts)
			cancel()
			if err := commandPromptJoin(t, done); err != context.Canceled {
				t.Fatal(err)
			}
		})
	}
}

func sshInvocationForPrompt(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestCommandPromptDeployUsesCallbackContext(t *testing.T) {
	for _, kind := range []string{"confirm", "activation", "health"} {
		t.Run(kind, func(t *testing.T) {
			in, producer := commandPromptPipe(t)
			_, line := commandInput(in)
			out := newCommandPromptOutput()
			e := &Engine{Context: context.Background()}
			d := e.deployOperationsWithOps(e.Context, nil, nil, line, out, out, cli.Invocation{}, deployProcessOps{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := commandPromptStart(t, cancel, in, producer, func() error {
				var err error
				switch kind {
				case "confirm":
					_, err = d.Confirm(ctx, deploy.Confirmation{})
				case "activation":
					_, err = d.ChooseActivation(ctx, deploy.Target{}, true)
				case "health":
					_, err = d.ChooseHealth(ctx, nil, health.Report{})
				}
				return err
			})
			commandPromptWait(t, out.prompts)
			cancel()
			if err := commandPromptJoin(t, done); err != context.Canceled {
				t.Fatal(err)
			}
		})
	}
}

func TestCommandPromptSupervisorGateAndCancellation(t *testing.T) {
	in, producer := commandPromptPipe(t)
	out := newCommandPromptOutput()
	s := newSupervisor(&Engine{Context: context.Background()}, ext.Invocation{}, in, out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	params := json.RawMessage(`{"message":"fixture","always":false}`)
	first := commandPromptStart(t, cancel, in, producer, func() error { _, err := s.handle(ctx, "ui.confirm", params); return err })
	commandPromptWait(t, out.prompts)
	// The active prompt owns only its gate, never the machine/cache mutex.
	if !s.mu.TryLock() {
		t.Fatal("prompt holds machine/cache mutex")
	}
	s.mu.Unlock()
	secondCtx, secondCancel := context.WithCancel(context.Background())
	waiting := &commandPromptWaitingContext{Context: secondCtx, waiting: make(chan struct{})}
	second := commandPromptStart(t, secondCancel, in, producer, func() error { _, err := s.handle(waiting, "ui.confirm", params); return err })
	commandPromptWait(t, waiting.waiting)
	secondCancel()
	if err := commandPromptJoin(t, second); err != context.Canceled {
		t.Fatal(err)
	}
	select {
	case <-out.prompts:
		t.Fatal("second prompt bypassed the gate")
	default:
	}
	cancel()
	if err := commandPromptJoin(t, first); err != context.Canceled {
		t.Fatal(err)
	}
	if len(s.promptGate) != 0 {
		t.Fatal("prompt gate leaked")
	}
	if _, err := io.WriteString(producer, "y\nn\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		value, err := s.handle(context.Background(), "ui.confirm", params)
		if err != nil || value.(map[string]any)["accepted"] != want {
			t.Fatal(value, err)
		}
	}
	hook := newSupervisor(&Engine{}, ext.Invocation{Event: "deploy.prepare"}, in, out)
	hook.prompt = func(context.Context) (string, error) { t.Fatal("hook acquired input"); return "", nil }
	if _, err := hook.handle(context.Background(), "ui.confirm", params); err == nil || err.Error() != "hooks cannot prompt" {
		t.Fatal(err)
	}
}

// Done is evaluated when the second handler reaches the gate select. This
// observes an actual queued waiter without a scheduler delay or product hook.
type commandPromptWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *commandPromptWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCommandPromptSupervisorSerializesQueuedAnswers(t *testing.T) {
	in, producer := commandPromptPipe(t)
	out := newCommandPromptOutput()
	s := newSupervisor(&Engine{Context: context.Background()}, ext.Invocation{}, in, out)
	params := json.RawMessage(`{"message":"fixture","always":false}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := commandPromptStart(t, cancel, in, producer, func() error {
		value, err := s.handle(ctx, "ui.confirm", params)
		if err == nil && value.(map[string]any)["accepted"] != true {
			return errors.New("first prompt lost its yes answer")
		}
		return err
	})
	commandPromptWait(t, out.prompts)
	waiting := &commandPromptWaitingContext{Context: ctx, waiting: make(chan struct{})}
	second := commandPromptStart(t, cancel, in, producer, func() error {
		value, err := s.handle(waiting, "ui.confirm", params)
		if err == nil && value.(map[string]any)["accepted"] != false {
			return errors.New("second prompt lost its no answer")
		}
		return err
	})
	commandPromptWait(t, waiting.waiting)
	select {
	case <-out.prompts:
		t.Fatal("second prompt emitted before acquiring the gate")
	default:
	}
	if _, err := io.WriteString(producer, "y\nn\n"); err != nil {
		t.Fatal(err)
	}
	if err := commandPromptJoin(t, first); err != nil {
		t.Fatal(err)
	}
	commandPromptWait(t, out.prompts)
	if err := commandPromptJoin(t, second); err != nil {
		t.Fatal(err)
	}
	if len(s.promptGate) != 0 {
		t.Fatal("prompt gate leaked")
	}
}
