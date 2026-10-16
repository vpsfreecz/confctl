package generation

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWriterSetCurrentMemoryBeforeEveryFailure(t *testing.T) {
	for _, phase := range []string{"random", "collision", "rename", "missing-parent"} {
		t.Run(phase, func(t *testing.T) {
			root, _, w, b := writerFixture(t)
			sentinel := errors.New("fixture " + phase)
			old := &Build{Name: "old", Current: true}
			sameName := &Build{Name: b.Name, Current: true}
			rejection := Rejected{Name: "rejected", Path: "retained", Reason: "retained reason"}
			list := &BuildList{Host: "list/host", Builds: []*Build{old, sameName}, Current: old, CurrentError: errors.New("old current failure"), Rejected: []Rejected{rejection}, rejected: map[string]Rejected{"rejected": rejection}, index: map[string]*Build{"old": old, b.Name: sameName}}
			dir := root + "/.confctl/generations/list:host"
			current := dir + "/current"
			if phase != "missing-parent" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "random" {
				w.suffix = func() (string, error) { return "", sentinel }
			}
			if phase == "collision" {
				if err := os.Symlink("existing", current+".new-abcdef"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "rename" {
				w.ops.rename = func(string, string) error { return sentinel }
			}
			err := w.SetCurrent(list, b)
			if phase == "random" || phase == "rename" {
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected filesystem failure")
			}
			if list.CurrentError != nil || list.Current != b || old.Current || sameName.Current || !b.Current || list.index[b.Name] != b || !reflect.DeepEqual(list.Builds, []*Build{old, sameName, b}) || !reflect.DeepEqual(list.Rejected, []Rejected{rejection}) || list.rejected["rejected"] != rejection {
				t.Fatal("memory prefix or rejected state changed", list)
			}
			if phase == "rename" {
				writerLink(t, current+".new-abcdef", b.Name)
			}
			if phase == "collision" {
				writerLink(t, current+".new-abcdef", "existing")
			}
			if phase == "random" {
				if _, err := os.Lstat(current + ".new-abcdef"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("random failure opened temp", err)
				}
			}
			if _, err := os.Lstat(current); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failure replaced current", err)
			}
			if phase == "missing-parent" {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("current setter repaired parent")
				}
			}
		})
	}
}

func TestWriterSetCurrentDiskKindsAndFreshReader(t *testing.T) {
	for _, kind := range []string{"absent", "valid", "broken", "external", "rejected", "regular", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, _, w, b := writerFixture(t)
			if err := w.Save(b); err != nil {
				t.Fatal(err)
			}
			current := filepath.Dir(b.Dir) + "/current"
			switch kind {
			case "valid":
				if err := os.Symlink(b.Name, current); err != nil {
					t.Fatal(err)
				}
			case "broken":
				if err := os.Symlink("missing", current); err != nil {
					t.Fatal(err)
				}
			case "external":
				external := root + "/external/" + b.Name
				if err := os.MkdirAll(external, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, current); err != nil {
					t.Fatal(err)
				}
			case "rejected":
				seed(t, root, b.Host, "bad", `{"mode":"legacy"}`)
				if err := os.Symlink("bad", current); err != nil {
					t.Fatal(err)
				}
			case "regular":
				if err := os.WriteFile(current, []byte("not a link"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(current, 0755); err != nil {
					t.Fatal(err)
				}
			}
			list, err := LoadBuildList(root, b.Host, Options{})
			if err != nil {
				t.Fatal(err)
			}
			selected := list.Builds[0]
			err = w.SetCurrent(list, selected)
			if kind == "directory" {
				if err == nil {
					t.Fatal("setter removed directory")
				}
				writerLink(t, current+".new-abcdef", b.Name)
			} else if err != nil {
				t.Fatal(err)
			} else {
				writerLink(t, current, b.Name)
				if _, err := os.Lstat(current + ".new-abcdef"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("successful rename retained temp")
				}
			}
			if list.Current != selected || list.CurrentError != nil || !selected.Current || len(list.Builds) != 1 {
				t.Fatal("existing object appended/reordered")
			}
			readOnly(t, root, func() {
				fresh, err := LoadBuildList(root, b.Host, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if fresh.CurrentError == nil || fresh.Current != nil {
						t.Fatal("fresh reader hid actual directory failure")
					}
				} else if fresh.CurrentError != nil || fresh.Current == nil || fresh.Current.Name != b.Name {
					t.Fatal("fresh reader current differs", fresh)
				}
			})
		})
	}
}

func TestWriterPhysicalConfigAndLiteralPathComponents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/physical/child", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"/spelling", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root+"/physical/child", root+"/spelling/link"); err != nil {
		t.Fatal(err)
	}
	configuration := root + "/spelling/link/.."
	login := "private-login"
	identity, err := ComputeRootIdentity(configuration, &login)
	if err != nil || identity.Configuration != root+"/physical" {
		t.Fatal(identity, err)
	}
	roots := root + "/injected-roots"
	w, err := NewWriter(configuration, func() (string, error) { return roots, nil })
	if err != nil {
		t.Fatal(err)
	}
	rollback := "fake/rollback"
	b, err := CreateFlake(configuration, "space /host$", "fake/top", &rollback, Object{}, Object{}, fixedWriterTime())
	if err != nil {
		t.Fatal(err)
	}
	if b.Dir != root+"/physical/.confctl/generations/space :host$/2026-10-08--12-03-04" {
		t.Fatal(b.Dir)
	}
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	writerLink(t, b.Dir+"/toplevel", "fake/top")
	writerLink(t, roots+"/"+RootName(b.Host, b.Name, "toplevel"), b.Dir+"/toplevel")
	// Neither metadata roles nor symlink targets are cleaned or reinterpreted.
	_, _, w, b = writerFixture(t)
	b.Inputs = Object{{"missing/role", "literal/input"}}
	if err := w.Save(b); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("malformed role got extra parents/sanitizing", err)
	}
	writerLink(t, b.Dir+"/auto_rollback", *b.AutoRollback)
	if _, err := os.Stat(b.Dir + "/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("role parent created")
	}
	_, _, w, b = writerFixture(t)
	if err := os.MkdirAll(b.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w.root+"/other/child", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(w.root+"/other/child", b.Dir+"/link"); err != nil {
		t.Fatal(err)
	}
	b.Inputs = Object{{"link/../role", "literal/input"}}
	if err := w.Save(b); err == nil {
		t.Fatal("root with malformed role should fail at its native destination")
	}
	writerLink(t, w.root+"/other/role.input", "literal/input")
	if _, err := os.Lstat(b.Dir + "/role.input"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("role link/.. lexically collapsed")
	}
}
