//go:build linux

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/vpsfreecz/confctl/internal/ui"
)

// Bindings are invocation-local and copied at construction. Only the console
// device is owned; the original streams remain borrowed and caller-stable.
type liveTerminalOps struct {
	OpenConsole  func() (*os.File, error)
	CloseConsole func(*os.File) error
	Dimensions   func(*os.File) (int, int, error)
	IoctlSize    func(*os.File, uintptr, *[4]uint16) (int, error)
	IsTTY        func(*os.File) (bool, error)
	Env          func(string) (string, bool)
	NotifyResize func(chan<- os.Signal) (func() error, error)
	Now          func() time.Time
	Wait         func(context.Context, time.Duration) error
}

type liveTerminal struct {
	stdin, stdout, stderr *os.File
	ops                   liveTerminalOps
	console               ui.LiveConsole
	deviceMu              sync.Mutex // acquisition/query/final close, never output
	device                *os.File
	closed                bool
	closeErr              error
	resizeMu              sync.Mutex
	activeResize          bool
}

func newLiveTerminal(stdin, stdout, stderr *os.File, ops liveTerminalOps) *liveTerminal {
	return &liveTerminal{stdin: stdin, stdout: stdout, stderr: stderr, ops: ops}
}

// All views, bars and messages borrow this one console. Nested operations use
// its lexical scope; never reenter root Sync or retain/pass a scope to a worker.
func (t *liveTerminal) Console() *ui.LiveConsole { return &t.console }

func (t *liveTerminal) isTTY(file *os.File) (bool, error) {
	if t.ops.IsTTY == nil {
		return false, fmt.Errorf("live terminal TTY is unavailable")
	}
	return t.ops.IsTTY(file)
}

func (t *liveTerminal) UIEnabled() (bool, error) {
	if t.ops.Env == nil {
		return false, fmt.Errorf("live terminal environment is unavailable")
	}
	if value, _ := t.ops.Env("CONFCTL_TTY"); value == "0" {
		return false, nil
	}
	if tty, err := t.isTTY(t.stdout); err != nil || !tty {
		return false, err
	}
	return t.isTTY(t.stderr)
}

func (t *liveTerminal) ViewEnabled(output *os.File) (bool, error) {
	if tty, err := t.isTTY(output); err != nil || !tty {
		return false, err
	}
	return t.UIEnabled()
}

// Unavailable opens are retried; only success is cached. Raw per-demand size
// errors retain the cache and their identity. The viewport owns its defaults.
func (t *liveTerminal) Dimensions() (int, int, bool, error) {
	t.deviceMu.Lock()
	defer t.deviceMu.Unlock()
	if t.closed {
		return 0, 0, false, fmt.Errorf("live terminal is closed")
	}
	if t.device == nil {
		if t.ops.OpenConsole == nil {
			return 0, 0, false, fmt.Errorf("live terminal console open is unavailable")
		}
		file, err := t.ops.OpenConsole()
		if err != nil || file == nil {
			return 0, 0, false, nil
		}
		t.device = file
	}
	if t.ops.Dimensions == nil {
		return 0, 0, true, fmt.Errorf("live terminal dimensions is unavailable")
	}
	rows, columns, err := t.ops.Dimensions(t.device)
	return rows, columns, true, err
}

// Call only after every view and resize release has quiesced, outside console
// scopes. This separate cleanup result cannot replace a caller's selected body
// or Stop primary. Only the owned console is closed, once, including on error.
func (t *liveTerminal) CloseConsole() error {
	t.deviceMu.Lock()
	defer t.deviceMu.Unlock()
	if t.closed {
		return t.closeErr
	}
	t.closed = true
	if t.device != nil {
		if t.ops.CloseConsole == nil {
			t.closeErr = fmt.Errorf("live terminal console close is unavailable")
		} else {
			t.closeErr = t.ops.CloseConsole(t.device)
		}
	}
	return t.closeErr
}

// Columns is sampled at every reached bar format, independently of viewport
// dimensions. It intentionally omits Readline/tput/stty providers. One buffer
// survives all source ioctl attempts, including their partial mutations.
func (t *liveTerminal) Columns() (*big.Int, error) {
	if t.ops.IoctlSize == nil {
		return nil, fmt.Errorf("live terminal ioctl size is unavailable")
	}
	var size [4]uint16
attempts:
	for _, request := range []uintptr{0x5413, 0x40087468, 0x5468} {
		for _, file := range []*os.File{t.stdout, t.stdin, t.stderr} {
			result, err := t.ops.IoctlSize(file, request, &size)
			if err != nil {
				var errno syscall.Errno
				if errors.As(err, &errno) {
					continue attempts // source whole-attempt SystemCallError rescue
				}
				return nil, err
			}
			if result >= 0 {
				if size[1] > 0 {
					return big.NewInt(int64(size[1])), nil
				}
				break attempts // successful zero ends the entire request chain
			}
		}
	}
	tty, err := t.isTTY(t.stderr)
	if err != nil {
		return nil, err
	}
	if tty {
		if t.ops.Dimensions == nil {
			return nil, fmt.Errorf("live terminal dimensions is unavailable")
		}
		_, columns, err := t.ops.Dimensions(t.stderr)
		if err != nil && !errors.Is(err, syscall.EOPNOTSUPP) {
			return nil, err
		}
		if err == nil && columns > 0 {
			return big.NewInt(int64(columns)), nil
		}
	}
	return t.environmentColumns()
}

