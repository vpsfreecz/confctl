package generation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type retentionEntry struct {
	Mode os.FileMode
	Kind string
	Data string
}

// Snapshot the complete seeded forest: bytes, modes, kinds and literal link
// text. Directory mtimes/sizes can change when an entry is removed.
func retentionForest(t *testing.T, root string) map[string]retentionEntry {
	t.Helper()
	ret := map[string]retentionEntry{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		r := retentionEntry{Mode: info.Mode(), Kind: "directory"}
		if info.Mode()&os.ModeSymlink != 0 {
			r.Kind = "symlink"
			r.Data, err = os.Readlink(path)
		} else if !info.IsDir() {
			r.Kind = "file"
			var data []byte
			data, err = os.ReadFile(path)
			r.Data = string(data)
		}
		ret[name] = r
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

func retentionWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
}

type removalFixture struct {
	forest, config, roots string
	w                     *Writer
	build                 *Build
	rootPaths, filePaths  []string
}

func newRemovalFixture(t *testing.T) removalFixture {
	t.Helper()
	forest := t.TempDir()
	config, roots := forest+"/configuration", forest+"/private-gcroots"
	if err := os.Mkdir(config, 0750); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"top", "rollback", "z", "a"} {
		retentionWrite(t, forest+"/private-store/"+role+"/sentinel", "store "+role+"\n")
	}
	top, rollback := forest+"/private-store/top", forest+"/private-store/rollback"
	b, err := CreateFlake(config, "nested/host", top, &rollback, Object{{"z", forest + "/private-store/z"}, {"a", forest + "/private-store/a"}}, Object{{"metadata", Object{{"n", "unchanged"}}}}, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(config, func() (string, error) { return roots, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b.Name, filepath.Dir(b.Dir)+"/current"); err != nil {
		t.Fatal(err)
	}
	retentionWrite(t, filepath.Dir(b.Dir)+"/sibling/generation.json", record(top, "2026-10-08T00:00:00Z"))
	retentionWrite(t, filepath.Dir(b.Dir)+"/rejected/generation.json", `{"mode":"swpins","opaque":true}`)
	retentionWrite(t, roots+"/unknown-root", "unknown root\n")
	f := removalFixture{forest: forest, config: config, roots: roots, w: w, build: b}
	for _, component := range []string{"toplevel", "auto_rollback", "input.z", "input.a"} {
		f.rootPaths = append(f.rootPaths, roots+"/"+RootName(b.Host, b.Name, component))
	}
	for _, component := range []string{"toplevel", "auto_rollback", "z.input", "a.input", "generation.json"} {
		f.filePaths = append(f.filePaths, b.Dir+"/"+component)
	}
	return f
}

func removalExpected(t *testing.T, f removalFixture, before map[string]retentionEntry, removed ...string) map[string]retentionEntry {
	t.Helper()
	ret := map[string]retentionEntry{}
	for k, v := range before {
		ret[k] = v
	}
	for _, path := range removed {
		name, err := filepath.Rel(f.forest, path)
		if err != nil {
			t.Fatal(err)
		}
		delete(ret, name)
	}
	return ret
}

func removalAssertForest(t *testing.T, f removalFixture, want map[string]retentionEntry) {
	t.Helper()
	got := retentionForest(t, f.forest)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("complete forest mismatch\ngot  %#v\nwant %#v", got, want)
	}
}

func removalTrace(f removalFixture, trace *[]string) removeOps {
	ops := nativeRemoveOps()
	label := func(op, path string) { *trace = append(*trace, op+":"+strings.TrimPrefix(path, f.forest+"/")) }
	return removeOps{
		unlink: func(path string) error { label("unlink", path); return ops.unlink(path) },
		stat:   func(path string) (os.FileInfo, error) { label("stat", path); return ops.stat(path) },
		lstat:  func(path string) (os.FileInfo, error) { label("lstat", path); return ops.lstat(path) },
		rmdir:  func(path string) error { label("rmdir", path); return ops.rmdir(path) },
	}
}

func removalHappyTrace(f removalFixture) []string {
	ret := []string{}
	for _, path := range f.rootPaths {
		ret = append(ret, "roots", "unlink:"+strings.TrimPrefix(path, f.forest+"/"))
	}
	for i, path := range f.filePaths {
		if i == 2 || i == 3 {
			ret = append(ret, "stat:"+strings.TrimPrefix(path, f.forest+"/"))
		}
		ret = append(ret, "unlink:"+strings.TrimPrefix(path, f.forest+"/"))
	}
	return append(ret, "rmdir:"+strings.TrimPrefix(f.build.Dir, f.forest+"/"))
}

func TestRemovalOrderedRootsFilesAndCurrentResidue(t *testing.T) {
	f := newRemovalFixture(t)
	list, err := LoadBuildList(f.config, f.build.Host, Options{})
	if err != nil || list.RequireResolvedCurrent() != nil {
		t.Fatal(list, err)
	}
	selected := list.Current
	before := retentionForest(t, f.forest)
	trace := []string{}
	f.w.roots = func() (string, error) { trace = append(trace, "roots"); return f.roots, nil }
	if err := f.w.remove(selected, removalTrace(f, &trace)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(trace, removalHappyTrace(f)) {
		t.Fatal("operation order", trace, removalHappyTrace(f))
	}
	removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
	removed = append(removed, f.build.Dir)
	removalAssertForest(t, f, removalExpected(t, f, before, removed...))
	if list.Current != selected || !selected.Current || len(list.Builds) != 2 || len(list.Rejected) != 1 {
		t.Fatal("removal mutated in-memory list", list)
	}
	fresh, err := LoadBuildList(f.config, f.build.Host, Options{})
	if err != nil || fresh.RequireResolvedCurrent() == nil || len(fresh.Builds) != 1 {
		t.Fatal("fresh unresolved-current boundary", fresh, err)
	}
	// Explicitly selected new call on the actual residue: required toplevel is
	// absent even though already missing roots are ignored.
	if err := f.w.Remove(selected); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("second removal became idempotent", err)
	}
	removalAssertForest(t, f, removalExpected(t, f, before, removed...))
}

func TestRemovalEveryUnlinkAndRmdirFailurePrefix(t *testing.T) {
	for fail := 0; fail < 10; fail++ {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			f := newRemovalFixture(t)
			before := retentionForest(t, f.forest)
			trace := []string{}
			f.w.roots = func() (string, error) { trace = append(trace, "roots"); return f.roots, nil }
			ops := removalTrace(f, &trace)
			unlink, rmdir := ops.unlink, ops.rmdir
			fault := errors.New("selected operation failure")
			calls := 0
			ops.unlink = func(path string) error {
				if calls == fail {
					trace = append(trace, "unlink:"+strings.TrimPrefix(path, f.forest+"/"))
					calls++
					return fault
				}
				calls++
				return unlink(path)
			}
			ops.rmdir = func(path string) error {
				if calls == fail {
					trace = append(trace, "rmdir:"+strings.TrimPrefix(path, f.forest+"/"))
					calls++
					return fault
				}
				return rmdir(path)
			}
			if err := f.w.remove(f.build, ops); err != fault {
				t.Fatal("failure identity", err)
			}
			paths := append(append([]string{}, f.rootPaths...), f.filePaths...)
			removalAssertForest(t, f, removalExpected(t, f, before, paths[:fail]...))
			full := removalHappyTrace(f)
			last := "rmdir:" + strings.TrimPrefix(f.build.Dir, f.forest+"/")
			if fail < len(paths) {
				last = "unlink:" + strings.TrimPrefix(paths[fail], f.forest+"/")
			}
			end := 0
			for i, label := range full {
				if label == last {
					end = i + 1
				}
			}
			if !reflect.DeepEqual(trace, full[:end]) || calls != fail+1 {
				t.Fatal("later operation after failure", trace, full[:end], calls)
			}
		})
	}
}

