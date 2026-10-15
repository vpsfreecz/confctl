package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

type repository struct{ root, admin, scope string }
type selection struct {
	Present bool
	Value   string
}
type installation struct {
	Root, Admin, Scope, Hooks string
	Previous                  selection
	Wrappers                  map[string]string
}

func git(ctx context.Context, q hookcheck.TrustRequest, args ...string) (string, error) {
	r, err := q.Capture.Invoke(ctx, hookcheck.Invocation{Program: q.Tools.Git.Program, Args: args, Dir: q.Capture.Dir, Env: q.Capture.Env})
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("git %s failed: %s", args[0], r.Stderr)
	}
	return strings.TrimSuffix(string(r.Stdout), "\n"), nil
}

func repo(ctx context.Context, q hookcheck.TrustRequest, forInstall bool) (repository, error) {
	root, err := git(ctx, q, "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, err
	}
	admin, err := git(ctx, q, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return repository{}, err
	}
	common, err := git(ctx, q, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return repository{}, err
	}
	scope := "--local"
	// The primary checkout shares --local with every linked worktree too.
	// Only the common repository's actual extension enables --worktree writes.
	r, err := q.Capture.Invoke(ctx, hookcheck.Invocation{Program: q.Tools.Git.Program, Args: []string{"config", "--local", "--bool", "--get", "extensions.worktreeConfig"}, Dir: q.Capture.Dir, Env: q.Capture.Env})
	if err != nil {
		return repository{}, err
	}
	if r.ExitCode == 0 && string(r.Stdout) == "true\n" {
		scope = "--worktree"
	} else {
		if !(r.ExitCode == 1 && len(r.Stdout) == 0 || r.ExitCode == 0 && string(r.Stdout) == "false\n") {
			return repository{}, errors.New("cannot read repository extensions.worktreeConfig")
		}
		shared := admin != common
		if forInstall && !shared {
			listing, err := git(ctx, q, "worktree", "list", "--porcelain", "-z")
			if err != nil {
				return repository{}, err
			}
			count := 0
			for _, field := range strings.Split(listing, "\x00") {
				if strings.HasPrefix(field, "worktree ") {
					count++
				}
			}
			if count == 0 {
				return repository{}, errors.New("cannot identify repository worktrees")
			}
			shared = count > 1
		}
		if forInstall && shared {
			return repository{}, errors.New("linked-worktree hook selection requires coordinated extensions.worktreeConfig setup")
		}
	}
	return repository{root, admin, scope}, nil
}

func privateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	i, err := os.Lstat(path)
	if err != nil {
		return err
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.IsDir() || i.Mode().Perm() != 0700 || s.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("not an owned private hook directory: %q", path)
	}
	return nil
}

func ownedFile(path string) ([]byte, error) {
	i, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.Mode().IsRegular() || s.Uid != uint32(os.Geteuid()) || s.Nlink != 1 {
		return nil, fmt.Errorf("not an owned regular hook file: %q", path)
	}
	return os.ReadFile(path)
}

func writeOwned(path string, data []byte, mode os.FileMode) error {
	if _, err := ownedFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path+".new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".new")
	if err := f.Chmod(mode); err != nil {
		return errors.Join(err, f.Close())
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}

func preparePolicy(ctx context.Context, q hookcheck.TrustRequest, b bindings) (string, error) {
	r, err := repo(ctx, q, false)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(r.admin, "confctl-hooks")
	if err = privateDirectory(dir); err != nil {
		return "", err
	}
	policy, err := hookcheck.FixedConfig(b.tools.Checker.Program)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "lefthook.yml")
	return path, writeOwned(path, policy, 0644)
}

func ownSelection(ctx context.Context, q hookcheck.TrustRequest, scope string) (selection, error) {
	r, err := q.Capture.Invoke(ctx, hookcheck.Invocation{Program: q.Tools.Git.Program, Args: []string{"config", scope, "--null", "--get-all", "core.hooksPath"}, Dir: q.Capture.Dir, Env: q.Capture.Env})
	if err != nil {
		return selection{}, err
	}
	if r.ExitCode == 1 && len(r.Stdout) == 0 {
		return selection{}, nil
	}
	if r.ExitCode != 0 || bytes.Count(r.Stdout, []byte{0}) != 1 {
		return selection{}, errors.New("ambiguous own hook selection")
	}
	return selection{true, strings.TrimSuffix(string(r.Stdout), "\x00")}, nil
}

var hookNames = []string{"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge", "pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update", "reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate", "fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist", "p4-post-changelist", "p4-pre-submit"}

