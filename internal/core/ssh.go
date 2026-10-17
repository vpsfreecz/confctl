package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/ui"
)

// Presence is distinct from content: an absent input leaves the child's pipe
// open, whereas an explicitly empty string supplies EOF. Files open per host.
type sshInput struct {
	String *string
	File   *string
}

// The inherited runner owns only SSH; it never uses the commit stop relay.
type sshDeps struct {
	Run         func(context.Context, string, []string, sshInput) (Result, error)
	Interactive func(context.Context, string, []string, io.Reader, io.Writer) (Result, error)
	RootShell   func() (string, error)
}

type sshArgumentError struct{ error }

// These leaf options share the same typed/alias PP layout as test-connection:
// encounter order first, then declarations, and nil distinct from empty text.
// Reuse that owning projector instead of introducing a second option catalogue.
func (e *Engine) LogSSHCLI(inv cli.Invocation) error { return e.LogTestConnectionCLI(inv) }

// Command mode uses pipes; interactive mode retains the caller's descriptors.
func (e *Engine) SSH(inv cli.Invocation, in io.Reader, out, stderr io.Writer) error {
	return e.runSSH(inv, in, out, stderr, sshDeps{Run: e.sshCommandProcess, Interactive: e.sshInteractiveProcess, RootShell: e.sshRootShell})
}

func (e *Engine) sshMachines(inv cli.Invocation, empty string) ([]Machine, error) {
	all, err := e.Machines(false)
	if err != nil {
		return nil, err
	}
	opts := handlerOptions(inv)
	var pattern *string
	if len(inv.Args) != 0 {
		pattern = &inv.Args[0]
	}
	managed := opts.Managed
	if managed == "" {
		managed = "yes"
	}
	ms, err := Select(all, pattern, opts.Attrs, opts.Tags, managed)
	if err != nil {
		for _, f := range opts.Attrs {
			if !strings.Contains(f, "=") {
				return nil, &sshArgumentError{fmt.Errorf("Invalid filter ''")}
			}
		}
		return nil, &sshArgumentError{err}
	}
	runnable := make([]Machine, 0, len(ms))
	for _, m := range ms {
		if m.CarrierName == nil && m.Target.Host != nil {
			runnable = append(runnable, m)
		}
	}
	if len(runnable) == 0 {
		return nil, fmt.Errorf("%s", empty)
	}
	return runnable, nil
}

func sshInvocationInput(inv cli.Invocation) sshInput {
	var input sshInput
	if p := inv.Options["input-string"]; p.Present {
		value := p.Value.String
		input.String = &value
	}
	if p := inv.Options["input-file"]; p.Present {
		value := p.Value.String
		input.File = &value
	}
	return input
}

func (e *Engine) sshConfirmation(inv cli.Invocation, machines []Machine, in io.Reader, out io.Writer, header, command string) error {
	if inv.Options["yes"].Value.Bool {
		return nil
	}
	table, err := e.List(machines, nil, true)
	if err != nil {
		return err
	}
	fmt.Fprint(out, header+"\n"+table)
	if command != "" {
		fmt.Fprint(out, "\nCommand: "+command+"\n")
	}
	_, line := commandInput(in)
	line = e.timedPromptLine(line)
	return testConnectionConfirm(e.Context, line, out)
}