func TestRemovalLazyRootResolutionAndMissingRoots(t *testing.T) {
	for fail := 1; fail <= 4; fail++ {
		t.Run(fmt.Sprintf("resolver %d", fail), func(t *testing.T) {
			f := newRemovalFixture(t)
			before := retentionForest(t, f.forest)
			calls := 0
			fault := errors.New("root/login resolver failure")
			f.w.roots = func() (string, error) {
				calls++
				if calls == fail {
					return "", fault
				}
				return f.roots, nil
			}
			if err := f.w.Remove(f.build); err != fault || calls != fail {
				t.Fatal("lazy root error/order", err, calls)
			}
			removalAssertForest(t, f, removalExpected(t, f, before, f.rootPaths[:fail-1]...))
		})
	}
	for slot := 0; slot < 4; slot++ {
		t.Run(fmt.Sprintf("directory root %d", slot), func(t *testing.T) {
			f := newRemovalFixture(t)
			if err := unlinkGenerationPath(f.rootPaths[slot]); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(f.rootPaths[slot], 0750); err != nil {
				t.Fatal(err)
			}
			before := retentionForest(t, f.forest)
			if err := f.w.Remove(f.build); err == nil {
				t.Fatal("unlink removed an empty directory")
			}
			removalAssertForest(t, f, removalExpected(t, f, before, f.rootPaths[:slot]...))
		})
	}
	for _, kind := range []string{"missing entry", "missing directory", "dangling", "regular", "directory", "nondirectory ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemovalFixture(t)
			if err := unlinkGenerationPath(f.rootPaths[0]); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing directory":
				f.w.roots = func() (string, error) { return f.forest + "/missing-roots", nil }
			case "dangling":
				if err := os.Symlink(f.forest+"/missing-target", f.rootPaths[0]); err != nil {
					t.Fatal(err)
				}
			case "regular":
				retentionWrite(t, f.rootPaths[0], "ordinary root bytes")
			case "directory":
				if err := os.Mkdir(f.rootPaths[0], 0750); err != nil {
					t.Fatal(err)
				}
			case "nondirectory ancestor":
				retentionWrite(t, f.forest+"/root-blocker", "file")
				f.w.roots = func() (string, error) { return f.forest + "/root-blocker", nil }
			}
			before := retentionForest(t, f.forest)
			err := f.w.Remove(f.build)
			bad := kind == "directory" || kind == "nondirectory ancestor"
			if (err != nil) != bad || kind == "nondirectory ancestor" && !errors.Is(err, syscall.ENOTDIR) {
				t.Fatal(kind, err)
			}
			if bad {
				removalAssertForest(t, f, before)
				return
			}
			removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
			if kind == "missing directory" {
				removed = append([]string{}, f.filePaths...)
			}
			removed = append(removed, f.build.Dir)
			removalAssertForest(t, f, removalExpected(t, f, before, removed...))
		})
	}
}

