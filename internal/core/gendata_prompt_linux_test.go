//go:build linux

package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"

	"github.com/vpsfreecz/confctl/internal/gendata"
	"github.com/vpsfreecz/confctl/internal/ui"
)

// Finite memory fill windows, not native read timing or an original oracle.
type gpLinuxReader struct {
	windows [][]byte
	err     error
	reads   int
	bytes   int
}

func (r *gpLinuxReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.windows) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := copy(p, r.windows[0])
	r.bytes += n
	r.windows[0] = r.windows[0][n:]
	if len(r.windows[0]) == 0 {
		r.windows = r.windows[1:]
	}
	return n, nil
}

type gpLinuxPipe struct{ read, write *os.File }

type gpLinuxScript struct {
	t       *testing.T
	stdin   *os.File
	reader  *gpLinuxReader
	input   *bufio.Reader
	output  bytes.Buffer
	ops     genDataPromptLinuxOps
	saved   syscall.Termios
	current syscall.Termios
	ids     int
	gets    int
	sets    []syscall.Termios
	bytes   int
	polls   int
	pipes   []gpLinuxPipe
	closed  map[*os.File]int
	mu      sync.Mutex
	trace   []string
}

func gpLinuxNew(t *testing.T, windows ...string) *gpLinuxScript {
	t.Helper()
	r := &gpLinuxReader{}
	for _, w := range windows {
		r.windows = append(r.windows, []byte(w))
	}
	s := &gpLinuxScript{t: t, stdin: new(os.File), reader: r, input: bufio.NewReader(r), closed: map[*os.File]int{}}
	s.saved = syscall.Termios{Iflag: 0xffffffff, Oflag: 0xffffffff, Cflag: 0xffffffff, Lflag: 0xffffffff, Line: 11, Ispeed: 123, Ospeed: 456}
	for i := range s.saved.Cc {
		s.saved.Cc[i] = uint8(i + 7)
	}
	s.current = s.saved
	s.ops.Identity = func(f *os.File) (basicInputIdentity, error) {
		if f != s.stdin {
			t.Fatal("identity received a different borrowed input")
		}
		s.ids++
		s.event("identity")
		return basicInputIdentity{FD: 17, Dev: 2, Ino: 3, Rdev: 4, Mode: 5}, nil
	}
	s.ops.Termios = func(f *os.File, request uintptr, a *syscall.Termios) error {
		if f != s.stdin {
			t.Fatal("termios received a different borrowed input")
		}
		switch request {
		case syscall.TCGETS:
			s.gets++
			s.event("get")
			*a = s.current
		case syscall.TCSETS:
			s.sets = append(s.sets, *a)
			s.current = *a
			s.event("set")
		default:
			t.Fatal("unexpected termios request")
		}
		return nil
	}
	s.ops.Wait = inputWaitOps{
		Pipe: func() (*os.File, *os.File, error) {
			p := gpLinuxPipe{new(os.File), new(os.File)}
			s.pipes = append(s.pipes, p)
			s.event("pipe")
			return p.read, p.write, nil
		},
		Poll: func(stdin, wake *os.File) error {
			if stdin != s.stdin || len(s.pipes) == 0 || wake != s.pipes[len(s.pipes)-1].read {
				t.Fatal("readiness lost original input/owned endpoint")
			}
			s.polls++
			s.event("poll")
			return nil
		},
		Byte: func(r *bufio.Reader) (byte, error) {
			if r != s.input {
				t.Fatal("byte callback received a second reader")
			}
			s.bytes++
			s.event("byte")
			return r.ReadByte()
		},
		Wake: func(*os.File) error { s.event("wake"); return nil },
		Close: func(f *os.File) error {
			if f == s.stdin {
				t.Fatal("closed borrowed input")
			}
			s.mu.Lock()
			s.closed[f]++
			s.mu.Unlock()
			s.event("close")
			return nil
		},
	}
	return s
}

func (s *gpLinuxScript) event(name string) {
	s.mu.Lock()
	s.trace = append(s.trace, name)
	s.mu.Unlock()
}

func (s *gpLinuxScript) prompt() *ui.GenDataPrompt {
	return newLinuxGenDataPromptWithOps(s.stdin, s.input, &s.output, func() error {
		s.released()
		s.event("flush")
		return nil
	}, s.ops)
}

func (s *gpLinuxScript) released() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pipes {
		if s.closed[p.read] != 1 || s.closed[p.write] != 1 {
			s.t.Fatal("scope not completely released or endpoint closed twice")
		}
	}
}

func (s *gpLinuxScript) suffix(want string) {
	s.t.Helper()
	b, err := io.ReadAll(s.input)
	if err != nil || string(b) != want {
		s.t.Fatal("retained memory suffix changed")
	}
}

type gpLinuxWriter struct{ err error }

func (w gpLinuxWriter) Write([]byte) (int, error) { return 0, w.err }

