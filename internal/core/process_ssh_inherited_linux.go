package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Kernel.system's interactive SSH shares the caller's process group and
// descriptors. Never apply the captured runner's Setpgid or Git's stop relay:
// the terminal and launching shell retain foreground/job-control ownership.
func (e *Engine) sshInteractiveProcess(ctx context.Context, root string, argv []string, in io.Reader, out io.Writer) (Result, error) {
	stdin, inputOK := in.(*os.File)
	stdout, outputOK := out.(*os.File)
	if !inputOK || !outputOK {
		return Result{ExitCode: -1}, fmt.Errorf("interactive SSH requires inherited file descriptors")
	}
	return runInheritedSSH(ctx, root, argv, stdin, stdout, os.Stderr)
}

func runInheritedSSH(ctx context.Context, root string, argv []string, stdin, stdout, stderr *os.File) (Result, error) {
	r, err := runInheritedChild(ctx, root, argv, stdin, stdout, stderr, nil)
	r.Command = sshShellJoin(argv)
	if err != nil {
		return r, err
	}
	_, err = sshStatus(ctx, r, nil)
	return r, err
}

// The inherited owner borrows all descriptors, keeps the caller's process
// group, and consumes exactly one Wait. Only SSH maps completed child status.
// An optional startup acknowledgment fires once, after Start or final failed
// setup cleanup; success does not certify later wait/restore completion.
func runInheritedChild(ctx context.Context, root string, argv []string, stdin *os.File, stdout, stderr io.Writer, onStarted func(error)) (r Result, err error) {
	r = Result{ExitCode: -1}
	acknowledged := false
	defer func() {
		if !acknowledged && onStarted != nil {
			onStarted(err)
		}
	}()
	if len(argv) == 0 {
		return r, fmt.Errorf("empty argv")
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil && !errors.Is(err, syscall.ENXIO) && !errors.Is(err, syscall.ENODEV) && !errors.Is(err, os.ErrNotExist) {
		return r, err
	}
	if tty != nil {
		var saved unix.Termios
		if err = sshTermios(tty, syscall.TCGETS, &saved); err != nil {
			_ = tty.Close()
			return r, err
		}
		defer func() {
			// Restore attributes only while our own job owns the foreground.
			// Never seize the terminal from the shell or another foreground job.
			group, groupErr := sshTerminalGroup(tty)
			if groupErr == nil && int(group) == syscall.Getpgrp() {
				groupErr = sshTermios(tty, syscall.TCSETS, &saved)
			}
			err = errors.Join(err, groupErr, tty.Close())
		}()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = root, os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	finished, escalationDone := make(chan struct{}), make(chan struct{})
	cancelled := false
	cmd.Cancel = func() error {
		// os.Process owns its child handle and synchronizes Signal with Wait;
		// Linux uses pidfd where available. No PID search or caller-group kill.
		cancelled = true
		err := cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			defer close(escalationDone)
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-finished:
			case <-timer.C:
				_ = cmd.Process.Kill()
			}
		}()
		return err
	}
	if err = cmd.Start(); err != nil {
		return r, err
	}
	acknowledged = true
	if onStarted != nil {
		onStarted(nil)
	}
	r.waitErr = cmd.Wait() // sole exit-consuming Wait; joins exec's Cancel call
	close(finished)
	if cancelled {
		<-escalationDone // no delayed child signaling survives this invocation
	}
	if cmd.ProcessState == nil {
		if r.waitErr != nil {
			return r, r.waitErr
		}
		return r, fmt.Errorf("inherited child did not return a process state")
	}
	r.ExitCode = cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		r.signal, r.Signal = status.Signal(), status.Signal().String()
	}
	err = inheritedChildError(ctx, r, nil)
	return r, err
}

// Completed nonzero/signal status stays a Result. Missing status and non-exit
// wait errors (including an unowned/missing child) are infrastructure failures.
func inheritedChildError(ctx context.Context, r Result, runErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	if r.waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(r.waitErr, &exit) {
			return r.waitErr
		}
	}
	if r.signal == 0 && r.ExitCode < 0 {
		return fmt.Errorf("command did not return an exit status")
	}
	return nil
}

func sshTermios(tty *os.File, request uintptr, attributes *unix.Termios) error {
	return basicNativeTermios(tty, request, attributes)
}

func sshTerminalGroup(tty *os.File) (int32, error) {
	var group int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&group)))
	if errno != 0 {
		return 0, errno
	}
	return group, nil
}

// Go's os/user model omits the passwd shell. Query the local NSS owner with
// literal argv rather than assuming /etc/passwd, root's name, $SHELL or sudo.
// The package environment must supply getent; absence is a visible spawn error.
func (e *Engine) sshRootShell() (string, error) {
	r, err := Process(e.Context, e.Root, []string{"getent", "passwd", "0"}, nil)
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		return "", commandError([]string{"getent", "passwd", "0"}, r)
	}
	line := strings.TrimSuffix(string(r.Stdout), "\n")
	fields := strings.Split(line, ":")
	if len(fields) != 7 || fields[2] != "0" || strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("invalid root passwd record from getent")
	}
	return fields[6], nil
}
