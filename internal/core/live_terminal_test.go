//go:build linux

package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/ui"
)

func terminalFiles() (in, out, errout *os.File) { return new(os.File), new(os.File), new(os.File) }

func TestLiveTerminalLazyBindingsAndCopiedOperations(t *testing.T) {
	called := 0
	ops := liveTerminalOps{Env: func(string) (string, bool) { called++; return "0", true }}
	terminal := newLiveTerminal(nil, nil, nil, ops)
	ops.Env = nil
	progress := terminal.ProgressOps(nil, nil)
	resize := terminal.NewResize()
	if called != 0 || progress.Now != nil || resize == nil || terminal.Console() != terminal.Console() {
		t.Fatal("construction reached a binding or changed shared console")
	}
	if enabled, err := terminal.UIEnabled(); enabled || err != nil || called != 1 {
		t.Fatalf("copied binding = %v,%v,%d", enabled, err, called)
	}
	for name, fn := range map[string]func() error{
		"open":  func() error { _, _, _, err := terminal.Dimensions(); return err },
		"ioctl": func() error { _, err := terminal.Columns(); return err },
		"wait":  func() error { return terminal.Wait(context.Background(), time.Second) },
		"TTY":   func() error { _, err := terminal.ViewEnabled(nil); return err },
	} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("%s missing binding = %v", name, err)
		}
	}
	if err := terminal.CloseConsole(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := terminal.Dimensions(); err == nil || err.Error() != "live terminal is closed" {
		t.Fatal(err)
	}
	missing := newLiveTerminal(nil, nil, nil, liveTerminalOps{})
	if _, err := missing.UIEnabled(); err == nil {
		t.Fatal("missing environment accepted")
	}
	if _, err := missing.NewResize().Subscribe(nil); err == nil {
		t.Fatal("missing callback accepted")
	}
}

func TestLiveTerminalConsoleRetryCacheRawErrorsAndOneClose(t *testing.T) {
	in, out, errout := terminalFiles()
	device := new(os.File)
	openErr, sizeErr, closeErr := errors.New("open"), errors.New("size"), errors.New("close")
	opens, sizes, closes := 0, 0, 0
	terminal := newLiveTerminal(in, out, errout, liveTerminalOps{
		OpenConsole: func() (*os.File, error) {
			opens++
			switch opens {
			case 1:
				return nil, openErr
			case 2:
				return nil, nil
			default:
				return device, nil
			}
		},
		Dimensions: func(file *os.File) (int, int, error) {
			if file != device {
				t.Fatal("substituted a borrowed file")
			}
			sizes++
			if sizes == 2 {
				return -1, 0, sizeErr
			}
			return 0, 0, nil
		},
		CloseConsole: func(file *os.File) error {
			if file != device {
				t.Fatal("closed borrowed stream")
			}
			closes++
			return closeErr
		},
	})
	for range 2 {
		if r, c, ok, err := terminal.Dimensions(); r != 0 || c != 0 || ok || err != nil {
			t.Fatalf("unavailable %d,%d,%v,%v", r, c, ok, err)
		}
	}
	for i := 0; i < 3; i++ {
		r, c, ok, err := terminal.Dimensions()
		if !ok || i == 1 && (err != sizeErr || !errors.Is(err, sizeErr)) || i != 1 && (err != nil || r != 0 || c != 0) {
			t.Fatalf("raw size %d,%d,%v,%v", r, c, ok, err)
		}
	}
	if opens != 3 || sizes != 3 {
		t.Fatalf("cache %d/%d", opens, sizes)
	}
	for range 2 {
		if err := terminal.CloseConsole(); err != closeErr {
			t.Fatal(err)
		}
	}
	if closes != 1 {
		t.Fatal("close repeated")
	}
	if _, _, _, err := terminal.Dimensions(); err == nil || err.Error() != "live terminal is closed" {
		t.Fatal(err)
	}
	if opens != 3 || sizes != 3 {
		t.Fatal("closed cache reopened")
	}
	missing := newLiveTerminal(nil, nil, nil, liveTerminalOps{OpenConsole: func() (*os.File, error) { return device, nil }})
	if _, _, ok, err := missing.Dimensions(); !ok || err == nil {
		t.Fatal("missing dimension binding accepted")
	}
	if err := missing.CloseConsole(); err == nil {
		t.Fatal("missing owned close accepted")
	}
}

func TestLiveTerminalIoctlAttemptsStreamOrderAndSharedBuffer(t *testing.T) {
	in, out, errout := terminalFiles()
	var trace []string
	var first *[4]uint16
	terminal := newLiveTerminal(in, out, errout, liveTerminalOps{
		IoctlSize: func(file *os.File, request uintptr, buf *[4]uint16) (int, error) {
			stream := map[*os.File]string{in: "in", out: "out", errout: "err"}[file]
			trace = append(trace, fmt.Sprintf("%x:%s", request, stream))
			if first == nil {
				first = buf
				if *buf != [4]uint16{} {
					t.Fatal("buffer not initialized")
				}
			} else if first != buf {
				t.Fatal("buffer replaced")
			}
			switch len(trace) {
			case 1:
				buf[0] = 7
				return -1, syscall.ENOTTY
			case 2:
				if buf[0] != 7 {
					t.Fatal("partial buffer erased")
				}
				buf[2] = 9
				return -1, nil
			case 3:
				if buf[2] != 9 {
					t.Fatal("partial buffer erased")
				}
				return -1, syscall.EINVAL
			case 4:
				buf[1] = 37
				return 0, nil
			default:
				t.Fatal("later ioctl reached")
				return 0, nil
			}
		},
		Env: func(string) (string, bool) { t.Fatal("native success read environment"); return "", false },
	})
	width, err := terminal.Columns()
	if err != nil || width.String() != "37" || !reflect.DeepEqual(trace, []string{"5413:out", "40087468:out", "40087468:in", "5468:out"}) {
		t.Fatalf("%v %v %v", width, err, trace)
	}
	width.SetInt64(1)
	trace = nil
	first = nil
	if width, err = terminal.Columns(); err != nil || width.String() != "37" {
		t.Fatal("cached/borrowed width")
	}
}

