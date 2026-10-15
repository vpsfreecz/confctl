//go:build linux

package core

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Opaque pointer identities only; these callbacks never call a file method,
// os.Pipe, native Poll/Wake or an operator input. Values/traces are source-derived.
func inputWaitFixture(t *testing.T) (stdin, read, write *os.File, ops inputWaitOps, trace func() []string) {
	t.Helper()
	stdin, read, write = new(os.File), new(os.File), new(os.File)
	var mutex sync.Mutex
	var events []string
	event := func(text string) { mutex.Lock(); events = append(events, text); mutex.Unlock() }
	ops = inputWaitOps{
		Pipe: func() (*os.File, *os.File, error) { event("pipe"); return read, write, nil },
		Poll: func(in, wake *os.File) error {
			if in != stdin || wake != read {
				t.Error("changed readiness identities")
			}
			event("poll")
			return nil
		},
		Byte: func(input *bufio.Reader) (byte, error) { event("byte"); return input.ReadByte() },
		Wake: func(file *os.File) error {
			if file != write {
				t.Error("wake identity")
			}
			event("wake")
			return nil
		},
		Close: func(file *os.File) error {
			switch file {
			case read:
				event("close read")
			case write:
				event("close write")
			default:
				t.Error("closed borrowed or foreign pointer")
			}
			return nil
		},
	}
	trace = func() []string { mutex.Lock(); defer mutex.Unlock(); return append([]string(nil), events...) }
	return
}

func TestInputWaitLinuxAcquirePartialAndOwnership(t *testing.T) {
	failure := errors.New("pipe failure")
	for _, name := range []string{"normal", "none error", "both error", "read error", "write error", "missing read", "missing write", "both nil", "borrowed read", "borrowed write", "both borrowed", "equal endpoints"} {
		t.Run(name, func(t *testing.T) {
			stdin, read, write, ops, _ := inputWaitFixture(t)
			input := basicBuffered("qTAIL")
			calls := 0
			var closed []*os.File
			ops.Pipe = func() (*os.File, *os.File, error) {
				calls++
				switch name {
				case "none error":
					return nil, nil, failure
				case "both error":
					return read, write, failure
				case "read error":
					return read, nil, failure
				case "write error":
					return nil, write, failure
				case "missing read":
					return nil, write, nil
				case "missing write":
					return read, nil, nil
				case "both nil":
					return nil, nil, nil
				case "borrowed read":
					return stdin, write, nil
				case "borrowed write":
					return read, stdin, nil
				case "both borrowed":
					return stdin, stdin, nil
				case "equal endpoints":
					return read, read, nil
				}
				return read, write, nil
			}
			ops.Close = func(file *os.File) error {
				if file == stdin {
					t.Fatal("borrowed input closed")
				}
				closed = append(closed, file)
				return nil
			}
			w, err := acquireInputWait(stdin, input, ops)
			if w == nil || calls != 1 {
				t.Fatal("partial owner lost")
			}
			if name == "normal" {
				if err != nil {
					t.Fatal(err)
				}
			} else if strings.Contains(name, "error") {
				if err != failure {
					t.Fatal("pipe error identity changed", err)
				}
			} else if err == nil {
				t.Fatal("malformed binding admitted")
			}
			if err != nil {
				w.Start(context.Background())
				if w.started {
					t.Fatal("failed acquisition started")
				}
				b := w.Byte(context.Background())
				if b.Err == nil || b.FromRead {
					t.Fatal("partial owner read")
				}
			}
			w.StopJoin()
			a := w.CloseOwned()
			b := w.CloseOwned()
			if a != b {
				t.Fatal("repeated close changed result")
			}
			want := []*os.File{read, write}
			switch name {
			case "none error", "both nil", "both borrowed":
				want = nil
			case "read error", "missing write", "borrowed write", "equal endpoints":
				want = []*os.File{read}
			case "write error", "missing read", "borrowed read":
				want = []*os.File{write}
			}
			if !reflect.DeepEqual(closed, want) || input.Buffered() != 5 {
				t.Fatal("unique close/input boundary", len(closed), len(want))
			}
		})
	}
}

