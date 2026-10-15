package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

func manifestFile(data []byte, present bool) goFileBinding {
	b := goFileBinding{Present: &present}
	if present {
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		b.SHA256 = &digest
	}
	return b
}
func putGoManifest(t *testing.T, output string, modules []goModuleBinding) {
	t.Helper()
	dir := filepath.Join(output, "share/confctl-hooks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(goModulesManifest{Modules: modules})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go-modules.json"), b, 0644); err != nil {
		t.Fatal(err)
	}
}
func goCheckFixture(t *testing.T) (hookcheck.ToolSet, hookcheck.Invocation, []goModuleBinding) {
	t.Helper()
	root := t.TempDir()
	output := t.TempDir()
	control := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "extensions"), 0755); err != nil {
		t.Fatal(err)
	}
	var modules []goModuleBinding
	for _, name := range []string{".", "extensions"} {
		data := []byte("module fixture.test/" + map[string]string{".": "root", "extensions": "extension"}[name] + "\n\ngo 1.23\n")
		if err := os.WriteFile(filepath.Join(root, name, "go.mod"), data, 0644); err != nil {
			t.Fatal(err)
		}
		modules = append(modules, goModuleBinding{Directory: name, GoMod: manifestFile(data, true), GoSum: manifestFile(nil, false)})
	}
	if err := os.Mkdir(filepath.Join(output, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "gofmt"} {
		if err := os.WriteFile(filepath.Join(output, "bin", name), []byte("fixture wrapper"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	putGoManifest(t, output, modules)
	tools := hookcheck.ToolSet{Gofmt: hookcheck.ToolIdentity{Output: output, Program: filepath.Join(output, "bin/gofmt")}}
	input := hookcheck.Invocation{Dir: root, Env: []string{"PATH=/declared/bin", "HOME=" + filepath.Join(control, "home"), "TMPDIR=" + filepath.Join(control, "tmp"), "XDG_CONFIG_HOME=" + filepath.Join(control, "config"), "GOCACHE=" + filepath.Join(control, "gocache"), "GOPATH=" + filepath.Join(control, "gopath"), "GOPROXY=https://poison.invalid,direct", "GOMODCACHE=/operator/cache", "GONOPROXY=*", "GOPRIVATE=*", "GOSUMDB=sum.poison.invalid", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=auto"}}
	return tools, input, modules
}

func TestCheckGoPreflightsAllDetachedModulesAndControlsEnvironment(t *testing.T) {
	tools, input, modules := goCheckFixture(t)
	// An existing immutable directory suffices for the scripted invocation; real
	// dependency content is exercised by the separately selected Nix fixture.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join("/nix/store", strings.Split(goBin, "/")[3])
	if !proxyOutput.MatchString(output) {
		t.Fatal("test requires declared store Go", output)
	}
	modules[1].Proxy = &output
	putGoManifest(t, tools.Gofmt.Output, modules)
	t.Setenv("GOPROXY", "https://ambient.invalid,direct")
	t.Setenv("GOPATH", "/operator/gopath")
	t.Setenv("GOMODCACHE", "/operator/modcache")
	var calls []hookcheck.Invocation
	executor := func(_ context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
		calls = append(calls, req)
		return hookcheck.ToolResult{}, nil
	}
	var out bytes.Buffer
	if err := checkGo(context.Background(), tools, input, &out, &out, executor); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatal(calls)
	}
	if calls[0].Program != filepath.Join(tools.Gofmt.Output, "bin/go") || calls[0].Dir != input.Dir || !reflect.DeepEqual(calls[0].Args, []string{"telemetry", "off"}) || !reflect.DeepEqual(calls[0].Env, calls[1].Env) {
		t.Fatal("telemetry must be disabled once before every semantic child", calls)
	}
	control := filepath.Dir(strings.TrimPrefix(input.Env[1], "HOME="))
	for i, req := range calls[1:] {
		dir := input.Dir
		if i >= 2 {
			dir = filepath.Join(dir, "extensions")
		}
		action := []string{"vet", "test"}[i%2]
		if req.Dir != dir || req.Program != filepath.Join(tools.Gofmt.Output, "bin/go") || !reflect.DeepEqual(req.Args, []string{action, "-mod=readonly", "./..."}) {
			t.Fatal(req)
		}
		env := map[string]string{}
		for _, e := range req.Env {
			k, v, _ := strings.Cut(e, "=")
			if _, ok := env[k]; ok {
				t.Fatal("duplicate environment", k)
			}
			env[k] = v
		}
		proxy := "off"
		if i >= 2 {
			proxy = "file://" + output
		}
		for k, want := range map[string]string{"GOPROXY": proxy, "GONOPROXY": "none", "GOPRIVATE": "", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly -modcacherw", "GOWORK": "off", "GOENV": "off", "GOMODCACHE": filepath.Join(control, "gopath", "pkg", "mod"), "GOPATH": filepath.Join(control, "gopath"), "GOCACHE": filepath.Join(control, "gocache"), "HOME": filepath.Join(control, "home"), "XDG_CONFIG_HOME": filepath.Join(control, "config"), "TMPDIR": filepath.Join(control, "tmp")} {
			if env[k] != want {
				t.Fatalf("%s = %q, want %q", k, env[k], want)
			}
		}
	}
	calls = nil
	executor = func(_ context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
		calls = append(calls, req)
		if len(calls) == 1 {
			return hookcheck.ToolResult{}, nil
		}
		return hookcheck.ToolResult{ExitCode: 1}, nil
	}
	if err := checkGo(context.Background(), tools, input, &out, &out, executor); err == nil || len(calls) != 2 {
		t.Fatal("child failure not propagated", err, calls)
	}
}

type goInitWriter func([]byte) (int, error)

func (w goInitWriter) Write(p []byte) (int, error) { return w(p) }

func TestCheckGoTelemetryInitializationFailureStopsSemanticChildren(t *testing.T) {
	for _, kind := range []string{"spawn", "nonzero", "cancel", "stdout", "stderr", "short-output"} {
		t.Run(kind, func(t *testing.T) {
			tools, input, _ := goCheckFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("initialization failure")
			var out, stderr io.Writer = io.Discard, io.Discard
			if kind == "stdout" {
				out = goInitWriter(func([]byte) (int, error) { return 0, sentinel })
			}
			if kind == "stderr" {
				stderr = goInitWriter(func([]byte) (int, error) { return 0, sentinel })
			}
			if kind == "short-output" {
				out = goInitWriter(func(p []byte) (int, error) { return len(p) - 1, nil })
			}
			calls := 0
			err := checkGo(ctx, tools, input, out, stderr, func(_ context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
				calls++
				if calls != 1 || !reflect.DeepEqual(req.Args, []string{"telemetry", "off"}) || req.Dir != input.Dir {
					t.Fatal("semantic child after failed initialization", req)
				}
				result := hookcheck.ToolResult{Stdout: []byte("initialize\n"), Stderr: []byte("diagnostic\n")}
				switch kind {
				case "spawn":
					return result, sentinel
				case "nonzero":
					result.ExitCode = 7
				case "cancel":
					cancel()
				}
				return result, nil
			})
			if err == nil || calls != 1 {
				t.Fatal("initialization failure was ignored", calls, err)
			}
			switch kind {
			case "spawn", "stdout", "stderr":
				if !errors.Is(err, sentinel) {
					t.Fatal("lost initialization error", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("lost initialization cancellation", err)
				}
			case "short-output":
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatal("lost short write", err)
				}
			}
		})
	}
}

func TestCheckGoNoModulesStartsNoChild(t *testing.T) {
	tools, input, modules := goCheckFixture(t)
	for _, module := range modules {
		if err := os.Remove(filepath.Join(input.Dir, module.Directory, "go.mod")); err != nil {
			t.Fatal(err)
		}
	}
	putGoManifest(t, tools.Gofmt.Output, []goModuleBinding{})
	if err := checkGo(context.Background(), tools, input, io.Discard, io.Discard, func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
		t.Fatal("no-module check initialized Go")
		return hookcheck.ToolResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGoModulePreflightRefusesBeforeAnyGoChild(t *testing.T) {
	cases := []string{"stale-extension", "stale-root", "sum-added", "sum-empty-vs-absent", "sum-absent-vs-empty", "no-declaration", "duplicate", "invalid-directory", "missing-proxy", "mutable-proxy", "symlink-mod", "symlink-sum", "symlink-module", "missing-manifest", "symlink-binding", "bad-json", "unknown-field", "missing-field", "missing-wrapper", "symlink-wrapper", "bad-cache", "cancel"}
	for _, change := range cases {
		t.Run(change, func(t *testing.T) {
			tools, input, modules := goCheckFixture(t)
			bindingPath := filepath.Join(tools.Gofmt.Output, "share/confctl-hooks/go-modules.json")
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			switch change {
			case "stale-extension":
				write(filepath.Join(input.Dir, "extensions/go.mod"), []byte("module other.test/sdk\n"))
			case "stale-root":
				write(filepath.Join(input.Dir, "go.mod"), []byte("module changed.test/root\n"))
			case "sum-added":
				write(filepath.Join(input.Dir, "extensions/go.sum"), []byte("changed SDK sums\n"))
			case "sum-empty-vs-absent":
				write(filepath.Join(input.Dir, "extensions/go.sum"), nil)
			case "sum-absent-vs-empty":
				modules[1].GoSum = manifestFile(nil, true)
				putGoManifest(t, tools.Gofmt.Output, modules)
			case "no-declaration":
				putGoManifest(t, tools.Gofmt.Output, modules[:1])
			case "duplicate":
				putGoManifest(t, tools.Gofmt.Output, append(modules, modules[1]))
			case "invalid-directory":
				modules[1].Directory = "../escape"
				putGoManifest(t, tools.Gofmt.Output, modules)
			case "missing-proxy":
				p := "/nix/store/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-absent-confctl-proxy"
				modules[1].Proxy = &p
				putGoManifest(t, tools.Gofmt.Output, modules)
			case "mutable-proxy":
				p := t.TempDir()
				modules[1].Proxy = &p
				putGoManifest(t, tools.Gofmt.Output, modules)
			case "symlink-mod", "symlink-sum":
				name := "go.mod"
				if change == "symlink-sum" {
					name = "go.sum"
				}
				path := filepath.Join(input.Dir, "extensions", name)
				os.Remove(path)
				if err := os.Symlink(filepath.Join(input.Dir, "go.mod"), path); err != nil {
					t.Fatal(err)
				}
			case "symlink-module":
				if err := os.Rename(filepath.Join(input.Dir, "extensions"), filepath.Join(input.Dir, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("moved", filepath.Join(input.Dir, "extensions")); err != nil {
					t.Fatal(err)
				}
			case "missing-manifest":
				os.Remove(bindingPath)
			case "symlink-binding":
				os.Rename(bindingPath, bindingPath+".other")
				if err := os.Symlink(bindingPath+".other", bindingPath); err != nil {
					t.Fatal(err)
				}
			case "bad-json":
				write(bindingPath, []byte("{}\n{}"))
			case "unknown-field":
				write(bindingPath, []byte(`{"modules":[],"unapproved":true}`))
			case "missing-field":
				b, _ := os.ReadFile(bindingPath)
				write(bindingPath, bytes.Replace(b, []byte(`"proxy":null,`), nil, 1))
				if bytes.Equal(b, mustRead(t, bindingPath)) {
					write(bindingPath, bytes.Replace(b, []byte(`,"proxy":null`), nil, 1))
				}
			case "missing-wrapper":
				os.Remove(filepath.Join(tools.Gofmt.Output, "bin/go"))
			case "symlink-wrapper":
				p := filepath.Join(tools.Gofmt.Output, "bin/go")
				os.Remove(p)
				os.Symlink("gofmt", p)
			case "bad-cache":
				input.Env = append(input.Env, "GOCACHE=/operator/cache")
			}
			ctx := context.Background()
			if change == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			calls := 0
			var out bytes.Buffer
			err := checkGo(ctx, tools, input, &out, &out, func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
				calls++
				return hookcheck.ToolResult{}, nil
			})
			if err == nil || calls != 0 {
				t.Fatalf("%s: err %v, calls %d", change, err, calls)
			}
			if change == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoSumEmptyBindingAndNoModuleDefaults(t *testing.T) {
	tools, input, modules := goCheckFixture(t)
	for i, b := range modules {
		if err := os.WriteFile(filepath.Join(input.Dir, b.Directory, "go.sum"), nil, 0644); err != nil {
			t.Fatal(err)
		}
		modules[i].GoSum = manifestFile(nil, true)
	}
	putGoManifest(t, tools.Gofmt.Output, modules)
	if got, err := preflightGoModules(input.Dir, tools.Gofmt.Output); err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	empty := t.TempDir()
	putGoManifest(t, tools.Gofmt.Output, []goModuleBinding{})
	if got, err := preflightGoModules(empty, tools.Gofmt.Output); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

func TestPreparedExistingHEADManifestRemovalRefusesBeforeGoChild(t *testing.T) {
	for _, manifest := range []string{"go.mod", "go.sum", "extensions/go.mod", "extensions/go.sum"} {
		for _, change := range []string{"delete", "rename", "symlink", "gitlink", "symlink-to-regular"} {
			t.Run(manifest+"/"+change, func(t *testing.T) {
				q, tools := existingManifestHEAD(t)
				path := filepath.Join(q.Capture.Dir, manifest)
				switch change {
				case "delete":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					runGit(t, q, "add", "--", manifest)
				case "rename":
					runGit(t, q, "mv", "--", manifest, manifest+".backup")
				case "symlink":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("not-in-the-index", path); err != nil {
						t.Fatal(err)
					}
					runGit(t, q, "add", "--", manifest)
				case "symlink-to-regular":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("not-in-the-index", path); err != nil {
						t.Fatal(err)
					}
					runGit(t, q, "add", "--", manifest)
					tree := runGit(t, q, "write-tree")
					message := filepath.Join(t.TempDir(), "symlink-head-message")
					if err := os.WriteFile(message, []byte("Existing symlink manifest HEAD\n"), 0600); err != nil {
						t.Fatal(err)
					}
					head := runGit(t, q, "commit-tree", tree, "-F", message)
					runGit(t, q, "update-ref", "HEAD", head)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("stale regular manifest\n"), 0644); err != nil {
						t.Fatal(err)
					}
					runGit(t, q, "add", "--", manifest)
				case "gitlink":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					head := runGit(t, q, "rev-parse", "HEAD")
					runGit(t, q, "update-index", "--add", "--cacheinfo", "160000", head, manifest)
				}
				status := runGit(t, q, "diff", "--cached", "--name-status", "-z", "-M")
				want := "T"
				if change == "delete" {
					want = "D"
				}
				if change == "rename" {
					want = "R100"
				}
				if !strings.HasPrefix(status, want+"\x00") {
					t.Fatalf("not the real requested Git status: %q", status)
				}
				io := hookcheck.LocalTrustIO()
				io.IdentifyTool = func(ctx context.Context, want hookcheck.ToolIdentity) (hookcheck.ToolIdentity, error) {
					return want, ctx.Err()
				}
				proposal, _, err := hookcheck.PrepareTrust(context.Background(), q, io)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := hookcheck.CommitTrust(context.Background(), q, proposal, io); err != nil {
					t.Fatal(err)
				}
				callbacks, formatters, children := 0, 0, 0
				out, err := runPreparedHookFixture(t, context.Background(), hookcheck.HookRequest{TrustRequest: q, Phase: hookcheck.PreCommit}, io,
					func(ctx context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
						values := map[string]string{}
						for _, e := range req.Env {
							k, v, _ := strings.Cut(e, "=")
							values[k] = v
						}
						input, err := preparedInput([]string{"pre-commit"}, func(k string) (string, bool) { v, ok := values[k]; return v, ok })
						if err != nil {
							t.Fatal(err)
						}
						var output bytes.Buffer
						findings, checkErr := hookcheck.CheckPrepared(ctx, input, q.Tools,
							func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
								formatters++
								return hookcheck.ToolResult{}, nil
							},
							func(ctx context.Context, prepared hookcheck.Invocation) error {
								callbacks++
								return checkGo(ctx, tools, prepared, &output, &output, func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
									children++
									return hookcheck.ToolResult{}, nil
								})
							})
						if checkErr == nil || hookcheck.HasErrors(findings) {
							t.Fatalf("manifest removal skipped exact-presence refusal: %#v %v", findings, checkErr)
						}
						return hookcheck.ToolResult{ExitCode: 1}, nil
					})
				if err != nil || !out.Started || out.Result.ExitCode != 1 || callbacks != 1 || formatters != 0 || children != 0 {
					t.Fatalf("wrong refusal boundary: out=%#v err=%v callback=%d formatters=%d children=%d", out, err, callbacks, formatters, children)
				}
			})
		}
	}
}

// Git objects/HEAD are real disposable state; tools are injected models. No
// Lefthook, Go semantic child, hook installation or application is executed.
func existingManifestHEAD(t *testing.T) (hookcheck.TrustRequest, hookcheck.ToolSet) {
	t.Helper()
	q, _ := installerFixture(t)
	tools, source, modules := goCheckFixture(t)
	for i, binding := range modules {
		dir := filepath.Join(q.Capture.Dir, binding.Directory)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), mustRead(t, filepath.Join(source.Dir, binding.Directory, "go.mod")), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), nil, 0644); err != nil {
			t.Fatal(err)
		}
		modules[i].GoSum = manifestFile(nil, true)
	}
	putGoManifest(t, tools.Gofmt.Output, modules)
	model := func(name string) hookcheck.ToolIdentity {
		output := "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-checker-model"
		return hookcheck.ToolIdentity{Output: output, Program: output + "/bin/" + name, Resolved: output + "/bin/" + name, SHA256: sha256.Sum256([]byte(name))}
	}
	gitTool, err := identify(toolSpec{Output: filepath.Dir(filepath.Dir(q.Tools.Git.Program)), Program: q.Tools.Git.Program})
	if err != nil {
		t.Fatal(err)
	}
	q.Tools = hookcheck.ToolSet{Checker: model("checker"), Git: gitTool, Lefthook: model("lefthook"), Nixfmt: model("nixfmt"), Gofmt: tools.Gofmt, ChildBinding: hookcheck.ChildBinding{Path: []string{filepath.Dir(gitTool.Program)}, Locale: "C.UTF-8"}}
	q.Tools.Gofmt.Resolved = q.Tools.Gofmt.Program
	q.Tools.Gofmt.SHA256 = sha256.Sum256(mustRead(t, q.Tools.Gofmt.Program))
	policy, err := hookcheck.FixedConfig(q.Tools.Checker.Program)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.Capture.Dir, "lefthook.yml"), policy, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.Capture.Dir, "README"), []byte("original\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, q, "add", "--", ".")
	tree := runGit(t, q, "write-tree")
	message := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(message, []byte("Disposable existing HEAD\n"), 0600); err != nil {
		t.Fatal(err)
	}
	head := runGit(t, q, "commit-tree", tree, "-F", message)
	runGit(t, q, "update-ref", "HEAD", head)
	return q, tools
}

func TestPreparedExistingHEADUnchangedManifestsDoNotReachGo(t *testing.T) {
	for _, phase := range []hookcheck.HookPhase{hookcheck.PreCommit, hookcheck.CommitMsg} {
		t.Run(string(phase), func(t *testing.T) {
			q, _ := existingManifestHEAD(t)
			if phase == hookcheck.CommitMsg {
				if err := os.Remove(filepath.Join(q.Capture.Dir, "extensions/go.sum")); err != nil {
					t.Fatal(err)
				}
				runGit(t, q, "add", "--", "extensions/go.sum")
			} else {
				if err := os.WriteFile(filepath.Join(q.Capture.Dir, "README"), []byte("unrelated\n"), 0644); err != nil {
					t.Fatal(err)
				}
				runGit(t, q, "add", "--", "README")
			}
			io := hookcheck.LocalTrustIO()
			io.IdentifyTool = func(ctx context.Context, want hookcheck.ToolIdentity) (hookcheck.ToolIdentity, error) {
				return want, ctx.Err()
			}
			proposal, _, err := hookcheck.PrepareTrust(context.Background(), q, io)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hookcheck.CommitTrust(context.Background(), q, proposal, io); err != nil {
				t.Fatal(err)
			}
			request := hookcheck.HookRequest{TrustRequest: q, Phase: phase}
			if phase == hookcheck.CommitMsg {
				request.CommentPrefix = "#"
				request.MessagePath = filepath.Join(t.TempDir(), "message")
				if err := os.WriteFile(request.MessagePath, []byte("Fixture subject\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			callbacks := 0
			_, err = runPreparedHookFixture(t, context.Background(), request, io, func(ctx context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
				values := map[string]string{}
				for _, e := range req.Env {
					k, v, _ := strings.Cut(e, "=")
					values[k] = v
				}
				input, err := preparedInput([]string{string(phase)}, func(k string) (string, bool) { v, ok := values[k]; return v, ok })
				if err != nil {
					t.Fatal(err)
				}
				findings, err := hookcheck.CheckPrepared(ctx, input, q.Tools, nil, func(context.Context, hookcheck.Invocation) error { callbacks++; return nil })
				if err != nil || hookcheck.HasErrors(findings) {
					t.Fatal(findings, err)
				}
				return hookcheck.ToolResult{}, nil
			})
			if err != nil || callbacks != 0 {
				t.Fatal("unrelated or commit-msg reached Go", callbacks, err)
			}
		})
	}
}

func TestRunHookRemovesActualPrivateModuleCache(t *testing.T) {
	for _, kind := range []string{"success", "nonzero", "failure", "cancel", "foreign-root"} {
		t.Run(kind, func(t *testing.T) {
			q, _ := existingManifestHEAD(t)
			file := filepath.Join(q.Capture.Dir, "selected.go")
			if err := os.WriteFile(file, []byte("package fixture\n"), 0644); err != nil {
				t.Fatal(err)
			}
			runGit(t, q, "add", "--", "selected.go")
			fs := hookcheck.LocalTrustIO()
			fs.IdentifyTool = func(ctx context.Context, want hookcheck.ToolIdentity) (hookcheck.ToolIdentity, error) {
				return want, ctx.Err()
			}
			proposal, _, err := hookcheck.PrepareTrust(context.Background(), q, fs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hookcheck.CommitTrust(context.Background(), q, proposal, fs); err != nil {
				t.Fatal(err)
			}
			proxy := cacheExtractionProxy(t)
			goProgram, err := exec.LookPath("go")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("semantic child failure")
			var control, snapshot, cache string
			outside := filepath.Join(t.TempDir(), "foreign")
			if err := os.Mkdir(outside, 0755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Restore only this fixture's directory after the parent's
				// preservation checks, so testing can remove its own TempDir.
				if err := os.Chmod(outside, 0755); err != nil {
					t.Error(err)
				}
			})
			foreignFile := filepath.Join(outside, "keep")
			if err := os.WriteFile(foreignFile, []byte("foreign bytes"), 0444); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(outside, 0555); err != nil {
				t.Fatal(err)
			}
			out, err := runPreparedHookFixture(t, ctx, hookcheck.HookRequest{TrustRequest: q, Phase: hookcheck.PreCommit}, fs,
				func(ctx context.Context, req hookcheck.Invocation) (hookcheck.ToolResult, error) {
					values := map[string]string{}
					for _, e := range req.Env {
						k, v, _ := strings.Cut(e, "=")
						values[k] = v
					}
					input, err := preparedInput([]string{"pre-commit"}, func(k string) (string, bool) { v, ok := values[k]; return v, ok })
					if err != nil {
						t.Fatal(err)
					}
					snapshot = preparedSnapshotPath(t, input)
					control = filepath.Dir(input.ContextPath)
					findings, err := hookcheck.CheckPrepared(ctx, input, q.Tools,
						func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
							return hookcheck.ToolResult{}, nil
						},
						func(ctx context.Context, prepared hookcheck.Invocation) error {
							env, err := moduleGoEnv(prepared.Env, goModuleBinding{Proxy: &proxy})
							if err != nil {
								t.Fatal(err)
							}
							var tmp string
							for _, e := range env {
								k, v, _ := strings.Cut(e, "=")
								if k == "GOMODCACHE" {
									cache = v
								}
								if k == "TMPDIR" {
									tmp = v
								}
							}
							if cache != filepath.Join(control, "gopath", "pkg", "mod") {
								t.Fatal("unowned module cache", cache)
							}
							dir := filepath.Join(tmp, "module-download")
							if err := os.Mkdir(dir, 0700); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture.test/cache-download\n\ngo 1.23\n"), 0600); err != nil {
								t.Fatal(err)
							}
							result, err := invoke(ctx, hookcheck.Invocation{Program: goProgram, Args: []string{"telemetry", "off"}, Dir: prepared.Dir, Env: env})
							t.Logf("actual private telemetry disable: program=%s status=%d stderr=%s", goProgram, result.ExitCode, result.Stderr)
							if err != nil || result.ExitCode != 0 {
								t.Fatalf("telemetry disable: %v status=%d stdout=%s stderr=%s", err, result.ExitCode, result.Stdout, result.Stderr)
							}
							argv := []string{"mod", "download", "-json", "example.test/cachedep@v1.0.0"}
							result, err = invoke(ctx, hookcheck.Invocation{Program: goProgram, Args: argv, Dir: dir, Env: env})
							t.Logf("actual Go cache extraction: program=%s argv=%q status=%d stderr=%s", goProgram, argv, result.ExitCode, result.Stderr)
							if err != nil || result.ExitCode != 0 {
								t.Fatalf("cache extraction: %v status=%d stdout=%s stderr=%s", err, result.ExitCode, result.Stdout, result.Stderr)
							}
							var download struct {
								Dir   string
								Error *json.RawMessage
							}
							if err := json.Unmarshal(result.Stdout, &download); err != nil {
								t.Fatal(err)
							}
							if download.Error != nil || download.Dir != filepath.Join(cache, "example.test/cachedep@v1.0.0") {
								t.Fatal("module not actually extracted", string(result.Stdout))
							}
							info, err := os.Lstat(download.Dir)
							if err != nil || !info.IsDir() || info.Mode().Perm()&0200 == 0 {
								t.Fatal("private module directory is not writable", info, err)
							}
							t.Logf("extracted module directory mode=%04o", info.Mode().Perm())
							info, err = os.Lstat(filepath.Join(download.Dir, "dep.go"))
							if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0200 != 0 {
								t.Fatal("ordinary downloaded source mode changed", info, err)
							}
							t.Logf("extracted module source mode=%04o", info.Mode().Perm())
							if err := os.Symlink(outside, filepath.Join(cache, "foreign-link")); err != nil {
								t.Fatal(err)
							}
							if kind == "foreign-root" {
								if err := os.WriteFile(filepath.Join(control, "foreign"), []byte("preserve"), 0600); err != nil {
									t.Fatal(err)
								}
							}
							if kind == "failure" {
								return sentinel
							}
							if kind == "cancel" {
								cancel()
								return ctx.Err()
							}
							return nil
						})
					if hookcheck.HasErrors(findings) {
						t.Fatal(findings)
					}
					status := 0
					if kind == "nonzero" {
						status = 7
					}
					return hookcheck.ToolResult{ExitCode: status}, err
				})
			if !out.Started {
				t.Fatal("parent never ran", err)
			}
			switch kind {
			case "failure":
				if !errors.Is(err, sentinel) {
					t.Fatal("lost primary error", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("lost cancellation", err)
				}
			case "foreign-root":
				if err == nil {
					t.Fatal("foreign root was silently removed")
				}
			default:
				if err != nil {
					t.Fatal("owned cache caused cleanup rejection", err)
				}
			}
			if kind == "nonzero" && out.Result.ExitCode != 7 {
				t.Fatal("changed ordinary failure status", out)
			}
			for _, path := range []string{cache, snapshot} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("owned resource retained", path, err)
				}
			}
			if kind == "foreign-root" {
				if got := string(mustRead(t, filepath.Join(control, "foreign"))); got != "preserve" {
					t.Fatal("foreign root bytes changed")
				}
				if err := os.Remove(filepath.Join(control, "foreign")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(control); err != nil {
					t.Fatal(err)
				}
			} else if _, err := os.Lstat(control); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("control directory retained", err)
			}
			if got := string(mustRead(t, foreignFile)); got != "foreign bytes" {
				t.Fatal("cleanup followed module-cache symlink")
			}
			info, err := os.Lstat(outside)
			if err != nil || info.Mode().Perm() != 0555 {
				t.Fatal("foreign modes changed", info, err)
			}
			info, err = os.Lstat(foreignFile)
			if err != nil || info.Mode().Perm() != 0444 {
				t.Fatal("foreign source mode changed", info, err)
			}
		})
	}
}

func cacheExtractionProxy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "example.test/cachedep/@v")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	module := []byte("module example.test/cachedep\n\ngo 1.23\n")
	if err := os.WriteFile(filepath.Join(dir, "v1.0.0.mod"), module, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v1.0.0.info"), []byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`), 0644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "v1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range []struct {
		name string
		data []byte
	}{{"go.mod", module}, {"dep.go", []byte("package cachedep\n\nconst Value = 42\n")}} {
		header := &zip.FileHeader{Name: "example.test/cachedep@v1.0.0/" + entry.name, Method: zip.Store}
		header.SetMode(0644)
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}
