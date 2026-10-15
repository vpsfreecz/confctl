package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/vpsfreecz/confctl/internal/ui"
)

// SOURCE-DERIVED scripted syscall/read/wake contracts. These files identify
// borrowed/owned handles only; no terminal ioctl/poll or native key is run.
type basicLinuxScript struct {
	t               *testing.T
	root            string
	in, out, stderr *os.File
	mutex           sync.Mutex
	events          []string
	saved           syscall.Termios
	sets            []syscall.Termios
	pipeCount       int
	owned           []*os.File
}

func (s *basicLinuxScript) event(event string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.events = append(s.events, event)
}
func (s *basicLinuxScript) trace() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]string(nil), s.events...)
}
func (s *basicLinuxScript) file(name string) *os.File {
	s.t.Helper()
	f, err := os.Create(filepath.Join(s.root, name))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = f.Close() })
	return f
}
func basicScript(t *testing.T) (*basicLinuxScript, basicLinuxOps) {
	t.Helper()
	s := &basicLinuxScript{t: t, root: t.TempDir(), saved: syscall.Termios{Iflag: 0xffffffff, Oflag: 0xffffffff, Cflag: 0xffffffff, Lflag: 0xffffffff, Line: 7, Ispeed: 17, Ospeed: 19}}
	for i := range s.saved.Cc {
		s.saved.Cc[i] = byte(i + 50)
	}
	s.in, s.out, s.stderr = s.file("stdin"), s.file("stdout"), s.file("stderr")
	identity := basicInputIdentity{FD: 3, Dev: 4, Ino: 5, Rdev: 6, Mode: 7}
	ops := basicLinuxOps{
		Size: func(f *os.File) (ui.BasicSize, error) {
			s.event("size:" + filepath.Base(f.Name()))
			return ui.BasicSize{Rows: big.NewInt(27), Columns: big.NewInt(80)}, nil
		},
		Termios: func(f *os.File, request uintptr, attributes *syscall.Termios) error {
			if request == syscall.TCGETS {
				s.event("get:" + filepath.Base(f.Name()))
				*attributes = s.saved
			} else {
				s.event("set:" + filepath.Base(f.Name()))
				s.sets = append(s.sets, *attributes)
			}
			return nil
		},
		Identity: func(f *os.File) (basicInputIdentity, error) {
			s.event("identity:" + filepath.Base(f.Name()))
			return identity, nil
		},
		Env: func(name string) (string, bool) { s.event("env:" + name); return "", false },
		Pipe: func() (*os.File, *os.File, error) {
			s.event("pipe")
			s.pipeCount++
			r, w := s.file(fmt.Sprintf("wake-read-%d", s.pipeCount)), s.file(fmt.Sprintf("wake-write-%d", s.pipeCount))
			s.owned = append(s.owned, r, w)
			return r, w, nil
		},
		Poll: func(in, wake *os.File) error {
			if in != s.in {
				s.t.Error("poll substituted stdin")
			}
			s.event("poll")
			return nil
		},
		Byte:  func(input *bufio.Reader) (byte, error) { s.event("byte"); return input.ReadByte() },
		Wake:  func(*os.File) error { s.event("wake"); return nil },
		Close: func(f *os.File) error { s.event("close:" + filepath.Base(f.Name())); return f.Close() },
	}
	return s, ops
}
func basicSizeStrings(size ui.BasicSize) []string {
	return []string{size.Rows.String(), size.Columns.String()}
}
func basicBuffered(text string) *bufio.Reader {
	r := bufio.NewReader(strings.NewReader(text))
	_, _ = r.Peek(len(text))
	return r
}
func basicBorrowedOpen(t *testing.T, s *basicLinuxScript) {
	t.Helper()
	for _, f := range []*os.File{s.in, s.out, s.stderr} {
		if _, err := f.Stat(); err != nil {
			t.Fatal("borrowed closed", err)
		}
	}
}
func basicOwnedClosed(t *testing.T, s *basicLinuxScript) {
	t.Helper()
	for _, f := range s.owned {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("owned left open", err)
		}
	}
}

