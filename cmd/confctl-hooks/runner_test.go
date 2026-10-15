package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

func runPreparedHookFixture(t *testing.T, ctx context.Context, q hookcheck.HookRequest, fs hookcheck.TrustIO, child hookcheck.Invoke) (hookcheck.HookOutcome, error) {
	t.Helper()
	return hookcheck.RunHook(ctx, q, fs, func(ctx context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
		if req.Program == q.Tools.Git.Program {
			if !reflect.DeepEqual(req.Args, []string{"init", "--quiet", "--template=", "--initial-branch=confctl-hook-runner", "."}) {
				t.Fatal("unexpected owned Git command", req)
			}
			return invoke(ctx, req)
		}
		return child(ctx, req)
	})
}

func preparedSnapshotPath(t *testing.T, input hookcheck.PreparedInput) string {
	t.Helper()
	b, err := os.ReadFile(input.ContextPath)
	if err != nil {
		t.Fatal(err)
	}
	var p struct{ Snapshot []byte }
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return string(p.Snapshot)
}

// This opt-in test uses already realized immutable tools. Its real Go semantic
// check has fresh caches, so the lead selects it through a verification watcher.
func TestPinnedLefthookRunnerUsesCapturedSnapshot(t *testing.T) {
	path := os.Getenv("CONFCTL_HOOK_RUNNER_TOOLS")
	if path == "" {
		t.Skip("pinned Git/Lefthook runner check not selected")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spec bindingSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	var tools hookcheck.ToolSet
	for _, pair := range []struct {
		spec toolSpec
		to   *hookcheck.ToolIdentity
	}{{spec.Checker, &tools.Checker}, {spec.Git, &tools.Git}, {spec.Lefthook, &tools.Lefthook}, {spec.Nixfmt, &tools.Nixfmt}, {spec.Gofmt, &tools.Gofmt}} {
		*pair.to, err = identify(pair.spec)
		if err != nil {
			t.Fatal(err)
		}
	}
	shell, err := identify(spec.Shell)
	if err != nil {
		t.Fatal(err)
	}
	tools.ChildBinding = hookcheck.ChildBinding{Path: append([]string(nil), spec.Path...), Locale: "C.UTF-8", RunnerShell: &shell}
	for _, kind := range []string{"partial-go", "invalid-staged", "empty-author", "commit-msg-valid", "commit-msg-empty"} {
		t.Run(kind, func(t *testing.T) {
			root, home, temp := t.TempDir(), t.TempDir(), t.TempDir()
			q := hookcheck.TrustRequest{Tools: tools, Capture: hookcheck.CaptureOptions{
				Dir: root, Git: tools.Git.Program, TempParent: temp, Invoke: invoke,
				Env: []string{"PATH=" + strings.Join(spec.Path, ":"), "HOME=" + home, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0"},
			}}
			runGit(t, q, "init", "--quiet", "--template=", "--initial-branch=fixture")
			runGit(t, q, "config", "user.name", "Fixture")
			runGit(t, q, "config", "user.email", "fixture@example.test")
			policy, err := hookcheck.FixedConfig(tools.Checker.Program)
			if err != nil {
				t.Fatal(err)
			}
			mod, err := os.ReadFile("../../go.mod")
			if err != nil {
				t.Fatal(err)
			}
			for name, b := range map[string][]byte{"lefthook.yml": policy, "go.mod": mod, "fixture.go": []byte("package fixture\n\nconst Value = 42\n")} {
				if err := os.WriteFile(filepath.Join(root, name), b, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := preflightGoModules(root, tools.Gofmt.Output); err != nil {
				t.Fatal("selected immutable tools must bind the current root module", err)
			}
			runGit(t, q, "add", "--", ".")
			message := filepath.Join(t.TempDir(), "message")
			if err := os.WriteFile(message, []byte("Fixture subject\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runGit(t, q, "commit", "--quiet", "-F", message)
			fs := hookcheck.LocalTrustIO()
			proposal, _, err := hookcheck.PrepareTrust(context.Background(), q, fs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hookcheck.CommitTrust(context.Background(), q, proposal, fs); err != nil {
				t.Fatal(err)
			}
			request := hookcheck.HookRequest{TrustRequest: q, Phase: hookcheck.PreCommit}
			switch kind {
			case "partial-go":
				file := filepath.Join(root, "fixture.go")
				if err := os.WriteFile(file, []byte("package fixture\n\nconst Value = 43\n"), 0644); err != nil {
					t.Fatal(err)
				}
				runGit(t, q, "add", "--", "fixture.go")
				if err := os.WriteFile(file, []byte("invalid unstaged Go source\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "invalid-staged":
				if err := os.WriteFile(filepath.Join(root, "invalid.nix"), []byte("{ broken = ; }\n"), 0644); err != nil {
					t.Fatal(err)
				}
				runGit(t, q, "add", "--", "invalid.nix")
			case "empty-author":
				request.Capture.Env = append(request.Capture.Env, "GIT_AUTHOR_EMAIL=")
			case "commit-msg-valid", "commit-msg-empty":
				request.Phase, request.MessagePath, request.CommentPrefix = hookcheck.CommitMsg, message, "#"
				if kind == "commit-msg-empty" {
					if err := os.WriteFile(message, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := runnerRepositoryImage(t, root)
			var runner, snapshot string
			initCalls, lefthookCalls := 0, 0
			out, err := hookcheck.RunHook(context.Background(), request, fs, func(ctx context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
				if req.Program == tools.Git.Program {
					initCalls++
					return invoke(ctx, req)
				}
				if req.Program != tools.Lefthook.Program {
					t.Fatal("unexpected parent child", req)
				}
				lefthookCalls++
				runner = req.Dir
				values := map[string]string{}
				for _, e := range req.Env {
					k, v, _ := strings.Cut(e, "=")
					values[k] = v
				}
				input, err := preparedInput([]string{string(request.Phase)}, func(k string) (string, bool) { v, ok := values[k]; return v, ok })
				if err != nil {
					t.Fatal(err)
				}
				snapshot = preparedSnapshotPath(t, input)
				if snapshot == runner || values["GIT_CEILING_DIRECTORIES"] != filepath.Dir(runner) {
					t.Fatal("runner acquired snapshot discovery", req)
				}
				if _, err := os.Lstat(filepath.Join(snapshot, ".git")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("snapshot became Git", err)
				}
				for _, name := range []string{"index", "objects/info/alternates", "refs/heads/fixture"} {
					if _, err := os.Lstat(filepath.Join(runner, ".git", name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("runner acquired original Git authority", name, err)
					}
				}
				config, err := os.ReadFile(filepath.Join(runner, ".git/config"))
				if err != nil || bytes.Contains(config, []byte(root)) || bytes.Contains(config, []byte("hooksPath")) || bytes.Contains(config, []byte("remote")) {
					t.Fatal("runner copied original configuration", string(config), err)
				}
				return invoke(ctx, req)
			})
			t.Logf("actual pinned phase=%s case=%s init=%d lefthook=%d status=%d error=%v\nstdout:\n%s\nstderr:\n%s", request.Phase, kind, initCalls, lefthookCalls, out.Result.ExitCode, err, out.Result.Stdout, out.Result.Stderr)
			if err != nil || !out.Started || initCalls != 1 || lefthookCalls != 1 {
				t.Fatal(out, err)
			}
			wantSuccess := kind == "partial-go" || kind == "commit-msg-valid"
			if (out.Result.ExitCode == 0) != wantSuccess {
				t.Fatal("actual checker result mismatch", kind, out)
			}
			if kind == "partial-go" && !bytes.Contains(out.Result.Stdout, []byte("[no test files]")) {
				t.Fatal("actual Go semantic child did not run", out)
			}
			if !reflect.DeepEqual(before, runnerRepositoryImage(t, root)) {
				t.Fatal("inner run changed original index/config/hooks/ref/working bytes")
			}
			for _, path := range []string{runner, snapshot} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("owned startup resource retained", path, err)
				}
			}
		})
	}
}

func runnerRepositoryImage(t *testing.T, root string) map[string]string {
	t.Helper()
	image := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		image[path] = info.Mode().String() + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}