func TestInputWaitLinuxAcquireRequiredBindingsBeforePipe(t *testing.T) {
	for _, name := range []string{"pipe", "poll", "byte", "wake", "close"} {
		t.Run(name, func(t *testing.T) {
			stdin, _, _, ops, trace := inputWaitFixture(t)
			switch name {
			case "pipe":
				ops.Pipe = nil
			case "poll":
				ops.Poll = nil
			case "byte":
				ops.Byte = nil
			case "wake":
				ops.Wake = nil
			case "close":
				ops.Close = nil
			}
			w, err := acquireInputWait(stdin, basicBuffered("q"), ops)
			if err == nil || err.Error() != "input wait "+name+" binding is unavailable" || len(trace()) != 0 {
				t.Fatal("validation reached resource or changed diagnostic", err)
			}
			w.StopJoin()
			w.CloseOwned()
			if len(trace()) != 0 {
				t.Fatal("missing binding invented cleanup")
			}
		})
	}
	for _, missing := range []string{"stdin", "reader"} {
		t.Run(missing, func(t *testing.T) {
			stdin, _, _, ops, trace := inputWaitFixture(t)
			input := basicBuffered("q")
			if missing == "stdin" {
				stdin = nil
			} else {
				input = nil
			}
			w, err := acquireInputWait(stdin, input, ops)
			if err == nil || err.Error() != "input wait original input is unavailable" || len(trace()) != 0 {
				t.Fatal(err)
			}
			w.StopJoin()
			w.CloseOwned()
		})
	}
}

func TestInputWaitLinuxByteReadOriginAndSuffix(t *testing.T) {
	sentinel := errors.New("operation")
	for _, tc := range []struct {
		name       string
		buffered   bool
		pollErrors []error
		readErr    error
		wantErr    error
		fromRead   bool
		polls      int
	}{
		{"buffered", true, nil, nil, nil, true, 0},
		{"readiness", false, nil, nil, nil, true, 1},
		{"retry poll EINTR", false, []error{syscall.EINTR, syscall.EINTR, nil}, nil, nil, true, 3},
		{"poll failure", false, []error{sentinel}, nil, sentinel, false, 1},
		{"poll ENOTTY", false, []error{syscall.ENOTTY}, nil, syscall.ENOTTY, false, 1},
		{"read failure", true, nil, sentinel, sentinel, true, 0},
		{"read ENOTTY", true, nil, syscall.ENOTTY, syscall.ENOTTY, true, 0},
		{"read EINTR is not retried", true, nil, syscall.EINTR, syscall.EINTR, true, 0},
		{"read EOF", false, nil, io.EOF, io.EOF, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin, read, _, ops, _ := inputWaitFixture(t)
			input := bufio.NewReader(strings.NewReader("qTAIL"))
			if tc.buffered {
				_, _ = input.Peek(5)
			}
			polls, reads := 0, 0
			ops.Poll = func(in, wake *os.File) error {
				if in != stdin || wake != read {
					t.Fatal("changed identities")
				}
				polls++
				if len(tc.pollErrors) >= polls {
					return tc.pollErrors[polls-1]
				}
				return nil
			}
			ops.Byte = func(reader *bufio.Reader) (byte, error) {
				if reader != input {
					t.Fatal("reader replaced")
				}
				reads++
				if tc.readErr != nil {
					return 0, tc.readErr
				}
				return reader.ReadByte()
			}
			w, err := acquireInputWait(stdin, input, ops)
			if err != nil {
				t.Fatal(err)
			}
			w.Start(context.Background())
			ops.Byte = nil // owner retains copied callbacks
			got := w.Byte(context.Background())
			if got.Err != tc.wantErr || got.FromRead != tc.fromRead || polls != tc.polls || reads != map[bool]int{false: 0, true: 1}[tc.fromRead] {
				t.Fatal("read origin/reachability", got, polls, reads)
			}
			if got.Err == nil {
				tail, _ := io.ReadAll(input)
				if got.Value != 'q' || string(tail) != "TAIL" {
					t.Fatal("read changed suffix")
				}
			}
			w.StopJoin()
			w.CloseOwned()
		})
	}
}

