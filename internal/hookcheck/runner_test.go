package hookcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Existing scripted tests model only Lefthook/lower-checker behavior. Let the
// new startup step use actual disposable Git, retaining those assertions.
func runFixtureHook(t *testing.T, ctx context.Context, q HookRequest, io TrustIO, child Invoke) (HookOutcome, error) {
	t.Helper()
	return RunHook(ctx, q, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
		if req.Program == q.Tools.Git.Program {
			if !reflect.DeepEqual(req.Args, []string{"init", "--quiet", "--template=", "--initial-branch=confctl-hook-runner", "."}) {
				t.Fatal("unexpected owned Git command", req)
			}
			return realInvoke(ctx, req)
		}
		return child(ctx, req)
	})
}

func fixtureSnapshot(t *testing.T, req Invocation) string {
	t.Helper()
	path, _ := envValue(req.Env, "CONFCTL_HOOK_CONTEXT")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p preparedContext
	if err := decodeCanonical(b, &p); err != nil {
		t.Fatal(err)
	}
	return string(p.Snapshot)
}

func preparedInput(t *testing.T, req Invocation, phase HookPhase) PreparedInput {
	t.Helper()
	path, ok := envValue(req.Env, "CONFCTL_HOOK_CONTEXT")
	if !ok {
		t.Fatal("missing context binding")
	}
	hash, _ := envValue(req.Env, "CONFCTL_HOOK_CONTEXT_SHA256")
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != 32 {
		t.Fatal("bad context digest binding")
	}
	var h [32]byte
	copy(h[:], b)
	message, _ := envValue(req.Env, "CONFCTL_HOOK_MESSAGE")
	return PreparedInput{path, h, phase, message}
}
func assertNarrowEnvironment(t *testing.T, req Invocation) {
	t.Helper()
	for _, k := range []string{"CREDENTIAL", "HOME_AMBIENT", "GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_CONFIG_COUNT", "SHELL", "ENV", "BASH_ENV", "LD_PRELOAD", "RUBYOPT", "LEFTHOOK", "SKIP"} {
		if _, ok := envValue(req.Env, k); ok {
			t.Fatalf("ambient child control leaked: %s", k)
		}
	}
	for _, k := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GOCACHE", "GOPATH"} {
		p, ok := envValue(req.Env, k)
		if !ok || !filepath.IsAbs(p) || strings.HasPrefix(p, req.Dir+"/") {
			t.Fatalf("control path collides with staged tree: %s %q", k, p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := envValue(req.Env, "GOTOOLCHAIN"); v != "local" {
		t.Fatal("toolchain auto download not disabled")
	}
}

func TestHookRunnerFixedDispatchPreparedReuseAndLiteralPaths(t *testing.T) {
	q, io, root := trustFixture(t, false)
	names := []string{" edge\n'$.nix", "invalid-\xff.go"}
	for _, n := range names {
		put(t, root, n, "staged bytes")
	}
	stage(t, root, q.Capture.Env, names...)
	q.Capture.Env = append(q.Capture.Env, "CREDENTIAL=synthetic-secret", "ENV=/inert/ambient", "BASH_ENV=/inert/ambient", "LD_PRELOAD=/inert/ambient", "RUBYOPT=inert", "SHELL=/inert/shell")
	// real Git must not inherit loader variables from this modeled caller in the
	// fixture. Capture still passes the exact caller env to its trusted adapter.
	gitInvoke := q.Capture.Invoke
	captures := 0
	q.Capture.Invoke = func(ctx context.Context, req Invocation) (ToolResult, error) {
		if req.Args[0] == "ls-files" {
			captures++
		}
		for _, k := range []string{"LD_PRELOAD", "RUBYOPT"} {
			var env []string
			for _, p := range req.Env {
				if !strings.HasPrefix(p, k+"=") {
					env = append(env, p)
				}
			}
			req.Env = env
		}
		return gitInvoke(ctx, req)
	}
	trustNow(t, q, io)
	captures = 0
	before := fixtureState(t, root)
	calls := 0
	var contextPath, snapshot string
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
		calls++
		snapshot = fixtureSnapshot(t, req)
		if req.Program != q.Tools.Lefthook.Program || !reflect.DeepEqual(req.Args, []string{"run", "--force", "--no-auto-install", "--no-stage-fixed", "--skip-lfs", "pre-commit"}) || len(req.Stdin) != 0 || req.Dir == snapshot {
			t.Fatalf("runner dispatch: %#v", req)
		}
		assertNarrowEnvironment(t, req)
		input := preparedInput(t, req, PreCommit)
		contextPath = input.ContextPath
		b, _ := os.ReadFile(contextPath)
		if bytes.Contains(b, []byte("synthetic-secret")) || bytes.Contains(b, []byte("GIT_INDEX_FILE")) {
			t.Fatal("arbitrary original context serialized")
		}
		var parsed preparedContext
		if err := decodeCanonical(b, &parsed); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range parsed.Entries {
			if bytes.Equal(e.Path, []byte(names[1])) {
				found = true
			}
		}
		if !found {
			t.Fatal("invalid UTF8 filename lost in context")
		}
		var tools []Invocation
		findings, err := CheckPrepared(ctx, input, q.Tools, func(ctx context.Context, tool Invocation) (ToolResult, error) {
			assertNarrowEnvironment(t, tool)
			tools = append(tools, tool)
			if _, ok := envValue(tool.Env, "CONFCTL_HOOK_CONTEXT"); ok {
				t.Fatal("runner context leaked into linter")
			}
			return ToolResult{}, nil
		}, func(context.Context, Invocation) error { return nil })
		if err != nil || HasErrors(findings) || len(tools) != 2 {
			t.Fatalf("prepared checks: %#v %v %#v", findings, err, tools)
		}
		wantNix := []string{"--check", filepath.Join(snapshot, names[0]), filepath.Join(snapshot, "file.nix")}
		if !reflect.DeepEqual(tools[0].Args, wantNix) || tools[0].Program != q.Tools.Nixfmt.Program || !reflect.DeepEqual(tools[1].Args, []string{"-l", filepath.Join(snapshot, names[1])}) {
			t.Fatalf("literal selected formatter argv: %#v", tools)
		}
		if _, err := os.Stat(snapshot); err != nil {
			t.Fatal("borrowed checker closed parent snapshot")
		}
		return ToolResult{Stdout: []byte("runner stdout\n"), Stderr: []byte("runner stderr\n")}, nil
	})
	if err != nil || !out.Started || calls != 1 || captures != 1 || string(out.Result.Stdout) != "runner stdout\n" || string(out.Result.Stderr) != "runner stderr\n" {
		t.Fatalf("one-capture/result: %#v %v calls=%d captures=%d", out, err, calls, captures)
	}
	for _, p := range []string{contextPath, snapshot} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned cleanup missing: %q %v", p, err)
		}
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("run changed original index/ref/worktree/config/hooks")
	}
}