func TestGenDataLinuxPromptConstruction(t *testing.T) {
	ctx := context.Background()
	// Native constructor is selected without invoking any callback/descriptor.
	if newLinuxGenDataPrompt(nil, nil, nil, nil) == nil {
		t.Fatal("native constructor did not return actual prompt")
	}
	failure := errors.New("scripted output refusal")
	for _, label := range []string{"<%= 1 + 1 %>: ", "% literal: ", "\x1b[31m: ", string([]byte{0xff})} {
		s := gpLinuxNew(t, "answer\n")
		p := s.prompt()
		if len(s.trace) != 0 {
			t.Fatal("constructor reached an operation")
		}
		s.ops.Wait.Pipe = nil // copied bindings are retained by the actual prompt
		if _, err := p.Ask(ctx, label, false); err != nil || s.output.String() != label || s.ids != 0 || s.gets != 0 {
			t.Fatal("literal label or inert/copy contract changed")
		}
	}
	p := newLinuxGenDataPromptWithOps(nil, nil, gpLinuxWriter{failure}, func() error { t.Fatal("flush after failed label"); return nil }, genDataPromptLinuxOps{})
	if _, err := p.Ask(ctx, "label: ", true); err != failure {
		t.Fatal("writer identity changed before lazy validation")
	}
	var out bytes.Buffer
	p = newLinuxGenDataPromptWithOps(nil, nil, &out, func() error { return failure }, genDataPromptLinuxOps{})
	if _, err := p.Ask(ctx, "label: ", true); err != failure || out.String() != "label: " {
		t.Fatal("flush refusal or label effect changed")
	}
	for _, mode := range []bool{false, true} {
		out.Reset()
		p = newLinuxGenDataPromptWithOps(nil, nil, &out, func() error { return nil }, genDataPromptLinuxOps{})
		if _, err := p.Ask(ctx, "label: ", mode); err == nil || out.String() != "label: " {
			t.Fatal("missing input was not refused lazily")
		}
	}
}

func TestGenDataLinuxPromptLine(t *testing.T) {
	readErr := errors.New("scripted line read refusal")
	for _, row := range []struct {
		name, input, answer, tail string
		read, want                error
	}{
		{"first-LF", "a\nTAIL", "a\n", "TAIL", nil, nil},
		{"CRLF", "a\r\nTAIL", "a\r\n", "TAIL", nil, nil},
		{"blank", "\nTAIL", "\n", "TAIL", nil, nil},
		{"empty-EOF", "", "", "", nil, io.EOF},
		{"final-CR", "a\r", "a\r", "", nil, nil},
		{"controls", "\x00\x03\nTAIL", "\x00\x03\n", "TAIL", nil, nil},
		{"invalid", "\xff\nTAIL", "", "TAIL", nil, ui.ErrGenDataPromptEncoding},
		{"truncated", "\xe2\x82", "", "", nil, ui.ErrGenDataPromptEncoding},
		{"partial-error", "\xff", "", "", readErr, readErr},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := gpLinuxNew(t)
			if row.input != "" {
				s.reader.windows = [][]byte{[]byte(row.input)}
			}
			s.reader.err = row.read
			answer, err := s.prompt().Ask(context.Background(), "line: ", false)
			if answer != row.answer || !errors.Is(err, row.want) || s.output.String() != "line: " || len(s.pipes) != 1 {
				t.Fatal("line admission/error/output changed")
			}
			s.released()
			if row.read == nil {
				s.suffix(row.tail)
			}
		})
	}
	for _, primary := range []error{nil, io.EOF, readErr} {
		s := gpLinuxNew(t)
		s.reader.err = primary
		closeErr := errors.New("scripted close refusal")
		s.ops.Wait.Close = func(*os.File) error { return closeErr }
		_, err := linuxGenDataLine(context.Background(), s.stdin, s.input, s.ops.Wait)
		want := closeErr
		if primary == readErr {
			want = readErr
		}
		if err != want {
			t.Fatal("line cleanup replaced a primary read or accepted EOF")
		}
	}
}