func TestLiveTerminalIoctlZeroGenericErrorsAndStderrFallback(t *testing.T) {
	in, out, errout := terminalFiles()
	sentinel := errors.New("binding")
	for _, tc := range []struct {
		name              string
		ioctlErr, sizeErr error
		tty               bool
		columns           int
		want              string
		wantErr           error
	}{
		{"zero ends requests", nil, nil, false, 0, "80", nil},
		{"generic", sentinel, nil, true, 42, "", sentinel},
		{"stderr positive", nil, nil, true, 42, "42", nil},
		{"stderr zero", nil, nil, true, 0, "80", nil},
		{"stderr not supported", nil, syscall.EOPNOTSUPP, true, 0, "80", nil},
		{"stderr failure", nil, sentinel, true, 0, "", sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, dimensions, environment := 0, 0, 0
			terminal := newLiveTerminal(in, out, errout, liveTerminalOps{
				IoctlSize: func(file *os.File, request uintptr, buf *[4]uint16) (int, error) {
					calls++
					if file != out || request != 0x5413 {
						t.Fatal("successful zero did not end chain")
					}
					return 0, tc.ioctlErr
				},
				IsTTY: func(file *os.File) (bool, error) {
					if file != errout {
						t.Fatal("wrong fallback stream")
					}
					return tc.tty, nil
				},
				Dimensions: func(file *os.File) (int, int, error) {
					if file != errout {
						t.Fatal("wrong winsize file")
					}
					dimensions++
					return 1, tc.columns, tc.sizeErr
				},
				Env: func(string) (string, bool) { environment++; return "", false },
			})
			width, err := terminal.Columns()
			if err != tc.wantErr || calls != 1 {
				t.Fatalf("%v %v calls%d", width, err, calls)
			}
			if err == nil && width.String() != tc.want {
				t.Fatalf("width %v", width)
			}
			if !tc.tty && dimensions != 0 || tc.ioctlErr != nil && (dimensions != 0 || environment != 0) || tc.columns > 0 && tc.sizeErr == nil && environment != 0 {
				t.Fatal("unreached fallback executed")
			}
		})
	}
	// All-negative success attempts visit all nine slots before stderr TTY.
	calls := 0
	terminal := newLiveTerminal(in, out, errout, liveTerminalOps{IoctlSize: func(*os.File, uintptr, *[4]uint16) (int, error) { calls++; return -1, nil }, IsTTY: func(*os.File) (bool, error) { return false, sentinel }})
	if _, err := terminal.Columns(); err != sentinel || calls != 9 {
		t.Fatalf("TTY error/order %v/%d", err, calls)
	}
}

func TestLiveTerminalEnvironmentReachedGettersAndEncoding(t *testing.T) {
	huge := strings.Repeat("9", 100)
	for _, tc := range []struct {
		name, first, second, lines, rows, ansicon, want string
		present                                         bool
		trace                                           []string
		err                                             string
	}{
		{"second access", "12", " +2_1tail\xff", "", "unused", "", "21", true, []string{"COLUMNS", "LINES", "COLUMNS"}, ""},
		{"missing lines", "12\njunk", "12\njunk", "", " -2_0tail\xff", "", "12", false, []string{"COLUMNS", "LINES", "ROWS", "COLUMNS"}, ""},
		{"zero multiline", "0\n12", "0\n12", "", "", "(31x-2)", "31", true, []string{"COLUMNS", "LINES", "COLUMNS", "ANSICON"}, ""},
		{"later line", "junk\n12", "junk\n12", "", "", "", "80", true, []string{"COLUMNS", "LINES", "COLUMNS", "ANSICON"}, ""},
		{"huge", "12", huge, " -2_0tail\xff", "", "", "" + huge, true, []string{"COLUMNS", "LINES", "COLUMNS"}, ""},
		{"greedy ansicon", "bad", "", "", "", "prefix(7x2)(9x3)suffix", "7", false, []string{"COLUMNS", "ANSICON"}, ""},
		{"nonLF dot", "bad", "", "", "", "(7\nx2)", "80", false, []string{"COLUMNS", "ANSICON"}, ""},
		{"invalid first", "12\xff", "", "", "", "", "", false, []string{"COLUMNS"}, "family COLUMNS is not valid UTF-8"},
		{"invalid ansicon", "bad", "", "", "", "\xff", "", false, []string{"COLUMNS", "ANSICON"}, "family ANSICON is not valid UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trace []string
			reads := 0
			terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{Env: func(key string) (string, bool) {
				trace = append(trace, key)
				switch key {
				case "COLUMNS":
					reads++
					if reads == 1 {
						return tc.first, true
					}
					return tc.second, true
				case "LINES":
					return tc.lines, tc.present
				case "ROWS":
					return tc.rows, true
				case "ANSICON":
					return tc.ansicon, true
				}
				t.Fatal(key)
				return "", false
			}})
			width, err := terminal.environmentColumns()
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatal(err)
				}
			} else if err != nil || width.String() != tc.want {
				t.Fatalf("%v %v want%s", width, err, tc.want)
			}
			if !reflect.DeepEqual(trace, tc.trace) {
				t.Fatalf("trace %v want%v", trace, tc.trace)
			}
		})
	}
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{Env: func(string) (string, bool) { return "", false }})
	a, _ := terminal.environmentColumns()
	a.SetInt64(1)
	b, _ := terminal.environmentColumns()
	if b.Int64() != 80 {
		t.Fatal("default aliased")
	}
}