func (e *Engine) runSSH(inv cli.Invocation, in io.Reader, out, stderr io.Writer, deps sshDeps) error {
	machines, err := e.sshMachines(inv, "No machines to ssh to")
	if err != nil {
		return err
	}
	input := sshInvocationInput(inv)
	if input.String != nil && input.File != nil {
		return &sshArgumentError{fmt.Errorf("use one of --input-string or --input-file")}
	}
	if len(inv.Args) == 1 {
		if len(machines) != 1 {
			return &sshArgumentError{fmt.Errorf("missing command")}
		}
		if err = e.sshConfirmation(inv, machines, in, out, "Open interactive shell on the following machine:", ""); err != nil {
			return err
		}
		if deps.Interactive == nil {
			return fmt.Errorf("inherited SSH runner is not integrated")
		}
		argv, err := sshInteractiveArgs(machines[0], deps.RootShell)
		if err != nil {
			return err
		}
		r, err := deps.Interactive(e.Context, e.Root, argv, in, out)
		// Kernel.system's false result is ignored. Spawn/adapter/cancellation
		// failures are still errors, never successful interactive completion.
		_, err = sshStatus(e.Context, r, err)
		return err
	}
	var command []string
	if len(inv.Args) > 1 {
		command = inv.Args[1:]
	}
	if len(inv.Args) == 0 && !inv.Options["yes"].Value.Bool {
		// Ruby passes args[1..] (nil) to Shellwords.join inside the
		// confirmation block. The table and blank line precede that failure;
		// this is not an interactive invocation or an assumed arity refusal.
		table, err := e.List(machines, nil, true)
		if err != nil {
			return err
		}
		fmt.Fprint(out, "Run command over SSH on the following machines:\n"+table+"\n")
		return fmt.Errorf("undefined method 'map' for nil")
	}
	if err = e.sshConfirmation(inv, machines, in, out, "Run command over SSH on the following machines:", sshShellJoin(command)); err != nil {
		return err
	}
	aggregate := inv.Options["aggregate"].Value.Bool
	if inv.Options["parallel"].Value.Bool {
		if len(inv.Args) == 0 {
			// Even --yes cannot bypass Shellwords.join(nil) in the parallel
			// Executing header. Serial --yes alone reaches the empty command.
			return fmt.Errorf("undefined method 'map' for nil")
		}
		ctx, cancel := context.WithCancel(e.Context)
		defer cancel()
		operation := *e
		operation.Context = ctx
		view, err := e.sshLive(ctx, out, stderr, command, machines, func(error) { cancel() })
		if err != nil {
			return err
		}
		var advance func(sshHostResult)
		completed := make(map[string]bool, len(machines))
		failedHost := false
		if view == nil {
			fmt.Fprintf(out, "Executing %s on %d machines\n", sshShellJoin(command), len(machines))
		} else {
			advance = func(r sshHostResult) {
				completed[r.Name] = true // callback is serialized by the SSH result owner
				state := ui.LiveSucceeded
				if r.Status != 0 {
					state = ui.LiveFailed
					failedHost = true
				}
				_ = view.SetRows(ui.LiveRow{ID: "host:" + r.Name, Label: r.Name, State: state, Detail: fmt.Sprintf("exit status %d", r.Status), Target: true})
				_ = view.Advance("ssh")
			}
		}
		results, err := operation.sshParallelProgress(machines, command, input, deps, advance)
		if view != nil {
			state := ui.LiveSucceeded
			if failedHost {
				state = ui.LiveFailed
			}
			if err != nil {
				state = ui.LiveIncomplete
			}
			_ = view.SetRows(ui.LiveRow{ID: "ssh", Label: "Commands", Current: big.NewInt(int64(len(completed))), Total: big.NewInt(int64(len(machines))), State: state})
			for _, m := range machines {
				if !completed[m.Name] {
					_ = view.SetRows(ui.LiveRow{ID: "host:" + m.Name, Label: m.Name, State: ui.LiveIncomplete, Target: true})
				}
			}
			err = errors.Join(err, view.Close())
			if e.Context.Err() != nil {
				err = errors.Join(err, e.Context.Err())
			}
		}
		if err != nil {
			return err
		}
		if aggregate {
			return sshPrintResults(out, results, true)
		}
		for _, r := range results {
			fmt.Fprintln(out, r.Name+":")
			if err := sshPrintResults(out, []sshHostResult{r}, false); err != nil {
				return err
			}
		}
		return nil
	}
	results := make([]sshHostResult, 0, len(machines))
	for _, m := range machines {
		if !aggregate {
			fmt.Fprintln(out, m.Name+":")
		}
		r, err := e.sshRunMachine(e.Context, m, command, input, deps)
		if err != nil {
			return err
		}
		if aggregate {
			results = append(results, r)
		} else if err = sshPrintResults(out, []sshHostResult{r}, false); err != nil {
			return err
		}
	}
	if aggregate {
		return sshPrintResults(out, results, true)
	}
	return nil
}