func TestGenDataLinuxPromptOuter(t *testing.T) {
	ctx := context.Background()
	for _, echo := range []bool{true, false} {
		s := gpLinuxNew(t, "\n")
		if !echo {
			s.saved.Lflag &^= syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL
			s.current = s.saved
		}
		if _, err := s.prompt().Ask(ctx, "outer: ", true); err != nil {
			t.Fatal("normal protected ask failed")
		}
		want := s.saved
		want.Lflag &^= syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL
		if len(s.sets) != 4 || s.sets[0] != want || s.sets[2] != want || s.sets[3] != s.saved || s.current != s.saved {
			t.Fatal("outer noecho/saved restoration changed fields")
		}
	}
	for _, restoreErr := range []error{nil, syscall.ENOTTY, errors.New("outer restore refusal")} {
		s := gpLinuxNew(t, "unused")
		setErr := errors.New("outer apply refusal")
		base := s.ops.Termios
		s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
			err := base(f, req, a)
			if req == syscall.TCSETS {
				if len(s.sets) == 1 {
					return setErr
				}
				return restoreErr
			}
			return err
		}
		_, err := s.prompt().Ask(ctx, "outer: ", true)
		want := setErr
		if restoreErr != nil && restoreErr != syscall.ENOTTY {
			want = restoreErr
		}
		if err != want || !errors.Is(err, want) || s.bytes != 0 || len(s.sets) != 2 || s.output.String() != "outer: " {
			t.Fatal("partial outer apply/restore precedence changed")
		}
	}
	// Cancellation after successful snapshot: acquired Restore exists and actual
	// Ask calls it once, while zero TCSETS/restore identity callbacks are reached.
	s := gpLinuxNew(t, "unused")
	canceled, cancel := context.WithCancel(ctx)
	base := s.ops.Termios
	s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
		err := base(f, req, a)
		if req == syscall.TCGETS {
			cancel()
		}
		return err
	}
	restored := 0
	p := ui.NewGenDataPrompt(ui.GenDataPromptIO{Writer: &s.output, Flush: func() error { return nil }, BeginProtected: func(c context.Context) (ui.GenDataProtectedInput, error) {
		protected, err := linuxGenDataBegin(c, s.stdin, s.input, s.ops)
		if protected.Restore == nil || protected.Character == nil || err != context.Canceled {
			t.Fatal("successful snapshot dropped acquired capability on cancellation")
		}
		restore := protected.Restore
		protected.Restore = func() error { restored++; return restore() }
		return protected, err
	}})
	if _, err := p.Ask(canceled, "outer: ", true); err != context.Canceled || restored != 1 || len(s.sets) != 0 || s.ids != 1 || s.bytes != 0 {
		t.Fatal("unattempted native restore was missing or invented a mutation")
	}
	for _, identityErr := range []error{syscall.ENOTTY, nil} {
		s := gpLinuxNew(t, "unused")
		base := s.ops.Identity
		s.ops.Identity = func(f *os.File) (basicInputIdentity, error) {
			id, err := base(f)
			if s.ids > 1 {
				id.Ino++
				return id, identityErr
			}
			return id, err
		}
		protected, err := linuxGenDataBegin(ctx, s.stdin, s.input, s.ops)
		if err != nil || protected.Restore == nil || protected.Restore() == nil || len(s.sets) != 1 {
			t.Fatal("outer identity error was rescued or replacement restored")
		}
	}
	for _, snapshotErr := range []error{syscall.ENOTTY, errors.New("snapshot refusal")} {
		s := gpLinuxNew(t)
		s.ops.Termios = func(*os.File, uintptr, *syscall.Termios) error { return snapshotErr }
		protected, err := linuxGenDataBegin(ctx, s.stdin, s.input, s.ops)
		if protected.Restore != nil || len(s.sets) != 0 || (snapshotErr == syscall.ENOTTY && (err != nil || protected.Character == nil)) || (snapshotErr != syscall.ENOTTY && err != snapshotErr) {
			t.Fatal("failed initial snapshot invented restore or changed rescue")
		}
	}
	// Outer apply/restore ENOTTY rescue is reached through actual Ask; no
	// permanent nonterminal cache is inferred for the inner character cycles.
	for _, phase := range []string{"apply", "restore"} {
		s := gpLinuxNew(t, "\nTAIL")
		base := s.ops.Termios
		s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
			err := base(f, req, a)
			if req == syscall.TCSETS && (phase == "apply" && len(s.sets) == 1 || phase == "restore" && len(s.sets) == 4) {
				return syscall.ENOTTY
			}
			return err
		}
		if _, err := s.prompt().Ask(ctx, "outer: ", true); err != nil || s.gets != 2 || len(s.sets) != 4 || s.current != s.saved || s.output.String() != "outer: \n" {
			t.Fatal("reached outer ENOTTY was not rescued with ordinary inner ownership")
		}
		s.released()
		s.suffix("TAIL")
	}
}

