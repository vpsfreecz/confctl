package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Three PTYs retain stream separation and true isatty on each original fd.
func ptyPair() (*os.File, *os.File, error) {
	// Keep the master nonblocking and registered with Go's runtime poller so
	// Close interrupts reads. File.Fd would switch an OpenFile FD to blocking;
	// perform ioctls through SyscallConn instead. Child slaves remain blocking.
	m, e := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, nil, e
	}
	raw, e := m.SyscallConn()
	if e != nil {
		m.Close()
		return nil, nil, e
	}
	unlock := int32(0)
	var n uint32
	var eno syscall.Errno
	e = raw.Control(func(fd uintptr) {
		_, _, eno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
		if eno == 0 {
			_, _, eno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n)))
		}
	})
	if e != nil {
		m.Close()
		return nil, nil, e
	}
	if eno != 0 {
		m.Close()
		return nil, nil, eno
	}
	// Zero clears deadlines; this is a pollability check, not a new timeout.
	if e = m.SetReadDeadline(time.Time{}); e != nil {
		m.Close()
		return nil, nil, fmt.Errorf("PTY master requires runtime polling: %w", e)
	}
	s, e := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0600)
	if e != nil {
		m.Close()
		return nil, nil, e
	}
	return m, s, nil
}

// Return the CLI wait error separately from a capture failure. Nonzero CLI exit
// remains an observable result, while leftover work/drain failure rejects it.
func runPTY(ctx context.Context, cmd *exec.Cmd, input string, out, errout *bytes.Buffer, tree *ownedTree) (error, error) {
	inM, inS, e := ptyPair()
	if e != nil {
		return e, nil
	}
	defer inM.Close()
	defer inS.Close()
	outM, outS, e := ptyPair()
	if e != nil {
		return e, nil
	}
	defer outM.Close()
	defer outS.Close()
	errM, errS, e := ptyPair()
	if e != nil {
		return e, nil
	}
	defer errM.Close()
	defer errS.Close()
	cmd.Stdin = inS
	cmd.Stdout = outS
	cmd.Stderr = errS
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if e = cmd.Start(); e != nil {
		return e, nil
	}
	tree.start(cmd.Process.Pid)
	_ = inS.Close()
	_ = outS.Close()
	_ = errS.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(out, outM) }()
	go func() { defer wg.Done(); _, _ = io.Copy(errout, errM) }()
	if input != "" {
		_, _ = io.WriteString(inM, input)
	}
	_, _ = inM.Write([]byte{4})
	e = cmd.Wait()
	captureErr := tree.finish(ctx.Err() != nil || e == exec.ErrWaitDelay)
	if drainErr := drainPTY(ctx, &wg, outM, errM); drainErr != nil {
		captureErr = drainErr
	}
	return e, captureErr
}

func drainPTY(ctx context.Context, wg *sync.WaitGroup, masters ...*os.File) error {
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		// cmd.Wait has ended its exec context watcher. Closing these pollable
		// masters explicitly unblocks both readers even after successful exit.
		for _, master := range masters {
			_ = master.Close()
		}
		<-drained
		return fmt.Errorf("test driver PTY drain canceled: %w", ctx.Err())
	}
}