func TestBasicLinuxSizeBranchOrderAndFailures(t *testing.T) {
	failure := errors.New("stderr winsize failed")
	for _, tc := range []struct {
		name           string
		stdout, stderr ui.BasicSize
		outErr, errErr error
		tty            bool
		want           []string
		trace          []string
		err            error
	}{
		{name: "stdout-positive-zero-rows", stdout: ui.BasicSize{Rows: big.NewInt(0), Columns: big.NewInt(100)}, want: []string{"0", "100"}, trace: []string{"size:stdout"}},
		{name: "stdout-zero-then-stderr", stdout: ui.BasicSize{Rows: big.NewInt(10), Columns: big.NewInt(0)}, stderr: ui.BasicSize{Rows: big.NewInt(23), Columns: big.NewInt(90)}, tty: true, want: []string{"23", "90"}, trace: []string{"size:stdout", "get:stderr", "size:stderr"}},
		{name: "stdout-error-stderr-positive", outErr: syscall.ENOTTY, stderr: ui.BasicSize{Rows: big.NewInt(25), Columns: big.NewInt(80)}, tty: true, want: []string{"25", "80"}, trace: []string{"size:stdout", "get:stderr", "size:stderr"}},
		{name: "stderr-not-tty-default", outErr: syscall.ENOTTY, want: []string{"27", "80"}, trace: []string{"size:stdout", "get:stderr", "env:COLUMNS", "env:ANSICON"}},
		{name: "stderr-zero-default", outErr: syscall.ENOTTY, stderr: ui.BasicSize{Rows: big.NewInt(5), Columns: big.NewInt(0)}, tty: true, want: []string{"27", "80"}, trace: []string{"size:stdout", "get:stderr", "size:stderr", "env:COLUMNS", "env:ANSICON"}},
		{name: "stderr-EOPNOTSUPP-default", outErr: syscall.ENOTTY, errErr: syscall.EOPNOTSUPP, tty: true, want: []string{"27", "80"}, trace: []string{"size:stdout", "get:stderr", "size:stderr", "env:COLUMNS", "env:ANSICON"}},
		{name: "stderr-other-error", outErr: syscall.ENOTTY, errErr: failure, tty: true, err: failure, trace: []string{"size:stdout", "get:stderr", "size:stderr"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ops := basicScript(t)
			ops.Size = func(f *os.File) (ui.BasicSize, error) {
				s.event("size:" + filepath.Base(f.Name()))
				if f == s.out {
					return tc.stdout, tc.outErr
				}
				if f != s.stderr {
					t.Fatal("wrong size fd")
				}
				return tc.stderr, tc.errErr
			}
			ops.Termios = func(f *os.File, request uintptr, _ *syscall.Termios) error {
				if f != s.stderr || request != syscall.TCGETS {
					t.Fatal("wrong TTY test")
				}
				s.event("get:stderr")
				if !tc.tty {
					return syscall.ENOTTY
				}
				return nil
			}
			size, err := linuxBasicSize(context.Background(), s.out, s.stderr, ops)
			if !errors.Is(err, tc.err) || err == nil && !reflect.DeepEqual(basicSizeStrings(size), tc.want) || !reflect.DeepEqual(s.trace(), tc.trace) {
				t.Fatal(size, err, s.trace(), tc.want, tc.trace)
			}
			basicBorrowedOpen(t, s)
		})
	}
	s, ops := basicScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := linuxBasicSize(ctx, s.out, s.stderr, ops); !errors.Is(err, context.Canceled) || len(s.trace()) != 0 {
		t.Fatal(err, s.trace())
	}
	ctx, cancel = context.WithCancel(context.Background())
	ops.Size = func(*os.File) (ui.BasicSize, error) { cancel(); return ui.BasicSize{}, syscall.ENOTTY }
	if _, err := linuxBasicSize(ctx, s.out, s.stderr, ops); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBasicLinuxEnvironmentLiteralScalarRules(t *testing.T) {
	const huge = "99999999999999999999999999999999999999999999999999"
	for _, tc := range []struct {
		env  map[string]string
		want []string
	}{
		{map[string]string{"COLUMNS": "80", "ROWS": "42"}, []string{"42", "80"}},
		{map[string]string{"COLUMNS": "80", "LINES": "", "ROWS": "42"}, []string{"0", "80"}},
		{map[string]string{"COLUMNS": huge, "LINES": "-" + huge}, []string{"-" + huge, huge}},
		{map[string]string{"COLUMNS": "80", "LINES": " \t+1_2tail"}, []string{"12", "80"}},
		{map[string]string{"COLUMNS": "0\n80", "LINES": "99"}, []string{"27", "80"}},
		{map[string]string{"COLUMNS": "80junk\n90", "LINES": "-2"}, []string{"-2", "80"}},
		{map[string]string{"COLUMNS": "x\n80", "LINES": "99"}, []string{"27", "80"}},
		{map[string]string{"COLUMNS": "80\n", "LINES": "-0"}, []string{"0", "80"}},
		{map[string]string{"COLUMNS": "+80", "ANSICON": "prefix(1_2x-3)"}, []string{"-3", "12"}},
		{map[string]string{"ANSICON": "first(80x2) later(90x3)"}, []string{"3", "80"}},
		{map[string]string{"ANSICON": "first(80x2)\nnext(90x3)"}, []string{"2", "80"}},
		{map[string]string{"ANSICON": "(80x2x3)"}, []string{"3", "80"}},
		{map[string]string{"ANSICON": "(-1x3)"}, []string{"27", "80"}},
		{map[string]string{}, []string{"27", "80"}},
	} {
		size, err := basicEnvironmentSize(func(name string) (string, bool) { v, ok := tc.env[name]; return v, ok })
		if err != nil || !reflect.DeepEqual(basicSizeStrings(size), tc.want) {
			t.Fatal(tc, size, err)
		}
	}
	for _, tc := range []struct{ text, want string }{
		{"", "0"}, {"+", "0"}, {"-", "0"}, {"_12", "0"}, {"1__2", "1"}, {"1_2_3", "123"}, {"1_", "1"}, {" \x00 2", "0"}, {"\v\f\r\n\t -12tail", "-12"}, {"0x10", "0"}, {"１２", "0"}, {"\u00a0 2", "0"}, {"-0", "0"}, {huge, huge},
	} {
		if got := basicDecimalPrefix(tc.text).String(); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	for _, env := range []map[string]string{{"COLUMNS": "\xff"}, {"COLUMNS": "80", "LINES": "\xff"}, {"ANSICON": "\xff"}} {
		if _, err := basicEnvironmentSize(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); !errors.Is(err, ui.ErrBasicEncoding) {
			t.Fatal(env, err)
		}
	}
	// Unreached row/ANSICON invalid bytes do not prevalidate the environment.
	size, err := basicEnvironmentSize(func(k string) (string, bool) {
		return map[string]string{"COLUMNS": "80", "LINES": "2", "ANSICON": "\xff", "ROWS": "\xff"}[k], true
	})
	if err != nil || size.Rows.String() != "2" {
		t.Fatal(size, err)
	}
}

func TestBasicLinuxRawMaskAndNativeLayout(t *testing.T) {
	s, _ := basicScript(t)
	raw := basicRawTermios(s.saved)
	if raw.Iflag != 0xfffffa14 || raw.Oflag != 0xfffffffe || raw.Cflag != 0xfffffeff || raw.Lflag != 0xffff7f84 || raw.Line != 7 || raw.Ispeed != 17 || raw.Ospeed != 19 {
		t.Fatal(raw)
	}
	for i, value := range raw.Cc {
		want := byte(i + 50)
		if i == syscall.VMIN {
			want = 1
		}
		if i == syscall.VTIME {
			want = 0
		}
		if value != want {
			t.Fatal(i, value, want)
		}
	}
	if unsafe.Sizeof(basicWinsize{}) != 8 || unsafe.Offsetof(basicWinsize{}.Columns) != 2 || unsafe.Sizeof(basicPollFD{}) != 8 || unsafe.Offsetof(basicPollFD{}.Events) != 4 || unsafe.Offsetof(basicPollFD{}.Revents) != 6 {
		t.Fatal("Linux syscall struct layout")
	}
	// Structure inspection does not execute ioctl/ppoll or prove another ABI.
}

func TestBasicLinuxBufferedOneCharacterAndConfirmationTail(t *testing.T) {
	for _, key := range []string{"q", "Q", "x", "\r", "\x03", "\x1a", "é", "界", "😀", "�"} {
		t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
			s, ops := basicScript(t)
			input := basicBuffered("y\n" + key + "TAIL")
			var confirm bytes.Buffer
			if err := testConnectionConfirm(input, &confirm); err != nil {
				t.Fatal(err)
			}
			got, err := linuxBasicKey(context.Background(), s.in, input, ops)
			tail, _ := io.ReadAll(input)
			if err != nil || got != key || string(tail) != "TAIL" || confirm.String() != "\nContinue? [y/N]: \n" {
				t.Fatal(got, err, string(tail), confirm.String())
			}
			want := []string{"identity:stdin", "get:stdin", "pipe", "set:stdin"}
			for range []byte(key) {
				want = append(want, "byte")
			}
			want = append(want, "identity:stdin", "set:stdin", "close:wake-read-1", "close:wake-write-1")
			if !reflect.DeepEqual(s.trace(), want) || len(s.sets) != 2 || s.sets[1] != s.saved {
				t.Fatal(s.trace(), want, s.sets)
			}
			basicOwnedClosed(t, s)
			basicBorrowedOpen(t, s)
		})
	}
}

