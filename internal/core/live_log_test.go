package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/ui"
)

func liveLogAttach(t *testing.T, l *liveLog, fn func([]byte) error) func() error {
	t.Helper()
	detach, err := l.Attach(fn)
	if err != nil || detach == nil {
		t.Fatal("attach", err)
	}
	return detach
}

func liveLogWrite(t *testing.T, l *liveLog, chunk []byte, want error) {
	t.Helper()
	n, err := l.Write(chunk)
	if n != len(chunk) || err != want {
		t.Fatalf("Write(%q) = %d, %v; want %d, %v", chunk, n, err, len(chunk), want)
	}
}

func TestLiveLogInertAndLazyBindings(t *testing.T) {
	calls := 0
	l := newLiveLog(func(p []byte) (int, error) { calls++; return len(p), nil })
	if calls != 0 {
		t.Fatal("construction called raw")
	}
	if detach, err := l.Attach(nil); err == nil || detach != nil || calls != 0 {
		t.Fatal("nil reader", err, calls)
	}
	unbound := newLiveLog(nil)
	reads := 0
	detach := liveLogAttach(t, unbound, func([]byte) error { reads++; return nil })
	if n, err := unbound.Write(nil); n != 0 || err == nil || reads != 0 {
		t.Fatal("missing raw", n, err, reads)
	}
	if err := detach(); err != nil {
		t.Fatal(err)
	}
	liveLogWrite(t, l, nil, nil)
	if calls != 1 {
		t.Fatal("empty write must reach raw once", calls)
	}
}

func TestLiveLogRawStagesCountAndPrefix(t *testing.T) {
	writeErr, flushErr := errors.New("raw write"), errors.New("raw flush")
	for _, tc := range []struct {
		name     string
		n        int
		writeErr error
		flushErr error
		want     error
		events   []string
	}{
		{"zero-error", 0, writeErr, nil, writeErr, []string{"write"}},
		{"partial-error", 2, writeErr, nil, writeErr, []string{"write"}},
		{"full-error", 4, writeErr, nil, writeErr, []string{"write"}},
		{"zero-short", 0, nil, nil, io.ErrShortWrite, []string{"write"}},
		{"partial-short", 2, nil, nil, io.ErrShortWrite, []string{"write"}},
		{"flush-error", 4, nil, flushErr, flushErr, []string{"write", "flush"}},
		{"success", 4, nil, nil, nil, []string{"write", "flush", "reader"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			events := []string{}
			chunk := []byte{'A', 0, 0xff, '\n'}
			// This is the supplied compound raw operation, not a second lock or
			// flush owned by liveLog. The stream keeps the actual write prefix.
			l := newLiveLog(func(p []byte) (int, error) {
				events = append(events, "write")
				raw.Write(p[:tc.n])
				if tc.writeErr != nil || tc.n != len(p) {
					return tc.n, tc.writeErr
				}
				events = append(events, "flush")
				return tc.n, tc.flushErr
			})
			liveLogAttach(t, l, func(p []byte) error {
				events = append(events, "reader")
				if !bytes.Equal(raw.Bytes(), chunk) || !bytes.Equal(p, chunk) {
					t.Error("reader preceded complete raw output")
				}
				return nil
			})
			n, err := l.Write(chunk)
			if n != tc.n || err != tc.want || !bytes.Equal(raw.Bytes(), chunk[:tc.n]) || !reflect.DeepEqual(events, tc.events) {
				t.Fatal(n, err, raw.Bytes(), events)
			}
		})
	}
	// A malformed short-nil raw binding still cannot notify readers.
	l := newLiveLog(func([]byte) (int, error) { return 1, nil })
	liveLogAttach(t, l, func([]byte) error { t.Error("reader after short raw"); return nil })
	if n, err := l.Write([]byte("xx")); n != 1 || err != io.ErrShortWrite {
		t.Fatal(n, err)
	}
}

func TestLiveLogRegistrationFirstErrorAndNextWrite(t *testing.T) {
	events := []string{}
	l := newLiveLog(func(p []byte) (int, error) { events = append(events, "raw", "flush"); return len(p), nil })
	sentinel := context.Canceled // just a callback result; no context policy here
	fail := true
	for _, name := range []string{"first", "second", "third"} {
		liveLogAttach(t, l, func([]byte) error {
			events = append(events, name)
			if name == "second" && fail {
				return sentinel
			}
			return nil
		})
	}
	liveLogWrite(t, l, []byte("one"), sentinel)
	if !reflect.DeepEqual(events, []string{"raw", "flush", "first", "second"}) {
		t.Fatal(events)
	}
	fail, events = false, nil
	liveLogWrite(t, l, []byte("two"), nil)
	if !reflect.DeepEqual(events, []string{"raw", "flush", "first", "second", "third"}) {
		t.Fatal("error latched or chunk replayed", events)
	}
}