func (t *liveTerminal) environmentColumns() (*big.Int, error) {
	if t.ops.Env == nil {
		return nil, fmt.Errorf("live terminal environment is unavailable")
	}
	columns, _ := t.ops.Env("COLUMNS")
	if !utf8.ValidString(columns) {
		return nil, fmt.Errorf("family COLUMNS is not valid UTF-8")
	}
	if basicColumnsLine.MatchString(columns) {
		rows, present := t.ops.Env("LINES")
		if !present {
			rows, _ = t.ops.Env("ROWS")
		}
		_ = basicDecimalPrefix(rows)      // evaluated first, though width discards rows
		columns, _ = t.ops.Env("COLUMNS") // original second getter; numeric bytes only
		if width := basicDecimalPrefix(columns); width.Sign() > 0 {
			return width, nil
		}
	}
	ansicon, _ := t.ops.Env("ANSICON")
	if !utf8.ValidString(ansicon) {
		return nil, fmt.Errorf("family ANSICON is not valid UTF-8")
	}
	if match := basicANSICON.FindStringSubmatch(ansicon); match != nil {
		_ = basicDecimalPrefix(match[2]) // source array evaluates row before column
		if width := basicDecimalPrefix(match[1]); width.Sign() > 0 {
			return width, nil
		}
	}
	return big.NewInt(80), nil
}

func (t *liveTerminal) Wait(ctx context.Context, duration time.Duration) error {
	if t.ops.Wait == nil {
		return fmt.Errorf("live terminal wait is unavailable")
	}
	return t.ops.Wait(ctx, duration)
}

func (t *liveTerminal) ProgressOps(output io.Writer, flush func() error) ui.FamilyProgressOps {
	return ui.FamilyProgressOps{Output: output, Flush: flush,
		IsTTY:   func() (bool, error) { return t.isTTY(t.stderr) },
		Columns: t.Columns, Measure: ui.MeasureFamilyProgress, Now: t.ops.Now}
}

// One interval per source view; construction starts no worker. Subscription
// errors belong to Open, callback errors to the outer family, cleanup to Stop.
// No function other than the outer caller consumes Errors. A callback must not
// release its own interval or Stop the view whose resize callback it is running.
type liveResize struct {
	terminal    *liveTerminal
	notify      chan os.Signal
	errors      chan error
	stop        chan struct{}
	joined      chan struct{}
	mu          sync.Mutex
	used        bool
	stopping    bool
	unregister  func() error
	releaseDone chan struct{}
	releaseErr  error
}

func (t *liveTerminal) NewResize() *liveResize {
	return &liveResize{terminal: t, notify: make(chan os.Signal, 1), errors: make(chan error, 1), stop: make(chan struct{}), joined: make(chan struct{})}
}

func (r *liveResize) Errors() <-chan error { return r.errors }

func (r *liveResize) Subscribe(callback func() error) (func() error, error) {
	r.mu.Lock()
	if r.used {
		r.mu.Unlock()
		return nil, fmt.Errorf("live terminal resize interval was already subscribed")
	}
	r.used = true
	r.mu.Unlock()
	fail := func(err error) (func() error, error) {
		close(r.errors)
		close(r.joined)
		return nil, err
	}
	if callback == nil {
		return fail(fmt.Errorf("live terminal resize callback is unavailable"))
	}
	if r.terminal.ops.NotifyResize == nil {
		return fail(fmt.Errorf("live terminal resize notification is unavailable"))
	}
	r.terminal.resizeMu.Lock()
	if r.terminal.activeResize {
		r.terminal.resizeMu.Unlock()
		return fail(fmt.Errorf("live terminal resize interval is already active"))
	}
	r.terminal.activeResize = true
	r.terminal.resizeMu.Unlock()
	unregister, err := r.terminal.ops.NotifyResize(r.notify)
	r.unregister = unregister
	if err == nil && unregister == nil {
		err = fmt.Errorf("live terminal resize notification returned no release")
	}
	if err != nil {
		close(r.errors)
		close(r.joined)
		if unregister != nil {
			return r.release, err // acquired partial subscription still has an owner
		}
		r.clearActive()
		return nil, err
	}
	go r.dispatch(callback)
	return r.release, nil
}

func (r *liveResize) dispatch(callback func() error) {
	defer close(r.joined)
	defer close(r.errors)
	for {
		select {
		case <-r.stop:
			return
		case <-r.notify:
			r.mu.Lock()
			admitted := !r.stopping
			r.mu.Unlock()
			if !admitted {
				return
			}
			if err := callback(); err != nil {
				r.errors <- err // never canceled/dropped, even during release
				return
			}
		}
	}
}

func (r *liveResize) clearActive() {
	r.terminal.resizeMu.Lock()
	r.terminal.activeResize = false
	r.terminal.resizeMu.Unlock()
}

func (r *liveResize) release() error {
	r.mu.Lock()
	if r.releaseDone != nil {
		done := r.releaseDone
		r.mu.Unlock()
		<-done
		return r.releaseErr
	}
	r.releaseDone = make(chan struct{})
	r.stopping = true // close admission before unregister, with no callback lock
	r.mu.Unlock()
	err := r.unregister()
	close(r.stop)
	<-r.joined // includes an admitted callback; no lock spans the join
	r.clearActive()
	r.mu.Lock()
	r.releaseErr = err
	close(r.releaseDone)
	r.mu.Unlock()
	return err
}