func TestBasicLinuxKeyAdmissionErrorsAndNoResynchronization(t *testing.T) {
	for _, tc := range []struct {
		name, input, tail string
		want              error
	}{
		{"empty", "", "", io.EOF},
		{"invalid-leading", "\xffq", "q", ui.ErrBasicEncoding},
		{"invalid-continuation", "\xc2qTAIL", "TAIL", ui.ErrBasicEncoding},
		{"overlong-stops-at-second", "\xe0\x80q", "q", ui.ErrBasicEncoding},
		{"surrogate-stops-at-second", "\xed\xa0q", "q", ui.ErrBasicEncoding},
		{"above-unicode-stops-at-second", "\xf4\x90q", "q", ui.ErrBasicEncoding},
		{"truncated", "\xe2\x82", "", ui.ErrBasicEncoding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ops := basicScript(t)
			input := basicBuffered(tc.input)
			key, err := linuxBasicKey(context.Background(), s.in, input, ops)
			tail, _ := io.ReadAll(input)
			if key != "" || !errors.Is(err, tc.want) || string(tail) != tc.tail || len(s.sets) != 2 || s.sets[1] != s.saved {
				t.Fatal(key, err, string(tail), s.sets)
			}
			basicOwnedClosed(t, s)
			basicBorrowedOpen(t, s)
		})
	}
}

