package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "check-prepared" {
		return checkPrepared(ctx, args[1:], out, stderr)
	}
	flags := flag.NewFlagSet("confctl-hooks", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bindings := flags.String("tools", "", "Packaged tool bindings")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	args = flags.Args()
	if *bindings == "" || len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: confctl-hooks --tools FILE {prepare|trust|install|restore|run}")
		return 2
	}
	b, err := loadBindings(*bindings)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	q := hookcheck.TrustRequest{Capture: hookcheck.CaptureOptions{Dir: cwd, Git: b.tools.Git.Program, Env: os.Environ(), Invoke: invoke}, Tools: b.tools}
	fs := hookcheck.LocalTrustIO()
	switch args[0] {
	case "prepare":
		if len(args) != 1 {
			return 2
		}
		var path string
		path, err = preparePolicy(ctx, q, b)
		if err == nil {
			fmt.Fprintln(out, path)
		}
	case "trust":
		if len(args) > 2 || len(args) == 2 && args[1] != "--yes" {
			return 2
		}
		var proposal *hookcheck.TrustProposal
		var changes []hookcheck.TrustChange
		proposal, changes, err = hookcheck.PrepareTrust(ctx, q, fs)
		if err == nil {
			for _, change := range changes {
				fmt.Fprintf(out, "%s:\n  old: %s  new: %s", change.Slot, change.Old, change.New)
			}
			if len(changes) != 0 && len(args) != 2 {
				fmt.Fprint(out, "Trust these hook changes? [y/N] ")
				line, readErr := bufio.NewReader(in).ReadString('\n')
				if readErr != nil || (strings.TrimSpace(line) != "y" && strings.TrimSpace(line) != "yes") {
					fmt.Fprintln(stderr, "Hook trust was not changed.")
					return 1
				}
			}
			_, err = hookcheck.CommitTrust(ctx, q, proposal, fs)
		}
	case "install":
		if len(args) != 1 {
			return 2
		}
		_, changes, trustErr := hookcheck.PrepareTrust(ctx, q, fs)
		if trustErr != nil {
			err = trustErr
		} else if len(changes) != 0 {
			err = hookcheck.ErrTrustRequired
		} else {
			err = installHooks(ctx, q, b)
		}
	case "restore":
		if len(args) != 1 {
			return 2
		}
		err = restoreHooks(ctx, q)
	case "run":
		if len(args) < 2 || len(args) > 3 {
			return 2
		}
		phase := hookcheck.HookPhase(args[1])
		if phase != hookcheck.PreCommit && phase != hookcheck.CommitMsg || phase == hookcheck.PreCommit && len(args) != 2 || phase == hookcheck.CommitMsg && len(args) != 3 {
			return 2
		}
		request := hookcheck.HookRequest{TrustRequest: q, Phase: phase}
		if phase == hookcheck.CommitMsg {
			request.MessagePath = args[2]
			request.CommentPrefix, err = commentPrefix(ctx, q)
			if err != nil {
				break
			}
		}
		var result hookcheck.HookOutcome
		result, err = hookcheck.RunHook(ctx, request, fs, invoke)
		out.Write(result.Result.Stdout)
		stderr.Write(result.Result.Stderr)
		if err == nil && result.Result.ExitCode != 0 {
			if result.Result.ExitCode < 0 || result.Result.ExitCode > 255 {
				return 1
			}
			return result.Result.ExitCode
		}
	default:
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// Git reports both aliases in its effective configuration order, including
// includes and scopes. Keep the last value verbatim; "auto" remains literal.
func commentPrefix(ctx context.Context, q hookcheck.TrustRequest) (string, error) {
	r, err := q.Capture.Invoke(ctx, hookcheck.Invocation{Program: q.Tools.Git.Program,
		Args: []string{"config", "--null", "--get-regexp", `^core\.(commentchar|commentstring)$`}, Dir: q.Capture.Dir, Env: q.Capture.Env})
	if err != nil {
		return "", err
	}
	if r.ExitCode == 1 && len(r.Stdout) == 0 {
		return "#", nil
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("cannot read Git comment prefix: %s", r.Stderr)
	}
	rows := bytes.Split(r.Stdout, []byte{0})
	if len(rows) < 2 || len(rows[len(rows)-1]) != 0 {
		return "", errors.New("malformed Git comment configuration")
	}
	prefix := ""
	for _, row := range rows[:len(rows)-1] {
		key, value, ok := bytes.Cut(row, []byte{'\n'})
		if !ok || string(key) != "core.commentchar" && string(key) != "core.commentstring" || len(value) == 0 {
			return "", errors.New("empty or malformed Git comment prefix")
		}
		prefix = string(value)
	}
	return prefix, nil
}

func preparedInput(args []string, env func(string) (string, bool)) (hookcheck.PreparedInput, error) {
	var input hookcheck.PreparedInput
	if len(args) != 1 || args[0] != string(hookcheck.PreCommit) && args[0] != string(hookcheck.CommitMsg) {
		return input, errors.New("check-prepared requires one hook phase")
	}
	path, pathOK := env("CONFCTL_HOOK_CONTEXT")
	digest, digestOK := env("CONFCTL_HOOK_CONTEXT_SHA256")
	message, messageOK := env("CONFCTL_HOOK_MESSAGE")
	h, err := hex.DecodeString(digest)
	if !pathOK || !filepath.IsAbs(path) || !digestOK || err != nil || len(h) != 32 || hex.EncodeToString(h) != digest || !messageOK {
		return input, errors.New("missing or malformed prepared hook input")
	}
	input.ContextPath, input.Phase, input.MessagePath = path, hookcheck.HookPhase(args[0]), message
	copy(input.SHA256[:], h)
	return input, nil
}

func checkPrepared(ctx context.Context, args []string, out, stderr io.Writer) int {
	input, err := preparedInput(args, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	tools, err := hookcheck.LoadPreparedTools(ctx, input)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	findings, err := hookcheck.CheckPrepared(ctx, input, tools, invoke, func(ctx context.Context, prepared hookcheck.Invocation) error {
		return checkGo(ctx, tools, prepared, out, stderr, invoke)
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, f := range findings {
		level := "error"
		if f.Warning {
			level = "warning"
		}
		fmt.Fprintf(out, "%s: %s %q:%d: %s\n", level, f.Check, f.Path, f.Line, f.Message)
	}
	if hookcheck.HasErrors(findings) {
		return 1
	}

	return 0
}

func checkGo(ctx context.Context, tools hookcheck.ToolSet, prepared hookcheck.Invocation, out, stderr io.Writer, exec hookcheck.Invoke) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tools.Gofmt.Program != filepath.Join(tools.Gofmt.Output, "bin/gofmt") {
		return errors.New("Go checks require the bound Go-tool wrapper")
	}
	for _, program := range []string{"gofmt", "go"} {
		path := filepath.Join(tools.Gofmt.Output, "bin", program)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("Go-tool wrapper is not a regular executable: %q", path)
		}
	}
	modules, err := preflightGoModules(prepared.Dir, tools.Gofmt.Output)
	if err != nil {
		return err
	}
	// Preflight every module and environment before starting even the root's vet.
	environments := make([][]string, len(modules))
	for i, binding := range modules {
		environments[i], err = moduleGoEnv(prepared.Env, binding)
		if err != nil {
			return err
		}
	}
	if len(modules) > 0 {
		// Go's supported command suppresses its telemetry sidecar within this
		// invocation's owned config tree before any vet/test process starts.
		result, err := exec(ctx, hookcheck.Invocation{Program: filepath.Join(tools.Gofmt.Output, "bin/go"), Args: []string{"telemetry", "off"}, Dir: prepared.Dir, Env: environments[0]})
		_, stdoutErr := io.Copy(out, bytes.NewReader(result.Stdout))
		_, stderrErr := io.Copy(stderr, bytes.NewReader(result.Stderr))
		if err := errors.Join(err, stdoutErr, stderrErr); err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("go telemetry off failed with status %d", result.ExitCode)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for i, binding := range modules {
		dir := filepath.Join(prepared.Dir, binding.Directory)
		for _, action := range []string{"vet", "test"} {
			result, err := exec(ctx, hookcheck.Invocation{Program: filepath.Join(tools.Gofmt.Output, "bin/go"), Args: []string{action, "-mod=readonly", "./..."}, Dir: dir, Env: environments[i]})
			out.Write(result.Stdout)
			stderr.Write(result.Stderr)
			if err != nil {
				return err
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("go %s failed with status %d", action, result.ExitCode)
			}
		}
	}
	return nil
}

// Each direct tool and its cooperating children share an owned process group.
// Cancellation kills that group and waits for the direct child; it does not
// claim control over descendants that deliberately create another session.
func invoke(ctx context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
	cmd := exec.CommandContext(ctx, req.Program, req.Args...)
	cmd.Dir, cmd.Env = req.Dir, req.Env
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := hookcheck.ToolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var status *exec.ExitError
	if errors.As(err, &status) {
		result.ExitCode = status.ExitCode()
		return result, nil
	}
	return result, err
}