func TestInputWaitLinuxByteContextBoundariesAndNoTailOverride(t *testing.T) {
	for _, name := range []string{"before", "after ready", "during EINTR", "successful read", "read error"} {
		t.Run(name, func(t *testing.T) {
			stdin, _, _, ops, _ := inputWaitFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			input := bufio.NewReader(strings.NewReader("qTAIL"))
			sentinel := errors.New("read failure")
			polls, reads := 0, 0
			ops.Poll = func(*os.File, *os.File) error {
				polls++
				if name == "after ready" {
					cancel()
				}
				if name == "during EINTR" {
					cancel()
					return syscall.EINTR
				}
				return nil
			}
			ops.Byte = func(r *bufio.Reader) (byte, error) {
				reads++
				value, err := r.ReadByte()
				cancel()
				if name == "read error" {
					return value, sentinel
				}
				return value, err
			}
			w, err := acquireInputWait(stdin, input, ops)
			if err != nil {
				t.Fatal(err)
			}
			w.Start(ctx)
			if name == "before" {
				cancel()
			}
			got := w.Byte(ctx)
			if name == "successful read" {
				if got.Err != nil || !got.FromRead || got.Value != 'q' || reads != 1 {
					t.Fatal("trailing cancellation replaced success")
				}
			} else if name == "read error" {
				if got.Err != sentinel || !got.FromRead {
					t.Fatal("read cause replaced")
				}
			} else if got.Err != context.Canceled || got.FromRead || reads != 0 {
				t.Fatal("context reached Byte", got)
			}
			if name == "before" && polls != 0 || name == "during EINTR" && polls != 1 {
				t.Fatal("poll context order", polls)
			}
			w.StopJoin()
			w.CloseOwned()
		})
	}
}

func TestInputWaitLinuxStopNormalAndIndependentCloseErrors(t *testing.T) {
	stdin, read, write, ops, trace := inputWaitFixture(t)
	readErr, writeErr := errors.New("read close"), errors.New("write close")
	baseClose := ops.Close
	ops.Close = func(f *os.File) error {
		_ = baseClose(f)
		if f == read {
			return readErr
		}
		if f == write {
			return writeErr
		}
		t.Fatal("borrowed close")
		return nil
	}
	w, err := acquireInputWait(stdin, basicBuffered("q"), ops)
	if err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background())
	w.Start(context.Background())
	if stopped := w.StopJoin(); stopped != (inputWaitStop{}) {
		t.Fatal(stopped)
	}
	w.Start(context.Background()) // stopped owner never starts another notifier
	closed := w.CloseOwned()
	if closed.Read != readErr || closed.Write != writeErr {
		t.Fatal("independent close causes lost")
	}
	if w.StopJoin() != (inputWaitStop{}) || w.CloseOwned() != closed || !reflect.DeepEqual(trace(), []string{"pipe", "close read", "close write"}) {
		t.Fatal("cleanup repeated", trace())
	}
}