func TestHookRefusalsBeforeExecutionAndLockOnlyUpdates(t *testing.T) {
	for _, change := range []string{"missing", "root-identity", "tool", "policy", "extra", "control", "service-unbound"} {
		t.Run(change, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			if change != "missing" {
				trustNow(t, q, io)
			}
			switch change {
			case "root-identity":
				q.Capture.Dir = root + "-moved"
				if err := os.Rename(root, q.Capture.Dir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Rename(q.Capture.Dir, root) })
			case "tool":
				q.Tools.Gofmt.SHA256 = sha256.Sum256([]byte("different installed tool"))
			case "policy":
				put(t, root, ".rubocop.yml", "changed finite policy")
				stage(t, root, q.Capture.Env, ".rubocop.yml")
			case "extra":
				put(t, root, ".lefthook-local.yml", "foreign")
			case "control":
				q.Capture.Env = append(q.Capture.Env, "LEFTHOOK=0")
			case "service-unbound":
				q.Tools.ServiceRuby = true
			}
			before := fixtureState(t, q.Capture.Dir)
			calls := 0
			out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(context.Context, Invocation) (ToolResult, error) { calls++; return ToolResult{}, nil })
			assertRefusal(t, err, "")
			if out.Started || calls != 0 || !reflect.DeepEqual(before, fixtureState(t, q.Capture.Dir)) {
				t.Fatal("untrusted execution or original mutation")
			}
		})
	}
	q, io, root := trustFixture(t, false)
	trustNow(t, q, io)
	put(t, root, "flake.lock", "new source update")
	stage(t, root, q.Capture.Env, "flake.lock")
	before := fixtureState(t, root)
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(context.Context, Invocation) (ToolResult, error) { return ToolResult{}, nil })
	if err != nil || !out.Started || !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatalf("source-only update needed retrust: %#v %v", out, err)
	}
}