func TestRemovalRecordEntryKindsAndRequiredMissing(t *testing.T) {
	for slot := 0; slot < 5; slot++ {
		for _, kind := range []string{"missing", "dangling", "regular", "directory"} {
			t.Run(fmt.Sprintf("%d %s", slot, kind), func(t *testing.T) {
				f := newRemovalFixture(t)
				path := f.filePaths[slot]
				if err := unlinkGenerationPath(path); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "dangling":
					if err := os.Symlink(f.forest+"/absent", path); err != nil {
						t.Fatal(err)
					}
				case "regular":
					retentionWrite(t, path, "literal entry")
				case "directory":
					if err := os.Mkdir(path, 0750); err != nil {
						t.Fatal(err)
					}
				}
				// Rollback deletion is unconditional even with nil payload.
				if slot == 1 {
					f.build.AutoRollback = nil
				}
				before := retentionForest(t, f.forest)
				err := f.w.Remove(f.build)
				bad := kind == "directory" || kind == "missing" && (slot == 0 || slot == 4)
				if (err != nil) != bad || kind == "missing" && bad && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(kind, slot, err)
				}
				removed := append([]string{}, f.rootPaths...)
				if bad {
					removed = append(removed, f.filePaths[:slot]...)
				} else {
					removed = append(removed, f.filePaths...)
					removed = append(removed, f.build.Dir)
				}
				removalAssertForest(t, f, removalExpected(t, f, before, removed...))
			})
		}
	}
}

