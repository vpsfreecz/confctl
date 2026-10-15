package inputs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// Inline stimuli exercise the pinned setter.rb operation boundaries. They are
// private unit inputs, not original CLI observations or replacement goldens.
const setOld = `{"nodes":{"root":{"inputs":{"a":"a"}},"a":{"locked":{"type":"github","owner":"old","repo":"repo","rev":"1111111111"},"original":{"type":"github","owner":"saved","repo":"repo"}}},"root":"ignored","version":7}`
const setNew = `{"nodes":{"root":{"inputs":{"a":"a"}},"a":{"locked":{"type":"github","owner":"old","repo":"repo","rev":"2222222222"},"original":{"type":"github","rev":"override"}}},"root":"ignored","version":7}`
const setPretty = "{\n  \"nodes\": {\n    \"root\": {\n      \"inputs\": {\n        \"a\": \"a\"\n      }\n    },\n    \"a\": {\n      \"locked\": {\n        \"type\": \"github\",\n        \"owner\": \"old\",\n        \"repo\": \"repo\",\n        \"rev\": \"2222222222\"\n      },\n      \"original\": {\n        \"type\": \"github\",\n        \"owner\": \"saved\",\n        \"repo\": \"repo\"\n      }\n    }\n  },\n  \"root\": \"ignored\",\n  \"version\": 7\n}\n"

