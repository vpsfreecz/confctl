package core

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// SOURCE-DERIVED scripted owners, never real children/shells/Git or terminal
// operations. OS pipes/private files test adapter ownership, not native Wait.
func pagerProcessFiles(t *testing.T) (*os.File, *os.File, *os.File) {
	t.Helper()
	root := t.TempDir()
	files := make([]*os.File, 3)
	for i, name := range []string{"stdin", "stdout", "stderr"} {
		f, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[i] = f
		t.Cleanup(func() { _ = f.Close() })
	}
	return files[0], files[1], files[2]
}

func pagerProcessBorrowedOpen(t *testing.T, files ...*os.File) {
	t.Helper()
	for _, f := range files {
		if _, err := f.Stat(); err != nil {
			t.Fatal("borrowed descriptor closed", err)
		}
	}
}

func TestPagerProcessLiteralShellDispatch(t *testing.T) {
	for _, command := range []string{"", " pager --arg ", "quoted 'x y' | other >out", "semi; $HOME `cmd`\nnext", "literal%25"} {
		args, err := pagerShellArgs("/declared/runtime shell", command)
		if err != nil || !reflect.DeepEqual(args, []string{"/declared/runtime shell", "-c", command}) {
			t.Fatal(args, err)
		}
	}
	for _, shell := range []string{"", "relative/sh", "$SHELL"} {
		if args, err := pagerShellArgs(shell, "cmd"); err == nil || args != nil || !strings.Contains(err.Error(), "not bound") {
			t.Fatal(args, err)
		}
	}
}

