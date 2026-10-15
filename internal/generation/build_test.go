package generation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Source-derived payload assertions. The optional original adapter in
// ruby_adapter_test.go is separate from the historical accepted corpus.
func seed(t *testing.T, root, host, name, raw string) string {
	t.Helper()
	dir := filepath.Join(root, ".confctl", "generations", SafeHost(host), name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generation.json"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}
func record(top, date string) string {
	return fmt.Sprintf(`{"mode":"flakes","date":%q,"toplevel":%q,"inputs":{}}`, date, top)
}
func treeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		value := ""
		if d.Type()&os.ModeSymlink != 0 {
			value, err = os.Readlink(path)
		} else if !d.IsDir() {
			var b []byte
			b, err = os.ReadFile(path)
			value = fmt.Sprintf("%x", sha256.Sum256(b))
		}
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s|%s|%d|%s|%s", rel, info.Mode(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), value))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
func readOnly(t *testing.T, root string, fn func()) {
	t.Helper()
	before := treeSnapshot(t, root)
	defer func() {
		after := treeSnapshot(t, root)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("read changed disposable tree\nbefore %v\nafter %v", before, after)
		}
	}()
	fn()
}

func TestRecordPayloadAndReadOnly(t *testing.T) {
	cases := []struct {
		name, payload, reason string
		unsupported           bool
	}{
		{"minimal", `{"mode":"flakes"}`, "", false},
		{"missing_mode", `{}`, "unsupported generation mode nil", true},
		{"null_mode", `{"mode":null}`, "unsupported generation mode nil", true},
		{"legacy_mode", `{"mode":"swpins"}`, "unsupported generation mode", true},
		{"new_mode", `{"mode":"flakes-v2"}`, "unsupported generation mode", true},
		{"null_inputs", `{"mode":"flakes","inputs":null}`, "invalid flake generation payload", false},
		{"wrong_input", `{"mode":"flakes","inputs":{"role":7}}`, "invalid flake generation payload", false},
		{"wrong_rollback", `{"mode":"flakes","auto_rollback":false}`, "invalid flake generation payload", false},
		{"null_rollback", `{"mode":"flakes","auto_rollback":null}`, "", false},
		{"empty_rollback", `{"mode":"flakes","auto_rollback":""}`, "", false},
		{"null_metadata", `{"mode":"flakes","inputs_info":null}`, "", false},
		{"false_metadata", `{"mode":"flakes","inputs_info":false}`, "", false},
		{"wrong_metadata", `{"mode":"flakes","inputs_info":[]}`, "invalid flake generation payload", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			top := filepath.Join(root, "system")
			var obj map[string]any
			if err := json.Unmarshal([]byte(tc.payload), &obj); err != nil {
				t.Fatal(err)
			}
			obj["toplevel"] = top
			obj["date"] = "2026-10-08T12:00:00Z"
			raw, _ := json.Marshal(obj)
			seed(t, root, "nested/host", "record", string(raw))
			readOnly(t, root, func() {
				build, err := LoadBuild(root, "nested/host", "record", Options{Location: time.UTC})
				if tc.reason == "" {
					if err != nil {
						t.Fatal(err)
					}
					if build.Mode != "flakes" || build.Toplevel != top || len(build.Inputs) != 0 {
						t.Fatalf("%+v", build)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("error %v; want %s", err, tc.reason)
				}
				re, ok := err.(*RecordError)
				if !ok || re.Unsupported != tc.unsupported {
					t.Fatalf("class %T: %v", err, err)
				}
			})
		})
	}
	for _, raw := range []string{`[]`, `null`, `{`, `{"mode":"flakes","date":"bad","toplevel":"x"}`, `{"mode":"flakes","date":"2026-10-08T12:00:00Z","toplevel":""}`, `{"mode":"flakes","date":null,"toplevel":"x"}`, `{"mode":"flakes","date":"2026-10-08T12:00:00Z"}`, `{"mode":"flakes"} true`} {
		root := t.TempDir()
		seed(t, root, "host", "bad", raw)
		readOnly(t, root, func() {
			if _, err := LoadBuild(root, "host", "bad", Options{}); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
}
func TestMetadataOrderNumbersAliasesAndDuplicates(t *testing.T) {
	root := t.TempDir()
	top := filepath.Join(root, "system")
	raw := fmt.Sprintf(`{"mode":"flakes","date":"2026-10-08T12:00:00Z","toplevel":%q,"inputs_info":{"z":{"rev":"abcdef012345","large":9007199254740993,"nested":[{"fraction":1.23456789123456789123,"short_rev":"a","shortRev":"b"}]},"a":null,"z":{"large":999999999999999999999999999999,"nested":[1e1000]}},"inputsInfo":{"ignored":"revision"},"newer_unknown":{"schema":999}}`, top)
	seed(t, root, "host", "named", raw)
	readOnly(t, root, func() {
		b, err := LoadBuild(root, "host", "named", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(b.InputsInfo) != 2 || b.InputsInfo[0].Name != "z" || b.InputsInfo[1].Name != "a" {
			t.Fatalf("order %v", b.InputsInfo)
		}
		z := b.InputsInfo.value("z").(Object)
		if z.value("large").(json.Number).String() != "999999999999999999999999999999" {
			t.Fatal(z)
		}
		if z.value("nested").([]any)[0].(json.Number).String() != "1e1000" {
			t.Fatal(z)
		}
		if _, ok := b.Raw.Lookup("newer_unknown"); !ok {
			t.Fatal("lost unknown metadata")
		}
	})
	for _, tc := range []struct{ first, expected string }{
		{`null`, "alias"}, {`false`, "alias"}, {`{}`, ""}, {`{"primary":"rev"}`, "primary"},
	} {
		raw := fmt.Sprintf(`{"mode":"flakes","date":"2026-10-08T12:00:00Z","toplevel":%q,"inputs_info":%s,"inputsInfo":{"alias":"rev"}}`, top, tc.first)
		seed(t, root, "host", "named", raw)
		readOnly(t, root, func() {
			b, err := LoadBuild(root, "host", "named", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.expected == "" {
				if len(b.InputsInfo) != 0 {
					t.Fatal(b.InputsInfo)
				}
			} else if _, ok := b.InputsInfo.Lookup(tc.expected); !ok {
				t.Fatal(b.InputsInfo)
			}
		})
	}
	v, err := decodeRecord([]byte(`{"x":{"large":9007199254740993,"decimal":1.23456789123456789123,"array":[{"a":1,"b":2,"a":3}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	x := v.(Object).value("x").(Object)
	if x.value("large").(json.Number) != "9007199254740993" || x.value("decimal").(json.Number) != "1.23456789123456789123" {
		t.Fatal(x)
	}
	nested := x.value("array").([]any)[0].(Object)
	if len(nested) != 2 || nested[0].Name != "a" || nested[0].Value.(json.Number) != "3" {
		t.Fatal(nested)
	}
}
func TestKernelLinks(t *testing.T) {
	root := t.TempDir()
	top := filepath.Join(root, "system")
	if err := os.Mkdir(top, 0755); err != nil {
		t.Fatal(err)
	}
	seed(t, root, "host", "named", record(top, "2026-10-08T12:00:00Z"))
	for _, tc := range []struct{ kernel, bz, want string }{
		{"", "", ""}, {"relative-linux-1", "", ""}, {"", "/nix/store/hash-linux-6.12.4/bzImage", "6.12.4"},
		{"/nix/store/hash-linux-6.12.3/bzImage", "/nix/store/hash-linux-6.12.4/bzImage", "6.12.3"},
	} {
		for _, name := range []string{"kernel", "bzImage"} {
			_ = os.Remove(filepath.Join(top, name))
		}
		if tc.kernel != "" {
			if err := os.Symlink(tc.kernel, filepath.Join(top, "kernel")); err != nil {
				t.Fatal(err)
			}
		}
		if tc.bz != "" {
			if err := os.Symlink(tc.bz, filepath.Join(top, "bzImage")); err != nil {
				t.Fatal(err)
			}
		}
		readOnly(t, root, func() {
			b, err := LoadBuild(root, "host", "named", Options{})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if b.KernelVersion != nil {
				got = *b.KernelVersion
			}
			if got != tc.want {
				t.Fatalf("%q want %q", got, tc.want)
			}
		})
	}
	_ = os.Remove(filepath.Join(top, "kernel"))
	if err := os.WriteFile(filepath.Join(top, "kernel"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	readOnly(t, root, func() {
		if _, err := LoadBuild(root, "host", "named", Options{}); err != nil {
			t.Fatal(err)
		}
	})
	seed(t, root, "host", "relative", record("system", "2026-10-08T12:00:00Z"))
	readOnly(t, root, func() {
		b, err := LoadBuild(root, "host", "relative", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if b.Toplevel != "system" || b.KernelVersion == nil || *b.KernelVersion != "6.12.4" {
			t.Fatal(b)
		}
	})
}

// Runtime-observed finite json2.21.2 admission cases for R1-F1. The root
// generation object contributes one nonempty level; an empty leaf does not.
func jsonAdmissionCases() []struct {
	name, value string
	admitted    bool
} {
	type edge = struct {
		name, value string
		admitted    bool
	}
	array := func(depth int, leaf string) string {
		return strings.Repeat("[", depth) + leaf + strings.Repeat("]", depth)
	}
	object := func(depth int, leaf string) string {
		return strings.Repeat(`{"x":`, depth) + leaf + strings.Repeat("}", depth)
	}
	return []edge{
		{"valid_pair", `"\ud800\udc00"`, true},
		{"incomplete_high", `"\ud800"`, false},
		{"high_before_text", `"\ud800x"`, false},
		{"high_before_nonlow", `"\ud800\u0041"`, false},
		{"two_highs", `"\ud800\ud800"`, false},
		{"standalone_low", `"\udc00"`, true},
		{"multiple_lows", `"x\udfff\udc00"`, true},
		{"low_then_bad_high", `"\udc00\ud800"`, false},
		{"escaped_backslash", `"\\ud800"`, true},
		{"high_in_key", `{"\ud800":0}`, false},
		{"low_in_key", `{"\udc00":0}`, true},
		{"array_nonempty_limit", array(99, "0"), true},
		{"array_nonempty_over", array(100, "0"), false},
		{"array_empty_leaf_limit", array(99, "[]"), true},
		{"object_empty_leaf_limit", array(99, "{}"), true},
		{"array_nonempty_leaf_over", array(99, "[0]"), false},
		{"object_nonempty_leaf_over", array(99, `{"x":0}`), false},
		{"array_empty_leaf_over", array(100, "[]"), false},
		{"object_nonempty_limit", object(99, "0"), true},
		{"object_nonempty_over", object(100, "0"), false},
		{"object_empty_leaf_limit2", object(99, "{}"), true},
		{"object_nonempty_leaf_over2", object(99, `{"x":0}`), false},
	}
}
func admissionRecord(top, value string) string {
	return fmt.Sprintf(`{"mode":"flakes","date":"2026-10-08T12:01:00Z","toplevel":%q,"extra":%s}`, top, value)
}
func TestReviewedJSONAdmissionAndRejectedSelection(t *testing.T) {
	for _, tc := range jsonAdmissionCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			top := root + "/missing-system"
			seed(t, root, "host", "good", record(top, "2026-10-08T12:00:00Z"))
			seed(t, root, "host", "candidate", admissionRecord(top, tc.value))
			readOnly(t, root, func() {
				list, err := LoadBuildList(root, "host", Options{})
				if err != nil {
					t.Fatal(err)
				}
				if tc.admitted {
					if len(list.Builds) != 2 || len(list.Rejected) != 0 || list.Current.Name != "candidate" {
						t.Fatalf("%+v", list)
					}
					if b, err := ResolveBuild(list, "0"); err != nil || b.Name != "candidate" {
						t.Fatal(b, err)
					}
				} else {
					if len(list.Builds) != 1 || len(list.Rejected) != 1 || list.Rejected[0].Name != "candidate" || list.Current.Name != "good" {
						t.Fatalf("%+v", list)
					}
					if _, err := ResolveBuild(list, "0"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
						t.Fatal(err)
					}
					if _, err := ResolveBuild(list, "candidate"); err == nil {
						t.Fatal("rejected name was admitted")
					}
				}
			})
			if !tc.admitted {
				if err := os.Symlink("candidate", root+"/.confctl/generations/host/current"); err != nil {
					t.Fatal(err)
				}
				readOnly(t, root, func() {
					list, err := LoadBuildList(root, "host", Options{})
					if err != nil {
						t.Fatal(err)
					}
					if list.Current != nil || list.CurrentError == nil {
						t.Fatalf("%+v", list)
					}
					if _, err := ResolveBuild(list, "current"); err == nil {
						t.Fatal("selected rejected current")
					}
				})
			}
		})
	}
	for _, tc := range []struct{ value, want string }{
		{`"\ud800\udc00"`, "\U00010000"}, {`"\udc00"`, "\xed\xb0\x80"},
		{`"x\udfff\udc00"`, "x\xed\xbf\xbf\xed\xb0\x80"}, {`"a\n\udc00\t\ud800\udc00"`, "a\n\xed\xb0\x80\t\U00010000"},
		{`"\\ud800"`, `\ud800`},
	} {
		v, err := decodeRecord([]byte(tc.value))
		if err != nil {
			t.Fatal(err)
		}
		if v.(string) != tc.want {
			t.Fatalf("%s bytes %x want %x", tc.value, v, tc.want)
		}
	}
	// A bad toplevel is rejected before a kernel lookup can see replacement text.
	root := t.TempDir()
	seed(t, root, "host", "good", record(root+"/missing", "2026-10-08T12:00:00Z"))
	seed(t, root, "host", "bad", `{"mode":"flakes","date":"2026-10-08T12:01:00Z","toplevel":"\ud800"}`)
	readOnly(t, root, func() {
		list, err := LoadBuildList(root, "host", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Rejected) != 1 || list.Current.Name != "good" {
			t.Fatalf("%+v", list)
		}
		if _, err := ResolveBuild(list, "1"); err == nil {
			t.Fatal("numeric selector ignored rejection")
		}
	})
}

func TestReviewedToplevelSymlinkParentComponents(t *testing.T) {
	root := t.TempDir()
	config := root + "/config"
	actual := root + "/actual"
	if err := os.MkdirAll(actual+"/child", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actual+"/system", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config+"/system", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual+"/child", config+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/hash-linux-physical/bzImage", actual+"/system/kernel"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/hash-linux-lexical/bzImage", config+"/system/kernel"); err != nil {
		t.Fatal(err)
	}
	for _, top := range []string{"link/../system", config + "/link/../system"} {
		seed(t, config, "host", "named", record(top, "2026-10-08T12:00:00Z"))
		readOnly(t, root, func() {
			build, err := LoadBuild(config, "host", "named", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if build.Toplevel != top || build.KernelVersion == nil || *build.KernelVersion != "physical" {
				t.Fatalf("%+v", build)
			}
			list, err := LoadBuildList(config, "host", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Builds) != 1 || *list.Current.KernelVersion != "physical" {
				t.Fatalf("%+v", list)
			}
		})
	}
	// Missing intermediate components retain kernelVersion's ENOENT behavior:
	// do not resolve/validate the whole toplevel before trying the two links.
	seed(t, config, "host", "missing", record("missing/../system", "2026-10-08T12:00:00Z"))
	readOnly(t, root, func() {
		b, err := LoadBuild(config, "host", "missing", Options{})
		if err != nil || b.KernelVersion != nil {
			t.Fatal(b, err)
		}
	})
}