func setTestArgument(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func setTestRoot(t *testing.T, data string) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := root + "/flake.lock"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func setTestOps() setOps {
	ops := defaultSetOps()
	ops.PID = func() int { return 42 }
	count := byte(0)
	ops.Random = func() ([6]byte, error) {
		count++
		return [6]byte{count, 0xab, 0xcd, 0xef, 0x00, 0xff}, nil
	}
	return ops
}

func setTestDeps(t *testing.T, root, data string) SetDeps {
	t.Helper()
	return SetDeps{Argument: setTestArgument, Run: func(_ context.Context, cwd string, argv []string) (Output, error) {
		if cwd != root {
			t.Fatalf("cwd %q", cwd)
		}
		return Output{}, os.WriteFile(argv[len(argv)-1], []byte(data), 0600)
	}}
}

func setTestPhase(t *testing.T, err error, phase SetPhase) *SetError {
	t.Helper()
	var failure *SetError
	if !errors.As(err, &failure) || failure.Phase != phase || failure.Error() != failure.Err.Error() {
		t.Fatalf("phase %v: %#v", phase, err)
	}
	return failure
}

func TestSetIndependentReadsExactReplacementAndFinalReread(t *testing.T) {
	root, path := setTestRoot(t, setOld)
	snapshot := strings.Replace(setOld, `"owner":"saved"`, `"owner":"second","unknown":[false,9007199254740993,{"z":-0.0}]`, 1)
	ops := setTestOps()
	reads := 0
	ops.ReadFile = func(p string) ([]byte, error) {
		if p == path {
			reads++
			if reads == 2 {
				return []byte(snapshot), nil
			}
		}
		return os.ReadFile(p)
	}
	deps := setTestDeps(t, root, setNew)
	run := deps.Run
	deps.Run = func(ctx context.Context, cwd string, argv []string) (Output, error) {
		want := []string{"nix", "flake", "lock", "--update-input", "a", "--override-input", "a", "github:old/repo/opaque?&/#", "--output-lock-file", root + "/flake.lock.tmp-42-01abcdef00ff"}
		if !reflect.DeepEqual(argv, want) || reads != 2 {
			t.Fatal(argv, reads)
		}
		if _, err := os.Lstat(argv[len(argv)-1]); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("TEMP was precreated", err)
		}
		return run(ctx, cwd, argv)
	}
	result, err := setWithOps(context.Background(), root, []any{"a"}, "opaque?&/#", UpdateOptions{}, deps, ops)
	if err != nil || reads != 3 || len(result.Changes) != 1 || result.Changes[0].Old.Rev != "1111111111" || result.Changes[0].New.Rev != "2222222222" {
		t.Fatal(result, err, reads)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasSuffix(data, []byte("}\n")) || bytes.HasSuffix(data, []byte("\n\n")) || !bytes.Contains(data, []byte(`"owner": "second"`)) || !bytes.Contains(data, []byte("9007199254740993")) || !bytes.Contains(data, []byte("-0.0")) {
		t.Fatal(string(data), err)
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm() != 0600 {
		t.Fatal(stat.Mode())
	}
	view, err := DecodeLock(data)
	if err != nil {
		t.Fatal(err)
	}
	view.data["nodes"].(map[string]any)["a"].(map[string]any)["original"] = nil
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, data) {
		t.Fatal("projection aliased file")
	}
}

func TestSetDeferredConsumersAndPrevalidateAll(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, message string
		phase                        SetPhase
	}{
		{"second-parse-before-first-shape", `false`, `{`, "", SnapshotRead},
		{"snapshot-scalar", setOld, `false`, "expected input object for dig", Validate},
		{"first-scalar-deferred", `false`, setOld, "expected input object for dig", Validate},
		{"snapshot-false-node-dig", setOld, `{"nodes":{"root":{"inputs":{"a":"a"}},"a":false}}`, "expected input object for dig", Validate},
		{"first-locked-scalar", strings.Replace(setOld, `"locked":{`, `"locked":7,"ignored":{`, 1), setOld, "", Validate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, path := setTestRoot(t, tc.first)
			ops := setTestOps()
			read := 0
			ops.ReadFile = func(p string) ([]byte, error) {
				if p != path {
					t.Fatal("TEMP read before validation")
				}
				read++
				if read == 1 {
					return []byte(tc.first), nil
				}
				return []byte(tc.second), nil
			}
			ops.Random = func() ([6]byte, error) { t.Fatal("name before validation"); return [6]byte{}, nil }
			deps := SetDeps{Argument: setTestArgument, Run: func(context.Context, string, []string) (Output, error) {
				t.Fatal("Run before validation")
				return Output{}, nil
			}}
			_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, ops)
			setTestPhase(t, err, tc.phase)
			if tc.message != "" && err.Error() != tc.message {
				t.Fatal(err)
			}
			if read != 2 {
				t.Fatal("independent read count", read)
			}
		})
	}
	root, _ := setTestRoot(t, setOld)
	ops := setTestOps()
	ops.PID = func() int { t.Fatal("PID before all names validated"); return 0 }
	deps := SetDeps{Argument: setTestArgument, Run: func(context.Context, string, []string) (Output, error) {
		t.Fatal("Run before all names validated")
		return Output{}, nil
	}}
	_, err := setWithOps(context.Background(), root, []any{"a", "missing"}, "rev", UpdateOptions{}, deps, ops)
	if err == nil || err.Error() != "unknown input 'missing'" {
		t.Fatal(err)
	}
	_, err = setWithOps(context.Background(), root, []any{false}, "rev", UpdateOptions{}, deps, ops)
	if err == nil || err.Error() != "unknown input 'false'" {
		t.Fatal("nonstring was converted to a key", err)
	}
}