func TestBasicLinuxKeyFailuresAndAcquisitionCleanup(t *testing.T) {
	failure := errors.New("script operation failure")
	for _, boundary := range []string{"identity", "get-ENOTTY", "pipe", "raw-set", "read", "poll", "restore", "replacement", "closed-identity", "close-read", "close-write"} {
		t.Run(boundary, func(t *testing.T) {
			s, ops := basicScript(t)
			input := basicBuffered("qTAIL")
			termios, identity, closeFile := ops.Termios, ops.Identity, ops.Close
			switch boundary {
			case "identity":
				ops.Identity = func(*os.File) (basicInputIdentity, error) {
					s.event("identity-failed")
					return basicInputIdentity{}, failure
				}
			case "get-ENOTTY":
				ops.Termios = func(*os.File, uintptr, *syscall.Termios) error { s.event("get-failed"); return syscall.ENOTTY }
			case "pipe":
				ops.Pipe = func() (*os.File, *os.File, error) { s.event("pipe-failed"); return nil, nil, failure }
			case "raw-set":
				ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
					err := termios(f, r, a)
					if r == syscall.TCSETS && len(s.sets) == 1 {
						return failure
					}
					return err
				}
			case "read":
				ops.Byte = func(*bufio.Reader) (byte, error) { s.event("byte-failed"); return 0, failure }
			case "poll":
				input = bufio.NewReader(strings.NewReader("qTAIL"))
				ops.Poll = func(*os.File, *os.File) error { s.event("poll-failed"); return failure }
			case "restore":
				ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
					err := termios(f, r, a)
					if r == syscall.TCSETS && len(s.sets) == 2 {
						return failure
					}
					return err
				}
			case "replacement", "closed-identity":
				calls := 0
				ops.Identity = func(f *os.File) (basicInputIdentity, error) {
					v, err := identity(f)
					calls++
					if calls == 2 {
						if boundary == "replacement" {
							v.Ino++
						} else {
							return v, os.ErrClosed
						}
					}
					return v, err
				}
			case "close-read", "close-write":
				ops.Close = func(f *os.File) error {
					err := closeFile(f)
					if strings.Contains(filepath.Base(f.Name()), strings.TrimPrefix(boundary, "close-")) {
						return failure
					}
					return err
				}
			}
			key, err := linuxBasicKey(context.Background(), s.in, input, ops)
			if key != "" || err == nil {
				t.Fatal(key, err)
			}
			if boundary == "replacement" {
				if !strings.Contains(err.Error(), "replaced") {
					t.Fatal(err)
				}
			} else if boundary == "get-ENOTTY" {
				if !errors.Is(err, syscall.ENOTTY) {
					t.Fatal(err)
				}
			} else if boundary == "closed-identity" {
				if !errors.Is(err, os.ErrClosed) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if boundary == "identity" || boundary == "get-ENOTTY" || boundary == "pipe" {
				if len(s.sets) != 0 || s.pipeCount > 0 {
					t.Fatal("changed raw before acquisition", s.trace())
				}
			}
			if boundary == "replacement" || boundary == "closed-identity" {
				if len(s.sets) != 1 {
					t.Fatal("restored replacement", s.sets)
				}
			}
			basicOwnedClosed(t, s)
			basicBorrowedOpen(t, s)
		})
	}
}

