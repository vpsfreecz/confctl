package hookcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Only disposable object/index/ref fixtures are used. There are no
// Git commit, installed-hook, trust, configuration, or worktree transitions.
func testGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureEnv() []string {
	// No credentials or unrelated inherited controls enter fixture invocations
	// or assertion diagnostics. Production Capture still preserves supplied env.
	return []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@", "GIT_OPTIONAL_LOCKS=0"}
}

func realInvoke(ctx context.Context, req Invocation) (ToolResult, error) {
	cmd := exec.CommandContext(ctx, req.Program, req.Args...)
	cmd.Dir, cmd.Env = req.Dir, req.Env
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := ToolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func fixtureGit(t *testing.T, root string, env []string, input []byte, args ...string) []byte {
	t.Helper()
	r, err := realInvoke(context.Background(), Invocation{testGit(t), args, root, env, input})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("fixture Git %q: status %d err %v stderr %s", args, r.ExitCode, err, r.Stderr)
	}
	return r.Stdout
}

func newFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, env := t.TempDir(), fixtureEnv()
	fixtureGit(t, root, env, nil, "init", "--quiet")
	return root, env
}

func put(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func stage(t *testing.T, root string, env []string, names ...string) {
	t.Helper()
	fixtureGit(t, root, env, nil, append([]string{"add", "--"}, names...)...)
}

func captureFixture(t *testing.T, root string, env []string) *Snapshot {
	t.Helper()
	s, err := Capture(context.Background(), CaptureOptions{root, testGit(t), t.TempDir(), env, realInvoke})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(nil); err != nil {
			t.Error(err)
		}
	})
	return s
}

type fileState struct {
	Mode fs.FileMode
	Data string
}

