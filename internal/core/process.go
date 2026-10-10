package core

import (
	"bytes"
	"context"
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
	r := Result{ExitCode: -1, Command: ShellJoin(argv)}
	if len(argv) == 0 {
		return r, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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
	cmd.Stdout = &out
	cmd.Stderr = &errout
	e := cmd.Start()
	if e == nil {
		group.bind(cmd.Process.Pid)
		e = cmd.Wait()
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
	}
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	return r, nil
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
