//go:build linux

package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unicode/utf8"

	"github.com/vpsfreecz/confctl/internal/ui"
)

// Only the reached Linux bindings for one borrowed original input and retained
// reader. Ask owns labels, editing and outer ensure; inputWait owns readiness
// and acquired wake resources. See gendata_prompt_linux.NOTICE for source and
// the intentional mask, single-attempt and UTF-8/fill-boundary differences.
type genDataPromptLinuxOps struct {
	Identity func(*os.File) (basicInputIdentity, error)
	Termios  func(*os.File, uintptr, *syscall.Termios) error
	Wait     inputWaitOps
}

// Both constructors are inert. Choosing native callbacks performs no native
// operation; their actual fd/ABI/terminal behavior needs separate verification.
func newLinuxGenDataPrompt(stdin *os.File, input *bufio.Reader, stdout io.Writer, flush func() error) *ui.GenDataPrompt {
	native := nativeBasicLinuxOps()
	return newLinuxGenDataPromptWithOps(stdin, input, stdout, flush, genDataPromptLinuxOps{
		Identity: native.Identity, Termios: native.Termios,
		Wait: inputWaitOps{Pipe: native.Pipe, Poll: native.Poll, Byte: native.Byte, Wake: native.Wake, Close: native.Close},
	})
}

func newLinuxGenDataPromptWithOps(stdin *os.File, input *bufio.Reader, stdout io.Writer, flush func() error, ops genDataPromptLinuxOps) *ui.GenDataPrompt {
	return ui.NewGenDataPrompt(ui.GenDataPromptIO{
		Writer: stdout, Flush: flush,
		Line: func(ctx context.Context) (string, error) { return linuxGenDataLine(ctx, stdin, input, ops.Wait) },
		BeginProtected: func(ctx context.Context) (ui.GenDataProtectedInput, error) {
			return linuxGenDataBegin(ctx, stdin, input, ops)
		},
	})
}

func linuxGenDataLine(ctx context.Context, stdin *os.File, input *bufio.Reader, ops inputWaitOps) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	wait, err := acquireInputWait(stdin, input, ops)
	var line []byte
	if err == nil {
		wait.Start(ctx)
		for {
			read := wait.Byte(ctx)
			err = read.Err
			if err != nil {
				break
			}
			line = append(line, read.Value)
			if read.Value == '\n' {
				break
			}
		}
	}
	stopped := wait.StopJoin()
	cleanup := genDataWaitError(stopped, wait.CloseOwned())
	if (err == nil || errors.Is(err, io.EOF)) && cleanup != nil {
		return "", cleanup
	}
	return string(line), err
}

func genDataModeBindings(ctx context.Context, stdin *os.File, input *bufio.Reader, ops genDataPromptLinuxOps) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stdin == nil || input == nil {
		return errors.New("authentication prompt original input is unavailable")
	}
	if ops.Identity == nil {
		return errors.New("authentication prompt identity binding is unavailable")
	}
	if ops.Termios == nil {
		return errors.New("authentication prompt termios binding is unavailable")
	}
	return nil
}

// attempted records a reached callback, including partial/error results. Context
// before every TCSETS can suppress the first attempt; no restore mutation is
// invented for that case. Basic keeps its own separate attempted/context policy.
func genDataSetTermios(ctx context.Context, stdin *os.File, attributes *syscall.Termios, ops genDataPromptLinuxOps) (attempted bool, err error) {
	for {
		if err = ctx.Err(); err != nil {
			return attempted, err
		}
		attempted = true
		err = ops.Termios(stdin, syscall.TCSETS, attributes)
		if !errors.Is(err, syscall.EINTR) {
			return attempted, err
		}
	}
}

// The bool identifies only a reached restore TCSETS error, never an identity
// error. Outer ENOTTY rescue and inner fallback must not classify infrastructure
// via a joined error. Stable descriptor identity/lifetime is a caller obligation.
func genDataRestore(stdin *os.File, identity basicInputIdentity, saved syscall.Termios, ops genDataPromptLinuxOps) (error, bool) {
	current, err := ops.Identity(stdin)
	if err != nil {
		return fmt.Errorf("authentication prompt input identity: %w", err), false
	}
	if current != identity {
		return errors.New("authentication prompt input descriptor was replaced"), false
	}
	_, err = genDataSetTermios(context.Background(), stdin, &saved, ops)
	return err, true
}

func linuxGenDataBegin(ctx context.Context, stdin *os.File, input *bufio.Reader, ops genDataPromptLinuxOps) (ui.GenDataProtectedInput, error) {
	if err := genDataModeBindings(ctx, stdin, input, ops); err != nil {
		return ui.GenDataProtectedInput{}, err
	}
	identity, err := ops.Identity(stdin)
	if err != nil {
		return ui.GenDataProtectedInput{}, err
	}
	var saved syscall.Termios
	err = ops.Termios(stdin, syscall.TCGETS, &saved)
	protected := ui.GenDataProtectedInput{Character: func(ctx context.Context) (string, error) {
		return linuxGenDataCharacter(ctx, stdin, input, ops)
	}}
	if err != nil {
		if errors.Is(err, syscall.ENOTTY) {
			return protected, nil
		}
		return ui.GenDataProtectedInput{}, err
	}
	attempted := false
	// Acquire and return this capability even when cancellation after snapshot
	// prevents every TCSETS. Actual Ask invokes it once; attempted=false is inert.
	protected.Restore = func() error {
		if !attempted {
			return nil
		}
		err, fromSet := genDataRestore(stdin, identity, saved, ops)
		if fromSet && errors.Is(err, syscall.ENOTTY) {
			return nil
		}
		return err
	}
	noecho := saved
	noecho.Lflag &^= syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL
	attempted, err = genDataSetTermios(ctx, stdin, &noecho, ops)
	if errors.Is(err, syscall.ENOTTY) {
		err = nil
	}
	return protected, err
}