func TestSetOverrideURLsSourceTruthiness(t *testing.T) {
	for _, tc := range []struct{ locked, original, want, failure string }{
		{`{"type":"github","owner":"","repo":"r","dir":""}`, `{"owner":"fallback","repo":"other","dir":"src"}`, "github:/r/x?&/#?dir=", ""},
		{`{"type":"github","owner":false,"repo":"r","dir":false}`, `{"owner":"o","dir":"nested/a"}`, "github:o/r/x?&/#?dir=nested/a", ""},
		{`{"type":"github","repo":"r"}`, `{}`, "", "missing github owner or repo"},
		{`{"type":"git","url":"git+git+https://repo?ref=main#fragment"}`, `{}`, "git+git+https://repo?ref=main#fragment&rev=x?&/#", ""},
		{`{"type":"git","url":""}`, `{"url":"fallback"}`, "git+?rev=x?&/#", ""},
		{`{"type":"git","url":false}`, `{"url":"git+ssh://repo"}`, "git+ssh://repo?rev=x?&/#", ""},
		{`{"type":"git"}`, `{}`, "", "missing git url"},
		{`{"type":"git","url":7}`, `{}`, "", "expected git URL string"},
	} {
		t.Run(tc.locked, func(t *testing.T) {
			doc, err := parseOrderedLock([]byte(`{"nodes":{"root":{"inputs":{"a":"a"}},"a":{"locked":` + tc.locked + `,"original":` + tc.original + `}}}`))
			if err != nil {
				t.Fatal(err)
			}
			info, err := setInputInfo(doc, "a", setTestArgument)
			if err != nil {
				t.Fatal(err)
			}
			url, err := setOverrideURL(info, "x?&/#", setTestArgument)
			if tc.failure != "" {
				if err == nil || err.Error() != tc.failure {
					t.Fatal(url, err)
				}
				return
			}
			if err != nil || url != tc.want {
				t.Fatal(url, err, tc.want)
			}
		})
	}
}