func TestLiveTerminalUIAndViewSelectionSamples(t *testing.T) {
	in, out, errout := terminalFiles()
	for _, tc := range []struct {
		name, value            string
		output, stdout, stderr bool
		view                   bool
		want                   bool
		trace                  []string
	}{
		{"forced off", "0", true, true, true, false, false, []string{"CONFCTL_TTY"}},
		{"stdout false", "", true, false, true, false, false, []string{"CONFCTL_TTY", "out"}},
		{"stderr false", "", true, true, false, false, false, []string{"CONFCTL_TTY", "out", "err"}},
		{"enabled", "00", true, true, true, false, true, []string{"CONFCTL_TTY", "out", "err"}},
		{"view output false", "", false, true, true, true, false, []string{"in"}},
		{"view forced off", "0", true, true, true, true, false, []string{"in", "CONFCTL_TTY"}},
		{"view enabled", "", true, true, true, true, true, []string{"in", "CONFCTL_TTY", "out", "err"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trace []string
			terminal := newLiveTerminal(in, out, errout, liveTerminalOps{Env: func(key string) (string, bool) { trace = append(trace, key); return tc.value, true }, IsTTY: func(file *os.File) (bool, error) {
				switch file {
				case in:
					trace = append(trace, "in")
					return tc.output, nil
				case out:
					trace = append(trace, "out")
					return tc.stdout, nil
				default:
					trace = append(trace, "err")
					return tc.stderr, nil
				}
			}})
			query := terminal.UIEnabled
			if tc.view {
				query = func() (bool, error) { return terminal.ViewEnabled(in) }
			}
			for range 2 {
				got, err := query()
				if got != tc.want || err != nil {
					t.Fatal(got, err)
				}
			}
			want := append(append([]string{}, tc.trace...), tc.trace...)
			if !reflect.DeepEqual(trace, want) {
				t.Fatal(trace, want)
			}
		})
	}
}

func TestLiveTerminalActualMeasurementBarBytesAndDynamicTTY(t *testing.T) {
	in, out, errout := terminalFiles()
	var output bytes.Buffer
	var trace []string
	ttyCalls := 0
	terminal := newLiveTerminal(in, out, errout, liveTerminalOps{
		IoctlSize: func(file *os.File, _ uintptr, size *[4]uint16) (int, error) {
			if file != out {
				t.Fatal("stream")
			}
			trace = append(trace, "columns")
			size[1] = 30
			return 0, nil
		},
		IsTTY: func(file *os.File) (bool, error) {
			if file != errout {
				t.Fatal("bar did not query original stderr")
			}
			trace = append(trace, "TTY")
			ttyCalls++
			return ttyCalls != 3, nil
		},
		Now: func() time.Time { trace = append(trace, "Now"); return time.Unix(100, 0) },
	})
	progress := ui.NewFamilyProgress(terminal.ProgressOps(&output, func() error { trace = append(trace, "flush"); return nil }))
	if err := terminal.Console().Sync(func(scope *ui.LiveConsoleScope) error {
		bar, err := progress.NewBar(scope, ui.FamilyProgressSpec{Label: "Checks", Total: big.NewInt(2)})
		if err != nil {
			return err
		}
		if err = bar.Advance(scope); err != nil {
			return err
		}
		return bar.Advance(scope)
	}); err != nil {
		t.Fatal(err)
	}
	// Hand-derived width30: without-bar19 =>11 slots, half rounds6; width20
	// at completion =>10 slots. Dynamic false third TTY suppresses final LF.
	want := "\x1b[1GChecks [======     ] 1/2 (50%)\x1b[1GChecks [==========] 2/2 (100%)"
	if output.String() != want {
		t.Fatalf("bytes %q want%q", output.String(), want)
	}
	if !reflect.DeepEqual(trace, []string{"Now", "Now", "columns", "TTY", "flush", "Now", "columns", "TTY", "flush", "Now", "TTY"}) {
		t.Fatal(trace)
	}
	for _, tc := range []struct {
		text  string
		width int
	}{{"\x1b[31me\u0301界\x1b[0m", 3}, {"a\tb\b", 2}, {"👩\u200d👩", 4}} {
		width, err := terminal.ProgressOps(nil, nil).Measure(tc.text)
		if err != nil || width != tc.width {
			t.Fatal(width, err)
		}
	}
	// Huge width is capped by the actual bar owner after full integer arithmetic.
	terminal.ops.IoctlSize = func(*os.File, uintptr, *[4]uint16) (int, error) { return 0, nil }
	terminal.ops.IsTTY = func(*os.File) (bool, error) { return false, nil }
	terminal.ops.Env = func(key string) (string, bool) {
		if key == "COLUMNS" {
			return strings.Repeat("9", 100), true
		}
		return "", false
	}
	ops := terminal.ProgressOps(&output, func() error { return nil })
	ops.IsTTY = func() (bool, error) { return true, nil }
	output.Reset()
	p := ui.NewFamilyProgress(ops)
	if err := terminal.Console().Sync(func(s *ui.LiveConsoleScope) error {
		b, e := p.NewBar(s, ui.FamilyProgressSpec{Mode: ui.FamilyCount, Label: "x", Total: big.NewInt(2)})
		if e != nil {
			return e
		}
		return b.Advance(s)
	}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "\x1b[1Gx ["+strings.Repeat("=", 40)+strings.Repeat(" ", 40)+"] 1" {
		t.Fatal("bar cap80 differs")
	}
}

func TestLiveTerminalWallClockGateAndReachedWait(t *testing.T) {
	var output bytes.Buffer
	times := []int64{10, 9, 10, 11}
	next := 0
	formats := 0
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{Now: func() time.Time { n := times[next]; next++; return time.Unix(n, 0) }, IoctlSize: func(*os.File, uintptr, *[4]uint16) (int, error) { formats++; return 0, nil }, IsTTY: func(*os.File) (bool, error) { return false, nil }, Env: func(string) (string, bool) { return "", false }})
	p := ui.NewFamilyProgress(terminal.ProgressOps(&output, nil))
	if err := terminal.Console().Sync(func(s *ui.LiveConsoleScope) error {
		b, e := p.NewBar(s, ui.FamilyProgressSpec{Mode: ui.FamilyCount, Label: "x", Total: big.NewInt(4)})
		if e != nil {
			return e
		}
		if e = b.Advance(s); e != nil {
			return e
		}
		return b.Advance(s)
	}); err != nil {
		t.Fatal(err)
	}
	if next != 4 || formats != 1 || output.Len() != 0 {
		t.Fatal("wall gate/TTY order", next, formats)
	}
	sentinel := errors.New("wait")
	ctx := context.Background()
	terminal.ops.Wait = func(got context.Context, d time.Duration) error {
		if got != ctx || d != time.Second {
			t.Fatal("wait changed")
		}
		return sentinel
	}
	if err := terminal.Wait(ctx, time.Second); err != sentinel {
		t.Fatal(err)
	}
}