// Explicit io-console alternative set_rawmode followed by intr:true. Preserve
// every other field; do not infer the installed HAVE_CFMAKERAW branch or use
// Basic's distinct mask. One stable fd attempt replaces source's duplicate
// failure-path setup through GetWriteFD of the same IO.
func genDataRawTermios(saved syscall.Termios) syscall.Termios {
	raw := saved
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.IGNPAR | syscall.PARMRK | syscall.INPCK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF | syscall.IXANY | syscall.IMAXBEL
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN | syscall.XCASE
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	raw.Iflag |= syscall.BRKINT
	raw.Lflag |= syscall.ISIG
	raw.Oflag |= syscall.OPOST
	return raw
}

func linuxGenDataCharacter(ctx context.Context, stdin *os.File, input *bufio.Reader, ops genDataPromptLinuxOps) (string, error) {
	if err := genDataModeBindings(ctx, stdin, input, ops); err != nil {
		return "", err
	}
	identity, err := ops.Identity(stdin)
	if err != nil {
		return "", err
	}
	var saved syscall.Termios
	if err = ops.Termios(stdin, syscall.TCGETS, &saved); err != nil {
		if errors.Is(err, syscall.ENOTTY) {
			return linuxGenDataFallback(ctx, stdin, input, ops.Wait)
		}
		return "", err
	}
	wait, err := acquireInputWait(stdin, input, ops.Wait)
	if err != nil {
		wait.StopJoin()
		wait.CloseOwned()
		return "", err // acquisition is infrastructure, including ENOTTY
	}
	raw := genDataRawTermios(saved)
	attempted, err := genDataSetTermios(ctx, stdin, &raw, ops)
	fromSource := attempted && err != nil
	var candidate string
	if err == nil {
		wait.Start(ctx)
		candidate, err, fromSource = genDataCandidate(ctx, wait, input)
	}
	stopped := wait.StopJoin()
	if attempted {
		if restoreErr, fromSet := genDataRestore(stdin, identity, saved, ops); restoreErr != nil {
			err, fromSource = restoreErr, fromSet
		}
	}
	cleanup := genDataWaitError(stopped, wait.CloseOwned())
	if fromSource && errors.Is(err, syscall.ENOTTY) {
		if cleanup != nil {
			return "", cleanup
		}
		return linuxGenDataFallback(ctx, stdin, input, ops.Wait)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if cleanup != nil {
		return "", cleanup
	}
	return candidate, err
}

// One fresh same-buffer getc scope. Its errors are never rescued recursively.
func linuxGenDataFallback(ctx context.Context, stdin *os.File, input *bufio.Reader, ops inputWaitOps) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	wait, err := acquireInputWait(stdin, input, ops)
	var candidate string
	if err == nil {
		wait.Start(ctx)
		candidate, err, _ = genDataCandidate(ctx, wait, input)
	}
	stopped := wait.StopJoin()
	cleanup := genDataWaitError(stopped, wait.CloseOwned())
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if cleanup != nil {
		return "", cleanup
	}
	return candidate, err
}

func genDataWaitError(stopped inputWaitStop, closed inputWaitClose) error {
	for _, err := range []error{stopped.Wake, stopped.WriteClose, closed.Read, closed.Write} {
		if err != nil {
			return err
		}
	}
	return nil
}

// Only UTF-8/no-readconv candidate grouping at the retained reader's actual
// fill boundary. Peek is bounded by Buffered and cannot refill. Initial invalid
// prefixes consume one byte; NEEDMORE consumes the initial window then one
// waited byte at a time, including an offending refill byte/truncated EOF.
// Ask alone decides whether an ordinary append admits the returned candidate.
func genDataCandidate(ctx context.Context, wait *inputWait, input *bufio.Reader) (string, error, bool) {
	var window [utf8.UTFMax]byte
	used := 0
	if input.Buffered() == 0 {
		read := wait.Byte(ctx)
		if read.Err != nil {
			return "", read.Err, read.FromRead
		}
		window[0], used = read.Value, 1
	}
	available := min(input.Buffered(), len(window)-used)
	peek, _ := input.Peek(available) // available never exceeds Buffered
	copy(window[used:], peek)
	initial := used + available
	needed := initial
	complete := utf8.FullRune(window[:initial])
	if complete {
		_, needed = utf8.DecodeRune(window[:initial])
	}
	for used < needed {
		read := wait.Byte(ctx)
		if read.Err != nil {
			return "", read.Err, read.FromRead
		}
		window[used] = read.Value
		used++
	}
	if complete {
		return string(window[:used]), nil, false
	}
	for !utf8.FullRune(window[:used]) {
		read := wait.Byte(ctx)
		if read.Err != nil {
			if errors.Is(read.Err, io.EOF) {
				return string(window[:used]), nil, false
			}
			return "", read.Err, read.FromRead
		}
		window[used] = read.Value
		used++
	}
	return string(window[:used]), nil, false
}
