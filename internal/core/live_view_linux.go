package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vpsfreecz/confctl/internal/ui"
)

type liveViewSpec struct {
	Header ui.LiveHeader
	Rows   []ui.LiveRow
	RawLog bool
}

type liveView interface {
	SetRows(...ui.LiveRow) error
	Advance(ids ...string) error
	Line(string) error
	WithPrompt(context.Context, func() error) error
	Flush(context.Context) error
	Err() error
	Failed() <-chan struct{}
	Close() error
}

var errLiveViewClosed = errors.New("live view is closed")

type liveViewRequest struct {
	kind string
	done chan error
}

// Only loop paints or reads the tail. mu protects coalesced model updates and
// first-error admission; no console/model lock spans prompts, children or joins.
type liveSession struct {
	mu             sync.Mutex
	model          *ui.LiveModel
	raw            bool
	err            error
	closing        bool
	failed         chan struct{}
	onError        func(error)
	notifications  sync.WaitGroup
	out            io.Writer
	console        *os.File
	tail           *liveLogTail
	rows, cols     int
	tick           uint64
	suspended      bool
	reservePending bool
	outputFailed   bool
	dirty          chan struct{}
	requests       chan liveViewRequest
	done           chan struct{}
	resize         chan os.Signal
	prompt         sync.Mutex
	closeOnce      sync.Once
	closeErr       error
}

