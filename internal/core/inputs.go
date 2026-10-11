package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	inputgit "github.com/vpsfreecz/confctl/internal/git"
	"github.com/vpsfreecz/confctl/internal/inputs"
)

type inputsArgumentError struct{ message string }

func (e *inputsArgumentError) Error() string { return e.message }

// Keep ordered channel JSON at this adapter boundary. Existing evaluator
// settings, impure policy and Nix fallback remain the authority for argv.
func (n *evaluator) inputJSON(installable string) ([]byte, error) {
	if err := n.argsSettings(); err != nil {
		return nil, err
	}
	return n.e.nix("eval", []string{installable}, nested(n.settings, "nix", "impureEval") == true, n.e.ShowTrace, n.max)
}

func (e *Engine) inputResolver() (*inputs.MachineResolver, error) {
	n, err := e.evaluator()
	if err != nil {
		return nil, err
	}
	return &inputs.MachineResolver{Eval: n.inputJSON}, nil
}

func (e *Engine) ListInputs(channel bool, args []string) (string, error) {
	// The source checks the flake before channel arity and before lock reads.
	if err := e.required(); err != nil {
		return "", err
	}
	if channel && len(args) > 1 {
		return "", &inputsArgumentError{"usage: confctl inputs channel ls [channel-pattern]"}
	}
	lock, err := inputs.LoadLock(filepath.Join(e.Root, "flake.lock"))
	if err != nil {
		return "", inputLockError(err, filepath.Join(e.Root, "flake.lock"))
	}
	var pattern *string
	if len(args) > 0 {
		pattern = &args[0]
	}
	rows := []map[string]any{}
	if !channel {
		names, err := lock.RootInputs()
		if err != nil {
			return "", err
		}
		for _, name := range names {
			info, err := lock.InputInfo(name)
			if err != nil {
				return "", err
			}
			if pattern != nil && !Match(*pattern, name) {
				continue
			}
			rows = append(rows, map[string]any{"input": name, "type": info.Type, "ref": info.Ref, "rev": info.ShortRev, "url": info.URL})
		}
		return Table(rows, []string{"input", "type", "ref", "rev", "url"}, true), nil
	}
	n, err := e.evaluator()
	if err != nil {
		return "", err
	}
	b, err := n.inputJSON(".#confctl.channels")
	if err != nil {
		return "", err
	}
	channels, err := inputs.DecodeChannels(b)
	if err != nil {
		return "", err
	}
	selected := inputs.SelectChannels(channels, pattern, Match)
	for _, target := range inputs.ChannelTargets(selected, nil) {
		info, err := lock.InputInfo(target.Input)
		if err != nil {
			return "", err
		}
		rows = append(rows, map[string]any{"channel": target.Channel, "role": target.Role, "input": target.Input, "rev": info.ShortRev, "url": info.URL})
	}
	return Table(rows, []string{"channel", "role", "input", "rev", "url"}, true), nil
}

func inputLockError(err error, path string) error {
	var fileError *os.PathError
	if errors.As(err, &fileError) && fileError.Op == "open" {
		return configurationError(err, "rb_sysopen", path)
	}
	return err
}

// Updater/SystemCommand changes CWD outside the command, unlike evaluation's
// explicit `cd ... && nix ...`. Keep this log start scoped to these operations.
func (e *Engine) inputCommand(ctx context.Context, root string, argv []string) (inputs.Output, error) {
	id, start := logID(), time.Now()
	e.logText("[" + id + "] Running " + ShellJoin(argv) + "\n")
	r, err := Process(ctx, root, argv, nil)
	e.processLogFinish(id, start, r)
	out := inputs.Output{Stdout: string(r.Stdout), Stderr: string(r.Stderr)}
	if err != nil {
		return out, err
	}
	if r.ExitCode != 0 {
		return out, &inputs.ExitError{Command: r.Command, Code: r.ExitCode, Output: out}
	}
	return out, nil
}

func (e *Engine) inputUpdateDeps() inputs.UpdateDeps {
	return inputs.UpdateDeps{
		Run: e.inputCommand, Argument: str,
		Load: func(path string, optional bool) (*inputs.Lock, error) {
			var lock *inputs.Lock
			var err error
			if optional {
				lock, err = inputs.LoadOptionalLock(path)
			} else {
				lock, err = inputs.LoadLock(path)
			}
			return lock, inputLockError(err, path)
		},
		Changelog: func(ctx context.Context, url, from, to string) (string, error) {
			cache, err := inputMirrorCache()
			if err != nil {
				return "", err
			}
			mirror := inputgit.Mirror{Root: e.Root, Cache: cache, URL: url, Run: func(ctx context.Context, root string, argv []string) (string, error) {
				out, err := e.inputCommand(ctx, root, argv)
				return out.Stdout, err
			}}
			if err := mirror.Setup(ctx); err != nil {
				var fileError *os.PathError
				if errors.As(err, &fileError) {
					switch fileError.Op {
					case "stat":
						return "", configurationError(err, "rb_file_s_stat", mirror.Path())
					case "mkdir":
						return "", configurationError(err, "dir_s_mkdir", fileError.Path)
					}
				}
				return "", err
			}
			return mirror.Log(ctx, from, to)
		},
		Commit: func(ctx context.Context, root, message string, editor bool) error {
			return inputgit.Commit(ctx, root, message, editor, commitStdoutTTY(os.Stdout), func(ctx context.Context, root string, argv []string) (int, error) {
				r, err := runInheritedCommit(ctx, root, argv, os.Stdin, os.Stdout, os.Stderr)
				return r.ExitCode, err
			})
		},
	}
}