// SSHArgs owns config/port/root routing. The legacy shared ShellJoin allows %,
// while Ruby Shellwords escapes it. Correct only this family's command string.
func sshShellJoin(argv []string) string { return strings.ReplaceAll(ShellJoin(argv), "%", "\\%") }

func sshCommandArgs(m Machine, command []string) ([]string, error) {
	if m.Target.Host == nil {
		return nil, fmt.Errorf("machine %s has no target", m.Name)
	}
	m.Target.Localhost = *m.Target.Host == "localhost"
	if m.Target.Localhost {
		if len(command) == 0 {
			return nil, fmt.Errorf("Cmd requires command argument")
		}
		if len(command) == 1 && command[0] == "" {
			return nil, fmt.Errorf("No command provided")
		}
		return SSHArgs(m, command)
	}
	// A zero-positional invocation is command mode with an empty remote
	// string, not interactive mode and not one empty remote argument.
	argv, err := SSHArgs(m, []string{""})
	if err != nil {
		return nil, err
	}
	argv[len(argv)-1] = sshShellJoin(command)
	return argv, nil
}

func sshInteractiveArgs(m Machine, rootShell func() (string, error)) ([]string, error) {
	if m.Target.Host != nil && *m.Target.Host == "localhost" {
		if rootShell == nil {
			return nil, fmt.Errorf("root passwd shell resolver is not integrated")
		}
		shell, err := rootShell()
		if err != nil {
			return nil, err
		}
		return []string{shell}, nil
	}
	argv, err := sshCommandArgs(m, nil)
	if err != nil {
		return nil, err
	}
	return argv[:len(argv)-1], nil
}

func sshStatus(ctx context.Context, r Result, runErr error) (int, error) {
	if primary := r.capturePrimary(); primary != nil {
		return 0, primary
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if runErr != nil {
		return 0, runErr
	}
	if r.waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(r.waitErr, &exit) {
			return 0, r.waitErr
		}
	}
	if r.signal != 0 {
		return int(r.signal), nil
	}
	if r.ExitCode < 0 {
		return 0, fmt.Errorf("command did not return an exit status")
	}
	return r.ExitCode, nil
}

type sshHostResult struct {
	Name   string
	Status int
	Result Result
}

func (e *Engine) sshRunMachine(ctx context.Context, m Machine, command []string, input sshInput, deps sshDeps) (sshHostResult, error) {
	argv, err := sshCommandArgs(m, command)
	if err != nil {
		return sshHostResult{}, err
	}
	r, err := func() (Result, error) {
		defer e.beginTiming(timingSSH)()
		return deps.Run(ctx, e.Root, argv, input)
	}()
	status, err := sshStatus(ctx, r, err)
	return sshHostResult{Name: m.Name, Status: status, Result: r}, err
}

func (e *Engine) sshParallel(machines []Machine, command []string, input sshInput, deps sshDeps) ([]sshHostResult, error) {
	return e.sshParallelProgress(machines, command, input, deps, nil)
}

func (e *Engine) sshParallelProgress(machines []Machine, command []string, input sshInput, deps sshDeps, advance func(sshHostResult)) ([]sshHostResult, error) {
	ctx, cancel := context.WithCancel(e.Context)
	defer cancel()
	var mu sync.Mutex
	var workers sync.WaitGroup
	results := make([]sshHostResult, 0, len(machines))
	var firstErr error
	for _, m := range machines {
		workers.Add(1)
		go func(m Machine) {
			defer workers.Done()
			r, err := e.sshRunMachine(ctx, m, command, input, deps)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			// Preserve actual completion insertion order. Never sort these rows,
			// transcripts, or logs by machine name to conceal scheduling.
			results = append(results, r)
			if advance != nil {
				advance(r)
			}
		}(m)
	}
	workers.Wait()
	if err := e.Context.Err(); err != nil {
		return nil, err
	}
	return results, firstErr
}

