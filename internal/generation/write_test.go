package generation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writerFixture(t *testing.T) (string, string, *Writer, *Build) {
	t.Helper()
	root := t.TempDir()
	roots := root + "/private-gcroots"
	w, err := NewWriter(root, func() (string, error) { return roots, nil })
	if err != nil {
		t.Fatal(err)
	}
	w.suffix = func() (string, error) { return "abcdef", nil }
	rollback := "/nix/store/rollback"
	b, err := CreateFlake(root, "nested/host", "/nix/store/system", &rollback, Object{{"z", "/nix/store/z"}, {"a", "/nix/store/a"}}, Object{}, time.Date(2026, 10, 8, 12, 3, 4, 123456789, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return root, roots, w, b
}

func writerLink(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil || got != want {
		t.Fatalf("link %s=%q want %q: %v", path, got, want, err)
	}
}

func TestWriterConstructionTimeAndKernel(t *testing.T) {
	for _, test := range []struct {
		name     string
		zone     *time.Location
		zoneText string
		utc      bool
	}{
		{"explicit UTC", time.UTC, "Z", true}, {"local UTC", time.FixedZone("local UTC", 0), "+00:00", false},
		{"positive", time.FixedZone("positive", 90*60), "+01:30", false}, {"negative", time.FixedZone("negative", -5*3600-30*60), "-05:30", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			before := treeSnapshot(t, root)
			rollback := "/fake/rollback"
			at := time.Date(2026, 10, 8, 12, 3, 4, 123456789, test.zone)
			b, err := CreateFlake(root, "nested/host", "/fake/system", &rollback, Object{}, Object{}, at)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(treeSnapshot(t, root), before) || b.Name != "2026-10-08--12-03-04" || b.Mode != "flakes" || b.Current || b.Date.UTC != test.utc || b.Date.Fraction().Cmp(new(big.Rat).SetFrac64(123456789, 1e9)) != 0 {
				t.Fatal("constructor effects/time changed", b)
			}
			roots := root + "/roots"
			w, err := NewWriter(root, func() (string, error) { return roots, nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Save(b); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(b.Dir + "/generation.json")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), `"date": "2026-10-08T12:03:04`+test.zoneText+`"`) || strings.Contains(string(content), "123456789") {
				t.Fatal(string(content))
			}
			loaded, err := LoadBuild(root, b.Host, b.Name, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Date.Fraction().Sign() != 0 || loaded.Date.UTC != test.utc || loaded.Date.Seconds().Cmp(b.Date.Seconds()) != 0 || b.Date.Fraction().Sign() == 0 {
				t.Fatal("persisted/memory precision or zone changed")
			}
		})
	}
	root := t.TempDir()
	if err := os.MkdirAll(root+"/system", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/aaa-linux-6.12.3/bzImage", root+"/system/kernel"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/bbb-linux-9.1/bzImage", root+"/system/bzImage"); err != nil {
		t.Fatal(err)
	}
	b, err := CreateFlake(root, "host", "system", nil, nil, nil, time.Unix(0, 0))
	if err != nil || b.KernelVersion == nil || *b.KernelVersion != "6.12.3" {
		t.Fatal(b, err)
	}
	if err := os.Remove(root + "/system/kernel"); err != nil {
		t.Fatal(err)
	}
	b, err = CreateFlake(root, "host", "system", nil, nil, nil, time.Unix(0, 0))
	if err != nil || b.KernelVersion == nil || *b.KernelVersion != "9.1" {
		t.Fatal(b, err)
	}
	if err := os.Remove(root + "/system/bzImage"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/system/kernel", nil, 0600); err != nil {
		t.Fatal(err)
	}
	b, err = CreateFlake(root, "host", "system", nil, nil, nil, time.Unix(0, 0))
	if err != nil || b.KernelVersion != nil {
		t.Fatal(b, err)
	}
	if _, err := CreateFlake(root, "host", "system/kernel/child", nil, nil, nil, time.Unix(0, 0)); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("nonignored readlink failure lost: %v", err)
	}
}

func TestWriterOrderedRecordLinksRootsAndModes(t *testing.T) {
	root, roots, w, b := writerFixture(t)
	b.InputsInfo = Object{{"z", Object{{"large", json.Number("9007199254740993")}, {"nested", []any{Object{{"huge", json.Number("999999999999999999999999999999")}, {"null", nil}, {"float", float64(1)}}}}}}, {"a", "žluť / <&>\n\"\\"}}
	var events []string
	old := w.ops
	w.ops.mkdir = func(path string) error { events = append(events, "mkdir:"+path); return old.mkdir(path) }
	w.ops.symlink = func(target, path string) error {
		events = append(events, "link:"+path+"=>"+target)
		return old.symlink(target, path)
	}
	w.ops.open = func(path string) (io.WriteCloser, error) {
		events = append(events, "open:"+path)
		return old.open(path)
	}
	w.roots = func() (string, error) {
		events = append(events, "roots")
		if _, err := os.Stat(b.Dir + "/generation.json"); err != nil {
			t.Error("root lookup precedes JSON", err)
		}
		return roots, nil
	}
	oldUmask := syscall.Umask(0027)
	defer syscall.Umask(oldUmask)
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	paths := []string{"toplevel", "auto_rollback", "z.input", "a.input"}
	targets := []string{b.Toplevel, *b.AutoRollback, "/nix/store/z", "/nix/store/a"}
	want := []string{"mkdir:" + b.Dir}
	for i, path := range paths {
		writerLink(t, b.Dir+"/"+path, targets[i])
		want = append(want, "link:"+b.Dir+"/"+path+"=>"+targets[i])
	}
	want = append(want, "open:"+b.Dir+"/generation.json")
	for i, component := range []string{"toplevel", "auto_rollback", "input.z", "input.a"} {
		name := RootName(b.Host, b.Name, component)
		writerLink(t, roots+"/"+name, b.Dir+"/"+paths[i])
		want = append(want, "roots", "mkdir:"+roots, "roots", "link:"+roots+"/"+name+"=>"+b.Dir+"/"+paths[i])
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("ordered effects:\n%v\nwant %v", events, want)
	}
	for path, mode := range map[string]os.FileMode{b.Dir: 0750, b.Dir + "/generation.json": 0640, roots: 0750} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode %s: %v %v", path, info, err)
		}
	}
	content, err := os.ReadFile(b.Dir + "/generation.json")
	if err != nil {
		t.Fatal(err)
	}
	// Literal source expectation: six fields, ordered metadata, exact tokens,
	// two-space pretty layout and one LF; no writer-driven golden update.
	wantJSON := `{
  "mode": "flakes",
  "date": "2026-10-08T12:03:04Z",
  "toplevel": "/nix/store/system",
  "auto_rollback": "/nix/store/rollback",
  "inputs": {
    "z": "/nix/store/z",
    "a": "/nix/store/a"
  },
  "inputs_info": {
    "z": {
      "large": 9007199254740993,
      "nested": [
        {
          "huge": 999999999999999999999999999999,
          "null": null,
          "float": 1.0
        }
      ]
    },
    "a": "žluť / <&>\n\"\\"
  }
}
`
	if string(content) != wantJSON {
		t.Fatalf("record bytes\n%s\nwant\n%s", content, wantJSON)
	}
	readOnly(t, root, func() {
		loaded, err := LoadBuild(root, b.Host, b.Name, Options{})
		if err != nil || loaded.InputsInfo[0].Name != "z" || loaded.InputsInfo[1].Name != "a" {
			t.Fatal(loaded, err)
		}
		nested := loaded.InputsInfo[0].Value.(Object)[1].Value.([]any)[0].(Object)
		if nested[0].Value != json.Number("999999999999999999999999999999") || nested[2].Value != json.Number("1.0") {
			t.Fatal(nested)
		}
	})
	if _, err := os.Lstat(filepath.Dir(b.Dir) + "/current"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Save changed current", err)
	}
}