func TestPreparedContextDriftRefusesBeforeLinter(t *testing.T) {
	for _, change := range []string{"digest", "context", "mode", "control-mode", "missing", "snapshot", "policy", "tool", "phase", "selection", "entry", "message"} {
		t.Run(change, func(t *testing.T) {
			q, io, _ := trustFixture(t, false)
			trustNow(t, q, io)
			out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				input := preparedInput(t, req, PreCommit)
				original, err := os.ReadFile(input.ContextPath)
				if err != nil {
					t.Fatal(err)
				}
				var p preparedContext
				decodeCanonical(original, &p)
				snapshot := string(p.Snapshot)
				tools := q.Tools
				restore := func() {}
				switch change {
				case "digest":
					input.SHA256 = sha256.Sum256([]byte("bad"))
				case "context":
					os.WriteFile(input.ContextPath, []byte("{}\n"), 0600)
					restore = func() { os.WriteFile(input.ContextPath, original, 0600) }
				case "mode":
					os.Chmod(input.ContextPath, 0644)
					restore = func() { os.Chmod(input.ContextPath, 0600) }
				case "control-mode":
					os.Chmod(filepath.Dir(input.ContextPath), 0755)
					restore = func() { os.Chmod(filepath.Dir(input.ContextPath), 0700) }
				case "missing":
					os.Rename(input.ContextPath, input.ContextPath+"-held")
					restore = func() { os.Rename(input.ContextPath+"-held", input.ContextPath) }
				case "snapshot":
					os.Rename(snapshot, snapshot+"-held")
					os.Mkdir(snapshot, 0700)
					restore = func() { os.Remove(snapshot); os.Rename(snapshot+"-held", snapshot) }
				case "policy":
					path := filepath.Join(snapshot, ".rubocop.yml")
					old, _ := os.ReadFile(path)
					os.WriteFile(path, []byte("different"), 0644)
					restore = func() { os.WriteFile(path, old, 0644) }
				case "tool":
					tools.Nixfmt.SHA256 = sha256.Sum256([]byte("different"))
				case "phase":
					input.Phase = CommitMsg
				case "selection":
					p.Selected = [][]byte{[]byte("not-staged")}
				case "entry":
					p.Entries[0].Mode = 0
				case "message":
					p.Message = []byte("/unexpected-message")
				}
				if change == "selection" || change == "entry" || change == "message" {
					b, _ := canonical(p)
					os.WriteFile(input.ContextPath, b, 0600)
					input.SHA256 = sha256.Sum256(b)
					restore = func() { os.WriteFile(input.ContextPath, original, 0600) }
				}
				defer restore()
				calls := 0
				_, err = CheckPrepared(ctx, input, tools, func(context.Context, Invocation) (ToolResult, error) { calls++; return ToolResult{}, nil }, nil)
				if err == nil || calls != 0 {
					t.Fatalf("drift accepted: %s %v calls%d", change, err, calls)
				}
				return ToolResult{}, nil
			})
			if err != nil || !out.Started {
				t.Fatalf("parent failed after restored drift: %#v %v", out, err)
			}
		})
	}
}

func TestPreparedLiteralMessagesAndAuthorPresence(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		t.Run(map[bool]string{false: "relative", true: "absolute"}[absolute], func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			trustNow(t, q, io)
			name := " msg\n'$ "
			content := "fixup! " + strings.Repeat("literal ", 15) + "\n\n# ignored\ndiff --git a/inert b/inert\n"
			put(t, root, name, content)
			os.Chmod(filepath.Join(root, name), 0600)
			message := name
			if absolute {
				message = filepath.Join(root, name)
			}
			before := fixtureState(t, root)
			out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: CommitMsg, MessagePath: message, CommentPrefix: "#"}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				input := preparedInput(t, req, CommitMsg)
				if input.MessagePath != filepath.Join(root, name) {
					t.Fatalf("literal original-CWD message: %q", input.MessagePath)
				}
				findings, err := CheckPrepared(ctx, input, q.Tools, func(context.Context, Invocation) (ToolResult, error) {
					t.Fatal("message phase launched formatter")
					return ToolResult{}, nil
				}, nil)
				if err != nil || HasErrors(findings) {
					t.Fatalf("message advisories: %#v %v", findings, err)
				}
				return ToolResult{}, nil
			})
			if err != nil || !out.Started || !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatalf("message rewritten/mutated: %#v %v", out, err)
			}
		})
	}
	q, io, _ := trustFixture(t, false)
	trustNow(t, q, io)
	q.Capture.Env = append(q.Capture.Env, "GIT_AUTHOR_EMAIL=")
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
		findings, err := CheckPrepared(ctx, preparedInput(t, req, PreCommit), q.Tools, func(context.Context, Invocation) (ToolResult, error) {
			t.Fatal("invalid author reached formatter")
			return ToolResult{}, nil
		}, nil)
		if err != nil || len(findings) != 1 || findings[0].Check != "AuthorEmail" || !HasErrors(findings) {
			t.Fatalf("present empty lost: %#v %v", findings, err)
		}
		return ToolResult{ExitCode: 1}, nil
	})
	if err != nil || !out.Started || out.Result.ExitCode != 1 {
		t.Fatalf("author result: %#v %v", out, err)
	}
}