func TestGenDataLinuxPromptGetch(t *testing.T) {
	s := gpLinuxNew(t, "abTAIL")
	if _, err := s.input.Peek(1); err != nil {
		t.Fatal("memory prefill failed")
	}
	for _, want := range []string{"a", "b"} {
		got, err := linuxGenDataCharacter(context.Background(), s.stdin, s.input, s.ops)
		if err != nil || got != want {
			t.Fatal("buffered character failed")
		}
	}
	// Independent literal bit policy, including unrelated fields and all Cc.
	want := s.saved
	want.Iflag = s.saved.Iflag &^ (syscall.IGNBRK | syscall.BRKINT | syscall.IGNPAR | syscall.PARMRK | syscall.INPCK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF | syscall.IXANY | syscall.IMAXBEL)
	want.Iflag |= syscall.BRKINT
	want.Oflag = (s.saved.Oflag &^ syscall.OPOST) | syscall.OPOST
	want.Lflag = (s.saved.Lflag &^ (syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN | syscall.XCASE)) | syscall.ISIG
	want.Cflag = (s.saved.Cflag &^ (syscall.CSIZE | syscall.PARENB)) | syscall.CS8
	want.Cc[syscall.VMIN], want.Cc[syscall.VTIME] = 1, 0
	if len(s.sets) != 4 || s.sets[0] != want || s.sets[2] != want || s.sets[1] != s.saved || s.sets[3] != s.saved || s.gets != 2 || s.ids != 4 || s.polls != 0 || len(s.pipes) != 2 {
		t.Fatal("per-character exact mode cycle or buffered bypass changed")
	}
	s.released()
	s.suffix("TAIL")
	for _, resultErr := range []error{io.EOF, errors.New("read refusal")} {
		s := gpLinuxNew(t)
		s.reader.err = resultErr
		_, err := linuxGenDataCharacter(context.Background(), s.stdin, s.input, s.ops)
		if err != resultErr || len(s.sets) != 2 || s.sets[1] != s.saved {
			t.Fatal("read/EOF did not preserve error and restore")
		}
		s.released()
	}
	s = gpLinuxNew(t, "unused")
	setErr := errors.New("partial raw refusal")
	base := s.ops.Termios
	s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
		err := base(f, req, a)
		if req == syscall.TCSETS && len(s.sets) == 1 {
			return setErr
		}
		return err
	}
	if _, err := linuxGenDataCharacter(context.Background(), s.stdin, s.input, s.ops); err != setErr || len(s.sets) != 2 || s.bytes != 0 || s.sets[1] != s.saved {
		t.Fatal("failed raw application repeated setup or omitted restoration")
	}
	s.released()
	s = gpLinuxNew(t, "aTAIL")
	base = s.ops.Termios
	s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
		err := base(f, req, a)
		if req == syscall.TCSETS && (len(s.sets) == 1 || len(s.sets) == 3) {
			return syscall.EINTR
		}
		return err
	}
	if got, err := linuxGenDataCharacter(context.Background(), s.stdin, s.input, s.ops); err != nil || got != "a" || len(s.sets) != 4 || s.sets[0] != s.sets[1] || s.sets[2] != s.saved || s.sets[3] != s.saved || len(s.pipes) != 1 {
		t.Fatal("finite EINTR apply/restore retry changed state or repeated acquisition")
	}
	s.released()
	s.suffix("TAIL")
	s = gpLinuxNew(t, "aTAIL")
	identity := s.ops.Identity
	s.ops.Identity = func(f *os.File) (basicInputIdentity, error) {
		id, err := identity(f)
		if s.ids > 1 {
			id.Ino++
		}
		return id, err
	}
	if _, err := linuxGenDataCharacter(context.Background(), s.stdin, s.input, s.ops); err == nil || len(s.sets) != 1 || s.bytes != 1 {
		t.Fatal("inner replacement was restored or accepted")
	}
	s.released()
	s.suffix("TAIL")
}

func TestGenDataLinuxPromptFallback(t *testing.T) {
	ctx := context.Background()
	for _, origin := range []string{"snapshot", "apply", "read", "restore"} {
		t.Run(origin, func(t *testing.T) {
			s := gpLinuxNew(t, "abTAIL")
			base := s.ops.Termios
			s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
				err := base(f, req, a)
				if origin == "snapshot" && req == syscall.TCGETS || origin == "apply" && req == syscall.TCSETS && len(s.sets) == 1 || origin == "restore" && req == syscall.TCSETS && len(s.sets) == 2 {
					return syscall.ENOTTY
				}
				return err
			}
			if origin == "read" {
				readByte := s.ops.Wait.Byte
				s.ops.Wait.Byte = func(r *bufio.Reader) (byte, error) {
					if s.bytes == 0 {
						s.bytes++
						return 0, syscall.ENOTTY
					}
					return readByte(r)
				}
			}
			got, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops)
			want, tail := "a", "bTAIL"
			if origin == "restore" {
				want, tail = "b", "TAIL"
			}
			if err != nil || got != want || len(s.pipes) != map[bool]int{true: 1, false: 2}[origin == "snapshot"] {
				t.Fatal("whole-getch source fallback or consumed character changed")
			}
			s.released()
			s.suffix(tail)
		})
	}
	for _, origin := range []string{"identity", "pipe", "poll", "restore-identity"} {
		s := gpLinuxNew(t, "ab")
		switch origin {
		case "identity":
			s.ops.Identity = func(*os.File) (basicInputIdentity, error) { return basicInputIdentity{}, syscall.ENOTTY }
		case "pipe":
			s.ops.Wait.Pipe = func() (*os.File, *os.File, error) { return nil, nil, syscall.ENOTTY }
		case "poll":
			s.ops.Wait.Poll = func(*os.File, *os.File) error { return syscall.ENOTTY }
		case "restore-identity":
			base := s.ops.Identity
			s.ops.Identity = func(f *os.File) (basicInputIdentity, error) {
				id, err := base(f)
				if s.ids > 1 {
					return id, syscall.ENOTTY
				}
				return id, err
			}
		}
		_, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops)
		if !errors.Is(err, syscall.ENOTTY) || len(s.pipes) > 1 || (origin != "restore-identity" && s.bytes != 0) {
			t.Fatal("infrastructure ENOTTY triggered fallback")
		}
		s.released()
	}
	for _, initialErr := range []error{syscall.ENOTTY, errors.New("initial read refusal")} {
		s := gpLinuxNew(t, "b")
		base := s.ops.Termios
		restoreErr := errors.New("inner restore refusal")
		s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
			err := base(f, req, a)
			if req == syscall.TCSETS && len(s.sets) == 2 {
				return restoreErr
			}
			return err
		}
		s.ops.Wait.Byte = func(*bufio.Reader) (byte, error) { s.bytes++; return 0, initialErr }
		if _, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops); err != restoreErr || s.bytes != 1 || len(s.pipes) != 1 {
			t.Fatal("inner restore did not supersede source error")
		}
	}
	s := gpLinuxNew(t)
	s.ops.Termios = func(*os.File, uintptr, *syscall.Termios) error { return syscall.ENOTTY }
	s.ops.Wait.Byte = func(*bufio.Reader) (byte, error) { s.bytes++; return 0, syscall.ENOTTY }
	if _, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops); err != syscall.ENOTTY || s.bytes != 1 || len(s.pipes) != 1 {
		t.Fatal("fallback recursively rescued its own error")
	}
	s = gpLinuxNew(t, "bTAIL")
	readErr := errors.New("superseded source read refusal")
	readByte := s.ops.Wait.Byte
	s.ops.Wait.Byte = func(r *bufio.Reader) (byte, error) {
		if s.bytes == 0 {
			s.bytes++
			return 0, readErr
		}
		return readByte(r)
	}
	base := s.ops.Termios
	s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
		err := base(f, req, a)
		if req == syscall.TCSETS && len(s.sets) == 2 {
			return syscall.ENOTTY
		}
		return err
	}
	if got, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops); err != nil || got != "b" || len(s.pipes) != 2 || s.bytes != 2 {
		t.Fatal("inner restore ENOTTY did not supersede non-ENOTTY source read")
	}
	s.released()
	s.suffix("TAIL")
}