// Ruby File.join preserves interior separators and gives an empty first
// component a leading slash. A present empty XDG value never selects HOME.
func inputMirrorCache() (string, error) {
	cache, present := os.LookupEnv("XDG_CACHE_HOME")
	if !present {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cache = strings.TrimRight(home, "/") + "/.cache"
	}
	return strings.TrimRight(cache, "/") + "/confctl", nil
}

func inputUpdateArgs(args []string, required []string, optional int) error {
	if len(args) < len(required) {
		return &inputsArgumentError{"missing argument <" + required[len(args)] + ">"}
	}
	if len(args) > len(required)+optional {
		extra := args[len(required)+optional:]
		message := "unknown argument: "
		if len(extra) > 1 {
			message = "unknown arguments: "
		}
		message += strings.Join(extra, " ")
		for _, arg := range extra {
			if strings.HasPrefix(arg, "-") {
				message += " (note that options must come before arguments)"
				break
			}
		}
		return &inputsArgumentError{message}
	}
	return nil
}

func inputShort(value any) string {
	if value == nil || value == false {
		return "-"
	}
	return str(value)
}

func (e *Engine) UpdateInputs(inv cli.Invocation) (string, error) {
	if err := e.required(); err != nil {
		return "", err
	}
	args, path := inv.Args, inv.Command.Path
	var names []any
	var targets []inputs.Target
	if len(path) == 2 {
		if inv.Options["all"].Value.Bool {
			lock, err := inputs.LoadOptionalLock(filepath.Join(e.Root, "flake.lock"))
			if err != nil {
				return "", inputLockError(err, filepath.Join(e.Root, "flake.lock"))
			}
			if lock == nil {
				return "", errors.New("flake.lock missing; specify inputs explicitly")
			}
			selected, err := lock.RootInputs()
			if err != nil {
				return "", err
			}
			for _, name := range selected {
				names = append(names, name)
			}
		} else {
			if len(args) == 0 {
				return "", &inputsArgumentError{"missing input name (or pass --all)"}
			}
			for _, name := range args {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return "", errors.New("no inputs selected")
		}
	} else if path[1] == "channel" {
		if err := inputUpdateArgs(args, []string{"channels"}, 1); err != nil {
			return "", err
		}
		n, err := e.evaluator()
		if err != nil {
			return "", err
		}
		b, err := n.inputJSON(".#confctl.channels")
		if err != nil {
			return "", err
		}
		channels, err := inputs.DecodeChannels(b)
		if err != nil {
			return "", err
		}
		selected := inputs.SelectChannels(channels, &args[0], Match)
		if len(selected) == 0 {
			return "", fmt.Errorf("no channels matched '%s'", args[0])
		}
		var role *string
		if len(args) > 1 {
			role = &args[1]
		}
		targets = inputs.ChannelTargets(selected, role)
		names = inputs.SelectedInputs(targets)
		if len(names) == 0 {
			return "", errors.New("no inputs selected (check role name?)")
		}
	} else {
		if err := inputUpdateArgs(args, []string{"machine", "role"}, 0); err != nil {
			return "", err
		}
		resolver, err := e.inputResolver()
		if err != nil {
			return "", err
		}
		name, err := resolver.Resolve(args[0], args[1])
		if err != nil {
			return "", err
		}
		names = []any{name}
	}
	boolean := func(key string) bool { return inv.Options[key].Value.Bool }
	opts := inputs.UpdateOptions{Commit: boolean("commit"), Changelog: boolean("changelog"), Editor: boolean("editor"), Downgrade: boolean("downgrade")}
	result, err := inputs.Update(e.Context, e.Root, names, opts, e.inputUpdateDeps())
	if err != nil {
		return "", err
	}
	var text strings.Builder
	if len(path) == 2 {
		if len(result.Changes) == 0 {
			return "No changes.\n", nil
		}
		fmt.Fprintf(&text, "Updated inputs (%d):\n", len(result.Changes))
		for _, change := range result.Changes {
			fmt.Fprintf(&text, "  %s: %s -> %s\n", change.Name, inputShort(change.Old.ShortRev), inputShort(change.New.ShortRev))
		}
	} else {
		lock, err := inputs.LoadLock(filepath.Join(e.Root, "flake.lock"))
		if err != nil {
			return "", inputLockError(err, filepath.Join(e.Root, "flake.lock"))
		}
		if path[1] == "machine" {
			targets = []inputs.Target{{Channel: args[0], Role: args[1], Input: names[0]}}
		}
		for _, target := range targets {
			info, err := lock.InputInfo(target.Input)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&text, "Updating %s in %s -> %s\n", target.Role, target.Channel, inputShort(info.ShortRev))
		}
	}
	return text.String(), nil
}