func TestSetRestorationAliasesOrderedDetachedValues(t *testing.T) {
	snapshot, err := parseOrderedLock([]byte(`{"nodes":{"root":{"inputs":{"a":"a","b":["b","ignored"],"c":"c","d":"d"}},"a":{"original":{"first":1,"big":123456789012345678901234567890}},"b":{"original":false},"c":{"original":null},"d":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	plans := []setPlan{}
	for _, name := range []any{"a", "b", "c", "d", "a"} {
		id, err := setResolveNodeID(snapshot, name, setTestArgument)
		if err != nil {
			t.Fatal(err)
		}
		original, err := setDig(snapshot.root, "nodes", id, "original")
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, setPlan{name: name, original: original.clone()})
	}
	doc, err := parseOrderedLock([]byte(`{"nodes":{"root":{"inputs":{"a":"same","b":"same","c":"c","d":"d"}},"same":{"before":0,"original":"override","after":{"unknown":[-0.0,9007199254740993]}},"c":{"before":0,"original":"delete","after":1},"d":false},"outer":[true,null]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := setRestoreOriginals(doc, plans, setTestArgument); err != nil {
		t.Fatal(err)
	}
	data, err := prettyOrderedLock(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"nodes\": {\n    \"root\": {\n      \"inputs\": {\n        \"a\": \"same\",\n        \"b\": \"same\",\n        \"c\": \"c\",\n        \"d\": \"d\"\n      }\n    },\n    \"same\": {\n      \"before\": 0,\n      \"original\": {\n        \"first\": 1,\n        \"big\": 123456789012345678901234567890\n      },\n      \"after\": {\n        \"unknown\": [\n          -0.0,\n          9007199254740993\n        ]\n      }\n    },\n    \"c\": {\n      \"before\": 0,\n      \"after\": 1\n    },\n    \"d\": {}\n  },\n  \"outer\": [\n    true,\n    null\n  ]\n}"
	if string(data) != want {
		t.Fatal(string(data))
	}
	// Removing the final repeated a leaves false, rather than deleting it.
	if err := setRestoreOriginals(doc, plans[:2], setTestArgument); err != nil {
		t.Fatal(err)
	}
	value, _ := setDig(doc.root, "nodes", "same", "original")
	if value.kind != orderedBool || value.boolean {
		t.Fatal(value)
	}
	if err := setRestoreOriginals(doc, plans[:1], setTestArgument); err != nil {
		t.Fatal(err)
	}
	plans[0].original.set("first", &orderedLockValue{kind: orderedString, text: "mutated"})
	value, _ = setDig(doc.root, "nodes", "same", "original", "first")
	if value.integer.String() != "1" {
		t.Fatal("restoration retained saved alias")
	}
	view, _ := doc.lockView()
	view.data["nodes"].(map[string]any)["same"].(map[string]any)["original"] = nil
	value, _ = setDig(doc.root, "nodes", "same", "original")
	if value.kind != orderedObject {
		t.Fatal("view retained tree alias")
	}
}

func TestSetRetryUsesSameTempAndGuardBeforeRead(t *testing.T) {
	for _, mode := range []string{"retry", "second-failure", "spawn", "nonmatching", "guard", "rate"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := setTestRoot(t, setOld)
			ops := setTestOps()
			deps := setTestDeps(t, root, setNew)
			run := deps.Run
			calls := [][]string{}
			tempReads := 0
			read := ops.ReadFile
			ops.ReadFile = func(p string) ([]byte, error) {
				if strings.Contains(p, ".tmp-") {
					tempReads++
				}
				return read(p)
			}
			ordinary := errors.New("experimental flakes spawn refused")
			exit := &ExitError{Command: "nix", Code: 1, Output: Output{Stderr: "Experimental nix-command disabled"}}
			deps.Run = func(ctx context.Context, cwd string, argv []string) (Output, error) {
				calls = append(calls, append([]string(nil), argv...))
				if mode == "spawn" {
					return Output{}, ordinary
				}
				if mode == "nonmatching" {
					return Output{}, &ExitError{Command: "nix", Code: 73, Output: Output{Stderr: "refused"}}
				}
				if mode == "guard" || mode == "rate" {
					warn := "unable to download 'https://api.github.com/repos/x': unavailable; using cached version"
					if mode == "rate" {
						warn += " rate limit exceeded"
					}
					return Output{Stderr: warn}, nil
				}
				if len(calls) == 1 || mode == "second-failure" {
					return Output{}, exit
				}
				return run(ctx, cwd, argv)
			}
			_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, ops)
			if mode == "retry" {
				if err != nil || tempReads != 1 {
					t.Fatal(err, tempReads)
				}
			} else if err == nil || tempReads != 0 {
				t.Fatal(err, tempReads)
			}
			wantCalls := 1
			if mode == "retry" || mode == "second-failure" {
				wantCalls = 2
			}
			if len(calls) != wantCalls {
				t.Fatal(calls)
			}
			if wantCalls == 2 {
				want := append([]string{"nix", "--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes"}, calls[0][1:]...)
				if !reflect.DeepEqual(calls[1], want) {
					t.Fatal(calls)
				}
			}
			if mode == "spawn" && err != ordinary {
				t.Fatal(err)
			}
			if mode == "second-failure" && err != exit {
				t.Fatal(err)
			}
		})
	}
}

func TestSetFilesFailuresAndPriorReplacement(t *testing.T) {
	failure := errors.New("injected file failure")
	for _, stage := range []SetPhase{FirstRead, SnapshotRead, TempName, TempRead, Restore, Format, TempWrite, Rename, FinalRead, Diff} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			root, path := setTestRoot(t, setOld)
			ops := setTestOps()
			deps := setTestDeps(t, root, setNew)
			reads := 0
			read := ops.ReadFile
			ops.ReadFile = func(p string) ([]byte, error) {
				if p == path {
					reads++
				}
				if stage == FirstRead && reads == 1 || stage == SnapshotRead && reads == 2 || stage == FinalRead && reads == 3 {
					return nil, failure
				}
				if p != path && stage == TempRead {
					return nil, &os.PathError{Op: "open", Path: p, Err: syscall.ENOENT}
				}
				if p != path && stage == Restore {
					return []byte(`{"nodes":{"root":{"inputs":{}}}}`), nil
				}
				if p != path && stage == Format {
					return []byte(strings.Replace(setNew, `"version":7`, `"version":1e999`, 1)), nil
				}
				if p == path && stage == Diff && reads == 3 {
					return []byte(strings.Replace(setNew, `"rev":"2222222222"`, `"rev":42`, 1)), nil
				}
				return read(p)
			}
			if stage == TempName {
				ops.Random = func() ([6]byte, error) { return [6]byte{}, failure }
			}
			if stage == TempWrite {
				ops.WriteFile = func(p string, b []byte) error {
					if err := os.WriteFile(p, b[:5], 0600); err != nil {
						t.Fatal(err)
					}
					return failure
				}
			}
			if stage == Rename {
				ops.Rename = func(string, string) error { return failure }
			}
			_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, ops)
			se := setTestPhase(t, err, stage)
			if stage == Rename && (se.Path == path || se.Target != path) {
				t.Fatal(se)
			}
			if stage == Format {
				var codec *orderedLockError
				if !errors.As(err, &codec) || codec.phase != "generate" {
					t.Fatal(err)
				}
			}
			if stage == TempRead {
				var file *os.PathError
				if !errors.As(err, &file) || file.Op != "open" {
					t.Fatal(err)
				}
			}
			data, _ := os.ReadFile(path)
			if stage == FinalRead || stage == Diff {
				if string(data) != setPretty {
					t.Fatal(string(data))
				}
			} else if string(data) != setOld {
				t.Fatal("failed operation replaced lock", string(data))
			}
			files, _ := filepath.Glob(root + "/flake.lock.tmp-*")
			if len(files) != 0 {
				t.Fatal("TEMP not cleaned", files)
			}
		})
	}
	root, path := setTestRoot(t, setOld)
	ops := setTestOps()
	deps := setTestDeps(t, root, setNew)
	run := deps.Run
	count := 0
	deps.Run = func(ctx context.Context, cwd string, argv []string) (Output, error) {
		count++
		if count == 2 {
			return Output{}, failure
		}
		return run(ctx, cwd, argv)
	}
	_, err := setWithOps(context.Background(), root, []any{"a", "a"}, "rev", UpdateOptions{}, deps, ops)
	data, _ := os.ReadFile(path)
	if err != failure || count != 2 || string(data) != setPretty {
		t.Fatal(err, count, string(data))
	}
}

