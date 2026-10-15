package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func (e *Engine) sshCommandProcess(ctx context.Context, root string, argv []string, input sshInput) (Result, error) {
	return e.sshProcess(ctx, root, argv, input, true, nil)
}

// The same comparable writer for stdout/stderr makes os/exec give both child
// descriptors the same pipe. Concatenating two separately captured buffers
// afterward would lose the original interleaving. Reuse owned-child cancellation.
func (e *Engine) sshProcess(ctx context.Context, root string, argv []string, input sshInput, merge bool, env []string) (Result, error) {
	r := Result{ExitCode: -1, Command: sshShellJoin(argv)}
	if len(argv) == 0 {
		return r, fmt.Errorf("empty argv")
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if env != nil {
		// CSSH's sole source env override is represented by TTY::Command as
		// this logged shell string. Execute literal argv without a shell;
		// keep the original command-log boundary and empty-export spelling.
		r.Command = "( export shellHook=\"\" ; " + r.Command + " )"
	}
	id := logID()
	e.logText("[" + id + "] Running " + r.Command + "\n")
	start := time.Now()
	stdin, closeInput, err := sshStdin(root, input)
	if err != nil {
		return r, err
	}
	defer closeInput()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Stdin = root, stdin
	cmd.Env = os.Environ()
	if env != nil {
		cmd.Env = env
	}
	group := prepareChild(cmd)
	var stdout, stderr bytes.Buffer
	chunk := func(b []byte) { e.logText("[" + id + "] \t" + strings.TrimSuffix(string(b), "\n") + "\n") }
	out := &testConnectionCapture{buffer: &stdout, chunk: chunk}
	cmd.Stdout = out
	cmd.Stderr = &testConnectionCapture{buffer: &stderr, chunk: chunk}
	if merge {
		cmd.Stderr = out
	}
	if err = cmd.Start(); err != nil {
		return r, err
	}
	group.bind(cmd.Process.Pid)
	r.waitErr = cmd.Wait()
	group.finish()
	r.Stdout, r.Stderr = stdout.Bytes(), stderr.Bytes()
	r.ExitCode = cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		r.signal, r.Signal = status.Signal(), status.Signal().String()
	}
	status, err := sshStatus(ctx, r, nil)
	if err != nil {
		return r, err
	}
	display := Result{ExitCode: status}
	e.processLogFinish(id, start, display)
	return r, nil
}
