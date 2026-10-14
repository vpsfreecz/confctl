package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type LiveViewOptions struct {
	Enabled       bool
	Header        string       // raw bytes; empty has no header lines
	Title         *string      // nil selects Log; an empty title stays empty
	Auto          bool         // otherwise the source callers' fixed size 10
	ReservedLines int          // nonnegative caller-owned count
	Output        io.Writer    // borrowed; never closed
	Console       *LiveConsole // shared with the invocation's bars/messages
}

// Releases must unregister only their own callback and quiesce callbacks in
// flight, even when returning an error. A partial acquisition returns its
// release alongside the operation error. AttachLogger never replays old logs.
type LiveViewOps struct {
	Dimensions      func() (rows, cols int, available bool, err error)
	Wait            func(context.Context, time.Duration) error
	SubscribeResize func(func() error) (release func() error, err error)
	AttachLogger    func(func([]byte) error) (detach func() error, err error)
}

// LiveView owns only its queue, feeder, renderer and callback releases. It has
// no parent watcher or child cancellation. Background errors are observed by
// Stop, not FeedLine/Flush. Caller body-versus-Stop precedence stays caller-owned.
type LiveView struct {
	options                            LiveViewOptions
	ops                                LiveViewOps
	title                              string
	mu                                 sync.Mutex
	queueReady                         *sync.Cond
	queue                              []string
	lines                              []string
	rows, cols, size                   int
	stopping                           bool
	generation, resizeEpoch            big.Int
	renderedGeneration, renderedResize big.Int
	resizeMu                           sync.Mutex
	readerMu                           sync.Mutex
	pending                            []byte
	detach, releaseResize              func() error
	feederDone, rendererDone           chan struct{}
	rendererContext                    context.Context
	cancelRenderer                     context.CancelFunc
	feederErr, rendererErr             error // read only after the corresponding join
	stopMu                             sync.Mutex
	stopDone                           chan struct{}
	stopErr                            error
}

// OpenLiveView renders synchronously before launching the two loops, then
// subscribes to resize and optionally attaches a logger. A failed operation
// stays primary after every acquired cleanup/join is attempted. Output, title
// validity and callbacks are checked only when reached; disabled views attach
// a requested logger but never touch terminal output or launch loops.
func OpenLiveView(options LiveViewOptions, ops LiveViewOps) (*LiveView, error) {
	if options.ReservedLines < 0 {
		return nil, fmt.Errorf("live view reserved lines must be nonnegative")
	}
	v := &LiveView{options: options, ops: ops, title: "Log", size: 10}
	v.options.Title = nil // retain a value, never the caller's mutable pointer
	if options.Title != nil {
		v.title = *options.Title
	}
	v.queueReady = sync.NewCond(&v.mu)
	fail := func(err error) (*LiveView, error) { _ = v.Stop(); return nil, err }
	if options.Enabled {
		rows, cols, size, err := v.dimensions()
		if err != nil {
			return fail(err)
		}
		v.rows, v.cols, v.size = rows, cols, size
		if err := v.initializeScreen(); err != nil {
			return fail(err)
		}
		initial := v.snapshot()
		if err := v.SyncConsole(func(scope *LiveConsoleScope) error {
			return v.renderFrame(scope, initial)
		}); err != nil {
			return fail(err)
		}
		v.rendererContext, v.cancelRenderer = context.WithCancel(context.Background())
		v.feederDone = make(chan struct{})
		go func() { defer close(v.feederDone); v.feedLoop() }()
		v.rendererDone = make(chan struct{})
		go func() { defer close(v.rendererDone); v.rendererErr = v.renderLoop() }()
		if ops.SubscribeResize == nil {
			return fail(fmt.Errorf("live view resize subscription is unavailable"))
		}
		v.releaseResize, err = ops.SubscribeResize(v.resize)
		if err != nil {
			return fail(err)
		}
		if v.releaseResize == nil {
			return fail(fmt.Errorf("live view resize subscription returned no release"))
		}
	}
	if ops.AttachLogger != nil {
		var err error
		v.detach, err = ops.AttachLogger(v.readLogger)
		if err != nil {
			return fail(err)
		}
		if v.detach == nil {
			return fail(fmt.Errorf("live view logger attachment returned no detach"))
		}
	}
	return v, nil
}

func (v *LiveView) SyncConsole(fn func(*LiveConsoleScope) error) error {
	return v.options.Console.Sync(fn)
}