func (e *Engine) openLiveView(ctx context.Context, stdout, stderr io.Writer, spec liveViewSpec, onError func(error)) (liveView, error) {
	out, outOK := stdout.(*os.File)
	errout, errOK := stderr.(*os.File)
	if !outOK || !errOK || os.Getenv("CONFCTL_TTY") == "0" || !sshIsTTY(out) || !sshIsTTY(errout) {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model, err := ui.NewLiveModel(spec.Header, spec.Rows, e.liveColor(stdout))
	if err != nil {
		if onError != nil {
			onError(err)
		}
		return nil, err
	}
	var tail *liveLogTail
	if spec.RawLog {
		tail, err = e.openLiveLogTail()
		if err != nil {
			if onError != nil {
				onError(err)
			}
			return nil, err
		}
	}
	// Keep SSH's controlling-console size selection and fallback. The supplied
	// streams alone decide admission; opening this never forces a redirected UI.
	console, _ := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	v := newLiveSession(stdout, console, tail, model, spec.RawLog, onError)
	v.fetchSize()
	if err := v.paint(false); err != nil {
		v.fail(err)
		_ = tail.close()
		if console != nil {
			_ = console.Close()
		}
		return nil, v.Err()
	}
	signal.Notify(v.resize, syscall.SIGWINCH)
	go v.loop()
	return v, nil
}

func newLiveSession(out io.Writer, console *os.File, tail *liveLogTail, model *ui.LiveModel, raw bool, onError func(error)) *liveSession {
	return &liveSession{out: out, console: console, tail: tail, model: model, raw: raw, onError: onError,
		rows: 24, cols: 80, reservePending: true, failed: make(chan struct{}), dirty: make(chan struct{}, 1), requests: make(chan liveViewRequest), done: make(chan struct{}), resize: make(chan os.Signal, 1)}
}

func (v *liveSession) Err() error              { v.mu.Lock(); defer v.mu.Unlock(); return v.err }
func (v *liveSession) Failed() <-chan struct{} { return v.failed }

func (v *liveSession) fail(err error) {
	if err == nil {
		return
	}
	v.mu.Lock()
	notify := v.latch(err)
	v.mu.Unlock()
	v.notify(notify, err)
}

// Called with mu held, before any closer can wait on notifications.
func (v *liveSession) latch(err error) bool {
	if v.err != nil || err == nil {
		return false
	}
	v.err = err
	close(v.failed)
	v.notifications.Add(1)
	return true
}

func (v *liveSession) notify(first bool, err error) {
	if !first {
		return
	}
	defer v.notifications.Done()
	if v.onError != nil {
		v.onError(err)
	}
}

func (v *liveSession) update(change func() error) error {
	v.mu.Lock()
	if v.err != nil {
		err := v.err
		v.mu.Unlock()
		return err
	}
	if v.closing {
		v.mu.Unlock()
		return errLiveViewClosed
	}
	err := change()
	first := v.latch(err)
	latched := v.err
	v.mu.Unlock()
	v.notify(first, err)
	select {
	case v.dirty <- struct{}{}:
	default:
	}
	return latched
}

func (v *liveSession) SetRows(rows ...ui.LiveRow) error {
	return v.update(func() error { return v.model.SetRows(rows...) })
}
func (v *liveSession) Advance(ids ...string) error {
	return v.update(func() error { return v.model.Advance(ids...) })
}
func (v *liveSession) Line(line string) error {
	return v.update(func() error {
		if v.raw {
			return fmt.Errorf("semantic line supplied to raw live view")
		}
		v.model.Line(line)
		return nil
	})
}

func (v *liveSession) fetchSize() {
	rows, cols := 24, 80
	if v.console != nil {
		rows, cols = sshTerminalSize(v.console)
	}
	v.resizeGeometry(rows, cols)
}

func (v *liveSession) resizeGeometry(rows, cols int) {
	rows, cols = max(rows, 1), max(cols, 1)
	if rows != v.rows || cols != v.cols {
		v.reservePending = true
	}
	v.rows, v.cols = rows, cols
}

func (v *liveSession) drain() error {
	end, err := v.tail.end()
	if err != nil {
		return err
	}
	return v.tail.drain(end, func(b []byte) { v.mu.Lock(); v.model.Feed(b); v.mu.Unlock() })
}

func (v *liveSession) paint(final bool) error {
	if v.outputFailed {
		return nil
	}
	if v.reservePending {
		// Ordinary LF scrolls the existing screen, including the once-only
		// caller prelude, into history. Never clear scrollback or replay it.
		if err := v.writeOutput(strings.Repeat("\n", max(v.rows, 1))); err != nil {
			return err
		}
		v.reservePending = false
	}
	v.mu.Lock()
	frame, remainder := v.model.FrameWithRemainder(v.rows, v.cols, v.tick)
	v.mu.Unlock()
	var b strings.Builder
	// Reserve a positive bounded region. Use no raw mode, alternate screen or
	// cursor visibility change; the caller's input and terminal modes are intact.
	b.WriteString("\x1b[H")
	for i := 0; i < max(v.rows-1, 1); i++ {
		b.WriteString("\x1b[2K\r")
		if i < len(frame) {
			b.WriteString(frame[i])
		}
		if i+1 < max(v.rows-1, 1) {
			b.WriteString("\n")
		}
	}
	// Every complete paint leaves the cursor on its reserve row. Besides
	// keeping prompts clear, this is the native end boundary for frame readers.
	// A one-row terminal has no reserve: position without erasing its only row.
	b.WriteString(fmt.Sprintf("\x1b[%d;1H", max(v.rows, 1)))
	if v.rows > 1 {
		b.WriteString("\x1b[2K")
	}
	if err := v.writeOutput(b.String()); err != nil {
		return err
	}
	if !final {
		return nil
	}
	// The retained panel and omitted rows share one model snapshot/selection.
	// A sole physical row has no reserve, so move off it even without remainder.
	var after strings.Builder
	if v.rows == 1 {
		after.WriteString("\n\r")
	}
	if len(remainder) > 0 {
		after.WriteString("\r")
		after.WriteString(strings.Join(remainder, "\n\r"))
		after.WriteString("\n\r")
	}
	if after.Len() == 0 {
		return nil
	}
	return v.writeOutput(after.String())
}

func (v *liveSession) writeOutput(text string) error {
	n, err := io.WriteString(v.out, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	if err != nil {
		v.outputFailed = true
	}
	return err
}

func (v *liveSession) loop() {
	defer close(v.done)
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case req := <-v.requests:
			err := v.drain()
			v.fail(err)
			switch req.kind {
			case "suspend":
				v.suspended = true
				v.fail(v.paint(false))
			case "resume":
				v.suspended = false
				v.fetchSize() // Include a coalesced resize still queued at the barrier.
				v.fail(v.paint(false))
			case "flush":
				if !v.suspended {
					v.fail(v.paint(false))
				}
			case "close":
				v.fetchSize() // A refused/cancelled prompt never issues resume.
				v.mu.Lock()
				v.model.Finish()
				v.mu.Unlock()
				v.fail(v.paint(true))
				v.fail(v.tail.close())
				if v.console != nil {
					v.fail(v.console.Close())
				}
				req.done <- v.Err()
				return
			}
			req.done <- v.Err()
		case <-v.dirty:
			// Coalesce updates until the next tick or an explicit barrier.
		case <-v.resize:
		resizeDrain:
			for {
				select {
				case <-v.resize:
				default:
					break resizeDrain
				}
			}
			v.fetchSize()
			v.fail(v.drain())
			if !v.suspended {
				v.fail(v.paint(false))
			}
		case <-timer.C:
			v.tick++
			v.fail(v.drain())
			if !v.suspended {
				v.fail(v.paint(false))
			}
		}
	}
}

func (v *liveSession) request(ctx context.Context, kind string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := liveViewRequest{kind: kind, done: make(chan error, 1)}
	select {
	case v.requests <- req:
	case <-v.done:
		return errors.Join(errLiveViewClosed, v.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *liveSession) Flush(ctx context.Context) error {
	if err := v.Err(); err != nil {
		return err
	}
	return v.request(ctx, "flush")
}

func (v *liveSession) WithPrompt(ctx context.Context, body func() error) error {
	v.prompt.Lock()
	defer v.prompt.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.Err(); err != nil {
		return err
	}
	if body == nil {
		err := fmt.Errorf("live prompt body is unavailable")
		v.fail(err)
		return v.Err()
	}
	if err := v.request(ctx, "suspend"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.Err(); err != nil {
		return err
	}
	if err := body(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.request(ctx, "resume")
}

func (v *liveSession) Close() error {
	v.closeOnce.Do(func() {
		v.prompt.Lock()
		defer v.prompt.Unlock()
		v.mu.Lock()
		v.closing = true
		v.mu.Unlock()
		signal.Stop(v.resize)
		v.closeErr = v.request(context.Background(), "close")
		<-v.done
		v.notifications.Wait()
	})
	return v.closeErr
}
