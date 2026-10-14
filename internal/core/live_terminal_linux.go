//go:build linux

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// These closures are lazy. Scripted source checks do not execute native console,
// ioctl, signal or timer operations. Actual ABI/CLOEXEC/TTY gates remain separate.
func newLinuxLiveTerminal(stdin, stdout, stderr *os.File) *liveTerminal {
	return newLiveTerminal(stdin, stdout, stderr, liveTerminalOps{
		OpenConsole:  func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) },
		CloseConsole: func(file *os.File) error { return file.Close() },
		Dimensions: func(file *os.File) (int, int, error) {
			if file == nil {
				return 0, 0, fmt.Errorf("live terminal dimensions file is unavailable")
			}
			size, err := basicNativeSize(file)
			if err != nil {
				return 0, 0, err
			}
			// The actual native size owner returns only four uint16 fields.
			return int(size.Rows.Int64()), int(size.Columns.Int64()), nil
		},
		IoctlSize: liveNativeIoctlSize,
		IsTTY: func(file *os.File) (bool, error) {
			if file == nil {
				return false, fmt.Errorf("live terminal TTY file is unavailable")
			}
			var attributes syscall.Termios
			err := basicNativeTermios(file, syscall.TCGETS, &attributes)
			var errno syscall.Errno
			if errors.As(err, &errno) {
				return false, nil // stable file's native terminal-status ioctl failure
			}
			return err == nil, err // Go file/control failures retain their identity
		},
		Env: os.LookupEnv,
		NotifyResize: func(notify chan<- os.Signal) (func() error, error) {
			signal.Notify(notify, syscall.SIGWINCH)
			return func() error { signal.Stop(notify); return nil }, nil
		},
		Now: time.Now,
		Wait: func(ctx context.Context, duration time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	})
}

// Only tty-screen's four-short request. Caller retains stable borrowed files;
// SyscallConn avoids File.Fd's possible blocking-mode side effect. KeepAlive
// preserves file/buffer lifetimes, without protection from hostile replacement.
func liveNativeIoctlSize(file *os.File, request uintptr, size *[4]uint16) (int, error) {
	if file == nil || size == nil {
		return -1, fmt.Errorf("live terminal ioctl size binding is unavailable")
	}
	fd, err := basicBorrowedFD(file)
	if err != nil {
		return -1, err
	}
	result, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(size)))
	runtime.KeepAlive(file)
	runtime.KeepAlive(size)
	if errno != 0 {
		return -1, errno
	}
	return int(result), nil
}
