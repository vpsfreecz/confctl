package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/vpsfreecz/confctl/internal/ui"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// These operations belong only to one reached Linux Basic size/key adapter.
// Tests script them without a terminal or competing input goroutine.
type basicLinuxOps struct {
	Size     func(*os.File) (ui.BasicSize, error)
	Termios  func(*os.File, uintptr, *unix.Termios) error
	Identity func(*os.File) (basicInputIdentity, error)
	Env      func(string) (string, bool)
	Pipe     func() (*os.File, *os.File, error)
	Poll     func(*os.File, *os.File) error
	Byte     func(*bufio.Reader) (byte, error)
	Wake     func(*os.File) error
	Close    func(*os.File) error
}

type basicInputIdentity struct {
	FD             uintptr
	Dev, Ino, Rdev uint64
	Mode           uint32
}

func newLinuxBasicPager(ctx context.Context, stdin, stdout, stderr *os.File, input *bufio.Reader) (ui.PagerSinkCloser, error) {
	return newLinuxBasicPagerWithOps(ctx, stdin, stdout, stderr, input, nativeBasicLinuxOps())
}

// Input is the same retained reader used by comparison confirmation. The
// UI owner samples size twice and retains its cursor and input policies.
func newLinuxBasicPagerWithOps(ctx context.Context, stdin, stdout, stderr *os.File, input *bufio.Reader, ops basicLinuxOps) (ui.PagerSinkCloser, error) {
	if stdin == nil || stdout == nil || stderr == nil || input == nil {
		return nil, fmt.Errorf("basic pager original descriptors/input are unavailable")
	}
	return ui.NewBasicPager(ctx, stdout, ui.BasicIO{
		Size: func(ctx context.Context) (ui.BasicSize, error) { return linuxBasicSize(ctx, stdout, stderr, ops) },
		Wrap: ui.WrapBasic,
		Key:  func(ctx context.Context) (string, error) { return linuxBasicKey(ctx, stdin, input, ops) },
	})
}

func linuxBasicSize(ctx context.Context, stdout, stderr *os.File, ops basicLinuxOps) (ui.BasicSize, error) {
	if err := ctx.Err(); err != nil {
		return ui.BasicSize{}, err
	}
	size, err := ops.Size(stdout)
	if stopped := ctx.Err(); stopped != nil {
		return ui.BasicSize{}, stopped
	}
	if err == nil && size.Columns.Sign() > 0 {
		return size, nil
	}
	var attributes unix.Termios
	tty := ops.Termios(stderr, syscall.TCGETS, &attributes) == nil
	if err := ctx.Err(); err != nil {
		return ui.BasicSize{}, err
	}
	if tty {
		size, err = ops.Size(stderr)
		if stopped := ctx.Err(); stopped != nil {
			return ui.BasicSize{}, stopped
		}
		if err == nil && size.Columns.Sign() > 0 {
			return size, nil
		}
		if err != nil && !errors.Is(err, syscall.EOPNOTSUPP) {
			return ui.BasicSize{}, err
		}
	}
	return basicEnvironmentSize(ops.Env)
}

var basicColumnsLine = regexp.MustCompile(`(?m)^[0-9]+$`)
var basicANSICON = regexp.MustCompile(`\((.*)x(.*)\)`)

// Only tty-screen's consumed decimal-prefix operation: ASCII space/sign and
// single underscores between digits, stopping at the first other character.
// Values stay arbitrary precision, never narrowed to a viewport/native int.
func basicDecimalPrefix(text string) *big.Int {
	text = strings.TrimLeft(text, " \t\n\v\f\r")
	negative := false
	if len(text) > 0 && (text[0] == '+' || text[0] == '-') {
		negative = text[0] == '-'
		text = text[1:]
	}
	var digits strings.Builder
	for i := 0; i < len(text); i++ {
		b := text[i]
		if b >= '0' && b <= '9' {
			digits.WriteByte(b)
			continue
		}
		if b == '_' && digits.Len() > 0 && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9' {
			continue
		}
		break
	}
	n := new(big.Int)
	if digits.Len() != 0 {
		n.SetString(digits.String(), 10)
	}
	if negative {
		n.Neg(n)
	}
	return n
}

