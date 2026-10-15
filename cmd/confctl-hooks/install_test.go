package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

func installerFixture(t *testing.T) (hookcheck.TrustRequest, bindings) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository with space\nand LF")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") && !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	env = append(env, "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	q := hookcheck.TrustRequest{Capture: hookcheck.CaptureOptions{Dir: root, Git: binary, Env: env, Invoke: invoke}, Tools: hookcheck.ToolSet{Git: hookcheck.ToolIdentity{Program: binary}}}
	runGit(t, q, "init", "--template=")
	runGit(t, q, "config", "user.name", "Fixture")
	runGit(t, q, "config", "user.email", "fixture@example.test")
	shell := hookcheck.ToolIdentity{Program: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bash/bin/bash"}
	b := bindings{launcher: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-tools/bin/confctl-hooks", tools: hookcheck.ToolSet{Checker: hookcheck.ToolIdentity{Program: "/nix/store/cccccccccccccccccccccccccccccccc-checker/bin/confctl-hooks"}, ChildBinding: hookcheck.ChildBinding{RunnerShell: &shell}}}
	return q, b
}

func runGit(t *testing.T, q hookcheck.TrustRequest, args ...string) string {
	t.Helper()
	s, err := git(context.Background(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPrepareInstallUpdateRestoreOwnHooks(t *testing.T) {
	q, b := installerFixture(t)
	ctx := context.Background()
	path, err := preparePolicy(ctx, q, b)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hookcheck.FixedConfig(b.tools.Checker.Program)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("candidate: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(q.Capture.Dir, "lefthook.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prepare wrote working policy", err)
	}
	if err := installHooks(ctx, q, b); err != nil {
		t.Fatal(err)
	}
	r, _ := repo(ctx, q, true)
	hooks := filepath.Join(r.admin, "confctl-hooks", "hooks")
	current, err := ownSelection(ctx, q, r.scope)
	if err != nil || current.Value != hooks {
		t.Fatal(current, err)
	}
	before, err := os.ReadFile(filepath.Join(hooks, "commit-msg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), " run commit-msg \"$@\"\n") {
		t.Fatalf("wrapper lost literal arguments: %q", before)
	}
	b.launcher = "/nix/store/dddddddddddddddddddddddddddddddd-new-tools/bin/confctl-hooks"
	if err := installHooks(ctx, q, b); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(hooks, "commit-msg"))
	if bytes.Equal(before, after) {
		t.Fatal("update did not replace launcher")
	}
	if err := restoreHooks(ctx, q); err != nil {
		t.Fatal(err)
	}
	current, err = ownSelection(ctx, q, r.scope)
	if err != nil || current.Present {
		t.Fatal(current, err)
	}
	if _, err := os.Lstat(hooks); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrappers retained", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("restore removed candidate", err)
	}
}

func TestInstallRefusesPredecessorAndForeignFiles(t *testing.T) {
	for _, kind := range []string{"own-selection", "extra-hook", "foreign-wrapper", "symlink-directory"} {
		t.Run(kind, func(t *testing.T) {
			q, b := installerFixture(t)
			r, _ := repo(context.Background(), q, true)
			dir := filepath.Join(r.admin, "confctl-hooks")
			switch kind {
			case "own-selection":
				runGit(t, q, "config", "core.hooksPath", "other hooks")
			case "extra-hook":
				if err := os.MkdirAll(filepath.Join(r.admin, "hooks"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(r.admin, "hooks", "post-checkout"), []byte("predecessor"), 0755); err != nil {
					t.Fatal(err)
				}
			case "foreign-wrapper":
				if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "hooks", "foreign"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := installHooks(context.Background(), q, b); err == nil {
				t.Fatal("accepted unrelated predecessor state")
			}
			current, _ := ownSelection(context.Background(), q, r.scope)
			if current.Present && kind != "own-selection" {
				t.Fatal("changed hook selection", current)
			}
		})
	}
}

func TestInstallWriteFailurePreservesPreviousInstallation(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "update"}[update], func(t *testing.T) {
			q, b := installerFixture(t)
			ctx := context.Background()
			r, _ := repo(ctx, q, true)
			dir := filepath.Join(r.admin, "confctl-hooks")
			hooks := filepath.Join(dir, "hooks")
			if update {
				if err := installHooks(ctx, q, b); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(hooks, 0700); err != nil {
					t.Fatal(err)
				}
			}
			old, oldErr := os.ReadFile(filepath.Join(hooks, "pre-commit"))
			sentinel := filepath.Join(r.admin, "config.lock")
			if err := os.WriteFile(sentinel, []byte("unrelated in-progress file"), 0600); err != nil {
				t.Fatal(err)
			}
			b.launcher += "-changed"
			if err := installHooks(ctx, q, b); err == nil {
				t.Fatal("write conflict accepted")
			}
			now, err := os.ReadFile(filepath.Join(hooks, "pre-commit"))
			if update && (err != nil || !bytes.Equal(now, old)) || !update && (!errors.Is(oldErr, os.ErrNotExist) || !errors.Is(err, os.ErrNotExist)) {
				t.Fatalf("partial replacement: %q %v", now, err)
			}
			got, _ := os.ReadFile(sentinel)
			if string(got) != "unrelated in-progress file" {
				t.Fatal("foreign temporary changed")
			}
		})
	}
}

func TestLinkedInstallRequiresOptInAndPreservesCommonConfig(t *testing.T) {
	q, b := installerFixture(t)
	runGit(t, q, "commit", "--allow-empty", "-m", "Fixture")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, q, "worktree", "add", "--detach", linked)
	other := q
	other.Capture.Dir = linked
	if err := installHooks(context.Background(), other, b); err == nil {
		t.Fatal("enabled shared setting implicitly")
	}
	runGit(t, q, "config", "extensions.worktreeConfig", "true")
	common := filepath.Join(q.Capture.Dir, ".git", "config")
	before, _ := os.ReadFile(common)
	if err := installHooks(context.Background(), other, b); err != nil {
		t.Fatal(err)
	}
	if err := restoreHooks(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(common)
	if !bytes.Equal(before, after) {
		t.Fatal("common config changed")
	}
}

func TestRestoreRefusesChangedWrapper(t *testing.T) {
	q, b := installerFixture(t)
	ctx := context.Background()
	if err := installHooks(ctx, q, b); err != nil {
		t.Fatal(err)
	}
	r, _ := repo(ctx, q, true)
	path := filepath.Join(r.admin, "confctl-hooks", "hooks", "pre-commit")
	if err := os.WriteFile(path, []byte("operator changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := restoreHooks(ctx, q); err == nil {
		t.Fatal("removed changed hook")
	}
	current, _ := ownSelection(ctx, q, r.scope)
	if !current.Present {
		t.Fatal("selection changed")
	}
}

func TestWrapperModesIgnoreRestrictiveUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrapper")
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	if err := writeOwned(path, []byte("wrapper"), 0755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal(info, err)
	}
}

func TestPrimaryInstallUsesOwnConfigAndPreservesSibling(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		linked, enabled, globalOnly bool
	}{
		{name: "single-enabled", enabled: true},
		{name: "linked-enabled", linked: true, enabled: true},
		{name: "linked-absent", linked: true},
		{name: "linked-global-only", linked: true, globalOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, b := installerFixture(t)
			ctx := context.Background()
			runGit(t, q, "commit", "--allow-empty", "-m", "Fixture")
			sibling := q
			if tc.linked {
				sibling.Capture.Dir = filepath.Join(t.TempDir(), "sibling")
				runGit(t, q, "worktree", "add", "--detach", sibling.Capture.Dir)
			}
			if tc.enabled {
				runGit(t, q, "config", "--local", "extensions.worktreeConfig", "true")
				runGit(t, q, "config", "--worktree", "fixture.primary", "keep")
				if tc.linked {
					runGit(t, sibling, "config", "--worktree", "fixture.sibling", "keep")
				}
			}
			if tc.globalOnly {
				global := filepath.Join(t.TempDir(), "global-config")
				if err := os.WriteFile(global, []byte("[extensions]\nworktreeConfig = true\n"), 0600); err != nil {
					t.Fatal(err)
				}
				for i, e := range q.Capture.Env {
					if strings.HasPrefix(e, "GIT_CONFIG_GLOBAL=") {
						q.Capture.Env[i] = "GIT_CONFIG_GLOBAL=" + global
					}
				}
				sibling.Capture.Env = append([]string(nil), q.Capture.Env...)
			}
			common := filepath.Join(q.Capture.Dir, ".git", "config")
			before, err := os.ReadFile(common)
			if err != nil {
				t.Fatal(err)
			}
			siblingHooks := ""
			siblingConfig := ""
			siblingFile := ""
			var siblingBytes []byte
			if tc.linked {
				siblingHooks = runGit(t, sibling, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
				siblingConfig = runGit(t, sibling, "config", "--null", "--list", "--show-origin", "--show-scope")
				siblingFile = filepath.Join(runGit(t, sibling, "rev-parse", "--absolute-git-dir"), "config.worktree")
				siblingBytes, err = os.ReadFile(siblingFile)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			assertUnaffected := func() {
				t.Helper()
				after, err := os.ReadFile(common)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("primary changed common config", err)
				}
				if !tc.linked {
					return
				}
				if got := runGit(t, sibling, "rev-parse", "--path-format=absolute", "--git-path", "hooks"); got != siblingHooks {
					t.Fatalf("sibling effective hooks changed: %q -> %q", siblingHooks, got)
				}
				if got := runGit(t, sibling, "config", "--null", "--list", "--show-origin", "--show-scope"); got != siblingConfig {
					t.Fatal("sibling effective config changed")
				}
				after, err = os.ReadFile(siblingFile)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if !bytes.Equal(siblingBytes, after) || (siblingBytes == nil) != errors.Is(err, os.ErrNotExist) {
					t.Fatal("sibling own config changed")
				}
			}
			err = installHooks(ctx, q, b)
			if !tc.enabled {
				if err == nil || !strings.Contains(err.Error(), "coordinated extensions.worktreeConfig") {
					t.Fatal("primary did not require coordinated opt-in", err)
				}
				assertUnaffected()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			primary, err := repo(ctx, q, true)
			if err != nil || primary.scope != "--worktree" {
				t.Fatal("primary chose shared config", primary, err)
			}
			selected := runGit(t, q, "config", "--worktree", "--get", "core.hooksPath")
			if selected != filepath.Join(primary.admin, "confctl-hooks", "hooks") {
				t.Fatal("primary own hook selection missing", selected)
			}
			assertUnaffected()
			if err := restoreHooks(ctx, q); err != nil {
				t.Fatal(err)
			}
			if selection, err := ownSelection(ctx, q, "--worktree"); err != nil || selection.Present {
				t.Fatal("primary restore retained hook selection", selection, err)
			}
			if got := runGit(t, q, "config", "--worktree", "--get", "fixture.primary"); got != "keep" {
				t.Fatal("restore changed unrelated primary config")
			}
			assertUnaffected()
		})
	}
}
