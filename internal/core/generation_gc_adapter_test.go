package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func gcAdapterInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"collect-garbage"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func gcAdapterEngine(t *testing.T) *Engine {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(root+"/flake.nix", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return &Engine{Root: root, Context: context.Background(), SettingsCache: map[string]any{"list": map[string]any{"columns": []any{"name"}}}}
}

func TestGenerationGCAdapterSelectionAndDecline(t *testing.T) {
	e := gcAdapterEngine(t)
	unmanaged, carried, targetless := mutationMachine("unmanaged"), mutationMachine("carried"), mutationMachine("targetless")
	carried.CarrierName, targetless.Target.Host = &unmanaged.Name, nil
	e.Inventory = []Machine{carried, unmanaged, targetless}
	var out bytes.Buffer
	calls := 0
	d := gcDependencies{
		Prepare: func(context.Context) (gcExecute, error) {
			calls++
			return func(_ context.Context, m Machine, command []string, feed func([]byte, []byte)) (Result, error) {
				if m.Name != "unmanaged" || strings.Join(command, " ") != "nix-collect-garbage" {
					t.Fatal(m, command)
				}
				feed([]byte("1 store paths deleted\n"), nil)
				return Result{}, nil
			}, nil
		},
		Open: func(_ context.Context, spec gcViewSpec) (gcView, bool, error) {
			if spec.Count != 1 || spec.FirstMachine != "unmanaged" {
				t.Fatal(spec)
			}
			return &generationGCTextView{out: &out}, false, nil
		},
	}
	if err := e.collectGarbage(gcAdapterInvocation(t), strings.NewReader("n\n"), &out, d); err == nil || err.Error() != "Aborted" || calls != 0 {
		t.Fatal(calls, err)
	}
	out.Reset()
	if err := e.collectGarbage(gcAdapterInvocation(t, "--yes"), nil, &out, d); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	if !strings.Contains(out.String(), "unmanaged: 1 store paths deleted\n") || strings.Contains(out.String(), "carried") {
		t.Fatal(out.String())
	}
	e.Inventory = []Machine{carried, targetless}
	if err := e.collectGarbage(gcAdapterInvocation(t, "--yes", "--max-concurrent-gc=0"), nil, &out, d); err == nil || err.Error() != "No machines to collect garbage on" || calls != 1 {
		t.Fatal(calls, err)
	}
}

func TestGenerationGCAdapterReachedSetupAndSignedNoWork(t *testing.T) {
	e := gcAdapterEngine(t)
	var out bytes.Buffer
	never := func(context.Context, string, []string, []byte, func([]byte, []byte) error) (Result, error) {
		t.Fatal("no-work entered runner")
		return Result{}, nil
	}
	for _, request := range []int64{0, -5} {
		out.Reset()
		if err := runGenerationGC(e.Context, []gcTask{{Host: "x", Machine: mutationMachine("x")}}, big.NewInt(request), e.generationGCAdapter(&out, io.Discard, never)); err != nil || out.String() != "Collecting garbage on x\n" {
			t.Fatal(out.String(), err)
		}
	}
	out.Reset()
	if err := runGenerationGC(e.Context, nil, big.NewInt(5), e.generationGCAdapter(&out, io.Discard, never)); err != nil || out.String() != "Collecting garbage on \n" {
		t.Fatal(out.String(), err)
	}
	if err := os.Remove(e.Root + "/flake.nix"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runGenerationGC(e.Context, nil, big.NewInt(0), e.generationGCAdapter(&out, io.Discard, never)); err == nil || out.Len() != 0 {
		t.Fatal("zero workers bypassed setup", out.String(), err)
	}
}

func TestGenerationGCAdapterPreservesStreamsAndFailureAggregation(t *testing.T) {
	for _, status := range []int{0, 23} {
		t.Run(string(rune('a'+status)), func(t *testing.T) {
			e := gcAdapterEngine(t)
			m := mutationMachine("fixture")
			m.Target.Localhost = true
			e.Inventory = []Machine{m}
			bash, err := exec.LookPath("bash")
			if err != nil {
				t.Fatal(err)
			}
			tools := t.TempDir()
			body := "#!" + bash + "\nprintf '%s\\0' \"$@\" > \"$TRACE\"\nprintf '1 store paths deleted\\n'\nprintf \"deleting '/nix/store/fake-only'\\n\" >&2\nexit \"$FAKE_GC_STATUS\"\n"
			if err := os.WriteFile(tools+"/nix-collect-garbage", []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", tools)
			t.Setenv("TRACE", e.Root+"/argv")
			t.Setenv("FAKE_GC_STATUS", big.NewInt(int64(status)).String())
			if err := e.OpenLog("fixture"); err != nil {
				t.Fatal(err)
			}
			defer e.Log.Close()
			var out bytes.Buffer
			err = e.collectGarbage(gcAdapterInvocation(t, "--yes"), nil, &out, e.generationGCDependencies(&out, io.Discard))
			if (status == 0 && err != nil) || (status != 0 && (err == nil || err.Error() != "Gargabe collection failed on: fixture")) {
				t.Fatal(out.String(), err)
			}
			if args, err := os.ReadFile(e.Root + "/argv"); err != nil || string(args) != "\x00" {
				t.Fatal("unexpected GC arguments", string(args), err)
			}
			log, err := os.ReadFile(e.Log.Name())
			if err != nil || strings.Count(string(log), "1 store paths deleted") != 1 || strings.Count(string(log), "deleting '/nix/store/fake-only'") != 1 {
				t.Fatal(string(log), err)
			}
			if !strings.Contains(out.String(), "fixture> 1 store paths deleted\n") || !strings.Contains(out.String(), "fixture> deleting '/nix/store/fake-only'\n") {
				t.Fatal(out.String())
			}
		})
	}
}

type gcAdapterFailWriter struct {
	calls int
	err   error
}

func (w *gcAdapterFailWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return len(b), nil // Header succeeds; the first progress event fails.
	}
	return 0, w.err
}

func TestGenerationGCAdapterOutputFailureCancelsAndJoins(t *testing.T) {
	e := gcAdapterEngine(t)
	boom := errors.New("progress write failed")
	out := &gcAdapterFailWriter{err: boom}
	joined := false
	d := e.generationGCAdapter(out, io.Discard, func(ctx context.Context, _ string, _ []string, input []byte, feed func([]byte, []byte) error) (Result, error) {
		if input != nil {
			t.Fatal("GC inherited a payload")
		}
		if err := feed([]byte("visible\n"), nil); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		joined = true
		return Result{}, ctx.Err()
	})
	err := runGenerationGC(e.Context, []gcTask{{Host: "fixture", Machine: Machine{Target: Target{Localhost: true}}}}, big.NewInt(1), d)
	if !errors.Is(err, boom) || !joined || out.calls != 2 {
		t.Fatal(joined, out.calls, err)
	}
	short := &generationWarningWriter{count: 0}
	d = e.generationGCAdapter(short, io.Discard, nil)
	if err := runGenerationGC(e.Context, nil, big.NewInt(0), d); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

type gcAdapterReadyWriter struct {
	mu    sync.Mutex
	body  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *gcAdapterReadyWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.body.Write(b)
	if strings.Contains(w.body.String(), "fixture> ready\n") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func TestGenerationGCAdapterCancellationJoinsOwnedFakeCommand(t *testing.T) {
	e := gcAdapterEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Context = ctx
	m := mutationMachine("fixture")
	m.Target.Localhost = true
	e.Inventory = []Machine{m}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "nix-collect-garbage"), []byte("#!"+bash+"\nprintf 'ready\\n'\nread -r unused\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	out := &gcAdapterReadyWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	inv := gcAdapterInvocation(t, "--yes")
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("owned fixture cleanup did not join")
			}
		}
	}()
	go func() {
		done <- e.collectGarbage(inv, nil, out, e.generationGCDependencies(out, io.Discard))
	}()
	select {
	case <-out.ready:
	case err := <-done:
		joined = true
		t.Fatal("child exited before barrier", err)
	case <-time.After(5 * time.Second):
		t.Fatal("fake command did not reach barrier")
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned command did not join")
	}
}
