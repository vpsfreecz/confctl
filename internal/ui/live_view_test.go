package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type liveWaitRequest struct {
	ctx      context.Context
	duration time.Duration
	proceed  chan struct{}
}
type liveWaitDriver struct{ requests chan liveWaitRequest }

func newLiveWaitDriver() *liveWaitDriver {
	return &liveWaitDriver{requests: make(chan liveWaitRequest, 16)}
}
func (d *liveWaitDriver) wait(ctx context.Context, duration time.Duration) error {
	req := liveWaitRequest{ctx: ctx, duration: duration, proceed: make(chan struct{})}
	d.requests <- req
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-req.proceed:
		return nil
	}
}
func liveEnabledOptions(out io.Writer) LiveViewOptions {
	return LiveViewOptions{Enabled: true, Output: out, Console: &LiveConsole{}}
}
func liveEnabledOps(driver *liveWaitDriver) LiveViewOps {
	return LiveViewOps{
		Dimensions:      func() (int, int, bool, error) { return 2, 8, true, nil },
		Wait:            driver.wait,
		SubscribeResize: func(func() error) (func() error, error) { return func() error { return nil }, nil },
	}
}

func TestLiveDisabledReachedLoggerFeedAndFlush(t *testing.T) {
	var reader func([]byte) error
	events := []string{}
	invalidTitle := "\xff"
	ctx := context.WithValue(context.Background(), struct{}{}, "caller")
	sentinel := errors.New("flush")
	v, err := OpenLiveView(LiveViewOptions{Title: &invalidTitle}, LiveViewOps{
		Dimensions:      func() (int, int, bool, error) { t.Fatal("disabled dimensions"); return 0, 0, false, nil },
		SubscribeResize: func(func() error) (func() error, error) { t.Fatal("disabled subscribe"); return nil, nil },
		Wait: func(got context.Context, duration time.Duration) error {
			if got != ctx || duration != time.Second {
				t.Fatal(got, duration)
			}
			events = append(events, "flush-1s")
			return sentinel
		},
		AttachLogger: func(callback func([]byte) error) (func() error, error) {
			events = append(events, "attach")
			reader = callback
			return func() error { events = append(events, "detach"); return nil }, nil
		},
	})
	if err != nil || v.feederDone != nil || v.rendererDone != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"\x00\t a\tb\n c \r\x00", "", "\u00a0x\u00a0"} {
		if err := v.FeedLine(line); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(v.queue, []string{"a\tb\n c", "", "\u00a0x\u00a0"}) {
		t.Fatal(v.queue)
	}
	if err := v.FeedLine("\xff"); err == nil || len(v.queue) != 3 {
		t.Fatal("invalid line effects", err)
	}
	// The logger copies a split UTF-8 sequence, then feeds only complete LF lines.
	chunk := []byte("split \xc3")
	if err := reader(chunk); err != nil {
		t.Fatal(err)
	}
	chunk[0] = 'X'
	if err := reader([]byte("\xa9\nsecond\nunterminated")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.queue[3:], []string{"split é", "second"}) || string(v.pending) != "unterminated" {
		t.Fatal(v.queue, string(v.pending))
	}
	if err := v.Flush(ctx); err != sentinel {
		t.Fatal(err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"attach", "flush-1s", "detach"}) || v.pending != nil || v.queue != nil {
		t.Fatal(events)
	}
	// Direct disabled mode needs neither terminal operations nor logger callbacks.
	direct, err := OpenLiveView(LiveViewOptions{}, LiveViewOps{})
	if err != nil || direct.detach != nil {
		t.Fatal(err)
	}
	if err := direct.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLoggerRawWriteBeforeReaderAndInvalidLineSuffix(t *testing.T) {
	var reader func([]byte) error
	v, err := OpenLiveView(LiveViewOptions{}, LiveViewOps{AttachLogger: func(fn func([]byte) error) (func() error, error) {
		reader = fn
		return func() error { return nil }, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	events := []string{}
	chunk := []byte("first\n\xff\nlater\nlast")
	raw.Write(chunk)
	events = append(events, "raw-write", "raw-flush")
	err = reader(chunk)
	events = append(events, "reader")
	if err == nil || !reflect.DeepEqual(events, []string{"raw-write", "raw-flush", "reader"}) || raw.String() != string(chunk) || !reflect.DeepEqual(v.queue, []string{"first"}) || string(v.pending) != "later\nlast" {
		t.Fatal(events, v.queue, string(v.pending), err)
	}
	if err := reader(nil); err != nil || !reflect.DeepEqual(v.queue, []string{"first", "later"}) || string(v.pending) != "last" {
		t.Fatal(v.queue, string(v.pending), err)
	}
	if err := v.Stop(); err != nil || v.pending != nil {
		t.Fatal(err)
	}
}

func TestLiveEnabledStartupEmptyFrameAndCopiedTitle(t *testing.T) {
	w := &liveWriteRecorder{}
	driver := newLiveWaitDriver()
	options := liveEnabledOptions(w)
	title := "T"
	options.Title, options.Header = &title, "a\nb"
	ops := liveEnabledOps(driver)
	events := []string{}
	ops.Dimensions = func() (int, int, bool, error) { events = append(events, "dimensions"); return 2, 8, true, nil }
	ops.SubscribeResize = func(fn func() error) (func() error, error) {
		if len(w.trace()) != 33 {
			t.Fatalf("subscription before complete empty frame: %q", w.trace())
		}
		events = append(events, "subscribe")
		return func() error { events = append(events, "release"); return nil }, nil
	}
	ops.AttachLogger = func(func([]byte) error) (func() error, error) {
		events = append(events, "attach")
		return func() error { events = append(events, "detach"); return nil }, nil
	}
	v, err := OpenLiveView(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	req := <-driver.requests
	if req.duration != 100*time.Millisecond || req.ctx == nil {
		t.Fatal(req)
	}
	want := "\n\n\x1b[2J\x1b[H\x1b[2K\x1b[1Ga\n\x1b[2K\x1b[1Gb\x1b[2K\x1b[1GT ----->\n" + strings.Repeat("\x1b[2K\x1b[1G\n", 10) + "\x1b[2K\x1b[1G<-------\n\n"
	if w.text() != want || !reflect.DeepEqual(events, []string{"dimensions", "subscribe", "attach"}) {
		t.Fatal(w.text(), events)
	}
	title = "mutated"
	if v.title != "T" || v.options.Title != nil {
		t.Fatal("retained title pointer")
	}
	// The first clean iteration still waits, and another clean iteration repeats
	// that post-iteration delay without writing a frame.
	close(req.proceed)
	second := <-driver.requests
	if second.duration != 100*time.Millisecond || w.text() != want {
		t.Fatal(second.duration, w.text())
	}
	if err := v.Stop(); err != nil || w.text() != want || !reflect.DeepEqual(events, []string{"dimensions", "subscribe", "attach", "detach", "release"}) {
		t.Fatal(events, err)
	}
}

// Observe the feeder's existing condition wait through its private locker.
// No production scheduler/test hook is added and no elapsed-time polling occurs.
type liveIdleLocker struct {
	mu   *sync.Mutex
	idle chan struct{}
}

func (l *liveIdleLocker) Lock()   { l.mu.Lock() }
func (l *liveIdleLocker) Unlock() { l.mu.Unlock(); l.idle <- struct{}{} }
func startTestLiveFeeder(v *LiveView) chan struct{} {
	idle := make(chan struct{}, 16)
	v.queueReady = sync.NewCond(&liveIdleLocker{mu: &v.mu, idle: idle})
	v.feederDone = make(chan struct{})
	go func() { defer close(v.feederDone); v.feedLoop() }()
	return idle
}

func TestLiveFeederFIFOExpansionAndResizeRetention(t *testing.T) {
	w := &liveWriteRecorder{}
	v := testBareLive(t, "", "T", 80, 20, w)
	v.options.Auto = true
	for _, line := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "a\tb", ""} {
		if err := v.FeedLine(line); err != nil {
			t.Fatal(err)
		}
	}
	idle := startTestLiveFeeder(v)
	<-idle
	v.mu.Lock()
	if v.generation.Int64() != 15 || len(v.lines) != 15 || v.lines[13] != "a  b" || v.lines[14] != "" {
		t.Fatal(v.lines, &v.generation)
	}
	v.mu.Unlock()
	v.ops.Dimensions = func() (int, int, bool, error) { return 10, 80, true, nil }
	if err := v.resize(); err != nil {
		t.Fatal(err)
	}
	if len(v.snapshot().lines) != 15 {
		t.Fatal("resize pruned prematurely")
	}
	if err := v.renderIteration(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.text(), "\x1b[2K\x1b[1G0\n") || strings.Contains(w.text(), "\x1b[2K\x1b[1G10\n") {
		t.Fatal(w.text())
	}
	if err := v.FeedLine("tail"); err != nil {
		t.Fatal(err)
	}
	<-idle
	s := v.snapshot()
	if !reflect.DeepEqual(s.lines, []string{"6", "7", "8", "9", "10", "11", "12", "a  b", "", "tail"}) || s.generation.Int64() != 16 {
		t.Fatal(s.lines, &s.generation)
	}
	v.ops.Dimensions = func() (int, int, bool, error) { return 25, 80, true, nil }
	if err := v.resize(); err != nil || len(v.snapshot().lines) != 10 {
		t.Fatal("growth restored dropped lines", err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveBackgroundErrorsAreStopOnlyAndHostWorkContinues(t *testing.T) {
	for _, phase := range []string{"write", "wait"} {
		t.Run(phase, func(t *testing.T) {
			failure := errors.New(phase)
			w := &liveWriteRecorder{}
			driver := newLiveWaitDriver()
			ops := liveEnabledOps(driver)
			if phase == "wait" {
				ops.Wait = func(ctx context.Context, d time.Duration) error {
					if d == time.Second {
						return nil
					}
					return failure
				}
			}
			v, err := OpenLiveView(liveEnabledOptions(w), ops)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "write" {
				req := <-driver.requests
				w.mu.Lock()
				w.failAt = len(w.writes) + 1
				w.err = failure
				w.mu.Unlock()
				v.mu.Lock()
				v.generation.SetInt64(1)
				v.mu.Unlock()
				close(req.proceed)
			}
			<-v.rendererDone
			if v.rendererErr != failure {
				t.Fatal(v.rendererErr)
			}
			hostCalls := 0
			if err := v.FeedLine("host still proceeds"); err != nil {
				t.Fatal(err)
			}
			hostCalls++
			if phase == "write" {
				done := make(chan error, 1)
				go func() { done <- v.Flush(context.Background()) }()
				req := <-driver.requests
				if req.duration != time.Second {
					t.Fatal(req.duration)
				}
				close(req.proceed)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			} else if err := v.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			hostCalls++
			if err := v.Stop(); err != failure || hostCalls != 2 {
				t.Fatal(err, hostCalls)
			}
			if err := v.Stop(); err != failure {
				t.Fatal(err)
			}
		})
	}
}

func TestLivePartialOpenKeepsOperationPrimaryAndCleansAllResources(t *testing.T) {
	for _, phase := range []string{"dimensions", "init", "title", "subscribe", "attach"} {
		t.Run(phase, func(t *testing.T) {
			failure, cleanup := errors.New(phase), errors.New("cleanup")
			w := &liveWriteRecorder{}
			driver := newLiveWaitDriver()
			options, ops := liveEnabledOptions(w), liveEnabledOps(driver)
			events := []string{}
			if phase == "dimensions" {
				ops.Dimensions = func() (int, int, bool, error) { return 0, 0, false, failure }
			}
			if phase == "init" {
				w.failAt, w.err = 1, failure
			}
			if phase == "title" {
				title := "\xff"
				options.Title, options.Header = &title, "raw\n"
			}
			ops.SubscribeResize = func(func() error) (func() error, error) {
				events = append(events, "subscribe")
				release := func() error { events = append(events, "release"); return cleanup }
				if phase == "subscribe" {
					return release, failure
				}
				return release, nil
			}
			ops.AttachLogger = func(reader func([]byte) error) (func() error, error) {
				events = append(events, "attach")
				if err := reader([]byte("partial tail")); err != nil {
					t.Fatal(err)
				}
				return func() error { events = append(events, "detach"); return cleanup }, failure
			}
			v, err := OpenLiveView(options, ops)
			if v != nil || err == nil {
				t.Fatal(v, err)
			}
			if phase != "title" && err != failure {
				t.Fatal("constructor operation replaced", err)
			}
			want := []string(nil)
			if phase == "subscribe" {
				want = []string{"subscribe", "release"}
			}
			if phase == "attach" {
				want = []string{"subscribe", "attach", "detach", "release"}
			}
			if !reflect.DeepEqual(events, want) && !(len(events) == 0 && len(want) == 0) {
				t.Fatal(events, want)
			}
			if phase == "dimensions" && len(w.trace()) != 0 {
				t.Fatal(w.trace())
			}
			if phase == "title" && !strings.HasSuffix(w.text(), "raw\n\x1b[2K\x1b[1G") {
				t.Fatal(w.text())
			}
			if strings.Contains(w.text(), "\x1b8") {
				t.Fatal("construction repaired cursor")
			}
		})
	}
	// Successful acquisitions must return their own cleanup handle.
	driver := newLiveWaitDriver()
	ops := liveEnabledOps(driver)
	ops.SubscribeResize = func(func() error) (func() error, error) { return nil, nil }
	if v, err := OpenLiveView(liveEnabledOptions(&liveWriteRecorder{}), ops); v != nil || err == nil {
		t.Fatal(v, err)
	}
	if v, err := OpenLiveView(LiveViewOptions{}, LiveViewOps{AttachLogger: func(func([]byte) error) (func() error, error) { return nil, nil }}); v != nil || err == nil {
		t.Fatal(v, err)
	}
}

func TestLiveStopCleanupPriorityQuiescenceAndNoPostStopResizeCommit(t *testing.T) {
	for _, first := range []string{"detach", "renderer", "release", "none"} {
		t.Run(first, func(t *testing.T) {
			detachErr, rendererErr, releaseErr := errors.New("detach"), errors.New("renderer"), errors.New("release")
			w := &liveWriteRecorder{}
			v := testBareLive(t, "", "T", 8, 1, w)
			events := []string{}
			v.pending, v.queue = []byte("tail"), []string{"not processed"}
			v.detach = func() error {
				events = append(events, "detach")
				if first == "detach" {
					return detachErr
				}
				return nil
			}
			v.feederDone, v.rendererDone = make(chan struct{}), make(chan struct{})
			v.rendererErr = rendererErr
			if first == "none" || first == "release" {
				v.rendererErr = nil
			}
			rendererJoined := make(chan struct{})
			v.cancelRenderer = func() { events = append(events, "cancel-renderer"); close(rendererJoined) }
			v.releaseResize = func() error {
				events = append(events, "release")
				if !v.stopping || v.queue != nil || v.pending != nil {
					t.Fatal("release before stop/drop")
				}
				return releaseErr
			}
			if first == "none" {
				v.releaseResize = func() error { events = append(events, "release"); return nil }
			}
			done := make(chan error, 1)
			go func() { done <- v.Stop() }()
			<-rendererJoined
			// Renderer can finish first, but Stop still waits on feeder before release.
			close(v.rendererDone)
			select {
			case err := <-done:
				t.Fatal("returned before feeder join", err)
			default:
			}
			close(v.feederDone)
			got := <-done
			want := map[string]error{"detach": detachErr, "renderer": rendererErr, "release": releaseErr, "none": nil}[first]
			if got != want || !reflect.DeepEqual(events, []string{"detach", "cancel-renderer", "release"}) || w.text() != "" {
				t.Fatal(got, events, w.text())
			}
			if err := v.Stop(); err != want || len(events) != 3 {
				t.Fatal(err, events)
			}
			v.ops.Dimensions = func() (int, int, bool, error) { t.Fatal("post-stop dimension"); return 0, 0, false, nil }
			if err := v.resize(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A callback already in Dimensions may finish during Stop; release quiesces
	// it, and its result cannot replace the previously committed viewport.
	w := &liveWriteRecorder{}
	v := testBareLive(t, "", "T", 8, 1, w)
	entered, returnSize, callbackDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	v.ops.Dimensions = func() (int, int, bool, error) { close(entered); <-returnSize; return 99, 99, true, nil }
	go func() { callbackDone <- v.resize() }()
	<-entered
	releaseEntered := make(chan struct{})
	v.releaseResize = func() error {
		close(releaseEntered)
		if err := <-callbackDone; err != nil {
			t.Error(err)
		}
		return nil
	}
	stopped := make(chan error, 1)
	go func() { stopped <- v.Stop() }()
	<-releaseEntered
	close(returnSize)
	if err := <-stopped; err != nil || v.cols != 8 || v.resizeEpoch.Sign() != 0 {
		t.Fatal(err, v.cols, &v.resizeEpoch)
	}
}

func TestLiveFlushCancellationAndFamilyErrorSelectors(t *testing.T) {
	driver := newLiveWaitDriver()
	v, err := OpenLiveView(LiveViewOptions{}, LiveViewOps{Wait: driver.wait})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- v.Flush(ctx) }()
	req := <-driver.requests
	if req.ctx != ctx || req.duration != time.Second {
		t.Fatal(req)
	}
	// Stop cancels no caller Flush and produces no terminal output.
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatal("Stop canceled caller Flush", err)
	default:
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	body, cleanup := errors.New("body"), errors.New("cleanup")
	ensure := func(body, stop error) error {
		if stop != nil {
			return stop
		}
		return body
	}
	gc := func(primary, stop error) error {
		if primary != nil {
			return primary
		}
		return stop
	}
	if ensure(body, cleanup) != cleanup || gc(body, cleanup) != body || ensure(body, nil) != body || gc(nil, cleanup) != cleanup {
		t.Fatal("family precedence collapsed")
	}
	// Build/copy composition stops the view without inventing a Flush.
	v, _ = OpenLiveView(LiveViewOptions{}, LiveViewOps{Wait: func(context.Context, time.Duration) error { t.Fatal("build/copy Flush"); return nil }})
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveResizeSerializesDimensionCommitWithoutHoldingState(t *testing.T) {
	v := testBareLive(t, "", "T", 8, 1, &liveWriteRecorder{})
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	calls := 0
	v.ops.Dimensions = func() (int, int, bool, error) {
		calls++
		if calls == 1 {
			close(firstEntered)
			<-releaseFirst
			return 10, 9, true, nil
		}
		return 20, 10, true, nil
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- v.resize() }()
	<-firstEntered
	if err := v.FeedLine("no state lock spans Dimensions"); err != nil {
		t.Fatal(err)
	}
	go func() { secondDone <- v.resize() }()
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if calls != 2 || v.cols != 10 || v.resizeEpoch.Cmp(big.NewInt(2)) != 0 {
		t.Fatal(calls, v.cols, &v.resizeEpoch)
	}
}

func TestLiveSubscribedResizeErrorIsSynchronousAndNotStopError(t *testing.T) {
	driver := newLiveWaitDriver()
	ops := liveEnabledOps(driver)
	failure := errors.New("dimension callback")
	calls := 0
	ops.Dimensions = func() (int, int, bool, error) {
		calls++
		if calls == 1 {
			return 2, 8, true, nil
		}
		return 99, 99, true, failure
	}
	var callback func() error
	releases := 0
	ops.SubscribeResize = func(fn func() error) (func() error, error) {
		callback = fn
		return func() error { releases++; return nil }, nil
	}
	v, err := OpenLiveView(liveEnabledOptions(&liveWriteRecorder{}), ops)
	if err != nil {
		t.Fatal(err)
	}
	<-driver.requests // immediate clean renderer is parked at its first wait
	before := v.snapshot()
	if err := callback(); err != failure || !reflect.DeepEqual(before, v.snapshot()) {
		t.Fatal(err)
	}
	if err := v.FeedLine("host continues"); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- v.Flush(context.Background()) }()
	req := <-driver.requests
	if req.duration != time.Second {
		t.Fatal(req.duration)
	}
	close(req.proceed)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if err := v.Stop(); err != nil || releases != 1 {
		t.Fatal(err, releases)
	}
}

func TestLiveStopJoinsBorrowedWriterWithoutForcedInterruption(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	w := &liveGateWriter{entered: entered, release: release}
	v := testBareLive(t, "", "T", 8, 1, w)
	v.generation.SetInt64(1)
	v.rendererContext, v.cancelRenderer = context.WithCancel(context.Background())
	v.rendererDone = make(chan struct{})
	go func() { defer close(v.rendererDone); v.rendererErr = v.renderLoop() }()
	<-entered
	canceled := make(chan struct{})
	originalCancel := v.cancelRenderer
	v.cancelRenderer = func() { originalCancel(); close(canceled) }
	stopped := make(chan error, 1)
	go func() { stopped <- v.Stop() }()
	<-canceled
	select {
	case err := <-stopped:
		t.Fatal("false join while writer blocked", err)
	default:
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	before := w.text()
	if err := v.Stop(); err != nil || w.text() != before {
		t.Fatal("repeated Stop output", err)
	}
}

func TestLiveMissingDependenciesAreCheckedOnlyWhenReached(t *testing.T) {
	if v, err := OpenLiveView(LiveViewOptions{}, LiveViewOps{}); err != nil || v == nil {
		t.Fatal(v, err)
	}
	if v, err := OpenLiveView(LiveViewOptions{Enabled: true}, LiveViewOps{}); v != nil || err == nil {
		t.Fatal(v, err)
	}
	ops := LiveViewOps{Dimensions: func() (int, int, bool, error) { return 1, 8, true, nil }}
	if v, err := OpenLiveView(LiveViewOptions{Enabled: true}, ops); v != nil || err == nil {
		t.Fatal("missing console reached", v, err)
	}
	if v, err := OpenLiveView(LiveViewOptions{Enabled: true, Console: &LiveConsole{}}, ops); v != nil || err == nil {
		t.Fatal("missing writer reached", v, err)
	}
	driver := newLiveWaitDriver()
	ops = liveEnabledOps(driver)
	ops.Wait = nil
	v, err := OpenLiveView(liveEnabledOptions(&liveWriteRecorder{}), ops)
	if err != nil {
		t.Fatal(err)
	}
	<-v.rendererDone
	if err := v.FeedLine("admitted"); err != nil {
		t.Fatal(err)
	}
	if err := v.Flush(context.Background()); err == nil {
		t.Fatal("Flush needs reached wait")
	}
	if err := v.Stop(); err == nil {
		t.Fatal("renderer missing wait observed at Stop")
	}
}

func TestLiveConcurrentStopDoesNotHoldLocksAcrossCleanup(t *testing.T) {
	v := testBareLive(t, "", "T", 8, 1, &liveWriteRecorder{})
	entered, release := make(chan struct{}), make(chan struct{})
	sentinel := errors.New("detach")
	calls := 0
	v.detach = func() error {
		calls++
		// Both state and stop acquisition locks are available to synchronous
		// cleanup helpers; quiescence happens without either lock held.
		v.mu.Lock()
		v.mu.Unlock()
		v.stopMu.Lock()
		v.stopMu.Unlock()
		close(entered)
		<-release
		return sentinel
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- v.Stop() }()
	<-entered
	go func() { second <- v.Stop() }()
	close(release)
	if err := <-first; err != sentinel {
		t.Fatal(err)
	}
	if err := <-second; err != sentinel || calls != 1 {
		t.Fatal(err, calls)
	}
}