func TestLiveResizePartialAcquisitionAndLazyFailures(t *testing.T) {
	operation, cleanup := errors.New("acquire"), errors.New("cleanup")
	for _, tc := range []struct {
		name    string
		notify  func(chan<- os.Signal) (func() error, error)
		partial bool
	}{
		{"missing", nil, false}, {"no release", func(chan<- os.Signal) (func() error, error) { return nil, nil }, false}, {"failed", func(chan<- os.Signal) (func() error, error) { return nil, operation }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{NotifyResize: tc.notify})
			r := terminal.NewResize()
			release, err := r.Subscribe(func() error { t.Fatal("failed subscription dispatched"); return nil })
			if err == nil || release != nil {
				t.Fatal(err)
			}
			if _, ok := <-r.Errors(); ok {
				t.Fatal("failed acquisition channel not empty closed")
			}
			if terminal.activeResize {
				t.Fatal("failed subscription retained active slot")
			}
		})
	}
	closes := 0
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{NotifyResize: func(chan<- os.Signal) (func() error, error) {
		return func() error { closes++; return cleanup }, operation
	}})
	r := terminal.NewResize()
	release, err := r.Subscribe(func() error { t.Fatal("partial subscription dispatched"); return nil })
	if err != operation || release == nil {
		t.Fatal(err)
	}
	if _, ok := <-r.Errors(); ok {
		t.Fatal("partial error channel")
	}
	for range 2 {
		if err = release(); err != cleanup {
			t.Fatal(err)
		}
	}
	if closes != 1 || terminal.activeResize {
		t.Fatal("partial cleanup")
	}
}

func TestLiveResizeFirstUnchangedErrorAndReleaseDuringCallback(t *testing.T) {
	callbackErr, cleanupErr := errors.New("resize"), errors.New("unregister")
	entered, finish, unregistered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var notify chan<- os.Signal
	calls, unregisters := 0, 0
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{NotifyResize: func(ch chan<- os.Signal) (func() error, error) {
		notify = ch
		return func() error { unregisters++; close(unregistered); return cleanupErr }, nil
	}})
	r := terminal.NewResize()
	release, err := r.Subscribe(func() error { calls++; close(entered); <-finish; return callbackErr })
	if err != nil {
		t.Fatal(err)
	}
	if release2, err := r.Subscribe(func() error { return nil }); release2 != nil || err == nil {
		t.Fatal("interval reused")
	}
	if release2, err := terminal.NewResize().Subscribe(func() error { return nil }); release2 != nil || err == nil {
		t.Fatal("concurrent invocation interval")
	}
	notify <- syscall.SIGWINCH
	<-entered
	results := make(chan error, 2)
	go func() { results <- release() }()
	<-unregistered
	go func() { results <- release() }()
	select {
	case <-results:
		t.Fatal("release returned before admitted callback")
	default:
	}
	notify <- syscall.SIGWINCH // queued later admission must be suppressed
	close(finish)
	for range 2 {
		if err := <-results; err != cleanupErr {
			t.Fatal(err)
		}
	}
	if err, ok := <-r.Errors(); !ok || err != callbackErr || !errors.Is(err, callbackErr) {
		t.Fatal(err, ok)
	}
	if _, ok := <-r.Errors(); ok {
		t.Fatal("more than first error")
	}
	if calls != 1 || unregisters != 1 || terminal.activeResize {
		t.Fatal(calls, unregisters)
	}
	// A separately scripted subscriber is unaffected by this release; no
	// process-wide signal reset is modeled or executed.
	other := make(chan os.Signal, 1)
	other <- syscall.SIGWINCH
	if <-other != syscall.SIGWINCH {
		t.Fatal("other subscriber altered")
	}
	terminal.ops.NotifyResize = func(chan<- os.Signal) (func() error, error) { return func() error { return nil }, nil }
	end, e := terminal.NewResize().Subscribe(func() error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if e = end(); e != nil {
		t.Fatal(e)
	}
}

// Barrier/memory writer: no descriptor, native timer or scheduler sleeps.
type terminalGateWriter struct {
	entered, proceed chan struct{}
	once             sync.Once
	bytes.Buffer
}

func (w *terminalGateWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.proceed })
	return w.Buffer.Write(p)
}

func (w *terminalGateWriter) WriteString(text string) (int, error) {
	return w.Write([]byte(text))
}