func TestGenDataLinuxPromptCleanup(t *testing.T) {
	ctx := context.Background()
	for _, primary := range []error{nil, io.EOF, syscall.ENOTTY, errors.New("primary read refusal")} {
		for _, endpoint := range []string{"read", "write"} {
			s := gpLinuxNew(t, "a")
			if primary != nil {
				s.ops.Wait.Byte = func(*bufio.Reader) (byte, error) { s.bytes++; return 0, primary }
			}
			closeErr := errors.New("owned close refusal")
			base := s.ops.Wait.Close
			s.ops.Wait.Close = func(f *os.File) error {
				base(f)
				p := s.pipes[len(s.pipes)-1]
				if endpoint == "read" && f == p.read || endpoint == "write" && f == p.write {
					return closeErr
				}
				return nil
			}
			_, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops)
			want := closeErr
			if primary != nil && primary != io.EOF && primary != syscall.ENOTTY {
				want = primary
			}
			if err != want || len(s.pipes) != 1 {
				t.Fatal("cleanup priority/rescuable source refusal changed")
			}
			s.released()
			trace := s.trace
			if len(trace) < 3 || !reflect.DeepEqual(trace[len(trace)-3:], []string{"set", "close", "close"}) {
				t.Fatal("inner restoration did not precede owned closes")
			}
		}
	}
	for _, partial := range []string{"read", "write", "both", "alias", "borrowed", "missing"} {
		s := gpLinuxNew(t)
		r, w := new(os.File), new(os.File)
		acquireErr := errors.New("partial acquisition refusal")
		s.ops.Wait.Pipe = func() (*os.File, *os.File, error) {
			switch partial {
			case "read":
				return r, nil, acquireErr
			case "write":
				return nil, w, acquireErr
			case "both":
				return r, w, acquireErr
			case "alias":
				return r, r, nil
			case "borrowed":
				return s.stdin, w, nil
			default:
				return r, nil, nil
			}
		}
		if _, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops); err == nil || len(s.sets) != 0 || s.bytes != 0 {
			t.Fatal("partial acquisition reached raw/input or success")
		}
		if (partial == "read" || partial == "both" || partial == "alias" || partial == "missing") && s.closed[r] != 1 || (partial == "write" || partial == "both" || partial == "borrowed") && s.closed[w] != 1 {
			t.Fatal("partial acquired endpoint was lost or closed twice")
		}
	}
	// Actual cancellation notifier: failed Wake still reaches write Close/HUP;
	// Poll waits for that close, and restore waits for the notifier to return.
	s := gpLinuxNew(t)
	canceled, cancel := context.WithCancel(ctx)
	hup, wakeReturned := make(chan struct{}), make(chan struct{})
	wakeErr, writeErr, readErr := errors.New("wake refusal"), errors.New("write close refusal"), errors.New("read close refusal")
	s.ops.Wait.Poll = func(*os.File, *os.File) error { cancel(); <-hup; return syscall.ENOTTY }
	s.ops.Wait.Wake = func(*os.File) error { s.event("wake"); close(wakeReturned); return wakeErr }
	baseClose := s.ops.Wait.Close
	s.ops.Wait.Close = func(f *os.File) error {
		baseClose(f)
		if f == s.pipes[0].write {
			close(hup)
			return writeErr
		}
		return readErr
	}
	baseTermios := s.ops.Termios
	s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
		if req == syscall.TCSETS && len(s.sets) == 1 {
			<-wakeReturned
			select {
			case <-hup:
			default:
				t.Fatal("restoration before notifier write close")
			}
			baseTermios(f, req, a)
			return syscall.ENOTTY // source restore makes teardown primary
		}
		return baseTermios(f, req, a)
	}
	if _, err := linuxGenDataCharacter(canceled, s.stdin, s.input, s.ops); err != wakeErr || len(s.pipes) != 1 || s.bytes != 0 {
		t.Fatal("failed-wake source ENOTTY did not stop fallback with first cleanup")
	}
	s.released()
	for _, row := range []struct {
		stop inputWaitStop
		end  inputWaitClose
		want error
	}{
		{inputWaitStop{wakeErr, writeErr}, inputWaitClose{readErr, writeErr}, wakeErr},
		{inputWaitStop{nil, writeErr}, inputWaitClose{readErr, nil}, writeErr},
		{inputWaitStop{}, inputWaitClose{readErr, writeErr}, readErr},
		{inputWaitStop{}, inputWaitClose{nil, writeErr}, writeErr},
	} {
		if genDataWaitError(row.stop, row.end) != row.want {
			t.Fatal("separate teardown causes lost fixed priority")
		}
	}
}