func TestSetMalformedTempAndWriteCloseError(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		root, path := setTestRoot(t, setOld)
		ops := setTestOps()
		deps := setTestDeps(t, root, "{")
		closeErr := &os.PathError{Op: "close", Path: "injected", Err: syscall.EIO}
		if !malformed {
			deps = setTestDeps(t, root, setNew)
			ops.WriteFile = func(p string, b []byte) error {
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
				return closeErr
			}
		}
		_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, ops)
		if malformed {
			setTestPhase(t, err, TempRead)
			var codec *orderedLockError
			if !errors.As(err, &codec) || codec.phase != "parse" {
				t.Fatal(err)
			}
		} else {
			setTestPhase(t, err, TempWrite)
			if !errors.Is(err, closeErr) {
				t.Fatal(err)
			}
		}
		data, _ := os.ReadFile(path)
		if string(data) != setOld {
			t.Fatal(string(data))
		}
	}
}

func TestSetExistenceAndNonrecursiveForcedCleanup(t *testing.T) {
	root, path := setTestRoot(t, setOld)
	for _, errno := range []error{syscall.ENOENT, syscall.EACCES, syscall.ENOTDIR} {
		ops := setTestOps()
		ops.Stat = func(string) (os.FileInfo, error) { return nil, errno }
		ops.ReadFile = func(string) ([]byte, error) { t.Fatal("read after false existence"); return nil, nil }
		_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, SetDeps{}, ops)
		if err == nil || err.Error() != "flake.lock missing; run confctl inputs update first" {
			t.Fatal(err)
		}
	}
	ops := setTestOps()
	ops.Stat = func(string) (os.FileInfo, error) { t.Fatal("NUL path reached stat"); return nil, nil }
	_, err := setWithOps(context.Background(), root+"\x00", []any{"a"}, "rev", UpdateOptions{}, SetDeps{}, ops)
	if err == nil || err.Error() != "path name contains null byte" {
		t.Fatal(err)
	}
	for _, kind := range []string{"regular", "live-link", "dangling-link", "directory", "unlink-error", "stat-error", "disappeared"} {
		t.Run(kind, func(t *testing.T) {
			p := root + "/cleanup-" + kind
			target := root + "/target-" + kind
			ops := defaultSetOps()
			switch kind {
			case "directory":
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p+"/retained", []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "live-link", "dangling-link":
				if kind == "live-link" {
					if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(p, []byte("temp"), 0640); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			unlink := ops.Unlink
			ops.Unlink = func(name string) error {
				called = true
				if kind == "unlink-error" {
					return syscall.EACCES
				}
				if kind == "disappeared" {
					if err := unlink(name); err != nil {
						t.Fatal(err)
					}
				}
				return unlink(name)
			}
			if kind == "stat-error" {
				ops.Stat = func(string) (os.FileInfo, error) { return nil, syscall.EACCES }
			}
			setRemoveTemp(p, ops)
			_, err := os.Lstat(p)
			retained := kind == "directory" || kind == "dangling-link" || kind == "unlink-error" || kind == "stat-error"
			if retained && err != nil || !retained && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(kind, err)
			}
			if called != (kind != "dangling-link" && kind != "stat-error") {
				t.Fatal(kind, called)
			}
			if kind == "live-link" {
				b, err := os.ReadFile(target)
				if err != nil || string(b) != "target" {
					t.Fatal(err, string(b))
				}
			}
			if kind == "directory" {
				if _, err := os.Stat(p + "/retained"); err != nil {
					t.Fatal("recursive cleanup", err)
				}
			}
		})
	}
	data, _ := os.ReadFile(path)
	if string(data) != setOld {
		t.Fatal(string(data))
	}
}

