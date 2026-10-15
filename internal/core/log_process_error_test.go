package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/inputs"
)

// Opaque identities only: these scripts never call methods on the file.
func logProcessEngine(ops engineLogOps) *Engine {
	f := &os.File{}
	return &Engine{Context: context.Background(), Log: f, logOwner: newEngineLog(f, ops)}
}

func logProcessInvocation(t *testing.T, argv ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestLogProcessRawStages(t *testing.T) {
	writeE, flushE, readerE := errors.New("write"), errors.New("flush"), errors.New("reader")
	p := []byte{'a', 0, 0xff, '\n'}
	for _, row := range []struct {
		n                        int
		write, flush, read, want error
		trace                    []string
	}{
		{0, writeE, nil, nil, writeE, []string{"write"}},
		{2, writeE, nil, nil, writeE, []string{"write"}},
		{4, writeE, nil, nil, writeE, []string{"write"}},
		{0, nil, nil, nil, io.ErrShortWrite, []string{"write"}},
		{2, nil, nil, nil, io.ErrShortWrite, []string{"write"}},
		{4, nil, flushE, nil, flushE, []string{"write", "flush"}},
		{4, nil, nil, readerE, readerE, []string{"write", "flush", "reader"}},
		{4, nil, nil, nil, nil, []string{"write", "flush", "reader"}},
	} {
		trace := []string{}
		var raw bytes.Buffer
		e := logProcessEngine(engineLogOps{
			Write: func(b []byte) (int, error) {
				trace = append(trace, "write")
				raw.Write(b[:row.n])
				return row.n, row.write
			},
			Flush: func() error { trace = append(trace, "flush"); return row.flush },
		})
		detach, err := e.attachLog(func(b []byte) error {
			trace = append(trace, "reader")
			if !bytes.Equal(b, p) {
				t.Fatal("borrowed bytes changed")
			}
			return row.read
		})
		if err != nil {
			t.Fatal(err)
		}
		n, err := e.logOwner.live.Write(p)
		if n != row.n || err != row.want || !bytes.Equal(raw.Bytes(), p[:row.n]) || !reflect.DeepEqual(trace, row.trace) {
			t.Fatal(n, err, trace, raw.Bytes())
		}
		if err := detach(); err != nil {
			t.Fatal(err)
		}
		// Raw ownership must be released on every path, including failures.
		if !e.logOwner.rawMu.TryLock() {
			t.Fatal("raw lock retained")
		}
		e.logOwner.rawMu.Unlock()
	}
	trace := []string{}
	e := logProcessEngine(engineLogOps{Write: func(b []byte) (int, error) { trace = append(trace, "write"); return len(b), nil }, Flush: func() error { trace = append(trace, "flush"); return nil }})
	detach, _ := e.attachLog(func([]byte) error { trace = append(trace, "read"); return nil })
	if err := e.logText(""); err != nil || !reflect.DeepEqual(trace, []string{"write", "flush", "read"}) {
		t.Fatal(err, trace)
	}
	if err := detach(); err != nil {
		t.Fatal(err)
	}
}

func TestLogProcessSharedScopedOwner(t *testing.T) {
	var raw bytes.Buffer
	flushes := 0
	var e *Engine
	e = logProcessEngine(engineLogOps{Write: func(p []byte) (int, error) {
		if e.logOwner.rawMu.TryLock() {
			e.logOwner.rawMu.Unlock()
			t.Error("raw operation lacks shared lock")
		}
		return raw.Write(p)
	}, Flush: func() error { flushes++; return nil }})
	e.Root, e.Color, e.Yes, e.ShowTrace = "literal/root", "never", true, true
	e.SettingsCache, e.Inventory = map[string]any{"key": "value"}, []Machine{{Name: "host"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scoped := e.scopedEngine(ctx)
	if scoped.Log != e.Log || scoped.logOwner != e.logOwner || scoped.Context != ctx || scoped.Root != e.Root || scoped.Color != e.Color || !scoped.Yes || !scoped.ShowTrace || scoped.Inventory[0].Name != "host" || scoped.SettingsCache["key"] != "value" {
		t.Fatal("scope copied another owner or lost fields")
	}
	var observed [][]byte
	var borrowed []byte
	failure := errors.New("subscriber refused")
	refuse := false
	detach, err := e.attachLog(func(p []byte) error {
		if flushes != len(observed)+1 || !bytes.HasSuffix(raw.Bytes(), p) {
			t.Fatal("subscriber preceded raw write and flush")
		}
		if borrowed != nil && (len(p) != len(borrowed) || &p[0] != &borrowed[0]) {
			t.Fatal("subscriber did not receive borrowed storage")
		}
		observed = append(observed, bytes.Clone(p))
		if refuse {
			return failure
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.logText("split \xc3"); err != nil {
		t.Fatal(err)
	}
	if err := e.logText("\xa9\n"); err != nil {
		t.Fatal(err)
	}
	if err := scoped.logMetadata("\xff\n"); err != nil || flushes != 2 || len(observed) != 2 {
		t.Fatal("metadata reached reader or flush", err, flushes, observed)
	}
	borrowed = []byte{0, 0xff, 0xfe, '\n'}
	if n, err := scoped.logOwner.live.Write(borrowed); n != len(borrowed) || err != nil {
		t.Fatal("borrowed write or completed raw count lost", n, err)
	}
	clear(borrowed) // callback copies survive only after the borrow has ended
	borrowed, refuse = nil, true
	if err := scoped.logText("rejected\x00\xff\n"); err != failure {
		t.Fatal("subscriber error was not propagated", err)
	}
	refuse = false
	if err := e.logText(""); err != nil {
		t.Fatal("error latched", err)
	}
	if len(observed) != 5 || string(observed[0]) != "split \xc3" || string(observed[1]) != "\xa9\n" || !bytes.Equal(observed[2], []byte{0, 0xff, 0xfe, '\n'}) || string(observed[3]) != "rejected\x00\xff\n" || len(observed[4]) != 0 {
		t.Fatal("borrowed chunks changed or were buffered", observed)
	}
	if err := detach(); err != nil {
		t.Fatal(err)
	}
	if len(e.logOwner.live.readers) != 0 {
		t.Fatal("subscriber did not detach")
	}
	if err := e.logText("\n"); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 5 || raw.String() != "split é\n\xff\n\x00\xff\xfe\nrejected\x00\xff\n\n" {
		t.Fatal("detached subscriber or raw bytes changed", raw.String(), observed)
	}
	// Distinct scoped/root writers all reach the same raw lock, with no cloned
	// subscription state. Cross-write notification order is not promised.
	var wg sync.WaitGroup
	for _, writer := range []*Engine{e, scoped} {
		wg.Add(1)
		go func(writer *Engine) {
			defer wg.Done()
			if err := writer.logText("later\n"); err != nil {
				t.Error(err)
			}
		}(writer)
	}
	wg.Wait()
	if strings.Count(raw.String(), "later\n") != 2 {
		t.Fatal("lost write")
	}
	logProcessServiceRefusal(t)
}

func TestLogProcessCaptureCountsAndOrigins(t *testing.T) {
	readE := errors.New("read failure")
	for _, callbackE := range []error{nil, io.EOF, syscall.EINTR, syscall.EAGAIN, syscall.EPIPE, syscall.EIO, context.Canceled} {
		for _, readErr := range []error{nil, io.EOF, readE} {
			owner := newCommandCapture(context.Background())
			var saved bytes.Buffer
			calls := 0
			c := &testConnectionCapture{buffer: &saved, owner: owner, stream: "stdout", chunk: func(p []byte) error {
				calls++
				if string(p) != "prefix" {
					t.Fatal(p)
				}
				return callbackE
			}}
			reader := &connectionReader{t: t, steps: []connectionRead{{"prefix", readErr}, {"sentinel", readE}}}
			// With no terminal cause, stop normally at the next exact EOF.
			if callbackE == nil && readErr == nil {
				reader.steps = reader.steps[:1]
			}
			n, err := c.ReadFrom(reader)
			want := callbackE
			if want == nil && readErr != io.EOF {
				want = readErr
			}
			if n != 6 || err != want || saved.String() != "prefix" || c.count != 6 || c.callbackErr != callbackE || calls != 1 {
				t.Fatal(n, err, c, calls)
			}
			if readErr == readE && c.readErr != readE {
				t.Fatal("read secondary lost")
			}
			if want != nil && reader.reads != 1 {
				t.Fatal("read after failure")
			}
			completion, joinedErr := owner.joined(&exec.ExitError{}, c)
			if joinedErr != want || completion.streams[0].count != 6 || completion.streams[0].stream != "stdout" {
				t.Fatal(joinedErr, completion)
			}
			owner.cancel(nil)
		}
	}
	for _, terminal := range []error{io.EOF, readE} {
		owner := newCommandCapture(context.Background())
		var saved bytes.Buffer
		c := &testConnectionCapture{buffer: &saved, owner: owner, chunk: func([]byte) error { t.Fatal("empty read callback"); return nil }}
		n, err := c.ReadFrom(&connectionReader{t: t, steps: []connectionRead{{err: terminal}}})
		if terminal == io.EOF {
			terminal = nil
		}
		if n != 0 || c.count != 0 || err != terminal {
			t.Fatal(n, err)
		}
		owner.cancel(nil)
	}
}

func TestLogProcessCauseArbitration(t *testing.T) {
	callbackE, readE := errors.New("callback"), errors.New("read")
	for _, first := range []string{"callback", "parent", "callback-canceled", "foreign-direct", "foreign-wrapped"} {
		parent, cancel := context.WithCancelCause(context.Background())
		owner := newCommandCapture(parent)
		foreign := newCommandCapture(context.Background())
		marker := &captureCause{foreign, captureCallback, callbackE}
		want := callbackE
		switch first {
		case "callback":
			owner.fail(captureCallback, callbackE)
			cancel(readE)
		case "parent":
			cancel(readE)
			owner.fail(captureCallback, callbackE)
			want = context.Canceled
		case "callback-canceled":
			owner.fail(captureCallback, context.Canceled)
			cancel(readE)
			want = context.Canceled
		case "foreign-direct":
			cancel(marker)
			owner.fail(captureRead, readE)
			want = context.Canceled
		case "foreign-wrapped":
			cancel(fmt.Errorf("outer: %w", marker))
			owner.fail(captureRead, readE)
			want = context.Canceled
		}
		completion, err := owner.joined(errors.New("derivative wait"))
		if err != want {
			t.Fatal(first, err, want)
		}
		owned := first == "callback" || first == "callback-canceled"
		if (completion.primary != nil) != owned {
			t.Fatal("origin inferred from error shape", first)
		}
		owner.cancel(nil)
		foreign.cancel(nil)
		cancel(nil)
	}
	// A blocked, never-bound child group is deliberately not involved in fail.
	// This proves nonblocking notification, not native os/exec watcher progress.
	owner := newCommandCapture(context.Background())
	unbound := &childGroup{ready: make(chan struct{})}
	done := make(chan struct{})
	go func() { owner.fail(captureRead, readE); close(done) }()
	<-done
	select {
	case <-unbound.ready:
		t.Fatal("notification mutated bind")
	default:
	}
	if context.Cause(owner.ctx).(*captureCause).owner != owner {
		t.Fatal("not this invocation")
	}
	owner.cancel(nil)
}

func TestLogProcessJoinedSelection(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := newCommandCapture(parent)
	defer owner.cancel(nil)
	first, second := errors.New("stdout callback"), errors.New("stderr read")
	var out, errs bytes.Buffer
	a := &testConnectionCapture{buffer: &out, stream: "stdout", owner: owner, chunk: func([]byte) error { return first }}
	b := &testConnectionCapture{buffer: &errs, stream: "stderr", owner: owner}
	startSecond, joinedA, joinedB := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = a.ReadFrom(&connectionReader{t: t, steps: []connectionRead{{"out", second}}})
		close(joinedA)
		close(startSecond)
	}()
	go func() {
		<-startSecond
		_, _ = b.ReadFrom(&connectionReader{t: t, steps: []connectionRead{{"err", second}}})
		close(joinedB)
	}()
	<-joinedA
	<-joinedB // actual source helper data is read only after both joins
	completion, err := owner.joined(&exec.ExitError{}, a, b)
	cancel()
	if err != first || completion.primary != first || out.String() != "out" || errs.String() != "err" || len(completion.streams) != 2 || completion.streams[0].callback != first || completion.streams[0].read != second || completion.streams[1].read != second || completion.streams[0].count != 3 || completion.streams[1].count != 3 {
		t.Fatal(err, completion)
	}
	// Completion is a joined value snapshot, not the mutable stream owner.
	a.count = 99
	if completion.streams[0].count != 3 {
		t.Fatal("receipt aliases owner")
	}
	for _, waitErr := range []error{nil, &exec.ExitError{}, errors.New("wait infrastructure")} {
		fresh := newCommandCapture(context.Background())
		_, err := fresh.joined(waitErr)
		want := waitErr
		if _, ok := waitErr.(*exec.ExitError); ok {
			want = nil
		}
		if err != want {
			t.Fatal(err, want)
		}
		fresh.cancel(nil)
	}
}

func TestLogProcessMetadataRefusal(t *testing.T) {
	failure := errors.New("metadata refusal")
	for _, call := range []func(*Engine) error{
		func(e *Engine) error { return e.LogCLI("ls", Options{}, nil) },
		func(e *Engine) error { return e.LogInputsCLI([]string{"inputs", "ls"}, []string{"literal"}) },
		func(e *Engine) error { return e.LogInputUpdateCLI(logProcessInvocation(t, "inputs", "update")) },
		func(e *Engine) error {
			return e.LogInputSetCLI(logProcessInvocation(t, "inputs", "set", "role", "rev"))
		},
		func(e *Engine) error { return e.LogTestConnectionCLI(connectionInvocation(t, "-y")) },
		func(e *Engine) error { return e.LogSSHCLI(connectionInvocation(t)) },
		func(e *Engine) error { return e.LogComparisonCLI(comparisonInvocation(t, "diff")) },
		func(e *Engine) error { return e.LogGenerationCLI(generationInvocation(t)) },
		func(e *Engine) error { return e.logMetadata("extension JSON\n") },
	} {
		writes := 0
		e := logProcessEngine(engineLogOps{Write: func(p []byte) (int, error) {
			writes++
			if len(p) == 0 {
				t.Fatal("missing assembled metadata")
			}
			return 1, failure
		}, Flush: func() error { t.Fatal("metadata flush"); return nil }})
		detach, _ := e.attachLog(func([]byte) error { t.Fatal("metadata reader"); return nil })
		if err := call(e); err != failure || writes != 1 {
			t.Fatal(err, writes)
		}
		if err := detach(); err != nil {
			t.Fatal(err)
		}
	}
	// The real Main acquisition/defer/body fault route cannot be replaced with
	// this finite projector check; it remains an explicit native entrypoint gap.
	for _, stage := range []string{"write", "flush", "reader"} {
		e := logProcessEngine(engineLogOps{Write: func(p []byte) (int, error) {
			if stage == "write" {
				return 0, failure
			}
			return len(p), nil
		}, Flush: func() error {
			if stage == "flush" {
				return failure
			}
			return nil
		}})
		detach, _ := e.attachLog(func([]byte) error {
			if stage == "reader" {
				return failure
			}
			return nil
		})
		var stderr bytes.Buffer
		warn := e.generationWarning(&stderr)
		list, err := generation.ParseHostList("h", "/profile", []byte("/system\ninvalid;/system;0\nlater;/system;0\n"), generation.Options{Warn: warn})
		if err != failure || list != nil || strings.Count(stderr.String(), "Invalid profile") != 1 {
			t.Fatal(stage, err, stderr.String())
		}
		if err := detach(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogProcessFinishBoundaries(t *testing.T) {
	failure := errors.New("start or summary")
	// Each actual adapter refuses at its start before input/child acquisition.
	for _, call := range []func(*Engine) error{
		func(e *Engine) error { _, err := e.Process([]string{"no-child"}, nil); return err },
		func(e *Engine) error { _, err := e.inputCommand(e.Context, "", []string{"no-child"}); return err },
		func(e *Engine) error {
			_, err := e.testConnectionProcess(e.Context, "", []string{"no-child"}, nil)
			return err
		},
		func(e *Engine) error {
			_, err := e.sshProcess(e.Context, "", []string{"no-child"}, sshInput{}, true, nil)
			return err
		},
	} {
		writes := 0
		e := logProcessEngine(engineLogOps{Write: func([]byte) (int, error) { writes++; return 0, failure }})
		if err := call(e); err != failure || writes != 1 {
			t.Fatal("start did not refuse", err, writes)
		}
	}
	var raw bytes.Buffer
	e := logProcessEngine(engineLogOps{Write: raw.Write, Flush: func() error { return nil }})
	e.Root = "root with spaces"
	id, start, err := e.processLogStart([]string{"nix", "eval", "literal"})
	if err != nil || !strings.Contains(raw.String(), "Running cd root\\ with\\ spaces && nix eval literal\n") {
		t.Fatal(err, raw.String())
	}
	var captured bytes.Buffer
	c := &testConnectionCapture{buffer: &captured, chunk: func(p []byte) error { return e.logText("[" + id + "] \t" + strings.TrimSuffix(string(p), "\n") + "\n") }}
	if _, err := c.ReadFrom(&connectionReader{t: t, steps: []connectionRead{{"one\n", nil}, {"two\n\n", io.EOF}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.processLogFinish(id, start, Result{ExitCode: 7, Stdout: []byte("DO NOT REPLAY"), Stderr: []byte("DO NOT REPLAY")}); err != nil {
		t.Fatal(err)
	}
	text := raw.String()
	if strings.Count(text, "Finished") != 1 || strings.Count(text, "one") != 1 || !strings.Contains(text, "\ttwo\n\n") || strings.Contains(text, "REPLAY") || !strings.Contains(text, "exit status 7 (failed)") {
		t.Fatal(text)
	}
	e.logOwner.ops.Write = func([]byte) (int, error) { return 0, failure }
	if err := e.processLogFinish(id, time.Now(), Result{ExitCode: 7}); err != failure {
		t.Fatal("summary failure lost", err)
	}
	// Actual inputCommand's completed-child/ExitError branch needs a native
	// child. No second runner is added to emulate that branch in this script.
}

func TestLogProcessOuterStatusLayers(t *testing.T) {
	failure := &exec.Error{Name: "cat", Err: exec.ErrNotFound}
	parent, cancel := context.WithCancel(context.Background())
	owner := newCommandCapture(parent)
	defer owner.cancel(nil)
	owner.fail(captureRead, failure)
	receipt, err := owner.joined(&exec.ExitError{})
	if err != failure {
		t.Fatal(err)
	}
	cancel()
	r := Result{ExitCode: 7, capture: receipt}
	if _, err := testConnectionStatus(parent, []string{"cat", "/proc/uptime"}, r, failure); err != failure {
		t.Fatal("missing-cat shape replaced capture", err)
	}
	if _, err := sshStatus(parent, r, failure); err != failure {
		t.Fatal(err)
	}
	for _, label := range []string{"start", "summary", "acquisition", "scripted"} {
		ordinary := errors.New(label)
		if _, err := testConnectionStatus(parent, nil, Result{ExitCode: 0}, ordinary); err != context.Canceled {
			t.Fatal(label, err)
		}
		if _, err := sshStatus(parent, Result{ExitCode: 0}, ordinary); err != context.Canceled {
			t.Fatal(label, err)
		}
	}
	if _, err := testConnectionStatus(context.Background(), []string{"cat", "/proc/uptime"}, Result{}, failure); err == failure || err.Error() != "No such file or directory - cat" {
		t.Fatal(err)
	}
	if n, err := sshStatus(context.Background(), Result{ExitCode: -1, signal: syscall.SIGTERM}, nil); n != int(syscall.SIGTERM) || err != nil {
		t.Fatal(n, err)
	}
	logProcessQueryRefusal(t)
}

func TestLogProcessCloseOnceAndReporting(t *testing.T) {
	failure := errors.New("cleanup")
	for _, row := range []struct {
		success       bool
		close, remove error
		wantRemove    int
	}{{false, nil, nil, 0}, {true, failure, nil, 0}, {true, nil, failure, 1}, {true, nil, nil, 1}} {
		closes, removes := 0, 0
		e := logProcessEngine(engineLogOps{Close: func() error { closes++; return row.close }, Remove: func() error { removes++; return row.remove }})
		want := row.close
		if want == nil && row.success {
			want = row.remove
		}
		if err := e.CloseLog(row.success); err != want {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := e.CloseLog(!row.success); err != want {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if closes != 1 || removes != row.wantRemove {
			t.Fatal("cleanup repeated/choice changed", closes, removes)
		}
		if err := e.OpenLog("no-reopen"); err == nil || err.Error() != "log is already acquired" {
			t.Fatal(err)
		}
	}
	for _, e := range []*Engine{{Log: &os.File{}}, {logOwner: newEngineLog(&os.File{}, engineLogOps{})}, {Log: &os.File{}, logOwner: newEngineLog(&os.File{}, engineLogOps{})}} {
		if err := e.logText("x"); err != errLogOwnerUnavailable {
			t.Fatal(err)
		}
		if err := e.logMetadata("x"); err != errLogOwnerUnavailable {
			t.Fatal(err)
		}
		if err := e.CloseLog(true); err != errLogOwnerUnavailable {
			t.Fatal(err)
		}
		if detach, err := e.attachLog(func([]byte) error { return nil }); detach != nil || err != errLogOwnerUnavailable {
			t.Fatal(err)
		}
	}
	disabled := &Engine{}
	if err := disabled.logText("x"); err != nil {
		t.Fatal(err)
	}
	if err := disabled.logMetadata("x"); err != nil {
		t.Fatal(err)
	}
	if err := disabled.CloseLog(true); err != nil {
		t.Fatal(err)
	}
	detach, err := disabled.attachLog(nil)
	if err != nil || detach == nil || detach() != nil {
		t.Fatal(err)
	}
	// A child's signal-shaped status is still a forwarded status. Only the
	// entrypoint's own received-signal branch selects cancellation exit 1.
	for _, status := range []int{0, 1, 7, 23, 64, 130, 143} {
		w := &generationWarningWriter{count: -1}
		if code, err := LogCleanupStatus(status, nil, w); code != status || err != nil || w.body.Len() != 0 {
			t.Fatal(status, code, err, w.body.String())
		}
		for _, row := range []struct {
			n         int
			err, want error
		}{{-1, nil, nil}, {0, failure, failure}, {1, nil, io.ErrShortWrite}} {
			w := &generationWarningWriter{count: row.n, err: row.err}
			code, err := LogCleanupStatus(status, failure, w)
			want := status
			if want == 0 {
				want = 1
			}
			if code != want || err != row.want || w.body.String() != "log cleanup: cleanup\n" {
				t.Fatal(code, err, w.body.String())
			}
		}
	}
}

func TestLogProcessCallerRescueBoundaries(t *testing.T) {
	ordinary := errors.New("log body refused")
	st := Status{Machine: Machine{Name: "h"}, Target: comparisonInfo(t, `{"a":{"rev":"new","url":"repo"},"b":{"rev":"new","url":"repo"}}`), Deployed: comparisonInfo(t, `{"a":"old","b":"old"}`)}
	for _, failure := range []error{ordinary, context.Canceled, fmt.Errorf("wrapped: %w", context.DeadlineExceeded)} {
		calls := 0
		sink := &comparisonSink{}
		err := comparisonRender(context.Background(), sink, comparisonInvocation(t, "changelog"), nil, st, func(context.Context, string, string, string, []string) (string, error) {
			calls++
			return "partial", failure
		})
		if failure == ordinary {
			if err != nil || calls != 2 || strings.Count(sink.String(), ordinary.Error()) != 2 {
				t.Fatal(err, calls, sink.String())
			}
		} else if err != failure || calls != 1 {
			t.Fatal(err, calls)
		}
		change := inputs.Change{Name: "a", Old: inputs.Info{Rev: "old", ShortRev: "old", URL: "repo"}, New: inputs.Info{Rev: "new", ShortRev: "new", URL: "repo"}}
		text, err := inputs.BuildCommitMessage(context.Background(), []inputs.Change{change}, true, false, func(context.Context, string, string, string) (string, error) { return "partial", failure })
		if failure == ordinary {
			if err != nil || !strings.Contains(text, "(changelog unavailable: log body refused)") {
				t.Fatal(text, err)
			}
		} else if err != failure {
			t.Fatal(err)
		}
	}
	// The actual mutation caller deliberately overrides deps.Run on parent
	// cancellation, without an inputs-library capture classifier or retry.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loads, runs := 0, 0
	_, err := inputs.Update(ctx, t.TempDir(), []any{"a"}, inputs.UpdateOptions{}, inputs.UpdateDeps{
		Argument: func(v any) string { return v.(string) },
		Load:     func(string, bool) (*inputs.Lock, error) { loads++; return nil, nil },
		Run: func(context.Context, string, []string) (inputs.Output, error) {
			runs++
			cancel()
			return inputs.Output{}, ordinary
		},
	})
	if err != context.Canceled || loads != 1 || runs != 1 {
		t.Fatal(err, loads, runs)
	}
}
