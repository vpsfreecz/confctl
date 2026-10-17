package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/ui"
)

type liveTestWriter struct {
	mu       sync.Mutex
	b        bytes.Buffer
	writeErr error
	short    bool
	writes   int
	after    int
}

func (w *liveTestWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writeErr != nil && w.writes > w.after {
		return 0, w.writeErr
	}
	if w.short && w.writes > w.after {
		return 0, nil
	}
	return w.b.Write(b)
}
func (w *liveTestWriter) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.b.String() }

func liveTestSession(t *testing.T, out io.Writer, raw bool, tail *liveLogTail, onError func(error)) *liveSession {
	t.Helper()
	m, err := ui.NewLiveModel(ui.LiveHeader{Operation: "Fixture live operation"}, []ui.LiveRow{{ID: "all", Label: "Tasks", Current: big.NewInt(0), Total: big.NewInt(2), State: ui.LiveRunning}}, false)
	if err != nil {
		t.Fatal(err)
	}
	v := newLiveSession(out, nil, tail, m, raw, onError)
	go v.loop()
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func TestLiveViewPromptBarrierKeepsQueuedInputAndDoesNotPaint(t *testing.T) {
	out := &liveTestWriter{}
	v := liveTestSession(t, out, false, nil, nil)
	// commandInput retains one owner and buffered answers through suspension.
	_, line := commandInput(strings.NewReader("y\ny\n"))
	if err := v.WithPrompt(context.Background(), func() error {
		before := out.text()
		if err := v.Line("held during prompt"); err != nil {
			return err
		}
		if err := v.Advance("all"); err != nil {
			return err
		}
		// A resize and a drain/render barrier must acknowledge without painting.
		v.resize <- os.Interrupt
		if err := v.Flush(context.Background()); err != nil {
			return err
		}
		if out.text() != before {
			return errors.New("painting crossed prompt acknowledgment")
		}
		return testConnectionConfirm(context.Background(), line, io.Discard)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.text(), "held during prompt") {
		t.Fatal("resume did not repaint", out.text())
	}
	if err := v.WithPrompt(context.Background(), func() error { return testConnectionConfirm(context.Background(), line, io.Discard) }); err != nil {
		t.Fatal("queued permission lost", err)
	}
	refusal := errors.New("prompt refused")
	if err := v.WithPrompt(context.Background(), func() error { return refusal }); err != refusal || v.Err() != nil {
		t.Fatal(err, v.Err())
	}
	before := out.text()
	if err := v.Line("retained after refusal"); err != nil {
		t.Fatal(err)
	}
	if err := v.Flush(context.Background()); err != nil || out.text() != before {
		t.Fatal("refusal resumed painting", err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.text(), "retained after refusal") {
		t.Fatal("final teardown lost suspended data")
	}
}

func TestLiveViewBindingFailureIsFirstAndNotificationJoins(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseCallback()
	var calls atomic.Int32
	v := liveTestSession(t, io.Discard, false, nil, func(error) { calls.Add(1); close(entered); <-release })
	updateDone := make(chan error, 1)
	go func() { updateDone <- v.Advance("unknown") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	closed := make(chan error, 1)
	go func() { closed <- v.Close() }()
	// Close cannot outlive an admitted onError, even though it holds no lock.
	select {
	case err := <-closed:
		releaseCallback()
		t.Fatal("close did not join callback", err)
	default:
	}
	if err := v.Line("later"); err != v.Err() {
		t.Fatal("first error replaced", err, v.Err())
	}
	releaseCallback()
	first := <-updateDone
	select {
	case err := <-closed:
		if err != first {
			t.Fatal(err, first)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not join")
	}
	if calls.Load() != 1 || v.Close() != first {
		t.Fatal("non-idempotent failure/close", calls.Load())
	}
	select {
	case <-v.Failed():
	default:
		t.Fatal("failure signal missing")
	}
}

func TestLiveViewOutputErrorsAndCancellationAreDistinct(t *testing.T) {
	for _, short := range []bool{false, true} {
		sentinel := errors.New("terminal write")
		out := &liveTestWriter{short: short}
		if !short {
			out.writeErr = sentinel
		}
		var calls atomic.Int32
		v := liveTestSession(t, out, false, nil, func(error) { calls.Add(1) })
		err := v.Flush(context.Background())
		want := sentinel
		if short {
			want = io.ErrShortWrite
		}
		if err != want || v.Close() != want || calls.Load() != 1 {
			t.Fatal(err, v.Err(), calls.Load())
		}
	}
	out := &liveTestWriter{}
	v := liveTestSession(t, out, false, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := v.WithPrompt(ctx, func() error { called = true; return nil }); err != context.Canceled || called || v.Err() != nil {
		t.Fatal(err, called, v.Err())
	}
	if err := v.Flush(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.text(), "Tasks") {
		t.Fatal("cancellation skipped final frame")
	}
}

func TestLiveViewRawFinalDrainAndBindingRefusal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "checked.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	e := &Engine{Log: f, logOwner: nativeEngineLog(f)}
	tail, err := e.openLiveLogTail()
	if err != nil {
		t.Fatal(err)
	}
	out := &liveTestWriter{}
	v := liveTestSession(t, out, true, tail, nil)
	if err := e.logText("final partial"); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	frame, after := liveLastPaint(t, out.text(), 24)
	if !strings.Contains(frame, "final partial") || after != "" {
		t.Fatal("close failed final file drain", out.text())
	}
	if _, err := tail.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("final drain retained its read endpoint")
	}
	if _, err := f.WriteString("writer still borrowed"); err != nil {
		t.Fatal("view closed writer", err)
	}
	v = liveTestSession(t, io.Discard, true, nil, nil)
	if err := v.Line("duplicate"); err == nil || v.Err() != err {
		t.Fatal("raw/semantic binding accepted", err)
	}
}

func liveLastPaint(t *testing.T, text string, rows int) (frame, after string) {
	t.Helper()
	marker := fmt.Sprintf("\x1b[%d;1H", max(rows, 1))
	if rows > 1 {
		marker += "\x1b[2K"
	}
	end := strings.LastIndex(text, marker)
	if end < 0 {
		t.Fatal("no complete paint", text)
	}
	start := strings.LastIndex(text[:end], "\x1b[H")
	if start < 0 {
		t.Fatal("no frame home", text)
	}
	return text[start : end+len(marker)], text[end+len(marker):]
}

func TestLiveViewConcurrentUpdatesKeepFinalPanelAndOmittedTasks(t *testing.T) {
	out := &liveTestWriter{}
	v := liveTestSession(t, out, false, nil, nil)
	if err := v.SetRows(ui.LiveRow{ID: "all", Label: "All tasks", Current: big.NewInt(0), Total: big.NewInt(80), State: ui.LiveRunning}); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for j := 0; j < 10; j++ {
				id := fmt.Sprintf("host-%d-%d", i, j)
				label := id + strings.Repeat(" full task label", 10)
				if err := v.SetRows(ui.LiveRow{ID: id, Label: label, State: ui.LiveSucceeded}); err != nil {
					t.Error(err)
				}
				if err := v.Advance("all"); err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Wait()
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	frame, after := liveLastPaint(t, out.text(), v.rows)
	// Registration order may vary between workers, but the final viewport and
	// its omitted identities must come from one selection, without header replay.
	expected, omitted := v.model.FrameWithRemainder(v.rows, v.cols, v.tick)
	for _, line := range expected {
		if !strings.Contains(frame, "\x1b[2K\r"+line) {
			t.Fatal("final selected row missing", line, frame)
		}
	}
	if after != "\r"+strings.Join(omitted, "\n\r")+"\n\r" || len(omitted) != 76 {
		t.Fatal("wrong omitted-only output", after)
	}
	for _, line := range omitted {
		if strings.Count(after, line) != 1 {
			t.Fatal("omitted task duplicated", line)
		}
	}
	if strings.Contains(after, "Fixture live operation:") || strings.Contains(after, "All tasks") || !strings.Contains(frame, "80/80 (100%)") {
		t.Fatal("coalescing lost counts")
	}
}

func TestLiveLogTailUsesFileOrderAndRefusesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e := &Engine{Log: f, logOwner: nativeEngineLog(f)}
	tail, err := e.openLiveLogTail()
	if err != nil {
		t.Fatal(err)
	}
	defer tail.close()
	first, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	detach, err := e.attachLog(func(b []byte) error {
		if string(b) == "first\n" {
			close(first)
			<-release
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.logText("first\n") }()
	joined := false
	defer func() {
		releaseCallback()
		if !joined {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("log writer did not join")
			}
		}
		if err := detach(); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first write not admitted")
	}
	if err := e.logText("second\n"); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	end, err := tail.end()
	if err != nil {
		t.Fatal(err)
	}
	if err := tail.drain(end, func(b []byte) { got.Write(b) }); err != nil {
		t.Fatal(err)
	}
	releaseCallback()
	err = <-done
	joined = true
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "first\nsecond\n" {
		t.Fatal("notification order replaced file order", got.String())
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if replacement, err := e.openLiveLogTail(); err == nil {
		_ = replacement.close()
		t.Fatal("replacement accepted")
	}
	e.logOwner = nil
	if _, err := e.openLiveLogTail(); err != errLogOwnerUnavailable {
		t.Fatal("unchecked owner accepted", err)
	}
}

func TestLiveViewReservationPreservesPreludeAndCompletePaintBoundary(t *testing.T) {
	out := &liveTestWriter{}
	// The caller's prelude is written before the model exists. It never enters
	// header, raw/semantic feed or task snapshots, so repaint cannot replay it.
	prelude := "Building machines\nnode-unique\nwith inputs\nos=/input-unique\n"
	_, _ = out.Write([]byte(prelude))
	model, err := ui.NewLiveModel(ui.LiveHeader{Command: "confctl build -y", Operation: "Build group", Detail: "1/1 (1 machines)", LogPath: "/log"}, []ui.LiveRow{{ID: "build", Label: "Building", State: ui.LiveRunning}}, false)
	if err != nil {
		t.Fatal(err)
	}
	v := newLiveSession(out, nil, nil, model, false, nil)
	v.resizeGeometry(32, 160)
	if err := v.paint(false); err != nil {
		t.Fatal(err)
	}
	text := out.text()
	prefix := prelude + strings.Repeat("\n", 32) + "\x1b[H"
	if !strings.HasPrefix(text, prefix) {
		t.Fatal("initial home overwrote unreserved prelude", text)
	}
	if !strings.HasSuffix(text, "\x1b[32;1H\x1b[2K") || strings.Count(text, "\x1b[2K\r") != 31 {
		t.Fatal("incomplete normal paint boundary", text)
	}
	before := len(text)
	if err := v.paint(false); err != nil {
		t.Fatal(err)
	}
	repeated := out.text()[before:]
	if !strings.HasPrefix(repeated, "\x1b[H") || strings.HasPrefix(repeated, "\n") {
		t.Fatal("tick reserved again", repeated)
	}
	v.resizeGeometry(32, 160)
	before = len(out.text())
	if err := v.paint(false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.text()[before:], "\x1b[H") {
		t.Fatal("unchanged geometry reserved again")
	}
	v.resizeGeometry(40, 120)
	before = len(out.text())
	if err := v.paint(false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.text()[before:], strings.Repeat("\n", 40)+"\x1b[H") {
		t.Fatal("growth did not protect existing scrollback")
	}
	if err := v.paint(true); err != nil {
		t.Fatal(err)
	}
	text = out.text()
	if strings.Count(text, "node-unique") != 1 || strings.Count(text, "/input-unique") != 1 {
		t.Fatal("prelude repeated in repaint/remainder", text)
	}
	v.resizeGeometry(1, 1)
	before = len(text)
	if err := v.paint(false); err != nil {
		t.Fatal(err)
	}
	tiny := out.text()[before:]
	if !strings.HasSuffix(tiny, "\x1b[1;1H") || strings.HasSuffix(tiny, "\x1b[1;1H\x1b[2K") {
		t.Fatal("tiny completion erased its sole row", tiny)
	}
}

func TestLiveViewResizeDefersReservationUntilPromptReturns(t *testing.T) {
	for _, decision := range []string{"accept", "refuse", "cancel"} {
		t.Run(decision, func(t *testing.T) {
			_, console := openCommitTestPTY(t)
			sshPTYSize(t, console, 24, 80)
			out := &liveTestWriter{}
			model, _ := ui.NewLiveModel(ui.LiveHeader{Operation: "Copy", Target: "node"}, nil, false)
			v := newLiveSession(out, console, nil, model, false, nil)
			v.fetchSize()
			if err := v.paint(false); err != nil {
				t.Fatal(err)
			}
			go v.loop()
			t.Cleanup(func() { _ = v.Close() })
			refusal := errors.New("prompt refusal")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := v.WithPrompt(ctx, func() error {
				before := out.text()
				sshPTYSize(t, console, 32, 160)
				v.resize <- os.Interrupt
				// Barrier does not guarantee resize selection order, so wait for the
				// renderer's acknowledged drain, without assuming a timer is a barrier.
				deadline := time.Now().Add(time.Second)
				consumed := false
				for time.Now().Before(deadline) {
					if err := v.Flush(context.Background()); err != nil {
						return err
					}
					// Only the renderer consumes this notification; it processes the
					// new dimensions before acknowledging a subsequent request.
					if len(v.resize) == 0 {
						consumed = true
						break
					}
				}
				if !consumed {
					return errors.New("resize notification was not consumed")
				}
				if out.text() != before {
					return errors.New("resize scrolled over a live prompt")
				}
				if decision == "refuse" {
					return refusal
				}
				if decision == "cancel" {
					cancel()
				}
				return nil
			})
			want := map[string]error{"accept": nil, "refuse": refusal, "cancel": context.Canceled}[decision]
			if err != want {
				t.Fatal(err)
			}
			beforeClose := out.text()
			if decision != "accept" && strings.Contains(beforeClose, strings.Repeat("\n", 32)) {
				t.Fatal("refused prompt resumed reservation")
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if strings.Count(out.text(), strings.Repeat("\n", 32)) != 1 {
				t.Fatal("resize reservation was skipped/repeated", out.text())
			}
			frame, after := liveLastPaint(t, out.text(), 32)
			if !strings.Contains(frame, "Copy: node") || after != "" {
				t.Fatal("resized final context replayed/lost", frame, after)
			}
		})
	}
}

func TestLiveViewFinalOneRowAdvancesBeforeRemainderOrResults(t *testing.T) {
	for _, withTasks := range []bool{false, true} {
		out := &liveTestWriter{}
		model, _ := ui.NewLiveModel(ui.LiveHeader{}, nil, false)
		if withTasks {
			_ = model.SetRows(ui.LiveRow{ID: "a", Label: "omitted task", State: ui.LiveIncomplete})
		}
		v := newLiveSession(out, nil, nil, model, false, nil)
		v.resizeGeometry(1, 80)
		if err := v.paint(true); err != nil {
			t.Fatal(err)
		}
		_, after := liveLastPaint(t, out.text(), 1)
		want := "\n\r"
		if withTasks {
			want += "\romitted task [incomplete]\n\r"
		}
		if after != want {
			t.Fatal("one-row final paint overwritten", after, want)
		}
	}
}

func TestLiveViewFinalPaintFailureHasNoRemainderFallback(t *testing.T) {
	out := &liveTestWriter{}
	model, _ := ui.NewLiveModel(ui.LiveHeader{Command: "confctl copy", LogPath: "/logs/full"}, nil, false)
	for i := range 10 {
		_ = model.SetRows(ui.LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprintf("task-%02d", i), State: ui.LiveFailed})
	}
	v := newLiveSession(out, nil, nil, model, false, nil)
	if err := v.paint(false); err != nil {
		t.Fatal(err)
	}
	before, writes := out.text(), out.writes
	sentinel := errors.New("final paint")
	out.writeErr = sentinel
	err := v.paint(true)
	v.fail(err)
	if err != sentinel || v.Err() != sentinel || out.text() != before || out.writes != writes+1 {
		t.Fatal("failed final frame generated extra output", err, v.Err())
	}
	if err := v.paint(true); err != nil || out.writes != writes+1 {
		t.Fatal("output failure retried/replayed summary", err)
	}
}

func TestLiveViewRemainderWriteFailureStillClosesAndNotifiesOnce(t *testing.T) {
	for _, short := range []bool{false, true} {
		out := &liveTestWriter{after: 2, short: short}
		want := io.ErrShortWrite
		if !short {
			want = errors.New("remainder write")
			out.writeErr = want
		}
		model, _ := ui.NewLiveModel(ui.LiveHeader{Operation: "Copy"}, nil, false)
		for i := range 10 {
			_ = model.SetRows(ui.LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprintf("task-%02d", i), State: ui.LiveIncomplete})
		}
		var calls atomic.Int32
		v := newLiveSession(out, nil, nil, model, false, func(error) { calls.Add(1) })
		go v.loop()
		if err := v.Close(); err != want || calls.Load() != 1 {
			t.Fatal("remainder error/notification lost", err, calls.Load())
		}
		_, after := liveLastPaint(t, out.text(), 24)
		if after != "" || out.writes != 3 || v.Close() != want || out.writes != 3 {
			t.Fatal("failed remainder retried/replaced frame", after, out.writes)
		}
		select {
		case <-v.done:
		default:
			t.Fatal("output failure left renderer running")
		}
	}
}

func TestInvocationTimingLivePaintCatchesUpAfterPromptWithoutHeaderReplay(t *testing.T) {
	c := &invocationTestClock{now: time.Now()}
	ledger := newInvocationTiming(c.read(), c.read)
	out := &liveTestWriter{}
	model, _ := ui.NewLiveModel(ui.LiveHeader{Command: "confctl ssh", Operation: "SSH", LogPath: "/log"}, nil, false)
	v := newLiveSession(out, nil, nil, model, false, nil)
	v.timing = ledger
	go v.loop()
	t.Cleanup(func() { _ = v.Close() })
	if err := v.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.text(), "Elapsed: 0s") {
		t.Fatal(out.text())
	}
	if err := v.WithPrompt(context.Background(), func() error {
		before := out.text()
		c.advance(65 * time.Second)
		if err := v.Line("retained during prompt"); err != nil {
			return err
		}
		if err := v.Flush(context.Background()); err != nil {
			return err
		}
		if out.text() != before {
			return errors.New("timing repainted during prompt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	frame, after := liveLastPaint(t, out.text(), 24)
	if !strings.Contains(frame, "Started: "+c.read().Add(-65*time.Second).Local().Format(time.RFC3339)+"  Elapsed: 1m 5s") || !strings.Contains(frame, "retained during prompt") || after != "" {
		t.Fatal("elapsed did not catch up/teardown replayed header", frame, after)
	}
	if ledger.Snapshot().Prompt != 0 {
		t.Fatal("whole prompt suspension was counted as a read")
	}
}