func TestSetCancellationPriorityAndEnsure(t *testing.T) {
	for _, stage := range []string{"run", "read", "write", "rename", "final", "changelog", "commit"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root, path := setTestRoot(t, setOld)
			ops := setTestOps()
			deps := setTestDeps(t, root, setNew)
			run := deps.Run
			read := ops.ReadFile
			write := ops.WriteFile
			rename := ops.Rename
			ordinary := errors.New("coincident failure")
			cleaned := false
			stat := ops.Stat
			ops.Stat = func(p string) (os.FileInfo, error) {
				if strings.Contains(p, ".tmp-") {
					cleaned = true
				}
				return stat(p)
			}
			ops.Unlink = func(p string) error { _ = syscall.Unlink(p); return ordinary }
			deps.Run = func(c context.Context, cwd string, argv []string) (Output, error) {
				out, err := run(c, cwd, argv)
				if stage == "run" {
					cancel()
					return out, ordinary
				}
				return out, err
			}
			lockReads := 0
			ops.ReadFile = func(p string) ([]byte, error) {
				if p == path {
					lockReads++
				}
				b, err := read(p)
				if stage == "read" && p != path || stage == "final" && lockReads == 3 {
					cancel()
					return b, ordinary
				}
				return b, err
			}
			ops.WriteFile = func(p string, b []byte) error {
				err := write(p, b)
				if stage == "write" {
					cancel()
					return ordinary
				}
				return err
			}
			ops.Rename = func(a, b string) error {
				err := rename(a, b)
				if stage == "rename" {
					cancel()
					return ordinary
				}
				return err
			}
			commits := 0
			deps.Changelog = func(context.Context, string, string, string) (string, error) {
				if stage == "changelog" {
					cancel()
					return "", ordinary
				}
				return "log", nil
			}
			deps.Commit = func(context.Context, string, string, bool) error {
				commits++
				if stage == "commit" {
					cancel()
					return ordinary
				}
				t.Fatal("commit after cancellation")
				return nil
			}
			_, err := setWithOps(ctx, root, []any{"a"}, "rev", UpdateOptions{Commit: true, Changelog: true}, deps, ops)
			if err != context.Canceled || !cleaned || commits != map[bool]int{true: 1, false: 0}[stage == "commit"] {
				t.Fatal(err, cleaned, commits)
			}
			data, _ := os.ReadFile(path)
			if stage == "run" || stage == "read" || stage == "write" {
				if string(data) != setOld {
					t.Fatal("replacement after cancellation")
				}
			} else if string(data) != setPretty {
				t.Fatal("completed replacement undone", string(data))
			}
		})
	}
}