type writerFaultFile struct {
	io.WriteCloser
	writeErr, closeErr error
	events             *[]string
}

func (f writerFaultFile) Write(data []byte) (int, error) {
	*f.events = append(*f.events, "write")
	if f.writeErr != nil {
		n, err := f.WriteCloser.Write(data[:7])
		if err != nil {
			return n, err
		}
		return n, f.writeErr
	}
	return f.WriteCloser.Write(data)
}
func (f writerFaultFile) Close() error {
	*f.events = append(*f.events, "close")
	err := f.WriteCloser.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func TestWriterFailurePrefixAndNoCleanup(t *testing.T) {
	for _, phase := range []string{"mkdir", "toplevel", "auto_rollback", "input.z", "input.a", "open", "encode", "write", "close", "encode+close", "write+close", "root-lookup1", "root-mkdir1", "root-lookup2", "root-link1", "root-link2", "root-link3"} {
		t.Run(phase, func(t *testing.T) {
			_, roots, w, b := writerFixture(t)
			sentinel := errors.New("fixture " + phase)
			closeError := errors.New("fixture close precedence")
			var events []string
			old := w.ops
			rootLookups, rootLinks := 0, 0
			w.ops.mkdir = func(path string) error {
				label := "mkdir"
				if path == roots {
					label = "root-mkdir1"
				}
				events = append(events, label)
				if phase == label {
					return sentinel
				}
				return old.mkdir(path)
			}
			w.ops.symlink = func(target, path string) error {
				label := ""
				if strings.HasPrefix(path, roots+"/") {
					rootLinks++
					label = fmt.Sprintf("root-link%d", rootLinks)
				} else {
					switch filepath.Base(path) {
					case "toplevel", "auto_rollback":
						label = filepath.Base(path)
					case "z.input":
						label = "input.z"
					case "a.input":
						label = "input.a"
					}
				}
				events = append(events, label)
				if phase == label {
					return sentinel
				}
				return old.symlink(target, path)
			}
			w.roots = func() (string, error) {
				rootLookups++
				label := fmt.Sprintf("root-lookup%d", rootLookups)
				events = append(events, label)
				if phase == label {
					return "", sentinel
				}
				return roots, nil
			}
			w.ops.open = func(path string) (io.WriteCloser, error) {
				events = append(events, "open")
				if phase == "open" {
					return nil, sentinel
				}
				file, err := old.open(path)
				if err != nil {
					return nil, err
				}
				var writeErr, closeErr error
				if strings.HasPrefix(phase, "write") {
					writeErr = sentinel
				}
				if strings.Contains(phase, "close") {
					closeErr = closeError
				}
				return writerFaultFile{file, writeErr, closeErr, &events}, nil
			}
			if strings.HasPrefix(phase, "encode") {
				b.InputsInfo = Object{{"bad", string([]byte{0xff})}}
			}
			err := w.Save(b)
			if strings.Contains(phase, "close") {
				if !errors.Is(err, closeError) {
					t.Fatalf("close precedence %v", err)
				}
			} else if phase == "encode" {
				if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
					t.Fatal(err)
				}
			} else if !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Dir(b.Dir) + "/current"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed Save changed current", err)
			}
			if phase == "mkdir" {
				if _, err := os.Stat(b.Dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("mkdir failure mutated", err)
				}
				return
			}
			if phase != "toplevel" {
				writerLink(t, b.Dir+"/toplevel", b.Toplevel)
			}
			if strings.HasPrefix(phase, "encode") {
				data, err := os.ReadFile(b.Dir + "/generation.json")
				if err != nil || len(data) != 0 {
					t.Fatal("encode failure must leave truncated file", string(data), err)
				}
				if !reflect.DeepEqual(events[len(events)-2:], []string{"open", "close"}) {
					t.Fatal(events)
				}
			}
			if strings.HasPrefix(phase, "write") {
				data, err := os.ReadFile(b.Dir + "/generation.json")
				if err != nil || string(data) != "{\n  \"mo" {
					t.Fatal("write failure prefix lost", string(data), err)
				}
			}
			if phase == "root-link2" || phase == "root-link3" {
				writerLink(t, roots+"/"+RootName(b.Host, b.Name, "toplevel"), b.Dir+"/toplevel")
			}
			if phase == "root-link3" {
				writerLink(t, roots+"/"+RootName(b.Host, b.Name, "auto_rollback"), b.Dir+"/auto_rollback")
			}
			if !strings.HasPrefix(phase, "root-") {
				if rootLookups != 0 {
					t.Fatal("roots looked up after earlier failure")
				}
			}
			// The failed stage is terminal: all later effects remain absent.
			if phase == "root-link2" && rootLinks != 2 || phase == "root-lookup2" && rootLinks != 0 || phase == "root-lookup1" && rootLookups != 1 {
				t.Fatal(events)
			}
		})
	}
}