// This includes the actual index, all Git metadata, unstaged and untracked files.
func fixtureState(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := make(map[string]fileState)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		var data []byte
		if entry.Type()&os.ModeSymlink != 0 {
			value, err := os.Readlink(path)
			if err != nil {
				return err
			}
			data = []byte(value)
		} else if !entry.IsDir() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = fileState{info.Mode(), string(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCaptureInitialAndAlternateIndexPreserveLiteralTree(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		t.Run(fmt.Sprint(alternate), func(t *testing.T) {
			root, env := newFixture(t)
			names := []string{"plain", " edge space ", "line\nbreak", "quote'\"$(inert).nix", "příliš-東京", "dir/target", "policy.conf"}
			for _, name := range names {
				put(t, root, name, "staged:"+name)
			}
			if err := os.Chmod(filepath.Join(root, "plain"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("dir/target", filepath.Join(root, "literal-link")); err != nil {
				t.Fatal(err)
			}
			stage(t, root, env, ".")
			index := filepath.Join(root, ".git", "index")
			if alternate {
				data, err := os.ReadFile(index)
				if err != nil {
					t.Fatal(err)
				}
				index = filepath.Join(root, " alternate\nindex ")
				if err := os.WriteFile(index, data, 0600); err != nil {
					t.Fatal(err)
				}
				// Relative literal value is resolved in the original caller CWD.
				env = append(env, "GIT_INDEX_FILE= alternate\nindex ")
			}
			put(t, root, "plain", "unstaged different bytes")
			put(t, root, "policy.conf", "unstaged policy")
			put(t, root, "untracked", "do not copy or remove")
			before := fixtureState(t, root)
			var calls []Invocation
			s, err := Capture(context.Background(), CaptureOptions{root, testGit(t), t.TempDir(), env,
				func(ctx context.Context, req Invocation) (ToolResult, error) {
					calls = append(calls, req)
					return realInvoke(ctx, req)
				}})
			if err != nil {
				t.Fatal(err)
			}
			if s.Origin() != root || s.GitDirectory() != filepath.Join(root, ".git") || s.IndexFile() != index {
				t.Fatalf("wrong original identity: %s %s %s", s.Origin(), s.GitDirectory(), s.IndexFile())
			}
			for _, name := range names {
				b, err := os.ReadFile(filepath.Join(s.Directory(), name))
				if err != nil || string(b) != "staged:"+name {
					t.Fatalf("staged bytes for %q: %q %v", name, b, err)
				}
			}
			if target, err := os.Readlink(filepath.Join(s.Directory(), "literal-link")); err != nil || target != "dir/target" {
				t.Fatalf("literal symlink: %q %v", target, err)
			}
			info, _ := os.Stat(filepath.Join(s.Directory(), "plain"))
			if info.Mode().Perm() != 0755 {
				t.Fatalf("staged executable mode: %v", info.Mode())
			}
			if _, err := os.Stat(filepath.Join(s.Directory(), "untracked")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("untracked file entered snapshot")
			}
			want := append(append([]string{}, names...), "literal-link")
			sort.Strings(want)
			if !reflect.DeepEqual(s.Selected(), want) {
				t.Fatalf("selected literal names: %q != %q", s.Selected(), want)
			}
			copy := s.Selected()
			copy[0] = "changed"
			if reflect.DeepEqual(copy, s.Selected()) {
				t.Fatal("snapshot metadata aliases caller")
			}
			if alternate {
				for _, call := range calls {
					value, present := envValue(call.Env, "GIT_INDEX_FILE")
					if !present || value != " alternate\nindex " {
						t.Fatalf("alternate-index context changed: %#v", call)
					}
				}
			}
			if err := s.Close(nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("capture/cleanup changed source/index/worktree/untracked state")
			}
		})
	}
}

func TestCapturePartialStagingBothDirections(t *testing.T) {
	for _, pair := range [][2]string{{"safe\n", "<<<<<<< unstaged\n"}, {"<<<<<<< staged\n", "safe\n"}} {
		t.Run(pair[0], func(t *testing.T) {
			root, env := newFixture(t)
			put(t, root, "marker.txt", pair[0])
			stage(t, root, env, "marker.txt")
			put(t, root, "marker.txt", pair[1])
			before := fixtureState(t, root)
			s := captureFixture(t, root, env)
			findings, err := CheckFiles(context.Background(), s, CheckOptions{})
			if err != nil || HasErrors(findings) != strings.HasPrefix(pair[0], "<<<<<<<") {
				t.Fatalf("checks did not observe staged revision: %#v %v", findings, err)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("checking changed actual state")
			}
		})
	}
}

func TestCaptureReplacementRefsKeepOriginalStagedBlob(t *testing.T) {
	for _, namespace := range []string{"refs/replace/", "refs/fixture-replacements/"} {
		t.Run(namespace, func(t *testing.T) {
			root, env := newFixture(t)
			const marker = "<<<<<<< staged conflict\n"
			const clean = "clean replacement\n"
			put(t, root, "marker.txt", marker)
			stage(t, root, env, "marker.txt")
			index := filepath.Join(root, " alternate index ")
			data, err := os.ReadFile(filepath.Join(root, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(index, data, 0600); err != nil {
				t.Fatal(err)
			}
			env = append(env, "GIT_INDEX_FILE= alternate index ")
			original := strings.TrimSpace(string(fixtureGit(t, root, env, []byte(marker), "hash-object", "--stdin")))
			replacement := strings.TrimSpace(string(fixtureGit(t, root, env, []byte(clean), "hash-object", "-w", "--stdin")))
			fixtureGit(t, root, env, nil, "update-ref", namespace+original, replacement)
			if namespace != "refs/replace/" {
				env = append(env, "GIT_REPLACE_REF_BASE="+namespace)
			}
			// Establish that this actual replacement changes an ordinary object read.
			if got := fixtureGit(t, root, env, nil, "cat-file", "blob", original); string(got) != clean {
				t.Fatalf("replacement fixture is ineffective: %q", got)
			}
			put(t, root, "marker.txt", "unstaged clean text")
			before, originalEnv := fixtureState(t, root), append([]string(nil), env...)
			defer func() {
				if !reflect.DeepEqual(before, fixtureState(t, root)) || !reflect.DeepEqual(env, originalEnv) {
					t.Error("replacement capture changed original index/ref/worktree/context")
				}
			}()
			var calls []Invocation
			s, err := Capture(context.Background(), CaptureOptions{root, testGit(t), t.TempDir(), env,
				func(ctx context.Context, req Invocation) (ToolResult, error) {
					calls = append(calls, req)
					return realInvoke(ctx, req)
				}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(nil); err != nil {
					t.Error(err)
				}
			}()
			if got, err := os.ReadFile(filepath.Join(s.Directory(), "marker.txt")); err != nil || string(got) != marker {
				t.Fatalf("replacement concealed staged bytes: %q %v", got, err)
			}
			findings, err := CheckFiles(context.Background(), s, CheckOptions{})
			if err != nil || len(findings) != 1 || findings[0].Check != "MergeConflicts" || !HasErrors(findings) {
				t.Fatalf("staged marker was concealed by replacement: %#v %v", findings, err)
			}
			if s.IndexFile() != index || len(s.Entries()) != 1 || s.Entries()[0].ObjectID != original {
				t.Fatalf("original staged object/index identity changed: %#v", s)
			}
			for _, call := range calls {
				if v, _ := envValue(call.Env, "GIT_NO_REPLACE_OBJECTS"); v != "1" {
					t.Fatalf("collection permits replacements: %q", call.Args)
				}
				if v, _ := envValue(call.Env, "GIT_INDEX_FILE"); v != " alternate index " || call.Dir != root {
					t.Fatal("original alternate-index coordinates changed")
				}
			}
		})
	}
}

func TestCaptureSubdirectoryRelativeIndexKeepsWholeRootTree(t *testing.T) {
	for _, relative := range []string{"false", "true"} {
		t.Run("diff.relative="+relative, func(t *testing.T) {
			root, env := newFixture(t)
			contents := map[string]string{
				"policy.conf":        "staged root config",
				"sibling/config.nix": "staged sibling import",
				"sub/file.nix":       "staged subdirectory file",
			}
			for name, content := range contents {
				put(t, root, name, content)
			}
			stage(t, root, env, ".")
			cwd := filepath.Join(root, "sub")
			const literalIndex = ".git/ alternate\nindex "
			env = append(env, "GIT_INDEX_FILE="+literalIndex,
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=diff.relative", "GIT_CONFIG_VALUE_0="+relative)
			// Resolve through Git in the original CWD: its repository setup defines
			// the effective relative-index coordinates, not filepath.Join(cwd, value).
			index := strings.TrimSuffix(string(fixtureGit(t, cwd, env, nil,
				"rev-parse", "--path-format=absolute", "--git-path", "index")), "\n")
			if index != filepath.Join(root, literalIndex) {
				t.Fatalf("unexpected fixture index identity: %q", index)
			}
			data, err := os.ReadFile(filepath.Join(root, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(index, data, 0600); err != nil {
				t.Fatal(err)
			}
			put(t, root, "primary-only", "absent from alternate index")
			stage(t, root, fixtureEnv(), "primary-only")
			for name := range contents {
				put(t, root, name, "unstaged different bytes")
			}
			put(t, root, "sub/untracked", "retained")
			before, originalEnv := fixtureState(t, root), append([]string(nil), env...)
			defer func() {
				if !reflect.DeepEqual(before, fixtureState(t, root)) || !reflect.DeepEqual(env, originalEnv) {
					t.Error("subdirectory capture changed original index/worktree/context")
				}
			}()
			var calls []Invocation
			s, err := Capture(context.Background(), CaptureOptions{cwd, testGit(t), t.TempDir(), env,
				func(ctx context.Context, req Invocation) (ToolResult, error) {
					calls = append(calls, req)
					return realInvoke(ctx, req)
				}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(nil); err != nil {
					t.Error(err)
				}
			}()
			if s.Origin() != root || s.GitDirectory() != filepath.Join(root, ".git") || s.IndexFile() != index {
				t.Fatalf("subdirectory repository/index identity: %#v", s)
			}
			want := []string{"policy.conf", "sibling/config.nix", "sub/file.nix"}
			var paths []string
			for _, entry := range s.Entries() {
				paths = append(paths, entry.Path)
			}
			if !reflect.DeepEqual(paths, want) || !reflect.DeepEqual(s.Selected(), want) {
				t.Fatalf("whole-root index/destination coordinates: %q %q", paths, s.Selected())
			}
			for name, content := range contents {
				if got, err := os.ReadFile(filepath.Join(s.Directory(), name)); err != nil || string(got) != content {
					t.Fatalf("whole-root staged bytes for %q: %q %v", name, got, err)
				}
			}
			for _, name := range []string{"primary-only", "sub/untracked"} {
				if _, err := os.Lstat(filepath.Join(s.Directory(), name)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("unstaged/other-index path entered snapshot: %q %v", name, err)
				}
			}
			for _, call := range calls {
				if value, _ := envValue(call.Env, "GIT_INDEX_FILE"); value != literalIndex || call.Dir != cwd {
					t.Fatal("literal relative index/original CWD was rebased")
				}
			}
		})
	}
}

func TestCaptureEmptyIndexAndRelativeTempParent(t *testing.T) {
	root, env := newFixture(t)
	before := fixtureState(t, root)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Capture(context.Background(), CaptureOptions{root, testGit(t), relative, env, realInvoke})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.Directory()) || len(s.Entries()) != 0 || len(s.Selected()) != 0 {
		t.Fatalf("empty snapshot identity: %#v", s)
	}
	if err := s.Close(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("initial missing index was written")
	}
}

func TestCaptureInjectedHEADAndWholeTreeWithoutCommitFixture(t *testing.T) {
	root, env := newFixture(t)
	put(t, root, "dir/selected", "staged selection")
	put(t, root, "config", "whole staged config")
	stage(t, root, env, ".")
	before := fixtureState(t, root)
	var requests []Invocation
	run := func(ctx context.Context, req Invocation) (ToolResult, error) {
		requests = append(requests, req)
		if reflect.DeepEqual(req.Args, []string{"rev-parse", "--verify", "--quiet", "HEAD"}) {
			return ToolResult{Stdout: []byte(strings.Repeat("1", 40) + "\n")}, nil
		}
		if req.Args[0] == "ls-tree" {
			return ToolResult{Stdout: []byte("100644 blob " + strings.Repeat("2", 40) + "\tdir/SELECTED\x00")}, nil
		}
		if req.Args[0] == "diff" {
			return ToolResult{Stdout: []byte("M\x00dir/selected\x00")}, nil
		}
		return realInvoke(ctx, req)
	}
	s, err := Capture(context.Background(), CaptureOptions{root, testGit(t), t.TempDir(), env, run})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Selected(), []string{"dir/selected"}) || len(caseFindings(s)) != 1 {
		t.Fatalf("HEAD/selected destination binding: %#v", s)
	}
	if b, err := os.ReadFile(filepath.Join(s.Directory(), "config")); err != nil || string(b) != "whole staged config" {
		t.Fatal("unchanged staged import/config missing from tree")
	}
	for _, req := range requests {
		if req.Args[0] == "diff" && !strings.Contains(strings.Join(req.Args, " "), "--diff-filter=ACMRT") {
			t.Fatal("unexpected cached status capture")
		}
	}
	if err := s.Close(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("injected HEAD collection changed real fixture")
	}
}

func TestCaptureUnmergedAndGitlinkEntries(t *testing.T) {
	for _, unmerged := range []bool{false, true} {
		t.Run(fmt.Sprint(unmerged), func(t *testing.T) {
			root, env := newFixture(t)
			put(t, root, "normal", "blob")
			stage(t, root, env, "normal")
			oid := strings.TrimSpace(string(fixtureGit(t, root, env, []byte("blob"), "hash-object", "-w", "--stdin")))
			if unmerged {
				fixtureGit(t, root, env, []byte("100644 "+oid+" 1\tunmerged\n"), "update-index", "--index-info")
			} else {
				fixtureGit(t, root, env, nil, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",module")
			}
			before, temp := fixtureState(t, root), t.TempDir()
			s, err := Capture(context.Background(), CaptureOptions{root, testGit(t), temp, env, realInvoke})
			if unmerged {
				if err == nil || !strings.Contains(err.Error(), "unmerged") {
					t.Fatalf("unmerged index accepted: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(s.Selected(), []string{"normal"}) {
					t.Fatalf("gitlink selected: %q", s.Selected())
				}
				if _, err := os.Lstat(filepath.Join(s.Directory(), "module")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("gitlink materialized")
				}
				if err := s.Close(nil); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("index/read failure changed fixture")
			}
		})
	}
}

func TestNULRenameCopyDestinationsAndRefusals(t *testing.T) {
	entries := []Entry{{Path: " renamed\n", Mode: 0100644}, {Path: "copy'$(x)", Mode: 0100644}, {Path: "module", Mode: 0160000}}
	selected, err := parseDestinations([]byte("R100\x00old\x00 renamed\n\x00C090\x00old\x00copy'$(x)\x00A\x00module\x00"), entries)
	if err != nil || !reflect.DeepEqual(selected, []string{" renamed\n", "copy'$(x)"}) {
		t.Fatalf("destinations: %q %v", selected, err)
	}
	for _, raw := range []string{"R100\x00old\x00", "A\x00absent\x00", "D\x00old\x00", "M\x00copy'$(x)"} {
		if _, err := parseDestinations([]byte(raw), entries); err == nil {
			t.Fatalf("invalid diff accepted: %q", raw)
		}
	}
	for _, name := range []string{"../escape", "/absolute", ".git/config", "x/.GIT/index", "x//y"} {
		raw := "100644 " + strings.Repeat("1", 40) + " 0\t" + name + "\x00"
		if _, err := parseEntries([]byte(raw)); err == nil {
			t.Fatalf("unsafe staged path accepted: %q", name)
		}
	}
}

func TestCaptureIndexMutationFailureAndCancellation(t *testing.T) {
	for _, failure := range []string{"index-mode-change", "index-byte-change", "head-change", "blob-spawn", "cancel-after-materialization"} {
		t.Run(failure, func(t *testing.T) {
			root, env := newFixture(t)
			put(t, root, "file", "staged")
			stage(t, root, env, "file")
			put(t, root, "untracked", "retained")
			before, temp := fixtureState(t, root), t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			headReads := 0
			boom := errors.New("injected spawn failure")
			run := func(ctx context.Context, req Invocation) (ToolResult, error) {
				if req.Args[0] == "cat-file" && failure == "blob-spawn" {
					return ToolResult{}, boom
				}
				r, err := realInvoke(ctx, req)
				if req.Args[0] == "cat-file" && failure == "index-mode-change" {
					if err := os.Chmod(filepath.Join(root, ".git/index"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if req.Args[0] == "cat-file" && failure == "index-byte-change" {
					data := []byte(before[".git/index"].Data + "changed")
					if err := os.WriteFile(filepath.Join(root, ".git/index"), data, before[".git/index"].Mode.Perm()); err != nil {
						t.Fatal(err)
					}
				}
				if reflect.DeepEqual(req.Args, []string{"rev-parse", "--verify", "--quiet", "HEAD"}) {
					headReads++
					if headReads == 2 && failure == "cancel-after-materialization" {
						cancel()
					}
					if headReads == 2 && failure == "head-change" {
						return ToolResult{Stdout: []byte(strings.Repeat("1", 40) + "\n")}, nil
					}
				}
				return r, err
			}
			s, err := Capture(ctx, CaptureOptions{root, testGit(t), temp, env, run})
			if err == nil || s != nil {
				t.Fatalf("failure accepted: %#v %v", s, err)
			}
			if failure == "blob-spawn" && !errors.Is(err, boom) {
				t.Fatalf("spawn failure masked: %v", err)
			}
			if failure == "cancel-after-materialization" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation masked: %v", err)
			}
			contents, _ := os.ReadDir(temp)
			if len(contents) != 0 {
				t.Fatal("failed collection left an owned snapshot")
			}
			after := fixtureState(t, root)
			if failure == "index-mode-change" {
				// Only the test's deliberate index-mode change is permitted.
				row := after[".git/index"]
				row.Mode = before[".git/index"].Mode
				after[".git/index"] = row
			}
			if failure == "index-byte-change" {
				row := after[".git/index"]
				row.Data = before[".git/index"].Data
				after[".git/index"] = row
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failure changed real index/worktree/untracked state")
			}
		})
	}
}

func TestSnapshotCleanupPrimaryErrorAndDirectoryIdentity(t *testing.T) {
	root, env := newFixture(t)
	put(t, root, "file", "staged")
	stage(t, root, env, "file")
	s := captureFixture(t, root, env)
	primary, cleanup := errors.New("primary check failure"), errors.New("unlink refusal")
	s.remove = func(string) error { return cleanup }
	err := s.Close(primary)
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("cleanup precedence: %v", err)
	}
	s.remove = os.RemoveAll
	dir := s.Directory()
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "foreign", "must remain")
	if err := s.Close(primary); err == nil || !errors.Is(err, primary) {
		t.Fatalf("replaced snapshot cleanup accepted: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "foreign")); err != nil || string(b) != "must remain" {
		t.Fatal("cleanup touched replaced directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
}

func TestTypeChangedDestinationsSelectOnlyRegularModuleManifests(t *testing.T) {
	entries := []Entry{
		{Path: "go.mod", Mode: 0100644}, {Path: "extensions/go.sum", Mode: 0100755},
		{Path: "other.go", Mode: 0100644}, {Path: "go.sum", Mode: 0120000},
		{Path: "extensions/go.mod", Mode: 0160000},
	}
	raw := []byte("T\x00go.mod\x00T\x00extensions/go.sum\x00T\x00other.go\x00T\x00go.sum\x00T\x00extensions/go.mod\x00")
	got, err := parseDestinations(raw, entries)
	if err != nil || !reflect.DeepEqual(got, []string{"extensions/go.sum", "go.mod"}) {
		t.Fatal(got, err)
	}
}