func TestPreparedServiceWarningsAndUnsupportedConfig(t *testing.T) {
	for _, kind := range []string{"ordinary", "warning", "Warning", "nested-staged", "nested-working"} {
		t.Run(kind, func(t *testing.T) {
			service := kind != "ordinary"
			q, io, root := trustFixture(t, service)
			put(t, root, "service/code.rb", "staged Ruby")
			stage(t, root, q.Capture.Env, "service/code.rb")
			trustNow(t, q, io)
			if strings.HasPrefix(kind, "nested") {
				put(t, root, "service/.rubocop.yml", "extra policy")
				if kind == "nested-staged" {
					stage(t, root, q.Capture.Env, "service/.rubocop.yml")
				}
			}
			calls := 0
			out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				calls++
				findings, err := CheckPrepared(ctx, preparedInput(t, req, PreCommit), q.Tools, func(ctx context.Context, tool Invocation) (ToolResult, error) {
					if tool.Program == q.Tools.Nixfmt.Program {
						return ToolResult{}, nil
					}
					if !service || tool.Program != q.Tools.RuboCop.Program {
						t.Fatal("unexpected service executable")
					}
					if v, _ := envValue(tool.Env, "GEM_HOME"); v != q.Tools.ChildBinding.Service.GemHome {
						t.Fatal("undeclared service environment")
					}
					return ToolResult{ExitCode: 1, Stderr: []byte(kind + ": retained\n")}, nil
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if HasErrors(findings) != (kind == "ordinary" || kind == "Warning") {
					t.Fatalf("service category/lane: %#v", findings)
				}
				status := 0
				if HasErrors(findings) {
					status = 1
				}
				return ToolResult{ExitCode: status}, nil
			})
			if strings.HasPrefix(kind, "nested") {
				assertRefusal(t, err, "unsupported-policy")
				if calls != 0 || out.Started {
					t.Fatal("unknown service policy executed")
				}
			} else if err != nil || !out.Started || calls != 1 {
				t.Fatalf("service run: %#v %v", out, err)
			}
		})
	}
}

func TestHookCancellationRawErrorsAndGuardedCleanup(t *testing.T) {
	for _, failure := range []string{"before", "capture", "after-policy", "inside", "spawn", "nonzero", "replace-control", "replace-snapshot"} {
		t.Run(failure, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			trustNow(t, q, io)
			before := fixtureState(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			primary := errors.New("adapter primary")
			calls := 0
			if failure == "before" {
				cancel()
			}
			if failure == "capture" {
				original := q.Capture.Invoke
				q.Capture.Invoke = func(ctx context.Context, req Invocation) (ToolResult, error) {
					r, err := original(ctx, req)
					if req.Args[0] == "cat-file" {
						cancel()
					}
					return r, err
				}
			}
			if failure == "after-policy" {
				original := io.IdentifyTool
				io.IdentifyTool = func(ctx context.Context, t ToolIdentity) (ToolIdentity, error) {
					out, err := original(ctx, t)
					if t == q.Tools.Gofmt {
						cancel()
					}
					return out, err
				}
			}
			out, err := runFixtureHook(t, ctx, HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				calls++
				if failure == "inside" {
					cancel()
					return ToolResult{Stdout: []byte("partial out"), Stderr: []byte("partial err"), ExitCode: 7}, context.Canceled
				}
				if failure == "replace-control" || failure == "replace-snapshot" {
					path := fixtureSnapshot(t, req)
					if failure == "replace-control" {
						input := preparedInput(t, req, PreCommit)
						path = filepath.Dir(input.ContextPath)
					}
					if err := os.Rename(path, path+"-held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					put(t, path, "foreign", "retained")
					t.Cleanup(func() {
						b, _ := os.ReadFile(filepath.Join(path, "foreign"))
						if string(b) != "retained" {
							t.Error("cleanup bypass removed replacement")
						}
						os.RemoveAll(path)
						os.RemoveAll(path + "-held")
					})
					return ToolResult{ExitCode: 3}, primary
				}
				if failure == "spawn" {
					return ToolResult{Stdout: []byte("partial out"), Stderr: []byte("partial err"), ExitCode: 7}, primary
				}
				return ToolResult{ExitCode: 5}, nil
			})
			if failure == "before" || failure == "capture" || failure == "after-policy" {
				if out.Started || calls != 0 || !errors.Is(err, context.Canceled) {
					t.Fatalf("prelaunch cancel: %#v %v", out, err)
				}
			} else {
				if !out.Started || calls != 1 {
					t.Fatalf("started state: %#v %v", out, err)
				}
				if failure == "inside" {
					if !errors.Is(err, context.Canceled) || string(out.Result.Stdout) != "partial out" || string(out.Result.Stderr) != "partial err" || out.Result.ExitCode != 7 {
						t.Fatalf("lost cancellation streams: %#v %v", out, err)
					}
				}
				if failure == "spawn" || strings.HasPrefix(failure, "replace") {
					if !errors.Is(err, primary) {
						t.Fatalf("cleanup masked primary: %v", err)
					}
				}
				if failure == "nonzero" && (err != nil || out.Result.ExitCode != 5) {
					t.Fatalf("nonzero turned into spawn/success: %#v %v", out, err)
				}
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("failure changed original state")
			}
		})
	}
}