func TestGenDataLinuxPromptCandidates(t *testing.T) {
	for _, row := range []struct {
		name         string
		windows      []string
		first, tail  string
		want         error
		read         error
		prefill      bool
		consumedByte int
	}{
		{"initial-invalid", []string{"\xe2A"}, "\xe2", "A", nil, nil, false, 1},
		{"refill-invalid", []string{"\xe2", "AB"}, "\xe2A", "B", nil, nil, false, 2},
		{"refill-valid", []string{"\xe2\x82", "\xacX"}, "€", "X", nil, nil, false, 3},
		{"truncated-EOF", []string{"\xe2\x82"}, "\xe2\x82", "", nil, nil, false, 3},
		{"illegal-leading", []string{"\xffq"}, "\xff", "q", nil, nil, false, 1},
		{"surrogate", []string{"\xed\xa0\x80"}, "\xed", "\xa0\x80", nil, nil, false, 1},
		{"range-refill", []string{"\xf4", "\x90A"}, "\xf4\x90", "A", nil, nil, false, 2},
		{"replacement", []string{"\xef\xbf\xbdq"}, "�", "q", nil, nil, false, 3},
		{"empty-EOF", nil, "", "", io.EOF, nil, false, 1},
		{"partial-error", []string{"\xe2"}, "", "", errors.New("candidate read refusal"), nil, false, 2},
		{"two-split", []string{"\xc2", "\xa2X"}, "¢", "X", nil, nil, false, 2},
		{"four-split", []string{"\xf0", "\x9f", "\x98", "\x80X"}, "😀", "X", nil, nil, false, 4},
		{"overlong", []string{"\xe0\x80\x80X"}, "\xe0", "\x80\x80X", nil, nil, false, 1},
		{"prefilled-invalid", []string{"\xe2AX"}, "\xe2", "AX", nil, nil, true, 1},
		{"prefilled-valid", []string{"€X"}, "€", "X", nil, nil, true, 3},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := gpLinuxNew(t, row.windows...)
			if row.want != nil && row.want != io.EOF {
				s.reader.err = row.want
			}
			if row.prefill {
				if _, err := s.input.Peek(1); err != nil {
					t.Fatal("prefill fixture failed")
				}
			}
			got, err := linuxGenDataFallback(context.Background(), s.stdin, s.input, s.ops.Wait)
			if got != row.first || !errors.Is(err, row.want) || s.bytes != row.consumedByte {
				t.Fatal("candidate consumption/error differed from literal source fixture")
			}
			s.released()
			if row.want == nil || row.want == io.EOF {
				s.suffix(row.tail)
			}
		})
	}
}

func TestGenDataLinuxPromptEditing(t *testing.T) {
	for _, row := range []struct{ name, input, answer, tail string }{
		{"codepoint-delete", "a€\b😀\x7fZ\nTAIL", "aZ", "TAIL"},
		{"clear", "abc\x15Z\nTAIL", "Z", "TAIL"},
		{"literal-controls", "\x01\t\nTAIL", "\x01\t", "TAIL"},
		{"CRLF", "a\r\nTAIL", "a", "\nTAIL"},
		{"escape-invalid-control", "a\x1b\xff\x03Z\nTAIL", "aZ", "TAIL"},
		{"escape-truncated", "a\x1b\xe2\x82", "a", ""},
		{"escape-empty-EOF", "a\x1b", "a", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := gpLinuxNew(t, row.input)
			answer, err := s.prompt().Ask(context.Background(), "edit: ", true)
			if err != nil || answer != row.answer || s.output.String() != "edit: \n" || s.gets < 2 || s.gets-1 != len(s.pipes) {
				t.Fatal("actual Ask editing/escape/EOF mode cycle changed")
			}
			if row.name == "escape-empty-EOF" && len(s.pipes) != 5 {
				t.Fatal("first escape-discard EOF skipped the second callback")
			}
			s.released()
			s.suffix(row.tail)
		})
	}
	for _, row := range []struct {
		input string
		want  error
	}{
		{"a\xff", ui.ErrGenDataPromptEncoding},
		{"a\xe2\x82", ui.ErrGenDataPromptEncoding},
		{"a\x03", ui.ErrGenDataPromptInterrupted},
	} {
		s := gpLinuxNew(t, row.input)
		_, err := s.prompt().Ask(context.Background(), "edit: ", true)
		if err != row.want || !errors.Is(err, row.want) || s.output.String() != "edit: " || s.current != s.saved {
			t.Fatal("reached append/interrupt failed identity/restore/output")
		}
		s.released()
	}
	s := gpLinuxNew(t, "ab\n")
	flushErr := errors.New("iteration flush refusal")
	flushes := 0
	p := newLinuxGenDataPromptWithOps(s.stdin, s.input, &s.output, func() error {
		s.released()
		flushes++
		if flushes == 2 {
			return flushErr
		}
		return nil
	}, s.ops)
	if _, err := p.Ask(context.Background(), "edit: ", true); err != flushErr || s.current != s.saved || s.bytes != 1 || s.output.String() != "edit: " {
		t.Fatal("iteration flush did not release then outer-restore without success LF")
	}
}