func TestBasicLinuxSetAndPollEINTRRetries(t *testing.T) {
	s, ops := basicScript(t)
	termios, poll := ops.Termios, ops.Poll
	setCalls, pollCalls := 0, 0
	ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
		if r == syscall.TCSETS {
			setCalls++
			if setCalls == 1 || setCalls == 3 {
				s.event("set-EINTR")
				return syscall.EINTR
			}
		}
		return termios(f, r, a)
	}
	ops.Poll = func(in, wake *os.File) error {
		pollCalls++
		if pollCalls == 1 {
			s.event("poll-EINTR")
			return syscall.EINTR
		}
		return poll(in, wake)
	}
	key, err := linuxBasicKey(context.Background(), s.in, bufio.NewReader(strings.NewReader("qTAIL")), ops)
	if err != nil || key != "q" || setCalls != 4 || pollCalls != 2 || len(s.sets) != 2 || s.sets[1] != s.saved {
		t.Fatal(key, err, setCalls, pollCalls, s.sets)
	}
	basicOwnedClosed(t, s)
}

func TestBasicLinuxCancellationBeforeAcquisitionAndBetweenBytes(t *testing.T) {
	for _, boundary := range []string{"before", "after-get", "after-pipe", "between-bytes", "during-raw-set", "during-restore"} {
		t.Run(boundary, func(t *testing.T) {
			s, ops := basicScript(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			termios, pipe, readByte := ops.Termios, ops.Pipe, ops.Byte
			input := basicBuffered("éTAIL")
			switch boundary {
			case "before":
				cancel()
			case "after-get":
				ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
					err := termios(f, r, a)
					if r == syscall.TCGETS {
						cancel()
					}
					return err
				}
			case "after-pipe":
				ops.Pipe = func() (*os.File, *os.File, error) { r, w, err := pipe(); cancel(); return r, w, err }
			case "between-bytes":
				ops.Byte = func(r *bufio.Reader) (byte, error) { b, err := readByte(r); cancel(); return b, err }
			case "during-raw-set":
				attempted := false
				ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
					if r == syscall.TCSETS && !attempted {
						attempted = true
						s.event("set-EINTR-cancel")
						cancel()
						return syscall.EINTR
					}
					return termios(f, r, a)
				}
			case "during-restore":
				ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
					err := termios(f, r, a)
					if r == syscall.TCSETS && len(s.sets) == 2 {
						cancel()
					}
					return err
				}
			}
			key, err := linuxBasicKey(ctx, s.in, input, ops)
			if key != "" || !errors.Is(err, context.Canceled) {
				t.Fatal(key, err)
			}
			trace := s.trace()
			if boundary == "before" && len(trace) != 0 {
				t.Fatal(trace)
			}
			if boundary == "after-get" || boundary == "after-pipe" {
				if len(s.sets) != 0 {
					t.Fatal("raw after cancel", s.sets)
				}
			}
			if boundary == "between-bytes" {
				tail, _ := io.ReadAll(input)
				if string(tail) != "\xa9TAIL" {
					t.Fatal("read a second byte after cancel", string(tail))
				}
			}
			if boundary == "during-raw-set" {
				if len(s.sets) != 1 || s.sets[0] != s.saved {
					t.Fatal("restore was suppressed by cancelled context", s.sets)
				}
			}
			basicOwnedClosed(t, s)
			basicBorrowedOpen(t, s)
			// Completed cleanup joined its notifier. No observer may append afterwards.
			if !reflect.DeepEqual(s.trace(), trace) {
				t.Fatal("delayed operation after return")
			}
		})
	}
}