func TestHookPrelaunchReceiptDriftAndAllOwnedCleanup(t *testing.T) {
	q, io, root := trustFixture(t, false)
	trustNow(t, q, io)
	before := fixtureState(t, root)
	baseOpen := io.Open
	reads := 0
	io.Open = func(root, admin string) (TrustDirectory, error) {
		d, err := baseOpen(root, admin)
		if err != nil {
			return nil, err
		}
		return &observedTrustDirectory{TrustDirectory: d, read: func(p ReceiptImage) (ReceiptImage, error) {
			reads++
			if reads == 2 {
				p.File.Inode++
			}
			return p, nil
		}}, nil
	}
	calls := 0
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(context.Context, Invocation) (ToolResult, error) { calls++; return ToolResult{}, nil })
	assertRefusal(t, err, "stale-identity")
	if out.Started || calls != 0 || reads != 2 || !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("receipt recheck drift executed/changed original")
	}
	entries, _ := os.ReadDir(q.Capture.TempParent)
	if len(entries) != 0 {
		t.Fatal("prelaunch refusal left owned snapshot/control")
	}
	// Read-handle cleanup fails, but both independent temporary owners must still
	// be closed, and the actual nonzero runner result must survive that failure.
	io.Open = func(root, admin string) (TrustDirectory, error) {
		d, err := baseOpen(root, admin)
		if err != nil {
			return nil, err
		}
		return &observedTrustDirectory{TrustDirectory: d, closeError: os.ErrPermission}, nil
	}
	out, err = runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(context.Context, Invocation) (ToolResult, error) {
		return ToolResult{ExitCode: 9, Stdout: []byte("out"), Stderr: []byte("err")}, nil
	})
	if !errors.Is(err, os.ErrPermission) || !out.Started || out.Result.ExitCode != 9 || string(out.Result.Stdout) != "out" || string(out.Result.Stderr) != "err" {
		t.Fatalf("cleanup lost result: %#v %v", out, err)
	}
	entries, _ = os.ReadDir(q.Capture.TempParent)
	if len(entries) != 0 {
		t.Fatal("handle cleanup prevented temporary owner cleanup")
	}
}