func TestRemovalInputPredicateShortCircuitAndVanishing(t *testing.T) {
	for _, kind := range []string{"live symlink", "dangling", "missing", "stat denied", "stat denied regular", "lstat denied", "vanished after stat"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemovalFixture(t)
			path := f.filePaths[2]
			if kind == "dangling" || kind == "missing" {
				if err := unlinkGenerationPath(path); err != nil {
					t.Fatal(err)
				}
				if kind == "dangling" {
					if err := os.Symlink(f.forest+"/absent", path); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "stat denied regular" {
				if err := unlinkGenerationPath(path); err != nil {
					t.Fatal(err)
				}
				retentionWrite(t, path, "ordinary input bytes")
			}
			before := retentionForest(t, f.forest)
			ops := nativeRemoveOps()
			statFn, lstatFn := ops.stat, ops.lstat
			calls := []string{}
			ops.stat = func(p string) (os.FileInfo, error) {
				if p != path {
					return statFn(p)
				}
				calls = append(calls, "stat")
				if kind == "stat denied" || kind == "stat denied regular" || kind == "lstat denied" {
					return nil, syscall.EACCES
				}
				info, err := statFn(p)
				if kind == "vanished after stat" && err == nil {
					if err := unlinkGenerationPath(p); err != nil {
						t.Fatal(err)
					}
				}
				return info, err
			}
			ops.lstat = func(p string) (os.FileInfo, error) {
				if p == path {
					calls = append(calls, "lstat")
					if kind == "lstat denied" {
						return nil, syscall.EACCES
					}
				}
				return lstatFn(p)
			}
			err := f.w.remove(f.build, ops)
			wantCalls := []string{"stat", "lstat"}
			if kind == "live symlink" || kind == "vanished after stat" {
				wantCalls = []string{"stat"}
			}
			bad := kind == "vanished after stat" || kind == "lstat denied" || kind == "stat denied regular"
			if !reflect.DeepEqual(calls, wantCalls) || (err != nil) != bad {
				t.Fatal("predicate order/error", calls, err)
			}
			removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
			if kind == "vanished after stat" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				removed = append(append([]string{}, f.rootPaths...), f.filePaths[:3]...)
			} else if kind == "lstat denied" || kind == "stat denied regular" {
				if !errors.Is(err, syscall.ENOTEMPTY) {
					t.Fatal("failed probes must leave input for final rmdir", err)
				}
				removed = append(append([]string{}, f.rootPaths...), f.filePaths[0], f.filePaths[1], f.filePaths[3], f.filePaths[4])
			} else {
				removed = append(removed, f.build.Dir)
			}
			removalAssertForest(t, f, removalExpected(t, f, before, removed...))
		})
	}
}