func TestInputWaitLinuxStopFailedWakeNeedsWriteCloseHUPAndJoin(t *testing.T) {
	stdin, read, write, ops, _ := inputWaitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polling, wakeEntered, wakeReturn, hup, closeEntered, closeReturn := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	wakeErr, writeErr, readErr := errors.New("wake failed"), errors.New("write close failed"), errors.New("read close failed")
	var mutex sync.Mutex
	var events []string
	event := func(s string) { mutex.Lock(); events = append(events, s); mutex.Unlock() }
	reads := 0
	ops.Poll = func(in, wake *os.File) error {
		if in != stdin || wake != read {
			t.Error("readiness identities")
		}
		event("poll")
		close(polling)
		<-hup
		event("HUP")
		return nil
	}
	ops.Byte = func(*bufio.Reader) (byte, error) { reads++; return 0, nil }
	ops.Wake = func(f *os.File) error {
		if f != write {
			t.Error("wake identity")
		}
		event("wake begin")
		close(wakeEntered)
		<-wakeReturn
		event("wake end")
		return wakeErr
	}
	ops.Close = func(f *os.File) error {
		if f == write {
			event("write close begin")
			close(hup)
			close(closeEntered)
			<-closeReturn
			event("write close end")
			return writeErr
		}
		if f != read {
			t.Error("borrowed close")
		}
		event("read close")
		return readErr
	}
	w, err := acquireInputWait(stdin, bufio.NewReader(strings.NewReader("q")), ops)
	if err != nil {
		t.Fatal(err)
	}
	w.Start(ctx)
	result := make(chan inputWaitByte, 1)
	go func() { result <- w.Byte(ctx) }()
	<-polling
	cancel()
	<-wakeEntered
	select {
	case <-result:
		t.Fatal("Wake alone released modeled Poll")
	default:
	}
	close(wakeReturn)
	<-closeEntered
	got := <-result
	if got.Err != context.Canceled || got.FromRead || reads != 0 {
		t.Fatal("canceled readiness read", got)
	}
	stopResult := make(chan inputWaitStop, 1)
	go func() { stopResult <- w.StopJoin() }()
	select {
	case <-stopResult:
		t.Fatal("join skipped unfinished write close")
	default:
	}
	close(closeReturn)
	stopped := <-stopResult
	if stopped.Wake != wakeErr || stopped.WriteClose != writeErr {
		t.Fatal("notifier causes changed")
	}
	closed := w.CloseOwned()
	if closed.Read != readErr || closed.Write != nil {
		t.Fatal("cancellation close was retried or conflated")
	}
	if w.StopJoin() != stopped || w.CloseOwned() != closed {
		t.Fatal("cleanup cache changed")
	}
	mutex.Lock()
	trace := append([]string(nil), events...)
	mutex.Unlock()
	if !reflect.DeepEqual(trace, []string{"poll", "wake begin", "wake end", "write close begin", "HUP", "write close end", "read close"}) {
		t.Fatal(trace)
	}
	// The write-close callback modeled HUP despite its error. A broken callback
	// that leaves an endpoint open is outside any universal unblock guarantee.
}

func TestInputWaitLinuxRacesStopAndCancelSelections(t *testing.T) {
	for _, name := range []string{"stop selected", "cancel selected", "both ready"} {
		t.Run(name, func(t *testing.T) {
			stdin, _, _, ops, trace := inputWaitFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, err := acquireInputWait(stdin, basicBuffered("q"), ops)
			if err != nil {
				t.Fatal(err)
			}
			if name == "both ready" {
				cancel()
			}
			w.Start(ctx)
			if name == "cancel selected" {
				cancel()
				<-w.done
			}
			stopped := w.StopJoin()
			closed := w.CloseOwned()
			cancel()
			if stopped != (inputWaitStop{}) || closed != (inputWaitClose{}) {
				t.Fatal("unexpected cleanup error")
			}
			if w.StopJoin() != stopped || w.CloseOwned() != closed {
				t.Fatal("repeat changed cleanup")
			}
			got := trace()
			normal := []string{"pipe", "close read", "close write"}
			canceled := []string{"pipe", "wake", "close write", "close read"}
			if name == "stop selected" && !reflect.DeepEqual(got, normal) || name == "cancel selected" && !reflect.DeepEqual(got, canceled) || name == "both ready" && !reflect.DeepEqual(got, normal) && !reflect.DeepEqual(got, canceled) {
				t.Fatal("selected order", got)
			}
			if !reflect.DeepEqual(trace(), got) {
				t.Fatal("post-join effect")
			}
		})
	}
}
