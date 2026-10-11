package inputs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Output struct{ Stdout, Stderr string }

// ExitError is a completed captured command's failure, distinct from start or
// invocation cancellation errors. Its presentation is pinned tty-command 0.10.1.
type ExitError struct {
	Command string
	Code    int
	Output
}

func (e *ExitError) Error() string {
	extract := func(s string) string {
		s = strings.Trim(s, "\x00\t\n\v\f\r ")
		if s == "" {
			return "Nothing written"
		}
		return s
	}
	return fmt.Sprintf("Running `%s` failed with\n  exit status: %d\n  stdout: %s\n  stderr: %s\n", e.Command, e.Code, extract(e.Stdout), extract(e.Stderr))
}

type UpdateOptions struct{ Commit, Changelog, Editor, Downgrade bool }
type UpdateResult struct{ Changes []Change }
type Changelog func(context.Context, string, string, string) (string, error)
type UpdateDeps struct {
	Run func(context.Context, string, []string) (Output, error)
	// Argument preserves Ruby to_s at the literal process boundary, while lock
	// lookups keep the original selected JSON value (including nil/false).
	Argument  func(any) string
	Load      func(string, bool) (*Lock, error)
	Changelog Changelog
	Commit    func(context.Context, string, string, bool) error
}

func LoadOptionalLock(path string) (*Lock, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	return LoadLock(path)
}

var cachedGitHub = regexp.MustCompile(`(?s)unable to download 'https://api\.github\.com/.*using cached version`)

func CheckUpdateOutput(out Output) error {
	text := out.Stdout + "\n" + out.Stderr
	if !cachedGitHub.MatchString(text) {
		return nil
	}
	if strings.Contains(strings.ToLower(text), "rate limit exceeded") {
		return errors.New("Nix could not refresh GitHub input metadata because the GitHub API rate limit was exceeded.\nIt used a cached version instead, so the selected input may not be the latest revision.\nConfigure a GitHub token in Nix access-tokens or retry later.")
	}
	return errors.New("Nix could not refresh GitHub input metadata and used a cached version instead.\nThe selected input may not be the latest revision; retry after the GitHub API is reachable.")
}

func Update(ctx context.Context, root string, names []any, opts UpdateOptions, deps UpdateDeps) (UpdateResult, error) {
	var result UpdateResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(names) == 0 {
		return result, errors.New("inputs empty")
	}
	path := filepath.Join(root, "flake.lock")
	old, err := deps.Load(path, true)
	if err != nil {
		return result, err
	}
	for extra := false; ; extra = true {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		argv := []string{"nix"}
		if extra {
			argv = append(argv, "--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes")
		}
		argv = append(argv, "flake", "update")
		for _, name := range names {
			argv = append(argv, deps.Argument(name))
		}
		var out Output
		out, err = deps.Run(ctx, root, argv)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err == nil {
			err = CheckUpdateOutput(out)
			break
		}
		var exit *ExitError
		if !extra && errors.As(err, &exit) {
			message := strings.ToLower(exit.Error())
			if strings.Contains(message, "experimental") && (strings.Contains(message, "nix-command") || strings.Contains(message, "flakes")) {
				continue
			}
		}
		return result, err
	}
	if err != nil {
		return result, err
	}
	next, err := deps.Load(path, false)
	if err != nil {
		return result, err
	}
	result.Changes, err = DiffLocks(old, next, names)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil || len(result.Changes) == 0 {
		return result, err
	}
	if opts.Commit {
		var message string
		message, err = BuildCommitMessage(ctx, result.Changes, opts.Changelog, opts.Downgrade, deps.Changelog)
		if err != nil {
			return result, err
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		err = deps.Commit(ctx, root, message, opts.Editor)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}