func TestPagerProcessProbeCapturesEOFAndRawStatus(t *testing.T) {
	failure := errors.New("probe infrastructure")
	for _, tc := range []struct {
		name    string
		result  Result
		runErr  error
		clean   bool
		wantErr error
	}{
		{name: "stdout-ignored", result: Result{ExitCode: 0, Stdout: []byte("out\x00\n")}, clean: true},
		{name: "stderr-whitespace", result: Result{ExitCode: 0, Stdout: []byte("full"), Stderr: []byte(" \n")}},
		{name: "shell-command-missing127", result: Result{ExitCode: 127, Stderr: []byte("missing")}},
		{name: "signal", result: Result{ExitCode: -1, signal: syscall.SIGTERM, Signal: syscall.SIGTERM.String(), waitErr: &exec.ExitError{}}},
		{name: "spawn-ENOENT", result: Result{ExitCode: -1, Stdout: []byte("partial"), Stderr: []byte("separate")}, runErr: syscall.ENOENT, wantErr: syscall.ENOENT},
		{name: "infrastructure", result: Result{ExitCode: 0, Stdout: []byte("partial")}, runErr: failure, wantErr: failure},
		{name: "nonexit-wait", result: Result{ExitCode: 0, waitErr: syscall.ECHILD}, wantErr: syscall.ECHILD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			r, clean, err := pagerProbeWithRunner(ctx, "/private/root", "/declared/sh", "exact | bytes", func(got context.Context, root string, argv []string, input []byte) (Result, error) {
				calls++
				if got != ctx || root != "/private/root" || !reflect.DeepEqual(argv, []string{"/declared/sh", "-c", "exact | bytes"}) || input == nil || len(input) != 0 {
					t.Fatal("changed route or EOF", root, argv, input)
				}
				return tc.result, tc.runErr
			})
			if calls != 1 || clean != tc.clean || !errors.Is(err, tc.wantErr) || !reflect.DeepEqual(r, tc.result) {
				t.Fatal(r, clean, err, calls)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(context.Context, string, []string, []byte) (Result, error) {
		t.Fatal("cancelled/unbound probe ran")
		return Result{}, nil
	}
	if _, _, err := pagerProbeWithRunner(ctx, "", "", "", never); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := pagerProbeWithRunner(context.Background(), "", "", "", never); err == nil {
		t.Fatal("unbound shell accepted")
	}
	ctx, cancel = context.WithCancel(context.Background())
	r, clean, err := pagerProbeWithRunner(ctx, "", "/declared/sh", "cmd", func(context.Context, string, []string, []byte) (Result, error) {
		cancel()
		return Result{ExitCode: 0, Stdout: []byte("partial")}, failure
	})
	if !errors.Is(err, context.Canceled) || clean || string(r.Stdout) != "partial" {
		t.Fatal(r, clean, err)
	}
}

func TestPagerProcessGitConfigBorrowedDescriptorsAndRawCompletion(t *testing.T) {
	in, out, errout := pagerProcessFiles(t)
	failure := errors.New("wait/restore failure")
	for _, tc := range []struct {
		name           string
		result         Result
		err, errorWant error
	}{
		{name: "nonzero", result: Result{ExitCode: 9}},
		{name: "signal", result: Result{ExitCode: -1, signal: syscall.SIGTERM, waitErr: &exec.ExitError{}}},
		{name: "restore", result: Result{ExitCode: 0}, err: failure, errorWant: failure},
		{name: "unowned-wait", result: Result{ExitCode: 0, waitErr: syscall.ECHILD}, errorWant: syscall.ECHILD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			text, err := pagerGitConfigWithRunner(ctx, "/private/root", in, errout, func(got context.Context, root string, argv []string, stdin *os.File, stdout, stderr io.Writer, ack func(error)) (Result, error) {
				calls++
				if got != ctx || root != "/private/root" || !reflect.DeepEqual(argv, []string{"git", "config", "--get-all", "core.pager"}) || stdin != in || stderr != errout || stdout == out || ack != nil {
					t.Fatal("not original descriptor route", root, argv)
				}
				if _, err := io.WriteString(stdout, " pager --literal \x00\n"); err != nil {
					t.Fatal(err)
				}
				return tc.result, tc.err
			})
			if calls != 1 || text != " pager --literal \x00\n" || !errors.Is(err, tc.errorWant) {
				t.Fatal(text, err, calls)
			}
			pagerProcessBorrowedOpen(t, in, out, errout)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pagerGitConfigWithRunner(ctx, "", in, errout, func(context.Context, string, []string, *os.File, io.Writer, io.Writer, func(error)) (Result, error) {
		t.Fatal("cancelled Git config ran")
		return Result{}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPagerProcessStartupAcknowledgmentAndPipeLifetime(t *testing.T) {
	in, out, errout := pagerProcessFiles(t)
	entered, permit := make(chan struct{}), make(chan struct{})
	var reader *os.File
	var payload []byte
	calls := 0
	run := func(ctx context.Context, root string, argv []string, stdin *os.File, stdout, stderr io.Writer, ack func(error)) (Result, error) {
		calls++
		reader = stdin
		if root != "/private/root" || !reflect.DeepEqual(argv, []string{"/declared/sh", "-c", "exact | bytes"}) || stdin == in || stdout != out || stderr != errout {
			t.Error("wrong inherited boundary", root, argv)
		}
		close(entered)
		<-permit
		ack(nil)
		var err error
		payload, err = io.ReadAll(stdin)
		return Result{ExitCode: 9}, err
	}
	type startedResult struct {
		pipe *pagerPipe
		err  error
	}
	result := make(chan startedResult, 1)
	go func() {
		p, err := startPagerPipeWithRunner(context.Background(), "/private/root", "/declared/sh", "exact | bytes", out, errout, run)
		result <- startedResult{p, err}
	}()
	<-entered
	select {
	case got := <-result:
		t.Fatal("writer returned before ack", got)
	default:
	}
	close(permit)
	got := <-result
	if got.err != nil || got.pipe == nil {
		t.Fatal(got)
	}
	writer := got.pipe.input.(*os.File)
	if n, err := got.pipe.Write([]byte("full\x00payload\n")); n != 13 || err != nil {
		t.Fatal(n, err)
	}
	if err := got.pipe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := got.pipe.Close(); err != nil || calls != 1 || got.pipe.result.ExitCode != 9 || string(payload) != "full\x00payload\n" {
		t.Fatal(err, calls, got.pipe.result, string(payload))
	}
	for _, f := range []*os.File{reader, writer} {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("owned end left open", err)
		}
	}
	pagerProcessBorrowedOpen(t, in, out, errout)
}

func TestPagerProcessStartupFailureJoinsAndCloses(t *testing.T) {
	in, out, errout := pagerProcessFiles(t)
	failure := errors.New("start failed")
	eof, permit := make(chan struct{}), make(chan struct{})
	var reader *os.File
	calls := 0
	result := make(chan error, 1)
	go func() {
		pipe, err := startPagerPipeWithRunner(context.Background(), "", "/declared/sh", "cmd", out, errout, func(_ context.Context, _ string, _ []string, stdin *os.File, _, _ io.Writer, ack func(error)) (Result, error) {
			calls++
			reader = stdin
			ack(failure)
			b, err := io.ReadAll(stdin)
			if err != nil || len(b) != 0 {
				t.Error("failed-start writer not closed", b, err)
			}
			close(eof)
			<-permit
			return Result{ExitCode: -1}, failure
		})
		if pipe != nil {
			t.Error("startup failure returned writer")
		}
		result <- err
	}()
	<-eof
	select {
	case err := <-result:
		t.Fatal("returned before owner completion", err)
	default:
	}
	close(permit)
	if err := <-result; !errors.Is(err, failure) || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := reader.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned reader left open", err)
	}
	pagerProcessBorrowedOpen(t, in, out, errout)
}

func TestPagerProcessCancellationJoinsAndCaches(t *testing.T) {
	in, out, errout := pagerProcessFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed, permit := make(chan struct{}), make(chan struct{})
	pipe, err := startPagerPipeWithRunner(ctx, "", "/declared/sh", "cmd", out, errout, func(ctx context.Context, _ string, _ []string, _ *os.File, _, _ io.Writer, ack func(error)) (Result, error) {
		ack(nil)
		<-ctx.Done()
		close(observed)
		<-permit
		return Result{ExitCode: -1, signal: syscall.SIGTERM}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-observed
	result := make(chan error, 1)
	go func() { result <- pipe.Close() }()
	select {
	case err := <-result:
		t.Fatal("close did not join owner", err)
	default:
	}
	close(permit)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := pipe.Close(); !errors.Is(err, context.Canceled) || pipe.result.signal != syscall.SIGTERM {
		t.Fatal(err, pipe.result)
	}
	pagerProcessBorrowedOpen(t, in, out, errout)
}

type pagerScriptInput struct {
	closeErr error
	closes   int
}

func (*pagerScriptInput) Write(b []byte) (int, error) { return len(b), nil }
func (w *pagerScriptInput) Close() error              { w.closes++; return w.closeErr }

func TestPagerProcessCloseErrorsRawResultsAndCache(t *testing.T) {
	failure, other := errors.New("owner restore error"), errors.New("input close error")
	for _, tc := range []struct {
		name                   string
		result                 Result
		runErr, closeErr, want error
	}{
		{name: "nonzero", result: Result{ExitCode: 127, waitErr: &exec.ExitError{}}},
		{name: "signal", result: Result{ExitCode: -1, signal: syscall.SIGPIPE, waitErr: &exec.ExitError{}}},
		{name: "missing-status", result: Result{ExitCode: -1}, want: errors.New("command did not return an exit status")},
		{name: "nonexit-wait", result: Result{ExitCode: 0, waitErr: syscall.ECHILD}, want: syscall.ECHILD},
		{name: "restore", result: Result{ExitCode: 0}, runErr: failure, want: failure},
		{name: "close", result: Result{ExitCode: 0}, closeErr: other, want: other},
		{name: "own-close-EPIPE", result: Result{ExitCode: 0}, closeErr: syscall.EPIPE},
		{name: "own-path-close-EPIPE", result: Result{ExitCode: 0}, closeErr: &os.PathError{Op: "close", Path: "owned", Err: syscall.EPIPE}},
		{name: "joined-EPIPE-plus-error", result: Result{ExitCode: 0}, closeErr: errors.Join(syscall.EPIPE, other), want: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &pagerScriptInput{closeErr: tc.closeErr}
			done := make(chan pagerChildCompletion, 1)
			done <- pagerChildCompletion{tc.result, tc.runErr}
			p := &pagerPipe{ctx: context.Background(), input: input, done: done}
			err := p.Close()
			if tc.name == "missing-status" {
				if err == nil || err.Error() != tc.want.Error() {
					t.Fatal(err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatal(err, tc.want)
			}
			if again := p.Close(); again != err || input.closes != 1 || !reflect.DeepEqual(p.result, tc.result) {
				t.Fatal("not cached", again, err, input.closes, p.result)
			}
		})
	}
	// Close and owner failure remain separately identifiable; context wins only
	// after the owner's joined completion, never by bypassing that join.
	input := &pagerScriptInput{closeErr: other}
	done := make(chan pagerChildCompletion, 1)
	done <- pagerChildCompletion{Result{ExitCode: 0}, failure}
	p := &pagerPipe{ctx: context.Background(), input: input, done: done}
	if err := p.Close(); !errors.Is(err, failure) || !errors.Is(err, other) {
		t.Fatal(err)
	}
}

func TestPagerProcessUnboundAndCanceledDoNotAcquire(t *testing.T) {
	_, out, errout := pagerProcessFiles(t)
	never := func(context.Context, string, []string, *os.File, io.Writer, io.Writer, func(error)) (Result, error) {
		t.Fatal("unbound/cancelled owner reached")
		return Result{}, nil
	}
	if p, err := startPagerPipeWithRunner(context.Background(), "", "", "cmd", out, errout, never); p != nil || err == nil {
		t.Fatal(p, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, err := startPagerPipeWithRunner(ctx, "", "/declared/sh", "cmd", out, errout, never); p != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(p, err)
	}
}

var _ io.WriteCloser = (*pagerPipe)(nil)

func TestPagerProcessCanceledStartupJoinsWithoutReturningWriter(t *testing.T) {
	in, out, errout := pagerProcessFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	var reader *os.File
	pipe, err := startPagerPipeWithRunner(ctx, "", "/declared/sh", "cmd", out, errout, func(_ context.Context, _ string, _ []string, stdin *os.File, _, _ io.Writer, ack func(error)) (Result, error) {
		calls++
		reader = stdin
		// Script cancellation after acquisition but before delivery of success ack.
		cancel()
		ack(nil)
		b, err := io.ReadAll(stdin)
		if err != nil || len(b) != 0 {
			t.Error(b, err)
		}
		return Result{ExitCode: 0}, nil
	})
	if pipe != nil || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(pipe, err, calls)
	}
	if _, err := reader.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned reader survived startup join", err)
	}
	pagerProcessBorrowedOpen(t, in, out, errout)
}