func TestBasicLinuxCancellationWakeNotifierJoinedBeforeClose(t *testing.T) {
	s, ops := basicScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polling, woken, release, hup := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	ops.Poll = func(*os.File, *os.File) error {
		s.event("poll-wait")
		close(polling)
		<-hup
		s.event("poll-HUP")
		return nil
	}
	wakeFailure := errors.New("script wake failure")
	ops.Wake = func(*os.File) error {
		s.event("wake-begin")
		close(woken)
		<-release
		s.event("wake-end")
		return wakeFailure
	}
	closeFile := ops.Close
	ops.Close = func(file *os.File) error {
		err := closeFile(file)
		if strings.HasPrefix(filepath.Base(file.Name()), "wake-write") {
			close(hup) // modeled pipe HUP, independent of the failed Wake callback
		}
		return err
	}
	result := make(chan error, 1)
	go func() {
		_, err := linuxBasicKey(ctx, s.in, bufio.NewReader(strings.NewReader("q")), ops)
		result <- err
	}()
	<-polling
	cancel()
	<-woken
	select {
	case err := <-result:
		t.Fatal("returned before notifier join", err)
	default:
	}
	for _, event := range s.trace() {
		if strings.HasPrefix(event, "close:") {
			t.Fatal("closed while notifier active", s.trace())
		}
	}
	close(release)
	err := <-result
	if !errors.Is(err, context.Canceled) || !errors.Is(err, wakeFailure) {
		t.Fatal(err)
	}
	trace := s.trace()
	wakeIndex, writeClose, restoreIndex, readClose := -1, -1, -1, -1
	for i, event := range trace {
		if event == "wake-end" {
			wakeIndex = i
		}
		if strings.HasPrefix(event, "close:wake-write") {
			writeClose = i
		}
		if event == "identity:stdin" && i > 0 {
			restoreIndex = i
		}
		if strings.HasPrefix(event, "close:wake-read") {
			readClose = i
		}
		if event == "byte" {
			t.Fatal("read after wake cancellation")
		}
	}
	if wakeIndex < 0 || writeClose < wakeIndex || restoreIndex < writeClose || readClose < restoreIndex || len(s.sets) != 2 || s.sets[1] != s.saved {
		t.Fatal(trace, s.sets)
	}
	basicOwnedClosed(t, s)
	basicBorrowedOpen(t, s)
}