func basicEnvironmentSize(env func(string) (string, bool)) (ui.BasicSize, error) {
	read := func(name string) (string, bool, error) {
		value, present := env(name)
		if present && !utf8.ValidString(value) {
			return "", false, fmt.Errorf("%w: %s is not valid UTF-8", ui.ErrBasicEncoding, name)
		}
		return value, present, nil
	}
	columns, _, err := read("COLUMNS")
	if err != nil {
		return ui.BasicSize{}, err
	}
	if basicColumnsLine.MatchString(columns) {
		width := basicDecimalPrefix(columns)
		if width.Sign() > 0 {
			rows, present, err := read("LINES")
			if err != nil {
				return ui.BasicSize{}, err
			}
			if !present {
				rows, _, err = read("ROWS")
				if err != nil {
					return ui.BasicSize{}, err
				}
			}
			return ui.BasicSize{Rows: basicDecimalPrefix(rows), Columns: width}, nil
		}
	}
	ansicon, _, err := read("ANSICON")
	if err != nil {
		return ui.BasicSize{}, err
	}
	if match := basicANSICON.FindStringSubmatch(ansicon); match != nil {
		width := basicDecimalPrefix(match[1])
		if width.Sign() > 0 {
			return ui.BasicSize{Rows: basicDecimalPrefix(match[2]), Columns: width}, nil
		}
	}
	return ui.BasicSize{Rows: big.NewInt(27), Columns: big.NewInt(80)}, nil
}

func basicRawTermios(saved unix.Termios) unix.Termios {
	raw := saved
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN | syscall.ECHOE | syscall.ECHOK
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	return raw
}

func basicSetTermios(ctx context.Context, stdin *os.File, attributes *unix.Termios, ops basicLinuxOps) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := ops.Termios(stdin, syscall.TCSETS, attributes)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// Each call owns only one raw interval/wake pipe/notifier. Stop and join the
// notifier before restoring/closing; the shared borrowed input is never closed,
// duplicated, discarded or made nonblocking. Restoration refuses replacement.
func linuxBasicKey(ctx context.Context, stdin *os.File, input *bufio.Reader, ops basicLinuxOps) (key string, err error) {
	if err = ctx.Err(); err != nil {
		return "", err
	}
	identity, err := ops.Identity(stdin)
	if err != nil {
		return "", err
	}
	var saved unix.Termios
	if err = ops.Termios(stdin, syscall.TCGETS, &saved); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	wait, err := acquireInputWait(stdin, input, inputWaitOps{Pipe: ops.Pipe, Poll: ops.Poll, Byte: ops.Byte, Wake: ops.Wake, Close: ops.Close})
	if err != nil {
		wait.StopJoin()
		closed := wait.CloseOwned()
		if closed.Read != nil || closed.Write != nil {
			err = errors.Join(err, closed.Read, closed.Write)
		}
		return "", err
	}
	rawAttempted := false
	defer func() {
		stopped := wait.StopJoin()
		var restoreErr error
		if rawAttempted {
			current, identityErr := ops.Identity(stdin)
			if identityErr != nil {
				restoreErr = fmt.Errorf("basic pager input identity: %w", identityErr)
			} else if current != identity {
				restoreErr = fmt.Errorf("basic pager input descriptor was replaced")
			} else {
				// Cancellation cannot suppress the acquired raw interval's restoration.
				restoreErr = basicSetTermios(context.Background(), stdin, &saved, ops)
			}
		}
		err = errors.Join(ctx.Err(), err)
		closed := wait.CloseOwned()
		writeClose := closed.Write
		if stopped.WriteClose != nil {
			writeClose = stopped.WriteClose
		}
		// Cancellation write-close happens before restore/read-close, but stays
		// in the existing final write-close presentation slot. The two write
		// close routes are mutually exclusive; no close is retried on error.
		err = errors.Join(restoreErr, err, stopped.Wake, closed.Read, writeClose)
		if err != nil {
			key = ""
		}
	}()
	raw := basicRawTermios(saved)
	if err = ctx.Err(); err != nil {
		return "", err
	}
	rawAttempted = true
	if err = basicSetTermios(ctx, stdin, &raw, ops); err != nil {
		return "", err
	}
	wait.Start(ctx)
	var character [utf8.UTFMax]byte
	needed := 1
	for used := 0; used < needed; used++ {
		read := wait.Byte(ctx)
		character[used], err = read.Value, read.Err
		if err != nil {
			if used > 0 && errors.Is(err, io.EOF) {
				return "", fmt.Errorf("%w: truncated key: %w", ui.ErrBasicEncoding, err)
			}
			return "", err
		}
		b := character[used]
		if used == 0 {
			switch {
			case b < 0x80:
				needed = 1
			case b >= 0xc2 && b <= 0xdf:
				needed = 2
			case b >= 0xe0 && b <= 0xef:
				needed = 3
			case b >= 0xf0 && b <= 0xf4:
				needed = 4
			default:
				return "", fmt.Errorf("%w: invalid key leading byte", ui.ErrBasicEncoding)
			}
		} else if b&0xc0 != 0x80 || used == 1 && (character[0] == 0xe0 && b < 0xa0 || character[0] == 0xed && b > 0x9f || character[0] == 0xf0 && b < 0x90 || character[0] == 0xf4 && b > 0x8f) {
			return "", fmt.Errorf("%w: invalid key continuation", ui.ErrBasicEncoding)
		}
	}
	if !utf8.Valid(character[:needed]) {
		return "", fmt.Errorf("%w: invalid key", ui.ErrBasicEncoding)
	}
	return string(character[:needed]), nil
}