func TestHookSubdirectoryAlternateIndexAndMessageMetadata(t *testing.T) {
	q, io, root := trustFixture(t, false)
	put(t, root, "sub/file.nix", "staged sub")
	stage(t, root, q.Capture.Env, "sub/file.nix")
	index := filepath.Join(root, ".git", " literal\nindex ")
	b, _ := os.ReadFile(filepath.Join(root, ".git/index"))
	if err := os.WriteFile(index, b, 0600); err != nil {
		t.Fatal(err)
	}
	q.Capture.Dir = filepath.Join(root, "sub")
	q.Capture.Env = append(q.Capture.Env, "GIT_INDEX_FILE=.git/ literal\nindex ", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=diff.relative", "GIT_CONFIG_VALUE_0=true")
	trustNow(t, q, io)
	name := " message\n'$ "
	put(t, root, "sub/"+name, "Subject\n\nBody.\n")
	before := fixtureState(t, root)
	captureCalls := 0
	invoke := q.Capture.Invoke
	q.Capture.Invoke = func(ctx context.Context, req Invocation) (ToolResult, error) {
		captureCalls++
		if req.Dir != q.Capture.Dir {
			t.Fatal("original CWD changed")
		}
		if index, _ := envValue(req.Env, "GIT_INDEX_FILE"); index != ".git/ literal\nindex " {
			t.Fatal("relative index spelling changed")
		}
		return invoke(ctx, req)
	}
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: CommitMsg, MessagePath: name, CommentPrefix: "#"}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
		input := preparedInput(t, req, CommitMsg)
		if input.MessagePath != filepath.Join(root, "sub", name) {
			t.Fatal("message resolved in Git root rather than original CWD")
		}
		findings, err := CheckPrepared(ctx, input, q.Tools, nil, nil)
		if err != nil || HasErrors(findings) {
			t.Fatalf("message: %#v %v", findings, err)
		}
		return ToolResult{}, nil
	})
	if err != nil || !out.Started || captureCalls == 0 || !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatalf("subdir alternate index/message: %#v %v", out, err)
	}
}

func TestHookUnencodablePrivateDiscoveryCeilingRefuses(t *testing.T) {
	q, io, root := trustFixture(t, false)
	trustNow(t, q, io)
	parent := filepath.Join(t.TempDir(), "colon:parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	q.Capture.TempParent = parent
	before := fixtureState(t, root)
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(context.Context, Invocation) (ToolResult, error) {
		t.Fatal("unencodable discovery ceiling reached child")
		return ToolResult{}, nil
	})
	assertRefusal(t, err, "unsupported-policy")
	if out.Started || !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("ceiling refusal changed original")
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 0 {
		t.Fatal("ceiling refusal leaked owners")
	}
}

func TestPreparedGoCallbackSelectionAndLifetime(t *testing.T) {
	for _, kind := range []string{"go", "root-mod", "root-sum", "extension-mod", "extension-sum", "mixed", "warning", "finding", "linter-error", "cancel", "callback-error", "nil", "unrelated", "commit-msg"} {
		t.Run(kind, func(t *testing.T) {
			q, io, root := trustFixture(t, kind == "warning")
			names := []string{"selected.go"}
			switch kind {
			case "root-mod":
				names = []string{"go.mod"}
			case "root-sum":
				names = []string{"go.sum"}
			case "extension-mod":
				names = []string{"extensions/go.mod"}
			case "extension-sum":
				names = []string{"extensions/go.sum"}
			case "mixed":
				names = []string{"selected.go", "go.mod", "go.sum", "extensions/go.mod", "extensions/go.sum"}
			case "unrelated", "commit-msg":
				names = []string{"README"}
			}
			for _, name := range names {
				data := "module fixture.test/selected\n"
				if strings.HasSuffix(name, ".go") {
					data = "package fixture\n"
				}
				put(t, root, name, data)
			}
			if kind == "warning" {
				put(t, root, "service.rb", "puts 'fixture'\n")
				names = append(names, "service.rb")
			}
			stage(t, root, q.Capture.Env, names...)
			trustNow(t, q, io)
			request := HookRequest{TrustRequest: q, Phase: PreCommit}
			if kind == "commit-msg" {
				request.Phase = CommitMsg
				request.CommentPrefix = "#"
				request.MessagePath = filepath.Join(root, "message")
				put(t, root, "message", "Fixture subject\n")
			}
			before := fixtureState(t, root)
			callbackCalls := 0
			sentinel := errors.New("selected Go failure")
			_, err := runFixtureHook(t, context.Background(), request, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				input := preparedInput(t, req, request.Phase)
				snapshot := fixtureSnapshot(t, req)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				callback := GoCheck(func(ctx context.Context, goInput Invocation) error {
					callbackCalls++
					if goInput.Program != "" || goInput.Args != nil || goInput.Stdin != nil || goInput.Dir != snapshot {
						t.Fatalf("synthetic tool request or wrong snapshot: %#v", goInput)
					}
					assertNarrowEnvironment(t, goInput)
					if _, ok := envValue(goInput.Env, "CONFCTL_HOOK_CONTEXT"); ok {
						t.Fatal("prepared input leaked into Go environment")
					}
					if _, err := os.Stat(goInput.Dir); err != nil {
						t.Fatal("snapshot released before synchronous callback", err)
					}
					if kind == "callback-error" {
						return sentinel
					}
					return ctx.Err()
				})
				if kind == "nil" {
					callback = nil
				}
				findings, checkErr := CheckPrepared(ctx, input, q.Tools, func(_ context.Context, tool Invocation) (ToolResult, error) {
					if kind == "cancel" {
						cancel()
					}
					if kind == "linter-error" {
						return ToolResult{}, sentinel
					}
					if kind == "finding" && tool.Program == q.Tools.Gofmt.Program {
						return ToolResult{Stdout: []byte("unformatted\n")}, nil
					}
					if kind == "warning" && tool.Program == q.Tools.RuboCop.Program {
						return ToolResult{ExitCode: 1, Stderr: []byte("warning: fixture\n")}, nil
					}
					return ToolResult{}, nil
				}, callback)
				switch kind {
				case "linter-error", "callback-error":
					if !errors.Is(checkErr, sentinel) {
						t.Fatal(checkErr)
					}
				case "cancel":
					if !errors.Is(checkErr, context.Canceled) {
						t.Fatal(checkErr)
					}
				case "nil":
					if checkErr == nil {
						t.Fatal("reached nil callback silently skipped Go checks")
					}
				default:
					if checkErr != nil {
						t.Fatal(checkErr)
					}
				}
				if (kind == "finding") != HasErrors(findings) {
					t.Fatalf("findings changed: %#v", findings)
				}
				if kind == "warning" && (len(findings) == 0 || !findings[0].Warning) {
					t.Fatal("warning lost", findings)
				}
				return ToolResult{}, nil
			})
			want := 1
			switch kind {
			case "finding", "linter-error", "cancel", "nil", "unrelated", "commit-msg":
				want = 0
			}
			if err != nil || callbackCalls != want || !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatalf("callback count/state: got %d want %d err %v", callbackCalls, want, err)
			}
		})
	}
}