func TestBasicLinuxSharedWaitPartialPipeCleanupAndRequiredBindings(t *testing.T) {
	failure := errors.New("partial pipe")
	for _, name := range []string{"both", "read", "write", "neither", "missing poll", "missing byte", "missing wake", "missing close"} {
		t.Run(name, func(t *testing.T) {
			s, ops := basicScript(t)
			pipe := ops.Pipe
			if strings.HasPrefix(name, "missing ") {
				switch name {
				case "missing poll":
					ops.Poll = nil
				case "missing byte":
					ops.Byte = nil
				case "missing wake":
					ops.Wake = nil
				case "missing close":
					ops.Close = nil
				}
			} else {
				ops.Pipe = func() (*os.File, *os.File, error) {
					if name == "neither" {
						return nil, nil, failure
					}
					r, w, e := pipe()
					if e != nil {
						return r, w, e
					}
					// Only return the endpoints transferred by this partial source
					// fixture; the discarded file model is closed by the fixture.
					if name == "read" {
						_ = w.Close()
						s.owned = s.owned[:1]
						w = nil
					}
					if name == "write" {
						_ = r.Close()
						s.owned = s.owned[1:]
						r = nil
					}
					return r, w, failure
				}
			}
			input := basicBuffered("qTAIL")
			key, err := linuxBasicKey(context.Background(), s.in, input, ops)
			if key != "" || err == nil || len(s.sets) != 0 || input.Buffered() != 5 {
				t.Fatal("partial wait affected raw/input")
			}
			if strings.HasPrefix(name, "missing ") {
				if s.pipeCount != 0 || err.Error() != "input wait "+strings.TrimPrefix(name, "missing ")+" binding is unavailable" {
					t.Fatal("required wait checks", err, s.trace())
				}
			} else if err != failure {
				t.Fatal("ordinary pipe error identity", err)
			}
			basicOwnedClosed(t, s)
			basicBorrowedOpen(t, s)
			trace := strings.Join(s.trace(), ",")
			if strings.Contains(trace, "wake,") || strings.Contains(trace, "set:") || strings.Contains(trace, "byte") {
				t.Fatal("partial acquisition started/read", trace)
			}
		})
	}
}

func TestBasicLinuxSharedWaitCancellationCloseErrorPresentation(t *testing.T) {
	s, ops := basicScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polling, hup := make(chan struct{}), make(chan struct{})
	wakeErr, writeErr, readErr, restoreErr := errors.New("wake failure"), errors.New("write close failure"), errors.New("read close failure"), errors.New("restore failure")
	ops.Poll = func(*os.File, *os.File) error { close(polling); <-hup; return nil }
	ops.Wake = func(*os.File) error { s.event("wake failed"); return wakeErr }
	closeFile, termios := ops.Close, ops.Termios
	closes := map[*os.File]int{}
	ops.Close = func(file *os.File) error {
		closes[file]++
		_ = closeFile(file)
		if strings.HasPrefix(filepath.Base(file.Name()), "wake-write") {
			close(hup)
			return writeErr
		}
		return readErr
	}
	ops.Termios = func(file *os.File, request uintptr, a *syscall.Termios) error {
		e := termios(file, request, a)
		if request == syscall.TCSETS && len(s.sets) == 2 {
			return restoreErr
		}
		return e
	}
	result := make(chan error, 1)
	go func() {
		key, e := linuxBasicKey(ctx, s.in, bufio.NewReader(strings.NewReader("q")), ops)
		if key != "" {
			t.Error("accepted canceled key")
		}
		result <- e
	}()
	<-polling
	cancel()
	err := <-result
	for _, cause := range []error{context.Canceled, wakeErr, writeErr, readErr, restoreErr} {
		if !errors.Is(err, cause) {
			t.Fatal("lost joined cause", err)
		}
	}
	if err.Error() != "restore failure\ncontext canceled\ncontext canceled\nwake failure\nread close failure\nwrite close failure" {
		t.Fatal("changed presentation order", err)
	}
	for _, file := range s.owned {
		if closes[file] != 1 {
			t.Fatal("close retried")
		}
	}
	if len(s.sets) != 2 || s.sets[1] != s.saved {
		t.Fatal("guarded restoration changed")
	}
	basicOwnedClosed(t, s)
	basicBorrowedOpen(t, s)
}