// SyscallConn exposes the borrowed descriptor without File.Fd's possible
// blocking-mode change. No terminal or descriptor is opened by this helper.
func basicBorrowedFD(file *os.File) (uintptr, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd uintptr
	err = connection.Control(func(value uintptr) { fd = value })
	return fd, err
}

func basicNativeIdentity(file *os.File) (basicInputIdentity, error) {
	fd, err := basicBorrowedFD(file)
	if err != nil {
		return basicInputIdentity{}, err
	}
	var info syscall.Stat_t
	if err = syscall.Fstat(int(fd), &info); err != nil {
		return basicInputIdentity{}, err
	}
	return basicInputIdentity{FD: fd, Dev: uint64(info.Dev), Ino: info.Ino, Rdev: uint64(info.Rdev), Mode: info.Mode}, nil
}

func basicNativeTermios(file *os.File, request uintptr, attributes *unix.Termios) error {
	fd, err := basicBorrowedFD(file)
	if err != nil {
		return err
	}
	switch request {
	case unix.TCGETS:
		var saved *unix.Termios
		saved, err = unix.IoctlGetTermios(int(fd), unix.TCGETS)
		if err == nil {
			*attributes = *saved
		}
	case unix.TCSETS:
		err = unix.IoctlSetTermios(int(fd), unix.TCSETS, attributes)
	default:
		err = unix.EINVAL
	}
	runtime.KeepAlive(file)
	return err
}

func basicNativeSize(file *os.File) (ui.BasicSize, error) {
	fd, err := basicBorrowedFD(file)
	if err != nil {
		return ui.BasicSize{}, err
	}
	columns, rows, err := term.GetSize(int(fd))
	runtime.KeepAlive(file)
	if err != nil {
		return ui.BasicSize{}, err
	}
	return ui.BasicSize{Rows: big.NewInt(int64(rows)), Columns: big.NewInt(int64(columns))}, nil
}

func basicNativePoll(stdin, wake *os.File) error {
	inputFD, err := basicBorrowedFD(stdin)
	if err != nil {
		return err
	}
	wakeFD, err := basicBorrowedFD(wake)
	if err != nil {
		return err
	}
	fds := []unix.PollFd{{Fd: int32(inputFD), Events: unix.POLLIN}, {Fd: int32(wakeFD), Events: unix.POLLIN}}
	_, err = unix.Ppoll(fds, nil, nil)
	runtime.KeepAlive(stdin)
	runtime.KeepAlive(wake)
	if err != nil {
		return err
	}
	for _, fd := range fds {
		if fd.Revents&unix.POLLNVAL != 0 {
			return syscall.EBADF
		}
	}
	for _, fd := range fds {
		if fd.Revents&(unix.POLLIN|unix.POLLERR|unix.POLLHUP) != 0 {
			return nil
		}
	}
	return fmt.Errorf("basic pager poll returned no ready descriptor")
}

func nativeBasicLinuxOps() basicLinuxOps {
	return basicLinuxOps{
		Size: basicNativeSize, Termios: basicNativeTermios, Identity: basicNativeIdentity, Env: os.LookupEnv, Pipe: os.Pipe, Poll: basicNativePoll,
		Byte: func(input *bufio.Reader) (byte, error) { return input.ReadByte() },
		Wake: func(file *os.File) error {
			for {
				n, err := file.Write([]byte{1})
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if err == nil && n != 1 {
					return fmt.Errorf("basic pager wake short write")
				}
				return err
			}
		},
		Close: func(file *os.File) error { return file.Close() },
	}
}