func TestLiveLogSnapshotAttachAndDetachBeforeAdmission(t *testing.T) {
	l := newLiveLog(func(p []byte) (int, error) { return len(p), nil })
	events := []string{}
	first := true
	var detachLater func() error
	liveLogAttach(t, l, func([]byte) error {
		events = append(events, "first")
		if first {
			first = false
			liveLogAttach(t, l, func([]byte) error { events = append(events, "new"); return nil })
			// Detaching a DIFFERENT, not-yet-admitted token is supported. The
			// snapshot still contains it; admission must recheck active.
			return detachLater()
		}
		return nil
	})
	detachLater = liveLogAttach(t, l, func([]byte) error { events = append(events, "detached"); return nil })
	liveLogAttach(t, l, func([]byte) error { events = append(events, "unrelated"); return nil })
	liveLogWrite(t, l, []byte("one"), nil)
	if !reflect.DeepEqual(events, []string{"first", "unrelated"}) {
		t.Fatal(events)
	}
	events = nil
	liveLogWrite(t, l, []byte("two"), nil)
	if err := detachLater(); err != nil || !reflect.DeepEqual(events, []string{"first", "unrelated", "new"}) {
		t.Fatal(events, err)
	}
}

func TestLiveLogDetachQuiescesEveryAdmittedWrite(t *testing.T) {
	l := newLiveLog(func(p []byte) (int, error) { return len(p), nil })
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	returned := make(chan struct{}, 2)
	detach := liveLogAttach(t, l, func([]byte) error {
		entered <- struct{}{}
		<-release
		returned <- struct{}{}
		return nil
	})
	writes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := l.Write([]byte("borrowed")); writes <- err }()
	}
	<-entered
	<-entered
	// Both callbacks have entered before any detacher starts. Observe private
	// admission state, but let the real detach close membership and quiesce it.
	l.mu.Lock()
	token := l.readers[0]
	admissions := token.inflight
	l.mu.Unlock()
	if admissions != 2 {
		t.Error("missing admissions", admissions)
	}
	starts := make(chan struct{}, 2)
	detached := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			starts <- struct{}{}
			err := detach()
			// Completion may not be reported until BOTH callbacks have given
			// up their borrowed input. The channel count is stable thereafter.
			if len(returned) != 2 {
				err = errors.New("detach returned before both callbacks")
			}
			detached <- err
		}()
	}
	<-starts
	<-starts
	select {
	case err := <-detached:
		t.Error("detach returned while callbacks borrowed input", err)
		detached <- err // retain the completion for the unconditional joins below
	default:
	}
	select {
	case <-token.done:
		t.Error("completion before callback return")
	default:
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-writes; err != nil {
			t.Error(err)
		}
		if err := <-detached; err != nil {
			t.Error(err)
		}
	}
	<-returned
	<-returned
	if len(l.readers) != 0 {
		t.Fatal("detached membership retained")
	}
	if err := detach(); err != nil {
		t.Fatal(err)
	}
	liveLogWrite(t, l, []byte("after"), nil)
}

func TestLiveLogDetachMembershipAndUnrelatedReader(t *testing.T) {
	l := newLiveLog(func(p []byte) (int, error) { return len(p), nil })
	calls := [2]int{}
	detach := liveLogAttach(t, l, func([]byte) error { calls[0]++; return nil })
	liveLogAttach(t, l, func([]byte) error { calls[1]++; return nil })
	liveLogWrite(t, l, nil, nil)
	if err := detach(); err != nil {
		t.Fatal(err)
	}
	if err := detach(); err != nil {
		t.Fatal(err)
	}
	liveLogWrite(t, l, nil, nil)
	if calls != [2]int{1, 2} {
		t.Fatal(calls)
	}
}