func TestGoToolOutputChangeRequiresExplicitRetrust(t *testing.T) {
	q, io, _ := trustFixture(t, false)
	trustNow(t, q, io)
	previous := q.Tools.Gofmt
	output := "/nix/store/" + strings.Repeat("d", 32) + "-hook-go-tools"
	q.Tools.Gofmt = ToolIdentity{Output: output, Program: output + "/bin/gofmt", Resolved: output + "/bin/gofmt", SHA256: previous.SHA256}
	calls := 0
	executor := func(context.Context, Invocation) (ToolResult, error) { calls++; return ToolResult{}, nil }
	_, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, executor)
	assertRefusal(t, err, "stale-identity")
	if calls != 0 {
		t.Fatal("changed manifest/proxy output reached runner")
	}
	trustNow(t, q, io)
	out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, executor)
	if err != nil || !out.Started || calls != 1 {
		t.Fatal("explicit retrust did not permit new Go output", out, err, calls)
	}
}

func TestOwnedRunnerInitializationAndGuardedLifetime(t *testing.T) {
	for _, kind := range []string{"success", "init-spawn", "init-status", "init-cancel", "runner-replaced", "runner-symlink", "gitfile", "git-symlink", "extra-config", "existing-policy", "context-drift", "foreign-control", "checker-failure"} {
		t.Run(kind, func(t *testing.T) {
			q, fs, root := trustFixture(t, false)
			trustNow(t, q, fs)
			before := fixtureState(t, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			primary := errors.New("owned child failure")
			var runner, control, snapshot string
			initCalls, runnerCalls := 0, 0
			out, err := RunHook(ctx, HookRequest{TrustRequest: q, Phase: PreCommit}, fs, func(ctx context.Context, req Invocation) (ToolResult, error) {
				if req.Program == q.Tools.Git.Program {
					initCalls++
					runner, control = req.Dir, filepath.Dir(req.Dir)
					b, err := os.ReadFile(filepath.Join(control, "context.json"))
					if err != nil {
						t.Fatal(err)
					}
					var p preparedContext
					if err := decodeCanonical(b, &p); err != nil {
						t.Fatal(err)
					}
					snapshot = string(p.Snapshot)
					entries, err := os.ReadDir(runner)
					if err != nil || len(entries) != 0 || runner == snapshot || filepath.Base(runner) != "runner" {
						t.Fatal("runner not empty and separate", entries, err)
					}
					ceiling, _ := envValue(req.Env, "GIT_CEILING_DIRECTORIES")
					if ceiling != control || !reflect.DeepEqual(req.Args, []string{"init", "--quiet", "--template=", "--initial-branch=confctl-hook-runner", "."}) {
						t.Fatal("unexpected initialization", req)
					}
					assertNarrowEnvironment(t, req)
					if kind == "init-spawn" {
						return ToolResult{Stderr: []byte("init diagnostic"), ExitCode: 23}, primary
					}
					if kind == "init-status" {
						return ToolResult{Stderr: []byte("init diagnostic"), ExitCode: 23}, nil
					}
					result, err := realInvoke(ctx, req)
					if err != nil || result.ExitCode != 0 {
						t.Fatal(result, err)
					}
					switch kind {
					case "init-cancel":
						cancel()
					case "runner-replaced", "runner-symlink":
						if err := os.Rename(runner, runner+"-held"); err != nil {
							t.Fatal(err)
						}
						foreign := t.TempDir()
						if kind == "runner-replaced" {
							if err := os.Mkdir(runner, 0700); err != nil {
								t.Fatal(err)
							}
							foreign = runner
						} else if err := os.Symlink(foreign, runner); err != nil {
							t.Fatal(err)
						}
						put(t, foreign, "foreign", "retained")
						t.Cleanup(func() {
							if b, _ := os.ReadFile(filepath.Join(foreign, "foreign")); string(b) != "retained" {
								t.Error("cleanup followed or removed replacement")
							}
							os.RemoveAll(control)
						})
					case "gitfile", "git-symlink":
						if err := os.RemoveAll(filepath.Join(runner, ".git")); err != nil {
							t.Fatal(err)
						}
						if kind == "gitfile" {
							put(t, runner, ".git", "gitdir: "+filepath.Join(root, ".git")+"\n")
						} else if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(runner, ".git")); err != nil {
							t.Fatal(err)
						}
					case "extra-config":
						put(t, runner, "lefthook-local.yml", "foreign config")
					case "existing-policy":
						put(t, runner, "lefthook.yml", "foreign policy")
					case "context-drift":
						if err := os.WriteFile(filepath.Join(control, "context.json"), []byte("{}\n"), 0600); err != nil {
							t.Fatal(err)
						}
					case "foreign-control":
						put(t, control, "foreign", "retained")
						t.Cleanup(func() { os.RemoveAll(control) })
					}
					return result, nil
				}
				runnerCalls++
				if req.Program != q.Tools.Lefthook.Program || req.Dir != runner {
					t.Fatal(req)
				}
				policy, id, err := readOwnedRegular(filepath.Join(runner, "lefthook.yml"), false)
				fixed, _ := FixedConfig(q.Tools.Checker.Program)
				if err != nil || id.Mode&07777 != 0400 || !bytes.Equal(policy, fixed) {
					t.Fatal("runner policy was not exact and read-only", err)
				}
				if kind == "checker-failure" {
					return ToolResult{ExitCode: 19, Stderr: []byte("checker diagnostic")}, primary
				}
				return ToolResult{}, nil
			})
			wantRunner := kind == "success" || kind == "foreign-control" || kind == "checker-failure"
			if initCalls != 1 || out.Started != wantRunner || runnerCalls != map[bool]int{false: 0, true: 1}[wantRunner] {
				t.Fatal("wrong child boundary", initCalls, runnerCalls, out, err)
			}
			if kind == "success" && err != nil || kind != "success" && err == nil {
				t.Fatal(kind, err)
			}
			if (kind == "init-spawn" || kind == "checker-failure") && !errors.Is(err, primary) {
				t.Fatal("lost primary child error", err)
			}
			if strings.HasPrefix(kind, "init-") && kind != "init-cancel" && (!strings.Contains(err.Error(), "status 23") || !strings.Contains(err.Error(), "init diagnostic")) {
				t.Fatal("lost initialization status/diagnostic", err)
			}
			if kind == "init-cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost initialization cancellation", err)
			}
			if kind == "foreign-control" {
				if b, _ := os.ReadFile(filepath.Join(control, "foreign")); string(b) != "retained" {
					t.Fatal("removed foreign control entry")
				}
			} else if !strings.HasPrefix(kind, "runner-") {
				if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("control retained", err)
				}
			}
			if _, err := os.Lstat(snapshot); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("snapshot retained", err)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("startup changed original repository")
			}
		})
	}
}