func TestGenDataLinuxPromptCancel(t *testing.T) {
	for _, mode := range []string{"line", "begin", "character"} {
		s := gpLinuxNew(t, "unused")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var err error
		switch mode {
		case "line":
			_, err = linuxGenDataLine(ctx, s.stdin, s.input, s.ops.Wait)
		case "begin":
			protected, e := linuxGenDataBegin(ctx, s.stdin, s.input, s.ops)
			err = e
			if protected.Restore != nil {
				t.Fatal("cancellation before acquisition created restore")
			}
		default:
			_, err = linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops)
		}
		if err != context.Canceled || len(s.trace) != 0 {
			t.Fatal("callback entry cancellation reached an operation")
		}
	}
	for _, input := range []string{"x", "\xe2"} {
		s := gpLinuxNew(t, input)
		ctx, cancel := context.WithCancel(context.Background())
		readByte := s.ops.Wait.Byte
		s.ops.Wait.Byte = func(r *bufio.Reader) (byte, error) {
			b, err := readByte(r)
			cancel()
			return b, err
		}
		got, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops)
		if input == "x" && (err != nil || got != "x") || input != "x" && err != context.Canceled || s.bytes != 1 || s.current != s.saved {
			t.Fatal("final callback success lost to tail context or next byte ignored cancellation")
		}
		s.released()
	}
	s := gpLinuxNew(t, "\nTAIL")
	ctx, cancel := context.WithCancel(context.Background())
	readByte := s.ops.Wait.Byte
	s.ops.Wait.Byte = func(r *bufio.Reader) (byte, error) { b, err := readByte(r); cancel(); return b, err }
	if got, err := linuxGenDataLine(ctx, s.stdin, s.input, s.ops.Wait); err != nil || got != "\n" {
		t.Fatal("complete echoed line gained a tail context override")
	}
	s.released()
	s.suffix("TAIL")
	for _, phase := range []string{"before-set", "set-EINTR", "poll"} {
		s := gpLinuxNew(t)
		ctx, cancel := context.WithCancel(context.Background())
		base := s.ops.Termios
		s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
			err := base(f, req, a)
			if phase == "before-set" && req == syscall.TCGETS {
				cancel()
			}
			if phase == "set-EINTR" && req == syscall.TCSETS && len(s.sets) == 1 {
				cancel()
				return syscall.EINTR
			}
			return err
		}
		if phase == "poll" {
			s.ops.Wait.Poll = func(*os.File, *os.File) error { cancel(); return nil }
		}
		_, err := linuxGenDataCharacter(ctx, s.stdin, s.input, s.ops)
		wantSets := 2
		if phase == "before-set" {
			wantSets = 0
		}
		if err != context.Canceled || len(s.sets) != wantSets || s.bytes != 0 {
			t.Fatal("cancellation attempt/restore/read reachability changed")
		}
		s.released()
	}
}