func TestLiveLogRawSerializationDoesNotImposeNotificationFIFO(t *testing.T) {
	var rawMu, eventsMu sync.Mutex
	var raw bytes.Buffer
	events := []string{}
	add := func(s string) { eventsMu.Lock(); events = append(events, s); eventsMu.Unlock() }
	l := newLiveLog(func(p []byte) (int, error) {
		rawMu.Lock()
		defer rawMu.Unlock()
		n, err := raw.Write(p)
		add("raw-" + string(p))
		add("flush-" + string(p))
		return n, err
	})
	aEntered, releaseA := make(chan struct{}), make(chan struct{})
	liveLogAttach(t, l, func(p []byte) error {
		add("first-" + string(p))
		if string(p) == "A" {
			close(aEntered)
			<-releaseA
		}
		return nil
	})
	liveLogAttach(t, l, func(p []byte) error { add("second-" + string(p)); return nil })
	aDone := make(chan error, 1)
	go func() { _, err := l.Write([]byte("A")); aDone <- err }()
	<-aEntered
	// B must complete while A is in its reader. Neither raw nor membership
	// ownership may span A's callback, and no FIFO dispatcher is allowed.
	liveLogWrite(t, l, []byte("B"), nil)
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	want := []string{"raw-A", "flush-A", "first-A", "raw-B", "flush-B", "first-B", "second-B", "second-A"}
	if raw.String() != "AB" || !reflect.DeepEqual(events, want) {
		t.Fatal(raw.String(), events)
	}
}

type liveLogMemorySink struct {
	bytes.Buffer
	closed bool
}

func (s *liveLogMemorySink) Close() error { s.closed = true; return nil }

func TestLiveLogBorrowedArbitraryBytesAndNoClose(t *testing.T) {
	sink := &liveLogMemorySink{}
	l := newLiveLog(func(p []byte) (int, error) { return sink.Write(p) })
	var observed [][]byte
	detach := liveLogAttach(t, l, func(p []byte) error { observed = append(observed, bytes.Clone(p)); return nil })
	for _, chunk := range [][]byte{nil, {}, {0, 0xff, 0xc3, '\n'}, bytes.Repeat([]byte{'x', 0, 0xfe}, 30000)} {
		before := bytes.Clone(chunk)
		liveLogWrite(t, l, chunk, nil)
		if !bytes.Equal(chunk, before) || !bytes.Equal(observed[len(observed)-1], before) {
			t.Fatal("borrowed bytes changed")
		}
		clear(chunk) // only after Write has returned
	}
	want := append([]byte{0, 0xff, 0xc3, '\n'}, bytes.Repeat([]byte{'x', 0, 0xfe}, 30000)...)
	if !bytes.Equal(sink.Bytes(), want) || len(observed) != 4 {
		t.Fatal("retained borrowed storage or replay")
	}
	if err := detach(); err != nil || sink.closed {
		t.Fatal("borrowed writer closed", err)
	}
	// No retained chunks are replayed by subsequent membership operations.
	liveLogWrite(t, l, nil, nil)
	if len(observed) != 4 || sink.closed {
		t.Fatal(len(observed), sink.closed)
	}
}

