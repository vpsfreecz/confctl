// Package git owns the inputs workflow's literal Git invocation boundaries.
package git

import (
	"context"
	"errors"
)

type InheritedRunner func(context.Context, string, []string) (int, error)

// CommitAdapterError identifies commit-only terminal/relay failures. Ordinary
// Git spawn and exit failures retain the source's "git commit failed" message.
type CommitAdapterError struct{ Err error }

func (e *CommitAdapterError) Error() string { return e.Err.Error() }
func (e *CommitAdapterError) Unwrap() error { return e.Err }

func Commit(ctx context.Context, root, message string, editor, stdoutTTY bool, run InheritedRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	argv := []string{"git", "commit"}
	if editor && stdoutTTY {
		argv = append(argv, "-e")
	}
	argv = append(argv, "-m", message, "flake.lock")
	code, err := run(ctx, root, argv)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var adapter *CommitAdapterError
	if errors.As(err, &adapter) {
		return err
	}
	if err != nil || code != 0 {
		return errors.New("git commit failed")
	}
	return nil
}