func TestSetMetadataNoChangeCommitRefusalAndEditor(t *testing.T) {
	for _, noChange := range []bool{true, false} {
		root, path := setTestRoot(t, setOld)
		next := setNew
		if noChange {
			next = strings.Replace(next, "2222222222", "1111111111", 1)
		}
		deps := setTestDeps(t, root, next)
		calls := 0
		failure := errors.New("git commit failed")
		deps.Commit = func(_ context.Context, cwd, message string, editor bool) error {
			calls++
			if cwd != root || !editor || message != "inputs: set a to 22222222\n\na: 11111111 -> 22222222" {
				t.Fatal(cwd, message, editor)
			}
			return failure
		}
		deps.Changelog = func(context.Context, string, string, string) (string, error) {
			t.Fatal("unexpected changelog")
			return "", nil
		}
		result, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{Commit: true, Editor: true}, deps, setTestOps())
		if noChange {
			if err != nil || len(result.Changes) != 0 || calls != 0 {
				t.Fatal(result, err, calls)
			}
		} else if err != failure || len(result.Changes) != 1 || calls != 1 {
			t.Fatal(result, err, calls)
		}
		data, _ := os.ReadFile(path)
		want := setPretty
		if noChange {
			want = strings.Replace(want, "2222222222", "1111111111", 1)
		}
		if string(data) != want {
			t.Fatal("replacement lost", string(data))
		}
	}
}

func TestSetDefaultOperationsAndLiteralPaths(t *testing.T) {
	if setPath("", "flake.lock") != "/flake.lock" || setPath("relative//root/", "flake.lock") != "relative//root/flake.lock" {
		t.Fatal("cleaned literal path")
	}
	root, path := setTestRoot(t, setOld)
	deps := setTestDeps(t, root, setNew)
	deps.Run = func(_ context.Context, cwd string, argv []string) (Output, error) {
		p := argv[len(argv)-1]
		prefix := fmt.Sprintf("%s/flake.lock.tmp-%d-", root, os.Getpid())
		suffix := strings.TrimPrefix(p, prefix)
		if !strings.HasPrefix(p, prefix) || len(suffix) != 12 || strings.Trim(suffix, "0123456789abcdef") != "" {
			t.Fatal(p)
		}
		return Output{}, os.WriteFile(p, []byte(setNew), 0640)
	}
	result, err := Set(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps)
	if err != nil || len(result.Changes) != 1 {
		t.Fatal(result, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal(info, err)
	}
	// Default write truncates an existing file without changing its mode.
	fresh := root + "/new-mode"
	if err := defaultSetOps().WriteFile(fresh, []byte("whole buffer")); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(fresh)
	if err != nil || info.Mode().Perm()&0111 != 0 {
		t.Fatal(info, err)
	}
}

func TestSetInitialOrderingEntropyAndRealMissingTemp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ops := setTestOps()
	ops.Stat = func(string) (os.FileInfo, error) { t.Fatal("initial failure reached stat"); return nil, nil }
	_, err := setWithOps(ctx, "", nil, "rev", UpdateOptions{}, SetDeps{}, ops)
	if err != context.Canceled {
		t.Fatal(err)
	}
	_, err = setWithOps(context.Background(), "", nil, "rev", UpdateOptions{}, SetDeps{}, ops)
	if err == nil || err.Error() != "inputs empty" {
		t.Fatal(err)
	}
	root, path := setTestRoot(t, "{")
	_, err = setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, SetDeps{}, setTestOps())
	setTestPhase(t, err, FirstRead)
	if err := os.WriteFile(path, []byte(setOld), 0600); err != nil {
		t.Fatal(err)
	}
	ops = setTestOps()
	ops.Random = func() ([6]byte, error) { return [6]byte{}, errors.New("entropy refused") }
	ops.Unlink = func(string) error { t.Fatal("entropy failure entered ensure"); return nil }
	deps := setTestDeps(t, root, setNew)
	deps.Run = func(context.Context, string, []string) (Output, error) {
		t.Fatal("entropy failure reached Run")
		return Output{}, nil
	}
	_, err = setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, ops)
	setTestPhase(t, err, TempName)
	deps.Run = func(context.Context, string, []string) (Output, error) { return Output{}, nil }
	_, err = setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, setTestOps())
	se := setTestPhase(t, err, TempRead)
	var open *os.PathError
	if !errors.As(err, &open) || open.Op != "open" || open.Path != se.Path || !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestSetRepeatedOperationsFreshNamesAndIndependentFinalInfo(t *testing.T) {
	root, path := setTestRoot(t, setOld)
	ops := setTestOps()
	deps := setTestDeps(t, root, setNew)
	run := deps.Run
	temps := []string{}
	deps.Run = func(ctx context.Context, cwd string, argv []string) (Output, error) {
		temps = append(temps, argv[len(argv)-1])
		if len(temps) == 2 {
			data, _ := os.ReadFile(path)
			if string(data) != setPretty {
				t.Fatal("second operation lost first restoration", string(data))
			}
		}
		return run(ctx, cwd, argv)
	}
	read := ops.ReadFile
	reads := 0
	ops.ReadFile = func(p string) ([]byte, error) {
		if p == path {
			reads++
			if reads == 3 {
				return []byte(strings.Replace(setPretty, "2222222222", "3333333333", 1)), nil
			}
		}
		return read(p)
	}
	result, err := setWithOps(context.Background(), root, []any{"a", "a"}, "rev", UpdateOptions{}, deps, ops)
	if err != nil || !reflect.DeepEqual(temps, []string{root + "/flake.lock.tmp-42-01abcdef00ff", root + "/flake.lock.tmp-42-02abcdef00ff"}) || reads != 3 || len(result.Changes) != 2 || result.Changes[0].New.Rev != "3333333333" || result.Changes[1].New.Rev != "3333333333" {
		t.Fatal(result, err, temps, reads)
	}
}

