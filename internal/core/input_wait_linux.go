//go:build linux

package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Only readiness, byte access and one owned cancellation pipe. Consumers keep
// their decoder, termios, context-tail and error-presentation policies. These
// callbacks are copied; the original stdin and retained reader stay borrowed.
type inputWaitOps struct {
	Pipe  func() (*os.File, *os.File, error)
	Poll  func(*os.File, *os.File) error
	Byte  func(*bufio.Reader) (byte, error)
	Wake  func(*os.File) error
	Close func(*os.File) error
}

type inputWaitByte struct {
	Value    byte
	Err      error
	FromRead bool // true only when the actual Byte callback was reached
}

type inputWaitStop struct{ Wake, WriteClose error }
type inputWaitClose struct{ Read, Write error }

// All methods are sequential on the caller, apart from the single notifier.
// StopJoin precedes consumer restoration and CloseOwned. The caller keeps fd
// identity/lifetime stable and excludes concurrent reads. Pointer rejection
// below cannot detect distinct objects aliasing the same kernel descriptor.
type inputWait struct {
	stdin                      *os.File
	input                      *bufio.Reader
	ops                        inputWaitOps
	read, write                *os.File
	acquired, started, stopped bool
	readClosed, writeClosed    bool
	stop, done                 chan struct{}
	stopResult                 inputWaitStop
	closeResult                inputWaitClose
}

// Return the owner even on partial acquisition, so cleanup can retain every
// unique acquired endpoint. A Pipe error remains unchanged; malformed successful
// bindings refuse before Start, never closing a returned borrowed stdin pointer.
func acquireInputWait(stdin *os.File, input *bufio.Reader, ops inputWaitOps) (*inputWait, error) {
	w := &inputWait{stdin: stdin, input: input, ops: ops, stop: make(chan struct{}), done: make(chan struct{})}
	for _, binding := range []struct {
		name    string
		missing bool
	}{
		{"pipe", ops.Pipe == nil}, {"poll", ops.Poll == nil},
		{"byte", ops.Byte == nil}, {"wake", ops.Wake == nil}, {"close", ops.Close == nil},
	} {
		if binding.missing {
			return w, fmt.Errorf("input wait %s binding is unavailable", binding.name)
		}
	}
	if stdin == nil || input == nil {
		return w, fmt.Errorf("input wait original input is unavailable")
	}
	read, write, err := ops.Pipe()
	if read != nil && read != stdin {
		w.read = read
	}
	if write != nil && write != stdin && write != w.read {
		w.write = write
	}
	if err != nil {
		return w, err
	}
	if read == stdin || write == stdin || read != nil && read == write {
		return w, fmt.Errorf("input wait pipe ownership binding is invalid")
	}
	if read == nil || write == nil {
		return w, fmt.Errorf("input wait pipe endpoint is unavailable")
	}
	w.acquired = true
	return w, nil
}

// Normal stop does not Wake or early-close. Cancellation owns one Wake attempt
// followed by one write-end close, even on Wake error: ordinary pipe HUP can
// then release Poll. A callback that fails to close/return cannot promise that
// progress; borrowed stdin is never closed to force it.
func (w *inputWait) Start(ctx context.Context) {
	if w.started || w.stopped || !w.acquired {
		return
	}
	w.started = true
	go func() {
		defer close(w.done)
		select {
		case <-w.stop:
		case <-ctx.Done():
			w.stopResult.Wake = w.ops.Wake(w.write)
			w.writeClosed = true // attempted, not a successful-close certificate
			w.stopResult.WriteClose = w.ops.Close(w.write)
		}
	}()
}

// No Peek/refill, decoder, error retry after Byte or trailing context override.
// FromRead separates actual read errors from readiness/context infrastructure
// without changing their identity or selecting either consumer's error policy.
func (w *inputWait) Byte(ctx context.Context) inputWaitByte {
	if !w.acquired || w.stopped {
		return inputWaitByte{Err: fmt.Errorf("input wait is not active")}
	}
	for {
		if err := ctx.Err(); err != nil {
			return inputWaitByte{Err: err}
		}
		if w.input.Buffered() > 0 {
			break
		}
		err := w.ops.Poll(w.stdin, w.read)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return inputWaitByte{Err: err}
		}
		if err := ctx.Err(); err != nil {
			return inputWaitByte{Err: err}
		}
		break
	}
	value, err := w.ops.Byte(w.input)
	return inputWaitByte{Value: value, Err: err, FromRead: true}
}

// Join before observing notifier results, restoring the consumer's state or
// closing the read end. Stop/cancel races may select either branch. Once this
// returns there is no delayed Wake/close; repeated calls reuse the same result.
func (w *inputWait) StopJoin() inputWaitStop {
	if !w.stopped {
		w.stopped = true
		close(w.stop)
	}
	if w.started {
		<-w.done
	}
	return w.stopResult
}

// Caller has joined and finished any acquired restoration. Attempt read then
// remaining write exactly once; a cancellation-attempted write close is never
// retried, including on error. Return separate causes without presentation.
func (w *inputWait) CloseOwned() inputWaitClose {
	if w.read != nil && !w.readClosed {
		w.readClosed = true
		w.closeResult.Read = w.ops.Close(w.read)
	}
	if w.write != nil && !w.writeClosed {
		w.writeClosed = true
		w.closeResult.Write = w.ops.Close(w.write)
	}
	return w.closeResult
}