// FeedLine accepts one whole line, including embedded LF. Valid UTF-8 is the
// bounded encoding contract; only source ASCII edges are stripped. Disabled
// views still admit and queue lines. The caller stops feeding before Stop.
func (v *LiveView) FeedLine(line string) error {
	if !utf8.ValidString(line) {
		return fmt.Errorf("live view line is not valid UTF-8")
	}
	line = strings.Trim(line, "\x00\t\n\v\f\r ")
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopping {
		return fmt.Errorf("live view is stopped")
	}
	v.queue = append(v.queue, line)
	v.queueReady.Signal()
	return nil
}

func (v *LiveView) feedLoop() {
	for {
		v.mu.Lock()
		for len(v.queue) == 0 && !v.stopping {
			v.queueReady.Wait()
		}
		if v.stopping {
			v.mu.Unlock()
			return
		}
		line := v.queue[0]
		v.queue[0] = ""
		v.queue = v.queue[1:]
		v.lines = append(v.lines, strings.ReplaceAll(line, "\t", "  "))
		for len(v.lines) > v.size {
			v.lines[0] = ""
			v.lines = v.lines[1:]
		}
		v.generation.Add(&v.generation, big.NewInt(1))
		v.mu.Unlock() // one source line per state operation, not a queue batch
	}
}

func (v *LiveView) readLogger(chunk []byte) error {
	v.readerMu.Lock()
	defer v.readerMu.Unlock()
	v.pending = append(v.pending, chunk...) // callbacks lend bytes only briefly
	for {
		index := strings.IndexByte(string(v.pending), '\n')
		if index < 0 {
			return nil
		}
		line := string(v.pending[:index+1])
		v.pending = v.pending[index+1:] // remove before invoking FeedLine
		if err := v.FeedLine(line); err != nil {
			return err
		}
	}
}

// Flush is exactly one caller-context wait, even when disabled. It is neither
// a render/queue barrier nor an observer of the renderer's stored result.
func (v *LiveView) Flush(ctx context.Context) error {
	if v.ops.Wait == nil {
		return fmt.Errorf("live view wait callback is unavailable")
	}
	return v.ops.Wait(ctx, time.Second)
}

// Stop detaches before stopping the view, joins feeder then renderer, and only
// then releases resize. It attempts every owned cleanup and returns the first
// detach/join/release error unchanged. It makes no final write or screen repair,
// never closes borrowed writers, and cannot interrupt a blocked borrowed write.
func (v *LiveView) Stop() error {
	v.stopMu.Lock()
	if v.stopDone != nil {
		done := v.stopDone
		v.stopMu.Unlock()
		<-done
		return v.stopErr
	}
	v.stopDone = make(chan struct{})
	v.stopMu.Unlock() // no lock spans callback quiescence or owned joins
	var result error
	first := func(err error) {
		if result == nil {
			result = err
		}
	}
	if v.detach != nil {
		first(v.detach())
		v.detach = nil
	}
	v.readerMu.Lock()
	v.pending = nil // unterminated/undelivered tail is deliberately not flushed
	v.readerMu.Unlock()
	v.mu.Lock()
	v.stopping = true
	v.queue = nil
	v.queueReady.Broadcast()
	v.mu.Unlock()
	if v.cancelRenderer != nil {
		v.cancelRenderer()
	}
	if v.feederDone != nil {
		<-v.feederDone
		first(v.feederErr)
	}
	if v.rendererDone != nil {
		<-v.rendererDone
		first(v.rendererErr)
	}
	if v.releaseResize != nil {
		first(v.releaseResize())
		v.releaseResize = nil
	}
	v.stopMu.Lock()
	v.stopErr = result
	close(v.stopDone)
	v.stopMu.Unlock()
	return result
}

func (v *LiveView) resize() error {
	v.resizeMu.Lock()
	defer v.resizeMu.Unlock()
	v.mu.Lock()
	stopping := v.stopping
	v.mu.Unlock()
	if stopping {
		return nil
	}
	rows, cols, size, err := v.dimensions()
	if err != nil {
		return err
	} // synchronous; never poison the renderer
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.stopping {
		v.rows, v.cols, v.size = rows, cols, size
		v.resizeEpoch.Add(&v.resizeEpoch, big.NewInt(1))
		// Shrink retains old lines; only a later feeder update prunes them.
	}
	return nil
}

func (v *LiveView) renderLoop() error {
	for {
		if v.isStopping() {
			return nil
		}
		if err := v.renderIteration(); err != nil {
			return err
		}
		if v.isStopping() {
			return nil
		}
		if v.ops.Wait == nil {
			return fmt.Errorf("live view wait callback is unavailable")
		}
		if err := v.ops.Wait(v.rendererContext, 100*time.Millisecond); err != nil {
			if v.rendererContext.Err() != nil && errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}

func (v *LiveView) isStopping() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopping
}