func checkPredecessor(ctx context.Context, q hookcheck.TrustRequest) error {
	dir, err := git(ctx, q, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return err
	}
	for _, name := range hookNames {
		if name == "pre-commit" || name == "commit-msg" {
			continue
		}
		i, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if i.Mode().Perm()&0111 != 0 {
			return fmt.Errorf("active predecessor hook needs an explicit migration: %s", name)
		}
	}
	return nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func installationBytes(state installation) []byte {
	b, _ := json.MarshalIndent(state, "", "  ")
	return append(b, '\n')
}

func readInstallation(path string) (installation, error) {
	var state installation
	b, err := ownedFile(path)
	if err != nil {
		return state, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&state); err != nil {
		return state, err
	}
	if !bytes.Equal(b, installationBytes(state)) || len(state.Wrappers) != 2 || state.Wrappers["pre-commit"] == "" || state.Wrappers["commit-msg"] == "" {
		return state, errors.New("invalid own hook installation record")
	}
	return state, nil
}

func checkInstallation(state installation, r repository, current selection) error {
	if state.Root != r.root || state.Admin != r.admin || state.Scope != r.scope || state.Hooks != filepath.Join(r.admin, "confctl-hooks", "hooks") || !current.Present || current.Value != state.Hooks {
		return errors.New("own hook installation identity or selection changed")
	}
	entries, err := os.ReadDir(state.Hooks)
	if err != nil || len(entries) != 2 {
		return errors.New("own hook directory membership changed")
	}
	for name, expected := range state.Wrappers {
		b, err := ownedFile(filepath.Join(state.Hooks, name))
		i, statErr := os.Lstat(filepath.Join(state.Hooks, name))
		if err != nil || string(b) != expected || statErr != nil || i.Mode().Perm() != 0755 {
			return fmt.Errorf("own hook wrapper changed: %s", name)
		}
	}
	return nil
}

func installHooks(ctx context.Context, q hookcheck.TrustRequest, b bindings) error {
	r, err := repo(ctx, q, true)
	if err != nil {
		return err
	}
	current, err := ownSelection(ctx, q, r.scope)
	if err != nil {
		return err
	}
	dir := filepath.Join(r.admin, "confctl-hooks")
	record := filepath.Join(dir, "install.json")
	old, err := readInstallation(record)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	previous := current
	if err == nil {
		if err := checkInstallation(old, r, current); err != nil {
			return err
		}
		previous = old.Previous
	} else {
		if current.Present {
			return errors.New("existing own core.hooksPath needs an explicit migration")
		}
		if err := checkPredecessor(ctx, q); err != nil {
			return err
		}
	}
	if err := privateDirectory(dir); err != nil {
		return err
	}
	hooks := filepath.Join(dir, "hooks")
	if err := privateDirectory(hooks); err != nil {
		return err
	}
	if old.Wrappers == nil {
		entries, err := os.ReadDir(hooks)
		if err != nil || len(entries) != 0 {
			return errors.New("foreign own hook directory")
		}
	}
	state := installation{Root: r.root, Admin: r.admin, Scope: r.scope, Hooks: hooks, Previous: previous, Wrappers: map[string]string{}}
	// Keep the previous recognized installation usable after an ordinary write
	// failure. Only files successfully replaced by this invocation are restored.
	var changed []string
	recordWritten := false
	rollback := func(primary error) error {
		var cleanup []error
		for _, phase := range changed {
			path := filepath.Join(hooks, phase)
			if old.Wrappers != nil {
				cleanup = append(cleanup, writeOwned(path, []byte(old.Wrappers[phase]), 0755))
			} else {
				cleanup = append(cleanup, os.Remove(path))
			}
		}
		if recordWritten {
			if old.Wrappers != nil {
				cleanup = append(cleanup, writeOwned(record, installationBytes(old), 0600))
			} else {
				cleanup = append(cleanup, os.Remove(record))
			}
		}
		return errors.Join(append([]error{primary}, cleanup...)...)
	}
	for _, phase := range []string{"pre-commit", "commit-msg"} {
		state.Wrappers[phase] = "#!" + b.tools.ChildBinding.RunnerShell.Program + "\nexec " + quote(b.launcher) + " run " + phase + " \"$@\"\n"
		if err := writeOwned(filepath.Join(hooks, phase), []byte(state.Wrappers[phase]), 0755); err != nil {
			return rollback(err)
		}
		changed = append(changed, phase)
	}
	if err := writeOwned(record, installationBytes(state), 0600); err != nil {
		return rollback(err)
	}
	recordWritten = true
	_, err = git(ctx, q, "config", r.scope, "core.hooksPath", hooks)
	if err != nil {
		// An ambiguous config result must leave the complete new wrappers/record
		// intact. Roll back only when the old selection is still observed.
		observed, readErr := ownSelection(ctx, q, r.scope)
		if readErr == nil && observed == current {
			return rollback(err)
		}
		return errors.Join(err, readErr)
	}
	return err
}

func restoreHooks(ctx context.Context, q hookcheck.TrustRequest) error {
	r, err := repo(ctx, q, true)
	if err != nil {
		return err
	}
	record := filepath.Join(r.admin, "confctl-hooks", "install.json")
	state, err := readInstallation(record)
	if err != nil {
		return err
	}
	current, err := ownSelection(ctx, q, r.scope)
	if err != nil {
		return err
	}
	if err := checkInstallation(state, r, current); err != nil {
		return err
	}
	if state.Previous.Present {
		_, err = git(ctx, q, "config", r.scope, "core.hooksPath", state.Previous.Value)
	} else {
		_, err = git(ctx, q, "config", r.scope, "--unset", "core.hooksPath")
	}
	if err != nil {
		return err
	}
	for name := range state.Wrappers {
		if err := os.Remove(filepath.Join(state.Hooks, name)); err != nil {
			return err
		}
	}
	return errors.Join(os.Remove(state.Hooks), os.Remove(record))
}