func TestLiveLogActualDisabledViewLFAdmissionAndTailDrop(t *testing.T) {
	var raw bytes.Buffer
	trace := []string{}
	l := newLiveLog(func(p []byte) (int, error) {
		n, err := raw.Write(p)
		trace = append(trace, "raw", "flush")
		return n, err
	})
	invalidTitle := "\xff"
	v, err := ui.OpenLiveView(ui.LiveViewOptions{Title: &invalidTitle}, ui.LiveViewOps{
		Dimensions:      func() (int, int, bool, error) { t.Error("disabled dimensions"); return 0, 0, false, nil },
		SubscribeResize: func(func() error) (func() error, error) { t.Error("disabled resize"); return nil, nil },
		AttachLogger: func(read func([]byte) error) (func() error, error) {
			trace = append(trace, "attach")
			detach, err := l.Attach(read)
			return func() error { trace = append(trace, "detach"); return detach() }, err
		},
		Wait: func(_ context.Context, d time.Duration) error {
			if d != time.Second {
				t.Error("disabled wait", d)
			}
			trace = append(trace, "flush-1s")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte("full\nsplit \xc3")
	liveLogWrite(t, l, chunk, nil)
	chunk[len(chunk)-1] = 0xff // view must already have copied this borrowed byte
	liveLogWrite(t, l, []byte("\xa9\n"), nil)
	bad := []byte("valid\n\xff\n\xfe\nlast")
	n, err := l.Write(bad)
	if n != len(bad) || err == nil || err.Error() != "live view line is not valid UTF-8" || !bytes.HasSuffix(raw.Bytes(), bad) {
		t.Fatal("raw must survive reached line rejection", n, err, raw.Bytes())
	}
	// A second invalid complete suffix remained after the first failed Feed.
	if n, err := l.Write(nil); n != 0 || err == nil || err.Error() != "live view line is not valid UTF-8" {
		t.Fatal("undelivered suffix lost or replayed", n, err)
	}
	liveLogWrite(t, l, nil, nil)          // no global latch; pending last is unterminated
	liveLogWrite(t, l, []byte{0xff}, nil) // invalid pending tail is not a line yet
	if err := v.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal("pending tail must be dropped", err)
	}
	liveLogWrite(t, l, []byte("\n"), nil) // detached; the old invalid tail cannot feed
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	if len(l.readers) != 0 || trace[0] != "attach" || !reflect.DeepEqual(trace[len(trace)-4:], []string{"flush-1s", "detach", "raw", "flush"}) {
		t.Fatal(trace, len(l.readers))
	}
	if got := raw.Bytes(); !bytes.HasPrefix(got, []byte("full\nsplit é\n")) {
		t.Fatal("raw byte order", got)
	}
	// Direct-feed mode supplies no AttachLogger at all, leaving membership empty.
	direct, err := ui.OpenLiveView(ui.LiveViewOptions{}, ui.LiveViewOps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.FeedLine("direct"); err != nil {
		t.Fatal(err)
	}
	if err := direct.Stop(); err != nil || len(l.readers) != 0 {
		t.Fatal(err)
	}
}

func TestLiveLogActualEnabledViewComposition(t *testing.T) {
	sink := &liveLogMemorySink{}
	var raw bytes.Buffer
	l := newLiveLog(func(p []byte) (int, error) { return raw.Write(p) })
	console := &ui.LiveConsole{}
	waiting := make(chan context.Context, 1)
	trace := []string{}
	v, err := ui.OpenLiveView(ui.LiveViewOptions{Enabled: true, Output: sink, Console: console}, ui.LiveViewOps{
		Dimensions: func() (int, int, bool, error) { trace = append(trace, "dimensions"); return 2, 8, true, nil },
		SubscribeResize: func(func() error) (func() error, error) {
			trace = append(trace, "subscribe")
			return func() error { trace = append(trace, "release"); return nil }, nil
		},
		AttachLogger: func(fn func([]byte) error) (func() error, error) {
			trace = append(trace, "attach")
			detach, err := l.Attach(fn)
			return func() error { trace = append(trace, "detach"); return detach() }, err
		},
		Wait: func(ctx context.Context, d time.Duration) error {
			if d == time.Second {
				trace = append(trace, "flush-1s")
				return nil
			}
			if d != 100*time.Millisecond {
				return errors.New("unexpected renderer wait")
			}
			waiting <- ctx
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := <-waiting // renderer parked before any input; no real timer/clock
	want := "\n\n\x1b[2J\x1b[H\x1b[2K\x1b[1GLog --->\n" + strings.Repeat("\x1b[2K\x1b[1G\n", 10) + "\x1b[2K\x1b[1G<-------\n\n"
	if sink.String() != want {
		t.Fatal("source empty frame", sink.String())
	}
	liveLogWrite(t, l, []byte("line\n\xc3"), nil)
	liveLogWrite(t, l, []byte("\xa9\n"), nil)
	if _, err := l.Write([]byte("\xff\n\xfe\npending")); err == nil {
		t.Fatal("invalid completed enabled line accepted")
	}
	if _, err := l.Write(nil); err == nil {
		t.Fatal("enabled suffix not retained")
	}
	liveLogWrite(t, l, nil, nil)
	if err := v.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Stop is deliberately outside reader callbacks and lexical console scopes.
	if err := v.Stop(); err != nil || ctx.Err() != context.Canceled || sink.closed || sink.String() != want {
		t.Fatal("stop/borrowed writer/final output", err, ctx.Err(), sink.closed)
	}
	liveLogWrite(t, l, []byte("after\n"), nil)
	if raw.String() != "line\né\n\xff\n\xfe\npendingafter\n" || !reflect.DeepEqual(trace, []string{"dimensions", "subscribe", "attach", "flush-1s", "detach", "release"}) || len(l.readers) != 0 {
		t.Fatal(raw.String(), trace, len(l.readers))
	}
}