func TestBasicLinuxRestorePriorityRetainsReadAndCancellation(t *testing.T) {
	s, ops := basicScript(t)
	readFailure, restoreFailure := errors.New("read failed"), errors.New("restore failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	termios := ops.Termios
	ops.Byte = func(*bufio.Reader) (byte, error) { return 0, readFailure }
	ops.Termios = func(f *os.File, r uintptr, a *syscall.Termios) error {
		err := termios(f, r, a)
		if r == syscall.TCSETS && len(s.sets) == 2 {
			cancel()
			return restoreFailure
		}
		return err
	}
	key, err := linuxBasicKey(ctx, s.in, basicBuffered("q"), ops)
	if key != "" || !errors.Is(err, readFailure) || !errors.Is(err, restoreFailure) || !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "restore failed\n") {
		t.Fatal(key, err)
	}
	basicOwnedClosed(t, s)
	basicBorrowedOpen(t, s)
}

func TestBasicLinuxActualCursorWrapAndSharedInputComposition(t *testing.T) {
	s, ops := basicScript(t)
	sample := 0
	ops.Size = func(f *os.File) (ui.BasicSize, error) {
		if f != s.out {
			t.Fatal("size substituted output")
		}
		sample++
		s.event(fmt.Sprintf("size-sample:%d", sample))
		if sample == 1 {
			return ui.BasicSize{Rows: big.NewInt(3), Columns: big.NewInt(1)}, nil
		}
		return ui.BasicSize{Rows: big.NewInt(100), Columns: big.NewInt(80)}, nil
	}
	input := basicBuffered("y\nqTAIL")
	var confirmation bytes.Buffer
	if err := testConnectionConfirm(input, &confirmation); err != nil {
		t.Fatal(err)
	}
	pager, err := newLinuxBasicPagerWithOps(context.Background(), s.in, s.out, s.stderr, input, ops)
	if err != nil || sample != 2 {
		t.Fatal(pager, err, sample)
	}
	// Actual source prompt100 has two lines at80, so source rows3 leaves one.
	if err = pager.Puts(""); err != nil {
		t.Fatal(err)
	}
	if err = pager.Puts("hello\n"); !errors.Is(err, ui.ErrPagerClosed) {
		t.Fatal(err)
	}
	if err = pager.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(s.out.Name())
	const want = "hello\n\n--- Page -1- Press enter/return to continue (or q to quit) ---\n"
	if err != nil || string(out) != want {
		t.Fatalf("got%q err%v want%q", out, err, want)
	}
	tail, _ := io.ReadAll(input)
	if string(tail) != "TAIL" {
		t.Fatal("discarded confirmation/key suffix", string(tail))
	}
	basicOwnedClosed(t, s)
	basicBorrowedOpen(t, s)
}

func TestBasicLinuxHugeAndOddSizeCursorDoesNotNarrow(t *testing.T) {
	for _, rows := range []string{"0", "-2", "99999999999999999999999999999999999999999999"} {
		s, ops := basicScript(t)
		ops.Size = func(*os.File) (ui.BasicSize, error) { return ui.BasicSize{}, syscall.ENOTTY }
		ops.Termios = func(*os.File, uintptr, *syscall.Termios) error { return syscall.ENOTTY }
		ops.Env = func(name string) (string, bool) {
			v, ok := map[string]string{"COLUMNS": "99999999999999999999999999999999999999999999", "LINES": rows}[name]
			return v, ok
		}
		pager, err := newLinuxBasicPagerWithOps(context.Background(), s.in, s.out, s.stderr, basicBuffered("q"), ops)
		if err != nil {
			t.Fatal(rows, err)
		}
		if err = pager.Puts("a\nb\nc\n"); err != nil {
			t.Fatal(rows, err)
		}
		if err = pager.Close(); err != nil {
			t.Fatal(err)
		}
		out, err := os.ReadFile(s.out.Name())
		if err != nil || string(out) != "a\nb\nc\n" {
			t.Fatal(rows, string(out), err)
		}
		if s.pipeCount != 0 {
			t.Fatal("zero/negative/huge rows unexpectedly clamped into key request")
		}
		basicBorrowedOpen(t, s)
	}
}