func sshPuts(out io.Writer, b []byte) error {
	_, err := out.Write(b)
	if err == nil && (len(b) == 0 || b[len(b)-1] != '\n') {
		_, err = fmt.Fprintln(out)
	}
	return err
}

func sshPrintResults(out io.Writer, results []sshHostResult, aggregate bool) error {
	if !aggregate {
		for _, r := range results {
			// The caller prints the header before Run (serial) or here after
			// all workers finish (parallel). Failure still prints result.err.
			b := r.Result.Stderr
			if r.Status == 0 {
				b = r.Result.Stdout
			}
			if err := sshPuts(out, b); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		return nil
	}
	type key struct {
		status int
		stdout string
	}
	groups := []key{}
	names := map[key][]string{}
	for _, r := range results {
		k := key{r.Status, string(r.Result.Stdout)}
		if _, ok := names[k]; !ok {
			groups = append(groups, k)
		}
		names[k] = append(names[k], r.Name)
	}
	for _, k := range groups {
		sort.Strings(names[k])
		fmt.Fprintf(out, "%s:\nExit status: %d\n", strings.Join(names[k], ", "), k.status)
		if err := sshPuts(out, []byte(k.stdout)); err != nil {
			return err
		}
		fmt.Fprintln(out)
	}
	return nil
}

// TTY::Command passes an existing filename as a spawn redirection. Its pinned
// convert_to_fd uses a Tempfile containing the literal string when absent.
// Reproduce the descriptor type too; don't silently invent a missing-file error.
func sshStdin(root string, input sshInput) (io.Reader, func(), error) {
	if input.String != nil {
		return strings.NewReader(*input.String), func() {}, nil
	}
	if input.File != nil {
		path := *input.File
		if !filepath.IsAbs(path) {
			// Let the filesystem traverse components: cleaning symlink/..
			// before Open would select a different file.
			path = root + string(os.PathSeparator) + path
		}
		if _, err := os.Stat(path); *input.File != "" && err == nil {
			f, err := os.Open(path)
			if err != nil {
				return nil, nil, err
			}
			return f, func() { _ = f.Close() }, nil
		}
		f, err := os.CreateTemp("", "confctl-ssh-input-")
		if err != nil {
			return nil, nil, err
		}
		close := func() { _ = f.Close(); _ = os.Remove(f.Name()) }
		if _, err = f.WriteString(*input.File); err == nil {
			_, err = f.Seek(0, io.SeekStart)
		}
		if err != nil {
			close()
			return nil, nil, err
		}
		return f, close, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	return r, func() { _ = r.Close(); _ = w.Close() }, nil
}

func (e *Engine) CSSH(inv cli.Invocation, in io.Reader, out io.Writer) error {
	machines, err := e.sshMachines(inv, "No machines to open cssh to")
	if err != nil {
		return err
	}
	if err = e.sshConfirmation(inv, machines, in, out, "Open cssh to the following machines:", ""); err != nil {
		return err
	}
	command := []string{"cssh", "-l", "root"}
	for _, m := range machines {
		command = append(command, *m.Target.Host)
	}
	argv := []string{"nix-shell", "-p", "perlPackages.AppClusterSSH", "--command", strings.Join(command, " ")}
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "shellHook=") {
			env = append(env, value)
		}
	}
	// Nix passes nil; pinned TTY::Command stringifies it to the empty export.
	// Preserve those source bytes. Original-family acceptance remains separate
	// from the fake-executable tests of this adapter and CLI registration.
	env = append(env, "shellHook=")
	_, err = e.sshProcess(e.Context, e.Root, argv, sshInput{}, false, env)
	return err // Preserve the handler's ignored false/nonzero child return.
}
