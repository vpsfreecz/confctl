package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/vpsfreecz/confctl/internal/git"
	"golang.org/x/term"
)

func commitAdapterFailure(err error) error {
	if err == nil {
		return nil
	}
	return &git.CommitAdapterError{Err: err}
}

func commitTerminalGroup(file *os.File) (int32, error) {
	var group int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&group)))
	if errno != 0 {
		return 0, errno
	}
	return group, nil
}

func commitStdoutTTY(file *os.File) bool {
	fd, err := basicBorrowedFD(file)
	if err != nil {
		return false
	}
	tty := term.IsTerminal(int(fd))
	runtime.KeepAlive(file)
	return tty
}

// Blocking SIGTTOU on this locked thread permits restoring the foreground group
// without changing the process-wide handler or unrelated signal mask bits.
func restoreCommitForeground(file *os.File, group int32) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	mask := uint64(1) << (uint(syscall.SIGTTOU) - 1)
	var previous uint64
	_, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 0, uintptr(unsafe.Pointer(&mask)), uintptr(unsafe.Pointer(&previous)), 8, 0, 0)
	if errno != 0 {
		return errno
	}
	defer func() {
		_, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 2, uintptr(unsafe.Pointer(&previous)), 0, 8, 0, 0)
		if errno != 0 {
			err = errors.Join(err, errno)
		}
	}()
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&group)))
	if errno != 0 {
		return errno
	}
	return nil
}

// Git commit alone inherits the caller's descriptors. Captured Process and SDK
// services retain their existing pipes, output and command-log contracts.
func runInheritedCommit(ctx context.Context, root string, argv []string, stdin, stdout, stderr *os.File) (r Result, err error) {
	return runInheritedCommitWithRelay(ctx, root, argv, stdin, stdout, stderr, "")
}

// Direct source tests inject a private helper path. The application always
// resolves the helper relative to its actual packaged executable.
func runInheritedCommitWithRelay(ctx context.Context, root string, argv []string, stdin, stdout, stderr *os.File, helperPath string) (r Result, err error) {
	return runInheritedCommitObserved(ctx, root, argv, stdin, stdout, stderr, helperPath, rawCommitWait)
}

// A private raw-wait seam records actual consumed events in finite adapter
// tests, and injects observer failures without a public process interface.
func runInheritedCommitObserved(ctx context.Context, root string, argv []string, stdin, stdout, stderr *os.File, helperPath string, wait commitRawWait) (r Result, err error) {
	r = Result{ExitCode: -1, Command: ShellJoin(argv)}
	if len(argv) == 0 {
		return r, errors.New("empty argv")
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	invocation, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(invocation, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = root, os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	owned := prepareChild(cmd)
	tty, err := openCommitTTY()
	if err != nil {
		return r, commitAdapterFailure(err)
	}
	var relay *commitStopRelay
	startFailed := false
	if tty != nil {
		defer func() {
			if relay != nil {
				err = errors.Join(err, commitAdapterFailure(relay.close()))
			}
			err = errors.Join(err, commitAdapterFailure(errors.Join(tty.restore(startFailed), tty.file.Close())))
		}()
		if helperPath == "" {
			helperPath, err = installedCommitRelay()
			if err != nil {
				return r, commitAdapterFailure(err)
			}
		}
		relay, err = startCommitStopRelay(ctx, helperPath, tty)
		if err != nil {
			return r, commitAdapterFailure(err)
		}
		if err = tty.beforeStart(cmd); err != nil {
			return r, commitAdapterFailure(err)
		}
	}
	err = cmd.Start()
	if err != nil {
		startFailed = true
		return r, err
	}
	owned.bind(cmd.Process.Pid)
	var adapterErr error
	if tty != nil {
		tty.gitGroup, tty.handed = cmd.Process.Pid, tty.startHandoff
		events := make(chan commitObservation, 1)
		observerStop, observerDone := make(chan struct{}), make(chan struct{})
		go observeCommitChild(cmd.Process.Pid, wait, events, observerStop, observerDone)
		adapterErr = commitJobLoop(ctx, events, relay.replies, relay.done, relay.exitError, commitJobOps{
			request:  func(sig syscall.Signal) error { return relay.requestStop(tty, sig) },
			eligible: relay.signals.eligible,
			resume: func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := tty.handoff(); err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return owned.signal(syscall.SIGCONT)
			},
			cont: func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return tty.handoff()
			},
		})
		if adapterErr != nil {
			cancel()
			_ = owned.cancel()
		}
		close(observerStop)
		<-observerDone // Exit is still waitable; never overlap the raw observer.
	}
	waitErr := cmd.Wait() // The only exit-consuming Git wait.
	owned.finish()
	err = commitAdapterFailure(adapterErr)
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			r.Signal = status.Signal().String()
		}
		// Ordinary child failure is a result, not a cancellation/start error.
	} else {
		err = errors.Join(err, waitErr)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return r, err
}
