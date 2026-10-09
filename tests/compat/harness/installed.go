package harness

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type firstWriter struct {
	mu    sync.Mutex
	start time.Time
	first *int64
	data  []byte
}

func (w *firstWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if *w.first == 0 && len(b) > 0 {
		*w.first = time.Since(w.start).Nanoseconds()
	}
	w.data = append(w.data, b...)
	return len(b), nil
}

// RunInstalled is the same process/state capture boundary for the isolated real
// tier. It uses real absolute packages and caller-owned synthetic configuration.
func RunInstalled(ctx context.Context, binary string, argv []string, config string, env []string) (Observation, int64, error) {
	o := Observation{Schema: 1, Executable: binary, ContractRevision: OracleRevision, ConfigRoot: config}
	var e error
	o.Before, e = Snapshot(config)
	if e != nil {
		return o, 0, e
	}
	cmd := exec.CommandContext(ctx, binary, argv...)
	cmd.Dir = config
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tree := newOwnedTree()
	cmd.Cancel = func() error {
		tree.signal(syscall.SIGTERM)
		go func() { time.Sleep(250 * time.Millisecond); tree.signal(syscall.SIGKILL) }()
		return nil
	}
	cmd.WaitDelay = 500 * time.Millisecond
	start := time.Now()
	var first int64
	out := &firstWriter{start: start, first: &first}
	errout := &firstWriter{start: start, first: new(int64)}
	cmd.Stdout = out
	cmd.Stderr = errout
	e = cmd.Start()
	var captureErr error
	if e == nil {
		tree.start(cmd.Process.Pid)
		e = cmd.Wait()
		captureErr = tree.finish(ctx.Err() != nil || e == exec.ErrWaitDelay)
	}
	o.WallNS = time.Since(start).Nanoseconds()
	o.Stdout = string(out.data)
	o.Stderr = string(errout.data)
	if cmd.ProcessState != nil {
		o.Exit = cmd.ProcessState.ExitCode()
		o.UserNS = cmd.ProcessState.UserTime().Nanoseconds()
		o.SystemNS = cmd.ProcessState.SystemTime().Nanoseconds()
		if r, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			o.MaxRSSKB = r.Maxrss
		}
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			o.Signal = status.Signal().String()
		}
	} else {
		return o, first, e
	}
	waitErr := e
	o.After, e = Snapshot(config)
	if e != nil {
		return o, first, e
	}
	if waitErr == exec.ErrWaitDelay {
		return o, first, fmt.Errorf("test driver inherited pipe cleanup: %w", waitErr)
	}
	if ctx.Err() != nil {
		return o, first, fmt.Errorf("test driver canceled: %w", ctx.Err())
	}
	if captureErr != nil {
		return o, first, captureErr
	}
	return o, first, nil
}