func TestSetCleanupFailureKeepsProcessCauseAndLiteralCallOrder(t *testing.T) {
	root, path := setTestRoot(t, setOld)
	ops := setTestOps()
	deps := setTestDeps(t, root, setNew)
	run := deps.Run
	primary := &ExitError{Command: "nix", Code: 73, Output: Output{Stderr: "refused"}}
	events := []string{}
	deps.Run = func(ctx context.Context, cwd string, argv []string) (Output, error) {
		events = append(events, "run")
		_, err := run(ctx, cwd, argv)
		if err != nil {
			t.Fatal(err)
		}
		return Output{}, primary
	}
	stat := ops.Stat
	ops.Stat = func(p string) (os.FileInfo, error) {
		if p != path {
			events = append(events, "stat")
		}
		return stat(p)
	}
	ops.Unlink = func(string) error { events = append(events, "unlink"); return syscall.EACCES }
	_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{}, deps, ops)
	if err != primary || !reflect.DeepEqual(events, []string{"run", "stat", "unlink"}) {
		t.Fatal(err, events)
	}
	files, _ := filepath.Glob(root + "/flake.lock.tmp-*")
	if len(files) != 1 {
		t.Fatal("suppressed unlink failure changed leftover", files)
	}
}

func TestSetChangelogCancellationPreventsCommitWithLiveContext(t *testing.T) {
	root, path := setTestRoot(t, setOld)
	deps := setTestDeps(t, root, setNew)
	deps.Changelog = func(context.Context, string, string, string) (string, error) {
		return "", fmt.Errorf("mirror: %w", context.DeadlineExceeded)
	}
	deps.Commit = func(context.Context, string, string, bool) error {
		t.Fatal("commit after changelog cancellation")
		return nil
	}
	_, err := setWithOps(context.Background(), root, []any{"a"}, "rev", UpdateOptions{Commit: true, Changelog: true, Editor: true, Downgrade: true}, deps, setTestOps())
	data, _ := os.ReadFile(path)
	if !errors.Is(err, context.DeadlineExceeded) || string(data) != setPretty {
		t.Fatal(err, string(data))
	}
}