func TestLiveTerminalSharedScopeAndBlockedWriterExclusion(t *testing.T) {
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{
		Now:   func() time.Time { return time.Unix(1, 0) },
		IsTTY: func(*os.File) (bool, error) { return true, nil },
	})
	writer := &terminalGateWriter{entered: make(chan struct{}), proceed: make(chan struct{})}
	view, err := ui.OpenLiveView(ui.LiveViewOptions{Console: terminal.Console()}, ui.LiveViewOps{})
	if err != nil {
		t.Fatal(err)
	}
	progress := ui.NewFamilyProgress(terminal.ProgressOps(writer, func() error { return nil }))
	first, second := make(chan error, 1), make(chan error, 1)
	attempt := make(chan struct{})
	go func() {
		first <- view.SyncConsole(func(s *ui.LiveConsoleScope) error {
			return s.Sync(func(n *ui.LiveConsoleScope) error {
				if n != s {
					return errors.New("different nested scope")
				}
				if e := n.Print(writer, "frame"); e != nil {
					return e
				}
				bar, err := progress.NewBar(n, ui.FamilyProgressSpec{Mode: ui.FamilyLiteral, Label: "bar"})
				if err != nil {
					return err
				}
				return bar.Finish(n)
			})
		})
	}()
	<-writer.entered
	go func() {
		close(attempt)
		second <- terminal.Console().Sync(func(s *ui.LiveConsoleScope) error { return s.Puts(writer, "message") })
	}()
	<-attempt
	select {
	case <-second:
		t.Fatal("message interleaved blocked frame")
	default:
	}
	close(writer.proceed)
	if e := <-first; e != nil {
		t.Fatal(e)
	}
	if e := <-second; e != nil {
		t.Fatal(e)
	}
	if writer.String() != "frame\x1b[1Gbar\nmessage\n" {
		t.Fatal(writer.String())
	}
	if err := view.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveTerminalFamilyProgressEventBoundaries(t *testing.T) {
	// Literal event traces exercise the seven source consumers through actual
	// released UI owners. Native output and family scheduling are not invoked.
	for _, name := range []string{"single copy", "bulk copy", "deploy", "health", "SSH", "build", "GC", "GC zero workers"} {
		t.Run(name, func(t *testing.T) {
			clock := 0
			terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{
				Now:       func() time.Time { clock++; return time.Unix(1, 0) },
				IoctlSize: func(*os.File, uintptr, *[4]uint16) (int, error) { return 0, nil },
				IsTTY:     func(*os.File) (bool, error) { return false, nil },
				Env:       func(string) (string, bool) { return "", false },
			})
			p := ui.NewFamilyProgress(terminal.ProgressOps(nil, nil))
			check := func(s *ui.LiveConsoleScope, b *ui.FamilyBar, count int64, done bool) error {
				state, err := b.State(s)
				if err != nil {
					return err
				}
				if state.Current.Cmp(big.NewInt(count)) != 0 || state.Done != done {
					return fmt.Errorf("state %v want %d/%v", state, count, done)
				}
				return nil
			}
			var trace []string
			var buildMulti *ui.FamilyMulti
			err := terminal.Console().Sync(func(s *ui.LiveConsoleScope) error {
				switch name {
				case "deploy":
					trace = append(trace, "activation message") // no bar/clock at this source site
					return nil
				case "single copy", "health", "SSH":
					label := map[string]string{"single copy": "Copying", "health": "Checks", "SSH": "Command"}[name]
					spec := ui.FamilyProgressSpec{Label: label}
					if name != "single copy" {
						spec.Total = big.NewInt(2)
					}
					b, err := p.NewBar(s, spec)
					if err != nil {
						return err
					}
					if name == "single copy" {
						trace = append(trace, "feed path", "set changed total")
						if err = b.SetTotal(s, big.NewInt(2)); err != nil {
							return err
						}
					}
					if name == "health" {
						trace = append(trace, "attempt failed", "attempt failed", "check completed")
					}
					if name == "SSH" {
						trace = append(trace, "result stored")
					}
					trace = append(trace, "Advance")
					if err = b.Advance(s); err != nil {
						return err
					}
					return check(s, b, 1, false)
				case "bulk copy":
					m, err := p.NewMulti(s, ui.FamilyProgressSpec{Label: "Copying"})
					if err != nil {
						return err
					}
					b, err := m.Register(s, "a")
					if err != nil {
						return err
					}
					c, err := m.Register(s, "b")
					if err != nil {
						return err
					}
					trace = append(trace, "register a", "register b", "set child total", "resume done top", "set top total", "Advance a")
					if err = b.SetTotal(s, big.NewInt(2)); err != nil {
						return err
					}
					if err = m.Top().Resume(s); err != nil {
						return err
					}
					total, err := m.Total(s)
					if err != nil {
						return err
					}
					if err = m.Top().SetTotal(s, total); err != nil {
						return err
					}
					if err = b.Advance(s); err != nil {
						return err
					}
					trace = append(trace, "literal nothing to do", "Advance b")
					if err = c.SetLiteral(s, "b: nothing to do"); err != nil {
						return err
					}
					if err = c.Advance(s); err != nil {
						return err
					}
					if err = check(s, b, 1, false); err != nil {
						return err
					}
					if err = check(s, c, 1, false); err != nil {
						return err
					}
					return check(s, m.Top(), 2, true)
				case "build":
					m, err := p.NewMulti(s, ui.FamilyProgressSpec{Label: "nix build"})
					if err != nil {
						return err
					}
					b, err := m.Register(s, "Building")
					if err != nil {
						return err
					}
					f, err := m.Register(s, "Fetching")
					if err != nil {
						return err
					}
					for _, child := range []*ui.FamilyBar{b, f} {
						if err = child.SetTotal(s, big.NewInt(2)); err != nil {
							return err
						}
						trace = append(trace, "child first positive total", "child Advance")
						if err = child.Advance(s); err != nil {
							return err
						}
					}
					before, err := m.Top().State(s)
					if err != nil {
						return err
					}
					if before.Total != nil {
						return errors.New("top updated before child event")
					}
					buildMulti = m
					return check(s, m.Top(), 2, false)
				default:
					m, err := p.NewMulti(s, ui.FamilyProgressSpec{Mode: ui.FamilyCount, Label: "Collecting garbage"})
					if err != nil {
						return err
					}
					b, err := m.Register(s, "a")
					if err != nil {
						return err
					}
					c, err := m.Register(s, "b")
					if err != nil {
						return err
					}
					trace = append(trace, "register a", "register b")
					if name == "GC zero workers" {
						if err = check(s, b, 0, false); err != nil {
							return err
						}
						if err = check(s, c, 0, false); err != nil {
							return err
						}
						return check(s, m.Top(), 0, false)
					}
					trace = append(trace, "raw nonpath line", "raw matched deleting path", "Advance a")
					if err = b.Advance(s); err != nil {
						return err
					}
					// The same borrowed scope makes status+Finish one atomic operation.
					trace = append(trace, "literal status a", "Finish a", "literal status b", "Finish b")
					if err = b.SetLiteral(s, "a: done"); err != nil {
						return err
					}
					if err = b.Finish(s); err != nil {
						return err
					}
					if err = c.SetLiteral(s, "b: error occurred"); err != nil {
						return err
					}
					if err = c.Finish(s); err != nil {
						return err
					}
					if err = check(s, b, 1, true); err != nil {
						return err
					}
					if err = check(s, c, 0, true); err != nil {
						return err
					}
					return check(s, m.Top(), 1, false)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if buildMulti != nil {
				// Source's top-total update is a separate later root scope.
				err = terminal.Console().Sync(func(s *ui.LiveConsoleScope) error {
					trace = append(trace, "later top total")
					total, err := buildMulti.Total(s)
					if err != nil {
						return err
					}
					return buildMulti.Top().SetTotal(s, total)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if name == "deploy" && clock != 0 {
				t.Fatal("activation acquired a bar clock")
			}
			if name == "health" && !reflect.DeepEqual(trace, []string{"attempt failed", "attempt failed", "check completed", "Advance"}) {
				t.Fatal(trace)
			}
			if name == "SSH" && !reflect.DeepEqual(trace, []string{"result stored", "Advance"}) {
				t.Fatal(trace)
			}
			if name == "GC zero workers" && !reflect.DeepEqual(trace, []string{"register a", "register b"}) {
				t.Fatal(trace)
			}
		})
	}
}

func TestLiveTerminalActualViewResizeErrorWaitsAndSequentialCache(t *testing.T) {
	device := new(os.File)
	sizeErr := errors.New("resize size")
	opens, sizes, closes := 0, 0, 0
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{OpenConsole: func() (*os.File, error) { opens++; return device, nil }, Dimensions: func(*os.File) (int, int, error) {
		sizes++
		if sizes == 3 {
			return 0, 0, sizeErr
		}
		return 1, 8, nil
	}, CloseConsole: func(file *os.File) error {
		if file != device {
			t.Fatal("borrowed close")
		}
		closes++
		return nil
	}})
	if _, _, _, e := terminal.Dimensions(); e != nil {
		t.Fatal(e)
	} // earlier source header demand
	for i := 0; i < 2; i++ {
		var notify chan<- os.Signal
		terminal.ops.NotifyResize = func(ch chan<- os.Signal) (func() error, error) { notify = ch; return func() error { return nil }, nil }
		renderWait := make(chan struct{})
		var once sync.Once
		terminal.ops.Wait = func(ctx context.Context, d time.Duration) error {
			if d == time.Second {
				return nil
			}
			if d != 100*time.Millisecond {
				t.Error("renderer duration", d)
			}
			once.Do(func() { close(renderWait) })
			<-ctx.Done()
			return ctx.Err()
		}
		resize := terminal.NewResize()
		var output bytes.Buffer
		view, e := ui.OpenLiveView(ui.LiveViewOptions{Enabled: true, Auto: true, ReservedLines: 10, Output: &output, Console: terminal.Console()}, ui.LiveViewOps{Dimensions: terminal.Dimensions, Wait: terminal.Wait, SubscribeResize: resize.Subscribe})
		if e != nil {
			t.Fatal(e)
		}
		<-renderWait
		if i == 0 {
			notify <- syscall.SIGWINCH
			if err := <-resize.Errors(); err != sizeErr {
				t.Fatal(err)
			}
			if e = view.FeedLine("after size failure"); e != nil {
				t.Fatal(e)
			}
			if e = view.Flush(context.Background()); e != nil {
				t.Fatal("resize poisoned Flush", e)
			}
		}
		if e = view.Stop(); e != nil {
			t.Fatal("resize became Stop error", e)
		}
		if !strings.HasPrefix(output.String(), "\n\x1b[2J\x1b[H\x1b[2K\x1b[1GLog --->\n") {
			t.Fatalf("initial frame prefix %q", output.String())
		}
		if closes != 0 {
			t.Fatal("view closed invocation cache")
		}
	}
	if e := terminal.CloseConsole(); e != nil {
		t.Fatal(e)
	}
	if opens != 1 || sizes != 4 || closes != 1 {
		t.Fatal(opens, sizes, closes)
	}
	// Disabled view retains one caller-context second, with no render work.
	flushes := 0
	terminal.ops.Wait = func(context.Context, time.Duration) error { flushes++; return nil }
	v, e := ui.OpenLiveView(ui.LiveViewOptions{}, ui.LiveViewOps{Wait: terminal.Wait})
	if e != nil {
		t.Fatal(e)
	}
	if e = v.Flush(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = v.Stop(); e != nil {
		t.Fatal(e)
	}
	if flushes != 1 {
		t.Fatal(flushes)
	}
}

func TestLiveTerminalSevenCallerObligations(t *testing.T) {
	// These are source-derived future adapter obligations, not production
	// family wiring or async Ruby main-stack/native evidence. The actual view,
	// bars, console, dimensions/cache and resize owner are composed privately.
	for _, family := range []struct {
		name                    string
		flush, logger, auto, gc bool
		reserve                 int
	}{
		{"single copy", false, false, false, false, 0}, {"bulk copy", false, false, false, false, 0},
		{"deploy", true, true, true, false, 10}, {"health", true, false, true, false, 10},
		{"SSH", true, true, true, false, 10}, {"build", false, true, true, false, 10}, {"GC", true, false, true, true, 10},
	} {
		for _, scenario := range []string{"normal", "ordinary false", "body", "resize before", "resize after", "Stop", "close", "primary plus cleanup", "resize during Stop", "skip or zero workers"} {
			t.Run(family.name+"/"+scenario, func(t *testing.T) {
				bodyErr, resizeErr, stopErr, closeErr := errors.New("body"), errors.New("resize"), errors.New("Stop"), errors.New("close")
				var trace []string
				var output bytes.Buffer
				device := new(os.File)
				sizeCalls := 0
				var notify chan<- os.Signal
				callbackEntered, callbackReturn := make(chan struct{}), make(chan struct{})
				terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{OpenConsole: func() (*os.File, error) { trace = append(trace, "open"); return device, nil }, Dimensions: func(*os.File) (int, int, error) {
					sizeCalls++
					if sizeCalls == 2 {
						if scenario == "resize during Stop" {
							close(callbackEntered)
							<-callbackReturn
						}
						return 0, 0, resizeErr
					}
					return 1, 30, nil
				}, CloseConsole: func(*os.File) error {
					trace = append(trace, "close")
					if scenario == "close" || scenario == "primary plus cleanup" {
						return closeErr
					}
					return nil
				}, NotifyResize: func(ch chan<- os.Signal) (func() error, error) {
					notify = ch
					return func() error {
						trace = append(trace, "release")
						if scenario == "resize during Stop" {
							close(callbackReturn)
						}
						if scenario == "Stop" || scenario == "primary plus cleanup" {
							return stopErr
						}
						return nil
					}, nil
				}, Now: func() time.Time { return time.Unix(10, 0) }, IoctlSize: func(*os.File, uintptr, *[4]uint16) (int, error) { return 0, nil }, IsTTY: func(*os.File) (bool, error) { return false, nil }, Env: func(string) (string, bool) { return "", false }})
				renderEntered := make(chan struct{})
				var once sync.Once
				terminal.ops.Wait = func(ctx context.Context, d time.Duration) error {
					if d == time.Second {
						trace = append(trace, "Flush")
						return nil
					}
					once.Do(func() { close(renderEntered) })
					<-ctx.Done()
					return ctx.Err()
				}
				resize := terminal.NewResize()
				ops := ui.LiveViewOps{Dimensions: terminal.Dimensions, Wait: terminal.Wait, SubscribeResize: resize.Subscribe}
				if family.logger {
					ops.AttachLogger = func(func([]byte) error) (func() error, error) {
						trace = append(trace, "attach")
						return func() error { trace = append(trace, "detach"); return nil }, nil
					}
				}
				view, e := ui.OpenLiveView(ui.LiveViewOptions{Enabled: true, Auto: family.auto, ReservedLines: family.reserve, Header: "h\n", Output: &output, Console: terminal.Console()}, ops)
				if e != nil {
					t.Fatal(e)
				}
				<-renderEntered
				check := func() error {
					select {
					case err := <-resize.Errors():
						return err
					default:
						return nil
					}
				}
				publish := func() { notify <- syscall.SIGWINCH; <-resize.joined }
				var primary error
				if scenario == "resize before" {
					publish()
				}
				if primary = check(); primary == nil {
					trace = append(trace, "dispatch")
					if e = view.FeedLine("raw partial\n"); e != nil {
						t.Fatal(e)
					}
					if scenario == "body" || scenario == "primary plus cleanup" || scenario == "ordinary false" && (family.name == "single copy" || family.name == "bulk copy" || family.name == "deploy" || family.name == "build") {
						primary = bodyErr
					}
					if scenario == "resize after" {
						publish()
					}
					if e = check(); primary == nil {
						primary = e
					}
				}
				if primary == nil && family.flush {
					if e = check(); e != nil {
						primary = e
					} else {
						primary = view.Flush(context.Background())
						if e = check(); primary == nil {
							primary = e
						}
					}
				}
				if scenario == "resize during Stop" {
					notify <- syscall.SIGWINCH
					<-callbackEntered
				}
				trace = append(trace, "Stop")
				stopped := view.Stop()
				if !family.gc && stopped != nil || family.gc && primary == nil {
					if stopped != nil {
						primary = stopped
					}
				}
				if e = check(); primary == nil {
					primary = e
				} // mandatory final resize check after join
				if scenario == "normal" || scenario == "ordinary false" || scenario == "skip or zero workers" {
					if primary == nil {
						trace = append(trace, "aggregate")
						if scenario == "ordinary false" && family.gc {
							primary = bodyErr // ordinary GC failures aggregate after normal Flush/Stop
						}
					}
				}
				closed := terminal.CloseConsole()
				if primary == nil {
					primary = closed
				}
				var want error
				switch scenario {
				case "ordinary false":
					if family.gc || family.name == "single copy" || family.name == "bulk copy" || family.name == "deploy" || family.name == "build" {
						want = bodyErr
					}
				case "body":
					want = bodyErr
				case "resize before", "resize after", "resize during Stop":
					want = resizeErr
				case "Stop":
					want = stopErr
				case "close":
					want = closeErr
				case "primary plus cleanup":
					want = stopErr
					if family.gc {
						want = bodyErr
					}
				}
				if primary != want || want != nil && !errors.Is(primary, want) {
					t.Fatalf("primary %v want%v trace%v", primary, want, trace)
				}
				joined := strings.Join(trace, ",")
				if scenario == "resize before" && strings.Contains(joined, "dispatch") {
					t.Fatal("dispatch after resize")
				}
				expectedFlush := family.flush && (scenario == "normal" || scenario == "ordinary false" && family.name != "deploy" || scenario == "Stop" || scenario == "close" || scenario == "resize during Stop" || scenario == "skip or zero workers")
				if strings.Contains(joined, "Flush") != expectedFlush {
					t.Fatalf("Flush %v", trace)
				}
				if !strings.HasSuffix(joined, "close") || strings.Index(joined, "release") < strings.Index(joined, "Stop") {
					t.Fatal("quiescence/close order", trace)
				}
				if family.logger && !strings.Contains(joined, "attach,dispatch") && scenario != "resize before" {
					t.Fatal("logger lifetime", trace)
				}
				// Selection-disabled deploy still attaches/detaches and normal Flushes.
				if family.name == "deploy" && scenario == "skip or zero workers" {
					calls := []string{}
					disabled, e := ui.OpenLiveView(ui.LiveViewOptions{}, ui.LiveViewOps{Wait: func(context.Context, time.Duration) error { calls = append(calls, "Flush"); return nil }, AttachLogger: func(func([]byte) error) (func() error, error) {
						calls = append(calls, "attach")
						return func() error { calls = append(calls, "detach"); return nil }, nil
					}})
					if e != nil {
						t.Fatal(e)
					}
					if e = disabled.Flush(context.Background()); e != nil {
						t.Fatal(e)
					}
					if e = disabled.Stop(); e != nil {
						t.Fatal(e)
					}
					if !reflect.DeepEqual(calls, []string{"attach", "Flush", "detach"}) {
						t.Fatal(calls)
					}
				}
			})
		}
	}
}

// Keep the imported short-write owner used by this composition explicit.
func TestLiveTerminalBorrowedWriterPartialError(t *testing.T) {
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{})
	if err := terminal.Console().Sync(func(s *ui.LiveConsoleScope) error { return s.Print(terminalShortWriter{}, "abc") }); err != io.ErrShortWrite {
		t.Fatal(err)
	}
}

type terminalShortWriter struct{}

func (terminalShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestLiveResizeSuccessAndOtherOwnedSubscriberSurvives(t *testing.T) {
	channels := make(map[chan<- os.Signal]bool)
	var mutex sync.Mutex
	ops := liveTerminalOps{NotifyResize: func(ch chan<- os.Signal) (func() error, error) {
		mutex.Lock()
		channels[ch] = true
		mutex.Unlock()
		return func() error { mutex.Lock(); delete(channels, ch); mutex.Unlock(); return nil }, nil
	}}
	a, b := newLiveTerminal(nil, nil, nil, ops).NewResize(), newLiveTerminal(nil, nil, nil, ops).NewResize()
	ackA, ackB := make(chan struct{}, 2), make(chan struct{}, 2)
	releaseA, err := a.Subscribe(func() error { ackA <- struct{}{}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := b.Subscribe(func() error { ackB <- struct{}{}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		a.notify <- syscall.SIGWINCH
		<-ackA
	}
	if err = releaseA(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-a.Errors(); ok {
		t.Fatal("successful callback emitted error")
	}
	mutex.Lock()
	alive := channels[b.notify] && !channels[a.notify] && len(channels) == 1
	mutex.Unlock()
	if !alive {
		t.Fatal("released unrelated registration")
	}
	b.notify <- syscall.SIGWINCH
	<-ackB
	if err = releaseB(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-b.Errors(); ok {
		t.Fatal("successful callback emitted error")
	}
}

func TestLiveTerminalCloseJoinsQueryAndActualTopFailureKeepsChild(t *testing.T) {
	entered, proceed, closing := make(chan struct{}), make(chan struct{}), make(chan struct{})
	device := new(os.File)
	closed := false
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{OpenConsole: func() (*os.File, error) { return device, nil }, Dimensions: func(*os.File) (int, int, error) {
		close(entered)
		<-proceed
		if closed {
			t.Error("close passed query")
		}
		return 1, 2, nil
	}, CloseConsole: func(*os.File) error { closed = true; return nil }})
	query, closeResult := make(chan error, 1), make(chan error, 1)
	go func() { _, _, _, e := terminal.Dimensions(); query <- e }()
	<-entered
	go func() { close(closing); closeResult <- terminal.CloseConsole() }()
	<-closing
	select {
	case <-closeResult:
		t.Fatal("close returned before query")
	default:
	}
	close(proceed)
	if e := <-query; e != nil {
		t.Fatal(e)
	}
	if e := <-closeResult; e != nil {
		t.Fatal(e)
	}
	if !closed {
		t.Fatal("close missing")
	}
	sentinel := errors.New("top columns")
	terminal.ops.Now = func() time.Time { return time.Unix(1, 0) }
	terminal.ops.IoctlSize = func(*os.File, uintptr, *[4]uint16) (int, error) { return -1, sentinel }
	var output bytes.Buffer
	p := ui.NewFamilyProgress(terminal.ProgressOps(&output, nil))
	if e := terminal.Console().Sync(func(s *ui.LiveConsoleScope) error {
		m, e := p.NewMulti(s, ui.FamilyProgressSpec{Mode: ui.FamilyCount, Label: "top"})
		if e != nil {
			return e
		}
		b, e := m.Register(s, "child")
		if e != nil {
			return e
		}
		if e = b.Advance(s); e != sentinel {
			return fmt.Errorf("top error %v", e)
		}
		top, _ := m.Top().State(s)
		child, _ := b.State(s)
		if top.Current.Int64() != 1 || child.Current.Sign() != 0 || output.Len() != 0 {
			return errors.New("top failure changed child or emitted bytes")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestLiveTerminalStopCancelsOnlyRendererWait(t *testing.T) {
	render, flush, finishFlush := make(chan struct{}), make(chan struct{}), make(chan struct{})
	terminal := newLiveTerminal(nil, nil, nil, liveTerminalOps{OpenConsole: func() (*os.File, error) { return new(os.File), nil }, Dimensions: func(*os.File) (int, int, error) { return 1, 8, nil }, NotifyResize: func(chan<- os.Signal) (func() error, error) { return func() error { return nil }, nil }})
	caller := context.Background()
	var once sync.Once
	terminal.ops.Wait = func(ctx context.Context, d time.Duration) error {
		if d == time.Second {
			if ctx != caller {
				t.Error("caller context replaced")
			}
			close(flush)
			<-finishFlush
			return ctx.Err()
		}
		if d != 100*time.Millisecond {
			t.Error("renderer duration")
		}
		once.Do(func() { close(render) })
		<-ctx.Done()
		return ctx.Err()
	}
	r := terminal.NewResize()
	var output bytes.Buffer
	v, e := ui.OpenLiveView(ui.LiveViewOptions{Enabled: true, Output: &output, Console: terminal.Console()}, ui.LiveViewOps{Dimensions: terminal.Dimensions, Wait: terminal.Wait, SubscribeResize: r.Subscribe})
	if e != nil {
		t.Fatal(e)
	}
	<-render
	result := make(chan error, 1)
	go func() { result <- terminal.Wait(caller, time.Second) }()
	<-flush
	if e = v.Stop(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-result:
		t.Fatal("Stop returned caller wait")
	default:
	}
	close(finishFlush)
	if e = <-result; e != nil {
		t.Fatal(e)
	}
}