func TestGenDataLinuxPromptComposition(t *testing.T) {
	ctx := context.Background()
	s := gpLinuxNew(t, "y\nxsynthetic-user\nsynthetic-password\n", "synthetic-answer\nyes\nTAIL")
	var page bytes.Buffer
	_, line := commandInput(s.input)
	if err := testConnectionConfirm(ctx, line, &page); err != nil || s.input.Buffered() == 0 || s.reader.reads != 1 {
		t.Fatal("actual confirmation stranded read-ahead")
	}
	keys := 0
	basic, err := ui.NewBasicPager(ctx, &page, ui.BasicIO{
		Size: func(context.Context) (ui.BasicSize, error) {
			return ui.BasicSize{Rows: big.NewInt(3), Columns: big.NewInt(120)}, nil
		},
		Wrap: ui.WrapBasic,
		Key: func(c context.Context) (string, error) {
			keys++
			return linuxBasicKey(c, s.stdin, s.input, basicLinuxOps{Identity: s.ops.Identity, Termios: s.ops.Termios, Pipe: s.ops.Wait.Pipe, Poll: s.ops.Wait.Poll, Byte: s.ops.Wait.Byte, Wake: s.ops.Wait.Wake, Close: s.ops.Wait.Close})
		},
	})
	if err != nil || basic.Puts("row") != nil || keys != 1 || s.bytes != 1 || s.reader.reads != 1 {
		t.Fatal("actual Basic/Wrap did not consume precisely the retained key")
	}
	if err := basic.Close(); err != nil {
		t.Fatal("Basic close failed")
	}
	root := coreGDSeed(t)
	steps := []coreGDStep{
		{method: "OPTIONS", uri: "/?describe=default", body: coreGDDescription},
		{method: "POST", uri: "/login", body: `{"status":true,"response":{"login":{"complete":false,"token":"synthetic-step","next_action":"device"}}}`},
		{method: "PUT", uri: "/device", body: coreGDLogin},
		coreGDResourceStep(), coreGDListStep("[]"),
	}
	provider := coreGDNewScript(t, steps, nil)
	provider.bindings.Prompt = s.prompt().Ask
	if err := runGenData(ctx, root, genDataAll, provider.bindings); err != nil {
		t.Fatal("actual prompt/provider/core/G1 composition failed")
	}
	provider.done()
	if s.output.String() != "User name: Password: \nAnswer: \napproved: " || s.current != s.saved || s.reader.reads != 2 {
		t.Fatal("actual ordered prompts/restore/fill windows changed")
	}
	s.released()
	s.suffix("TAIL")
	for path, want := range map[string]string{coreGDContainerPath: coreGDEmptyContainers, coreGDNetworkPath: coreGDEmptyNetworks} {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(b) != want {
			t.Fatal("actual fixed-file composition changed")
		}
	}
	for _, phase := range []string{"username", "password", "challenge"} {
		root := coreGDSeed(t)
		before := coreGDTree(t, root)
		input := ""
		steps := []coreGDStep{}
		if phase == "password" {
			input = "synthetic-user\n\x03"
		}
		if phase == "challenge" {
			input = "synthetic-user\nsynthetic-password\n\x03"
			steps = []coreGDStep{{method: "OPTIONS", uri: "/?describe=default", body: coreGDDescription}, {method: "POST", uri: "/login", body: `{"status":true,"response":{"login":{"complete":false,"token":"synthetic-step","next_action":"device"}}}`}}
		}
		s := gpLinuxNew(t)
		if input != "" {
			s.reader.windows = [][]byte{[]byte(input)}
		}
		provider := coreGDNewScript(t, steps, nil)
		provider.bindings.Prompt = s.prompt().Ask
		provider.bindings.Machines = func(context.Context) ([]Machine, error) { t.Fatal("inventory after prompt refusal"); return nil, nil }
		err := runGenData(ctx, root, genDataAll, provider.bindings)
		var auth *gendata.AuthenticationError
		if !errors.As(err, &auth) || err.Error() != "authentication failed: authentication prompt failed" || errors.Is(err, ui.ErrGenDataPromptInterrupted) || errors.Is(err, io.EOF) {
			t.Fatal("existing authentication redaction boundary changed")
		}
		provider.done()
		coreGDCheckTree(t, root, before)
		s.released()
	}
	// A real Ask restore refusal while context is canceled remains the Ask cause;
	// the unchanged provider separately chooses context at its answer boundary.
	root = coreGDSeed(t)
	before := coreGDTree(t, root)
	s = gpLinuxNew(t, "synthetic-user\n\x03")
	canceled, cancel := context.WithCancel(ctx)
	restoreErr := errors.New("scripted final restore refusal")
	base := s.ops.Termios
	s.ops.Termios = func(f *os.File, req uintptr, a *syscall.Termios) error {
		err := base(f, req, a)
		if req == syscall.TCSETS && len(s.sets) == 4 {
			cancel()
			return restoreErr
		}
		return err
	}
	provider = coreGDNewScript(t, nil, nil)
	actual := s.prompt()
	provider.bindings.Prompt = func(c context.Context, label string, protected bool) (string, error) {
		answer, err := actual.Ask(c, label, protected)
		if protected && err != restoreErr {
			t.Fatal("Ask did not retain final restore error")
		}
		return answer, err
	}
	err = runGenData(canceled, root, genDataAll, provider.bindings)
	if !errors.Is(err, context.Canceled) || errors.Is(err, restoreErr) {
		t.Fatal("provider did not retain its separate context policy")
	}
	provider.done()
	coreGDCheckTree(t, root, before)
	// Actual All's successful first replacement remains after later transport
	// failure; the prompt and provider do not compensate fixed-file effects.
	root = coreGDSeed(t)
	s = gpLinuxNew(t, "synthetic-user\nsynthetic-password\nTAIL")
	steps = append(coreGDLoginSteps(), coreGDResourceStep(), coreGDStep{method: "GET", uri: "/networks", token: "synthetic-token", failure: errors.New("scripted later request refusal"), body: "unused"})
	provider = coreGDNewScript(t, steps, nil)
	provider.bindings.Prompt = s.prompt().Ask
	if err := runGenData(ctx, root, genDataAll, provider.bindings); err == nil {
		t.Fatal("later scripted provider refusal became success")
	}
	provider.done()
	b, readErr := os.ReadFile(filepath.Join(root, coreGDContainerPath))
	if readErr != nil || string(b) != coreGDEmptyContainers {
		t.Fatal("later failure undid actual first replacement")
	}
	if _, err := os.Lstat(filepath.Join(root, coreGDContainerPath) + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful first sibling remained")
	}
	s.released()
}
