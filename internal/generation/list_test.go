package generation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGenerationWarningBuildRefusal(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "h", "bad", `{`)
	seed(t, root, "h", "good", record(root+"/system", "2026-10-09T00:00:00Z"))
	seed(t, root, "h", "later-bad", `{`)
	failure := errors.New("warning refusal")
	for _, late := range []bool{false, true} {
		calls := 0
		readOnly(t, root, func() {
			list, err := LoadBuildList(root, "h", Options{Enumerate: func(string) ([]string, error) {
				if late {
					return []string{"bad", "good"}, nil
				}
				return []string{"bad", "later-bad", "good"}, nil
			}, Warn: func(message string) error {
				calls++
				if calls == 1 && late {
					return nil
				}
				return failure
			}})
			if list != nil || err != failure || calls != map[bool]int{false: 1, true: 2}[late] {
				t.Fatal("warning did not refuse immediately or returned partial list")
			}
		})
	}
	list, err := LoadBuildList(root, "h", Options{})
	if err != nil || len(list.Builds) != 1 || list.Current == nil {
		t.Fatal("nil warning changed successful selection")
	}
}

func names(l *BuildList) []string {
	n := []string{}
	for _, b := range l.Builds {
		n = append(n, b.Name)
	}
	return n
}
func TestEnumerationRejectionsTieOrderAndWarnings(t *testing.T) {
	root := t.TempDir()
	host := "nested/host"
	top := filepath.Join(root, "system")
	for _, n := range []string{"z", "a", "middle"} {
		date := "2026-10-08T12:00:00.123456789123Z"
		if n == "middle" {
			date = "2026-10-08T12:00:00.123456789124Z"
		}
		seed(t, root, host, n, record(top, date))
	}
	seed(t, root, host, "legacy", `{"mode":"swpins"}`)
	seed(t, root, host, "corrupt", `{`)
	dir := filepath.Join(root, ".confctl", "generations", SafeHost(host))
	if err := os.Mkdir(filepath.Join(dir, "missing-json"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("z", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	order := []string{".", "..", "middle", "z", "legacy", "alias", "a", "corrupt", "file", "missing-json"}
	var warnings []string
	readOnly(t, root, func() {
		l, err := LoadBuildList(root, host, Options{Location: time.UTC, Enumerate: func(string) ([]string, error) { return order, nil }, Warn: func(s string) error { warnings = append(warnings, s); return nil }})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(names(l), []string{"z", "a", "middle"}) {
			t.Fatal(names(l))
		}
		if len(l.Rejected) != 3 || l.Rejected[0].Name != "legacy" || l.Rejected[1].Name != "corrupt" || l.Rejected[2].Name != "missing-json" {
			t.Fatal(l.Rejected)
		}
		if l.Current == nil || l.Current.Name != "middle" {
			t.Fatal(l.Current)
		}
		if len(warnings) != 4 || !strings.Contains(warnings[0], "legacy") || !strings.Contains(warnings[1], "corrupt") || !strings.Contains(warnings[2], "missing-json") || !strings.Contains(warnings[3], "selecting newest supported") {
			t.Fatal(warnings)
		}
		if _, err := ResolveBuild(l, "0"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatal(err)
		}
		if _, err := ResolveBuild(l, "legacy"); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatal(err)
		}
		if b, err := ResolveBuild(l, "z"); err != nil || b == nil {
			t.Fatal(b, err)
		}
	})
}
func TestCurrentIdentityAndSelection(t *testing.T) {
	for _, kind := range []string{"absent", "relative", "absolute", "path_alias", "broken", "broken_basename", "external", "file", "rejected", "empty"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			host := "host"
			top := filepath.Join(root, "system")
			if kind == "empty" {
				readOnly(t, root, func() {
					l, err := LoadBuildList(root, host, Options{})
					if err != nil || l.Current != nil || len(l.Builds) != 0 {
						t.Fatal(l, err)
					}
					b, err := ResolveBuild(l, "current")
					if err != nil || b != nil {
						t.Fatal(b, err)
					}
				})
				return
			}
			a := seed(t, root, host, "A", record(top, "2026-10-08T12:00:00Z"))
			seed(t, root, host, "B", record(top, "2026-10-08T12:01:00Z"))
			dir := filepath.Dir(a)
			current := filepath.Join(dir, "current")
			target := ""
			switch kind {
			case "relative":
				target = "A"
			case "absolute":
				target = a
			case "path_alias":
				if err := os.Symlink(dir, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(root, "alias", "A")
			case "broken":
				target = "missing"
			case "broken_basename":
				target = "missing-dir/A"
			case "external":
				outside := filepath.Join(root, "outside", "A")
				if err := os.MkdirAll(outside, 0755); err != nil {
					t.Fatal(err)
				}
				target = outside
			case "file":
				if err := os.WriteFile(current, []byte("A"), 0644); err != nil {
					t.Fatal(err)
				}
			case "rejected":
				seed(t, root, host, "legacy", `{"mode":"swpins"}`)
				target = "legacy"
			}
			if target != "" {
				if err := os.Symlink(target, current); err != nil {
					t.Fatal(err)
				}
			}
			readOnly(t, root, func() {
				l, err := LoadBuildList(root, host, Options{})
				if err != nil {
					t.Fatal(err)
				}
				bad := kind == "broken" || kind == "broken_basename" || kind == "external" || kind == "file" || kind == "rejected"
				if bad {
					if l.Current != nil || l.CurrentError == nil {
						t.Fatal(l)
					}
					if _, err := ResolveBuild(l, "current"); err == nil {
						t.Fatal("accepted unresolved current")
					}
				} else {
					want := "A"
					if kind == "absent" {
						want = "B"
					}
					if l.Current == nil || l.Current.Name != want || l.CurrentError != nil {
						t.Fatal(l)
					}
				}
				if b, err := ResolveBuild(l, "A"); err != nil || b == nil || b.Name != "A" {
					t.Fatal(b, err)
				}
				if kind != "rejected" {
					if b, err := ResolveBuild(l, "0"); err != nil || b.Name != "B" {
						t.Fatal(b, err)
					}
				}
			})
		})
	}
}
func TestNumericBoundsAndLiteralNames(t *testing.T) {
	root := t.TempDir()
	for i, n := range []string{"A", "B", "C", "+1", "old", "none", "1d"} {
		seed(t, root, "host", n, record(filepath.Join(root, "system"), fmt.Sprintf("2026-10-08T12:%02d:00Z", i)))
	}
	readOnly(t, root, func() {
		l, err := LoadBuildList(root, "host", Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ input, want string }{
			{"0", "1d"}, {"-0", "1d"}, {"00", "1d"}, {"1", "A"}, {"-1", "none"}, {"-2", "old"}, {"7", "1d"}, {"8", ""}, {"-7", ""}, {"+1", "+1"}, {"old", "old"}, {"none", "none"}, {"1d", "1d"}, {"A*", ""}, {"0x1", ""}, {"1\n", ""}, {"99999999999999999999999999999999999999", ""}, {"-99999999999999999999999999999999999999", ""},
		} {
			b, err := ResolveBuild(l, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if b != nil {
				got = b.Name
			}
			if got != tc.want {
				t.Fatalf("%q got %q want %q", tc.input, got, tc.want)
			}
		}
	})
}