func TestRemovalUnknownContentAndDirectorySymlink(t *testing.T) {
	for _, kind := range []string{"extra file", "extra directory", "old swpin", "current replacement", "directory symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemovalFixture(t)
			actualFiles := append([]string{}, f.filePaths...)
			switch kind {
			case "extra file":
				retentionWrite(t, f.build.Dir+"/unknown", "never recursively removed")
			case "extra directory":
				if err := os.Mkdir(f.build.Dir+"/unknown", 0750); err != nil {
					t.Fatal(err)
				}
			case "old swpin":
				if err := os.Symlink(f.forest+"/private-store/z", f.build.Dir+"/old.swpin"); err != nil {
					t.Fatal(err)
				}
			case "current replacement":
				if err := os.Symlink("kept", f.build.Dir+"/current.new-abcdef"); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				target := f.forest + "/private-record-target"
				if err := os.Rename(f.build.Dir, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, f.build.Dir); err != nil {
					t.Fatal(err)
				}
				for i, path := range actualFiles {
					actualFiles[i] = target + strings.TrimPrefix(path, f.build.Dir)
				}
			}
			before := retentionForest(t, f.forest)
			err := f.w.Remove(f.build)
			if err == nil || kind != "directory symlink" && !errors.Is(err, syscall.ENOTEMPTY) {
				t.Fatal("nonrecursive rmdir", err)
			}
			var pe *os.PathError
			if !errors.As(err, &pe) || pe.Op != "rmdir" || pe.Path != f.build.Dir {
				t.Fatal("rmdir path/cause", err)
			}
			removed := append(append([]string{}, f.rootPaths...), actualFiles...)
			removalAssertForest(t, f, removalExpected(t, f, before, removed...))
		})
	}
}

func TestRemovalNoncurrentAbsentCurrentAndOldLayout(t *testing.T) {
	for _, current := range []string{"other", "absent"} {
		for _, rollback := range []string{"null", "omitted"} {
			t.Run(current+" "+rollback, func(t *testing.T) {
				f := newRemovalFixture(t)
				currentPath := filepath.Dir(f.build.Dir) + "/current"
				if err := unlinkGenerationPath(currentPath); err != nil {
					t.Fatal(err)
				}
				if current == "other" {
					if err := os.Symlink("sibling", currentPath); err != nil {
						t.Fatal(err)
					}
				}
				if err := unlinkGenerationPath(f.filePaths[1]); err != nil {
					t.Fatal(err)
				}
				field := ""
				if rollback == "null" {
					field = `,"auto_rollback":null`
				}
				// Manually seeded Ruby-compatible layout, not a Ruby observation.
				raw := fmt.Sprintf(`{"mode":"flakes","date":"2026-10-09T12:00:00Z","toplevel":%q%s,"inputs":{"z":%q,"a":%q},"inputsInfo":{"z":{"opaque":[1]}},"extra":"untouched"}`, f.build.Toplevel, field, f.forest+"/private-store/z", f.forest+"/private-store/a")
				retentionWrite(t, f.filePaths[4], raw)
				list, err := LoadBuildList(f.config, f.build.Host, Options{})
				if err != nil || list.RequireResolvedCurrent() != nil {
					t.Fatal(list, err)
				}
				selected, err := ResolveBuild(list, f.build.Name)
				if err != nil || selected == nil || selected.AutoRollback != nil || len(selected.InputsInfo) != 1 {
					t.Fatal("old-layout reader", selected, err)
				}
				before := retentionForest(t, f.forest)
				if err := f.w.Remove(selected); err != nil {
					t.Fatal(err)
				}
				removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
				removed = append(removed, f.build.Dir)
				removalAssertForest(t, f, removalExpected(t, f, before, removed...))
				fresh, err := LoadBuildList(f.config, f.build.Host, Options{})
				if err != nil || fresh.RequireResolvedCurrent() != nil || fresh.Current == nil || fresh.Current.Name != "sibling" {
					t.Fatal("remaining current", fresh, err)
				}
				if len(list.Builds) != 2 || len(list.Rejected) != 1 {
					t.Fatal("stale list mutated")
				}
			})
		}
	}
}

