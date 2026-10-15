package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// This narrow callback is the existing inherited owner, replaced only by
// scripted source tests. It is not a transport or process-supervision API.
type inheritedChildRun func(context.Context, string, []string, *os.File, io.Writer, io.Writer, func(error)) (Result, error)

func pagerShellArgs(runtimeShell, command string) ([]string, error) {
	if !filepath.IsAbs(runtimeShell) {
		return nil, fmt.Errorf("comparison pager runtime shell is not bound to an absolute path")
	}
	return []string{runtimeShell, "-c", command}, nil
}

type pagerChildCompletion struct {
	result Result
	err    error
}

type pagerPipe struct {
	ctx    context.Context
	input  io.WriteCloser
	done   <-chan pagerChildCompletion
	once   sync.Once
	result Result
	err    error
}

func (p *pagerPipe) Write(b []byte) (int, error) { return p.input.Write(b) }

// Close owns only input shutdown and the inherited owner's completion join.
// Completed child code/signal is cached as a result; no second Wait exists.
func (p *pagerPipe) Close() error {
	p.once.Do(func() {
		closeErr := pagerInputCloseError(p.input.Close())
		completed := <-p.done
		p.result = completed.result
		p.err = pagerCompletionError(p.ctx, completed, closeErr, nil)
	})
	return p.err
}

// Only EPIPE on this owned pipe's input close is benign. A joined error with
// another failure must not be discarded just because it also contains EPIPE.
func pagerInputCloseError(err error) error {
	if err == syscall.EPIPE {
		return nil
	}
	if path, ok := err.(*os.PathError); ok && path.Err == syscall.EPIPE {
		return nil
	}
	return err
}

func pagerCompletionError(ctx context.Context, completed pagerChildCompletion, closeErr, startupErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := inheritedChildError(ctx, completed.result, completed.err)
	if startupErr != nil && !errors.Is(err, startupErr) {
		err = errors.Join(startupErr, err)
	}
	return errors.Join(closeErr, err)
}

// The caller supplies the separately package-bound absolute runtime shell.
// These adapters stay private and unwired until the native/package handoff.
func startPagerPipe(ctx context.Context, root, runtimeShell, command string, stdout, stderr *os.File) (*pagerPipe, error) {
	return startPagerPipeWithRunner(ctx, root, runtimeShell, command, stdout, stderr, runInheritedChild)
}

func startPagerPipeWithRunner(ctx context.Context, root, runtimeShell, command string, stdout, stderr io.Writer, run inheritedChildRun) (*pagerPipe, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	argv, err := pagerShellArgs(runtimeShell, command)
	if err != nil {
		return nil, err
	}
	input, output, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	started := make(chan error, 1)
	done := make(chan pagerChildCompletion, 1)
	go func() {
		r, err := run(ctx, root, argv, input, stdout, stderr, func(err error) { started <- err })
		// The child owner borrows its input. This adapter owns this pipe end.
		err = errors.Join(err, input.Close())
		done <- pagerChildCompletion{r, err}
	}()
	startupErr := <-started
	if startupErr == nil {
		startupErr = ctx.Err()
	}
	if startupErr != nil {
		closeErr := pagerInputCloseError(output.Close())
		completed := <-done // startup failure still joins all acquired ownership
		return nil, pagerCompletionError(ctx, completed, closeErr, startupErr)
	}
	return &pagerPipe{ctx: ctx, input: output, done: done}, nil
}

// Backticks' ordinary completed Git status is ignored; stdout is captured,
// stderr and fd0 remain the actual borrowed descriptors. No Engine log call.
func pagerGitConfig(ctx context.Context, root string, stdin, stderr *os.File) (string, error) {
	return pagerGitConfigWithRunner(ctx, root, stdin, stderr, runInheritedChild)
}

func pagerGitConfigWithRunner(ctx context.Context, root string, stdin, stderr *os.File, run inheritedChildRun) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var stdout bytes.Buffer
	r, err := run(ctx, root, []string{"git", "config", "--get-all", "core.pager"}, stdin, &stdout, stderr, nil)
	return stdout.String(), inheritedChildError(ctx, r, err)
}

type pagerCapturedRun func(context.Context, string, []string, []byte) (Result, error)

// Probe reuses the unlogged captured owner with a nonnil empty input: immediate
// EOF, unlike Process's deliberately open nil input. Full captures stay in r.
func pagerProbe(ctx context.Context, root, runtimeShell, command string) (Result, bool, error) {
	return pagerProbeWithRunner(ctx, root, runtimeShell, command, Process)
}

func pagerProbeWithRunner(ctx context.Context, root, runtimeShell, command string, run pagerCapturedRun) (Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1}, false, err
	}
	argv, err := pagerShellArgs(runtimeShell, command)
	if err != nil {
		return Result{ExitCode: -1}, false, err
	}
	r, err := run(ctx, root, argv, []byte{})
	if err = inheritedChildError(ctx, r, err); err != nil {
		return r, false, err
	}
	return r, r.ExitCode == 0 && r.signal == 0 && len(r.Stderr) == 0, nil
}