func TestWriterCollisionsMalformedTargetsAndLateRootLookup(t *testing.T) {
	_, roots, w, b := writerFixture(t)
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, w.root)
	if err := w.Save(b); !errors.Is(err, os.ErrExist) {
		t.Fatalf("same-second collision allocated/overwrote: %v", err)
	}
	if !reflect.DeepEqual(before, treeSnapshot(t, w.root)) {
		t.Fatal("collision changed existing tree")
	}
	_, _, w, b = writerFixture(t)
	b.AutoRollback = nil
	if err := w.Save(b); err == nil {
		t.Fatal("nil rollback skipped")
	}
	writerLink(t, b.Dir+"/toplevel", b.Toplevel)
	if _, err := os.Lstat(b.Dir + "/generation.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("nil rollback created record", err)
	}
	_, _, w, b = writerFixture(t)
	w.roots = func() (string, error) { _, err := ComputeRootIdentity(w.root, nil); return "", err }
	if err := w.Save(b); err == nil || err.Error() != "login identity is missing" {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(b.Dir + "/generation.json"); err != nil || len(data) == 0 {
		t.Fatal("missing login erased JSON", err)
	}
	_, roots, w, b = writerFixture(t)
	if err := os.MkdirAll(b.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.Dir+"/generation.json", []byte("interrupted record"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Save(b); err != nil {
		t.Fatal("existing partial directory prematurely rejected", err)
	}
	if info, err := os.Stat(b.Dir + "/generation.json"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("truncation changed existing mode")
	}
	// Removing only links permits a later record/root collision to be reached.
	for _, name := range []string{"toplevel", "auto_rollback", "z.input", "a.input"} {
		if err := os.Remove(b.Dir + "/" + name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Save(b); !errors.Is(err, os.ErrExist) {
		t.Fatal("root collision skipped/repaired", err)
	}
	writerLink(t, roots+"/"+RootName(b.Host, b.Name, "toplevel"), b.Dir+"/toplevel")
	_, _, w, b = writerFixture(t)
	if err := os.MkdirAll(b.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/missing", b.Dir+"/toplevel"); err != nil {
		t.Fatal(err)
	}
	if err := w.Save(b); !errors.Is(err, os.ErrExist) {
		t.Fatal("dangling entry replaced", err)
	}
	writerLink(t, b.Dir+"/toplevel", "/missing")
}

func TestWriterEncoderRejectsWithoutReplacementAndPreservesEmptyMaps(t *testing.T) {
	for _, value := range []any{string([]byte{0xff}), json.Number(""), json.Number("1e"), math.NaN(), math.Inf(1), map[string]any{"unowned": "map"}, Object{{string([]byte{0xff}), "value"}}} {
		_, _, w, b := writerFixture(t)
		b.InputsInfo = Object{{"bad", value}}
		if err := w.Save(b); err == nil {
			t.Fatalf("unrepresentable encoded: %#v", value)
		}
		if data, err := os.ReadFile(b.Dir + "/generation.json"); err != nil || len(data) != 0 {
			t.Fatalf("failure before open or replacement bytes %q %v", data, err)
		}
	}
	_, _, w, b := writerFixture(t)
	b.Inputs = nil
	b.InputsInfo = nil
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(b.Dir + "/generation.json")
	if !strings.Contains(string(data), `"inputs": {}`) || !strings.Contains(string(data), `"inputs_info": {}`) {
		t.Fatal(string(data))
	}
}

func TestWriterRootResolverSecondAccessAndShortWrite(t *testing.T) {
	root, _, w, b := writerFixture(t)
	lookups := 0
	w.roots = func() (string, error) {
		lookups++
		if lookups == 1 {
			return root + "/mkdir-root", nil
		}
		return root + "/different-destination-root", nil
	}
	if err := w.Save(b); !errors.Is(err, os.ErrNotExist) || lookups != 2 {
		t.Fatalf("root access point cached/prevalidated: %v %d", err, lookups)
	}
	if info, err := os.Stat(root + "/mkdir-root"); err != nil || !info.IsDir() {
		t.Fatal("root mkdir prefix missing", err)
	}
	if data, err := os.ReadFile(b.Dir + "/generation.json"); err != nil || len(data) == 0 {
		t.Fatal("root error erased record", err)
	}
	_, _, w, b = writerFixture(t)
	oldOpen := w.ops.open
	w.ops.open = func(path string) (io.WriteCloser, error) {
		file, err := oldOpen(path)
		if err != nil {
			return nil, err
		}
		return writerShortFile{file}, nil
	}
	if err := w.Save(b); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short write accepted", err)
	}
	if data, err := os.ReadFile(b.Dir + "/generation.json"); err != nil || string(data) != "{\n  \"mo" {
		t.Fatal("short-write prefix missing", string(data), err)
	}
}

type writerShortFile struct{ io.WriteCloser }

func (f writerShortFile) Write(data []byte) (int, error) { return f.WriteCloser.Write(data[:7]) }

func TestWriterSourceGeneratorNestingAndCycleBoundary(t *testing.T) {
	for _, arrays := range []int{97, 98, 99} {
		_, _, w, b := writerFixture(t)
		var value any = "leaf"
		for i := 0; i < arrays; i++ {
			value = []any{value}
		}
		b.InputsInfo = Object{{"nested", value}}
		err := w.Save(b)
		// One record + one info object + the arrays: 100 succeeds, 101
		// fails during generation and leaves the already-open file empty.
		if arrays <= 98 {
			if err != nil {
				t.Fatal(arrays, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "nesting exceeds 100") {
			t.Fatal("source generator nesting boundary lost", arrays, err)
		} else if content := mustWriterRead(t, b.Dir+"/generation.json"); len(content) != 0 {
			t.Fatal("nesting error left usable JSON", string(content))
		}
	}
	_, _, w, b := writerFixture(t)
	cycle := []any{nil}
	cycle[0] = cycle
	b.InputsInfo = Object{{"cycle", cycle}}
	if err := w.Save(b); err == nil || !strings.Contains(err.Error(), "nesting exceeds 100") {
		t.Fatal("cyclic source input recursed without the source limit", err)
	}
	if len(mustWriterRead(t, b.Dir+"/generation.json")) != 0 {
		t.Fatal("cycle encoded after error")
	}
}