func TestRemovalSequentialCallerStopsAfterSecondFailure(t *testing.T) {
	f := newRemovalFixture(t)
	builds := []*Build{f.build}
	for i := 1; i < 3; i++ {
		b, err := CreateFlake(f.config, f.build.Host, f.build.Toplevel, f.build.AutoRollback, f.build.Inputs, f.build.InputsInfo, time.Date(2026, 10, 9, 12, 0, i, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.w.Save(b); err != nil {
			t.Fatal(err)
		}
		builds = append(builds, b)
	}
	if err := unlinkGenerationPath(builds[1].Dir + "/toplevel"); err != nil {
		t.Fatal(err)
	}
	before := retentionForest(t, f.forest)
	visited := []string{}
	var failure error
	for _, b := range builds {
		visited = append(visited, b.Name)
		if failure = f.w.Remove(b); failure != nil {
			break
		}
	}
	if !errors.Is(failure, os.ErrNotExist) || !reflect.DeepEqual(visited, []string{builds[0].Name, builds[1].Name}) {
		t.Fatal("private caller did not stop", visited, failure)
	}
	removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
	removed = append(removed, f.build.Dir)
	for _, component := range []string{"toplevel", "auto_rollback", "input.z", "input.a"} {
		removed = append(removed, f.roots+"/"+RootName(builds[1].Host, builds[1].Name, component))
	}
	removalAssertForest(t, f, removalExpected(t, f, before, removed...))
}

func TestRemovalWriterReaderCompositionAndPathIdentity(t *testing.T) {
	for _, spelling := range []string{"symlink", "link dotdot", "moved namespace"} {
		t.Run(spelling, func(t *testing.T) {
			f := newRemovalFixture(t)
			configuration := f.config
			switch spelling {
			case "symlink":
				configuration = f.forest + "/configuration-alias"
				if err := os.Symlink(f.config, configuration); err != nil {
					t.Fatal(err)
				}
			case "link dotdot":
				if err := os.Mkdir(f.config+"/child", 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.config+"/child", f.forest+"/child-alias"); err != nil {
					t.Fatal(err)
				}
				configuration = f.forest + "/child-alias/.."
			case "moved namespace":
				configuration = f.forest + "/moved-configuration"
				if err := os.Rename(f.config, configuration); err != nil {
					t.Fatal(err)
				}
			}
			roots := f.roots
			if spelling == "moved namespace" {
				roots = f.forest + "/new-private-roots"
			}
			w, err := NewWriter(configuration, func() (string, error) { return roots, nil })
			if err != nil {
				t.Fatal(err)
			}
			list, err := LoadBuildList(configuration, f.build.Host, Options{})
			if err != nil || list.RequireResolvedCurrent() != nil {
				t.Fatal(list, err)
			}
			before := retentionForest(t, f.forest)
			if err := w.Remove(list.Current); err != nil {
				t.Fatal(err)
			}
			removed := append([]string{}, f.rootPaths...)
			if spelling == "moved namespace" {
				removed = nil // The new identity never scans the old root namespace.
			}
			for _, path := range f.filePaths {
				removed = append(removed, configuration+strings.TrimPrefix(path, f.config))
			}
			removed = append(removed, configuration+strings.TrimPrefix(f.build.Dir, f.config))
			// Expected snapshot names must address the physical path, including
			// resolving the seeded link/.. spelling before lexical simplification.
			for i, path := range removed {
				if strings.HasPrefix(path, configuration+"/") {
					removed[i] = w.root + strings.TrimPrefix(path, configuration)
				}
			}
			removalAssertForest(t, f, removalExpected(t, f, before, removed...))
		})
	}
	// Exercise the complete writer/current/reader chain with the selected object.
	f := newRemovalFixture(t)
	list, err := LoadBuildList(f.config, f.build.Host, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.w.suffix = func() (string, error) { return "abcdef", nil }
	if err := f.w.SetCurrent(list, f.build); err != nil {
		t.Fatal(err)
	}
	read, err := LoadBuildList(f.config, f.build.Host, Options{})
	if err != nil || read.Current == nil || read.Current.Name != f.build.Name {
		t.Fatal(read, err)
	}
	if err := f.w.Remove(read.Current); err != nil {
		t.Fatal(err)
	}
	writerLink(t, filepath.Dir(f.build.Dir)+"/current", f.build.Name)
}

func TestRemovalLiteralRoleAndNULFailurePhase(t *testing.T) {
	t.Run("role link dotdot", func(t *testing.T) {
		f := newRemovalFixture(t)
		f.build.Inputs = append(f.build.Inputs, Member{"r/../x", "unused by removal"})
		rootAlias := f.roots + "/" + RootName(f.build.Host, f.build.Name, "input.r")
		inputTarget, rootTarget := f.forest+"/private-role-input", f.forest+"/private-role-root"
		for _, p := range []string{inputTarget + "/child", rootTarget + "/child"} {
			if err := os.MkdirAll(p, 0750); err != nil {
				t.Fatal(err)
			}
		}
		for _, link := range []struct{ path, target string }{{f.build.Dir + "/r", inputTarget + "/child"}, {rootAlias, rootTarget + "/child"}} {
			if err := os.Symlink(link.target, link.path); err != nil {
				t.Fatal(err)
			}
		}
		retentionWrite(t, inputTarget+"/x.input", "physically reached input")
		retentionWrite(t, rootTarget+"/x", "physically reached root")
		retentionWrite(t, f.build.Dir+"/x.input", "lexically collapsed input stays")
		retentionWrite(t, f.roots+"/x", "lexically collapsed root stays")
		before := retentionForest(t, f.forest)
		if err := f.w.Remove(f.build); !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatal("literal alias path or nonrecursive residue", err)
		}
		removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
		removed = append(removed, inputTarget+"/x.input", rootTarget+"/x")
		removalAssertForest(t, f, removalExpected(t, f, before, removed...))
	})
	t.Run("slash role", func(t *testing.T) {
		f := newRemovalFixture(t)
		f.build.Inputs = append(f.build.Inputs, Member{"nested/leaf", f.forest + "/private-store/z"})
		path := f.build.Dir + "/nested/leaf.input"
		rootPath := f.roots + "/" + RootName(f.build.Host, f.build.Name, "input.nested/leaf")
		for _, p := range []string{path, rootPath} {
			if err := os.MkdirAll(filepath.Dir(p), 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.forest+"/private-store/z", p); err != nil {
				t.Fatal(err)
			}
		}
		before := retentionForest(t, f.forest)
		if err := f.w.Remove(f.build); !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatal("literal nested role leaves parent directory", err)
		}
		removed := append(append([]string{}, f.rootPaths...), f.filePaths...)
		removed = append(removed, rootPath, path)
		removalAssertForest(t, f, removalExpected(t, f, before, removed...))
	})
	t.Run("NUL at reached root", func(t *testing.T) {
		f := newRemovalFixture(t)
		f.build.Inputs = append(f.build.Inputs, Member{"bad\x00role", "unused"})
		before := retentionForest(t, f.forest)
		err := f.w.Remove(f.build)
		var pe *os.PathError
		if !errors.Is(err, syscall.EINVAL) || !errors.As(err, &pe) || pe.Op != "unlink" || !strings.Contains(pe.Path, "input.bad\x00role") {
			t.Fatal("root conversion failure", err)
		}
		removalAssertForest(t, f, removalExpected(t, f, before, f.rootPaths...))
	})
	t.Run("NUL predicate after injected root success", func(t *testing.T) {
		f := newRemovalFixture(t)
		f.build.Inputs = append(f.build.Inputs, Member{"bad\x00role", "unused"})
		before := retentionForest(t, f.forest)
		ops := nativeRemoveOps()
		unlink := ops.unlink
		ops.unlink = func(path string) error {
			if strings.Contains(path, "input.bad\x00role") {
				return nil // Private fault seam isolates the later predicate boundary.
			}
			return unlink(path)
		}
		err := f.w.remove(f.build, ops)
		var pe *os.PathError
		if !errors.Is(err, syscall.EINVAL) || !errors.As(err, &pe) || pe.Op != "stat" {
			t.Fatal("NUL became false predicate", err)
		}
		removed := append(append([]string{}, f.rootPaths...), f.filePaths[:4]...)
		removalAssertForest(t, f, removalExpected(t, f, before, removed...))
	})
}
