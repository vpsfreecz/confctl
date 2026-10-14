package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type Result struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal"`
	Command  string `json:"command"`
	Stdout   []byte `json:"stdout_b64"`
	Stderr   []byte `json:"stderr_b64"`
	signal   syscall.Signal
	waitErr  error
	capture  *captureCompletion // private joined provenance, never wire metadata
}

type captureOrigin uint8

const (
	captureCallback captureOrigin = iota + 1
	captureRead
)

type captureCause struct {
	owner  *commandCapture
	origin captureOrigin
	err    error
}

func (c *captureCause) Error() string { return "captured command copy failed" }

type captureReceipt struct {
	stream         string
	count          int64
	callback, read error
}
type captureCompletion struct {
	primary error
	streams []captureReceipt
}

// Nonzero-sized invocation identity: only this direct winning cause is owned.
// Notification never calls childGroup.cancel (which can await pre-Start bind).
type commandCapture struct {
	parent, ctx context.Context
	cancel      context.CancelCauseFunc
}

func newCommandCapture(parent context.Context) *commandCapture {
	ctx, cancel := context.WithCancelCause(parent)
	return &commandCapture{parent, ctx, cancel}
}
func (c *commandCapture) fail(origin captureOrigin, err error) {
	if err != nil {
		c.cancel(&captureCause{c, origin, err})
	}
}

// Captures are written by their respective copy goroutines. joined is reached
// only after Wait has joined those copies; the receipt keeps value snapshots.
func (c *commandCapture) joined(waitErr error, streams ...*testConnectionCapture) (*captureCompletion, error) {
	completion := &captureCompletion{}
	for _, s := range streams {
		completion.streams = append(completion.streams, captureReceipt{s.stream, s.count, s.callbackErr, s.readErr})
	}
	if winner, ok := context.Cause(c.ctx).(*captureCause); ok && winner.owner == c {
		completion.primary = winner.err
		return completion, winner.err
	}
	// A parent's arbitrary cause (including a foreign or wrapped marker) is
	// presented at the existing ctx.Err boundary. Error shape grants no origin.
	if c.ctx.Err() != nil {
		return completion, c.parent.Err()
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			return completion, waitErr
		}
	}
	return completion, nil
}
func (r Result) capturePrimary() error {
	if r.capture != nil {
		return r.capture.primary
	}
	return nil
}

func ShellJoin(argv []string) string {
	a := make([]string, len(argv))
	for i, s := range argv {
		if s == "" {
			a[i] = "''"
			continue
		}
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_-.,:+/@%", r) {
				b.WriteRune(r)
			} else if r == '\n' {
				b.WriteString("'\n'")
			} else {
				b.WriteRune('\\')
				b.WriteRune(r)
			}
		}
		a[i] = b.String()
	}
	return strings.Join(a, " ")
}
func SSHArgs(m Machine, argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	if m.Target.Localhost {
		return argv, nil
	}
	if m.Target.Host == nil {
		return nil, fmt.Errorf("machine %s has no target", m.Name)
	}
	a := []string{"ssh"}
	if c := os.Getenv("CONFCTL_SSH_CONFIG"); c != "" {
		a = append(a, "-F", c)
	}
	a = append(a, "-l", "root")
	if m.Target.Port != 22 {
		a = append(a, "-p", fmt.Sprint(m.Target.Port))
	}
	return append(a, *m.Target.Host, ShellJoin(argv)), nil
}
func Process(ctx context.Context, root string, argv []string, input []byte) (Result, error) {
	return processWithTestConnectionLog(ctx, root, argv, input, nil)
}

func processWithTestConnectionLog(ctx context.Context, root string, argv []string, input []byte, chunk func([]byte) error) (Result, error) {
	r := Result{ExitCode: -1, Command: ShellJoin(argv)}
	if len(argv) == 0 {
		return r, fmt.Errorf("empty argv")
	}
	owner := newCommandCapture(ctx)
	defer owner.cancel(nil)
	cmd := exec.CommandContext(owner.ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = os.Environ()
	group := prepareChild(cmd)
	if input == nil {
		r, w, err := os.Pipe()
		if err != nil {
			return Result{}, err
		}
		defer r.Close()
		defer w.Close()
		cmd.Stdin = r
	} else {
		cmd.Stdin = bytes.NewReader(input)
	}
	var out, errout bytes.Buffer
	stdout := &testConnectionCapture{buffer: &out, chunk: chunk, owner: owner, stream: "stdout"}
	stderr := &testConnectionCapture{buffer: &errout, chunk: chunk, owner: owner, stream: "stderr"}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	e := cmd.Start()
	if e == nil {
		group.bind(cmd.Process.Pid)
		e = cmd.Wait()
		r.waitErr = e
		group.finish()
	}
	r.Stdout = out.Bytes()
	r.Stderr = errout.Bytes()
	if cmd.ProcessState == nil {
		return r, e
	}
	r.ExitCode = cmd.ProcessState.ExitCode()
	if s, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && s.Signaled() {
		r.Signal = s.Signal().String()
		r.signal = s.Signal()
	}
	r.capture, e = owner.joined(e, stdout, stderr)
	return r, e
}
func RunMachine(ctx context.Context, root string, m Machine, argv []string, input []byte) (Result, error) {
	a, e := SSHArgs(m, argv)
	if e != nil {
		return Result{}, e
	}
	return Process(ctx, root, a, input)
}
func commandError(a []string, r Result) error {
	return fmt.Errorf("Running `%s` failed with exit status %d\n%s", strings.Join(a, " "), r.ExitCode, string(r.Stderr))
}
