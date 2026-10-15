package generation

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func fixedWriterTime() time.Time { return time.Date(2026, 10, 8, 12, 3, 4, 123456789, time.UTC) }

func TestWriterReuseFirstObjectWithoutMetadataOrRootRepair(t *testing.T) {
	root, roots, w, b := writerFixture(t)
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	// The original supported record predates the optional rollback link. Reuse
	// deliberately does not inspect or reconstruct it or any missing GC root.
	if err := os.Remove(b.Dir + "/auto_rollback"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(roots + "/" + RootName(b.Host, b.Name, "auto_rollback")); err != nil {
		t.Fatal(err)
	}
	list, err := LoadBuildList(root, b.Host, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := list.Builds[0]
	other := *first
	other.InputsInfo = Object{{"other", "metadata"}}
	rollback := "different"
	other.AutoRollback = &rollback
	kernel := "different"
	other.KernelVersion = &kernel
	list.Builds = append(list.Builds, &other)
	beforeJSON, _ := os.ReadFile(b.Dir + "/generation.json")
	w.roots = func() (string, error) { t.Fatal("reuse looked up roots"); return "", nil }
	input := Object{{"a", "/nix/store/a"}, {"z", "/nix/store/z"}}
	match := FindReusable(list, b.Toplevel, input)
	if match != first {
		t.Fatal("not first matching object", match)
	}
	if err := w.SetCurrent(list, match); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeJSON, mustWriterRead(t, b.Dir+"/generation.json")) {
		t.Fatal("reuse rewrote metadata")
	}
	for _, path := range []string{b.Dir + "/auto_rollback", roots + "/" + RootName(b.Host, b.Name, "auto_rollback")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("reuse repaired missing link", path, err)
		}
	}
	if got := FindReusable(list, "different", input); got != nil {
		t.Fatal("unequal toplevel reused")
	}
	if got := FindReusable(list, b.Toplevel, Object{{"a", "wrong"}, {"z", "/nix/store/z"}}); got != nil {
		t.Fatal("unequal input reused")
	}
	if got := FindReusable(list, b.Toplevel, Object{{"different", "/nix/store/a"}, {"z", "/nix/store/z"}}); got != nil {
		t.Fatal("unequal input keys reused")
	}
	empty := &Build{Toplevel: "empty", Inputs: Object{}}
	if got := FindReusable(&BuildList{Builds: []*Build{empty}}, "empty", nil); got != empty {
		t.Fatal("empty map not reusable")
	}
}

func mustWriterRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriterReuseCurrentEffectSurvivesLaterHostFailure(t *testing.T) {
	root, _, w, b := writerFixture(t)
	if err := w.Save(b); err != nil {
		t.Fatal(err)
	}
	list, err := LoadBuildList(root, b.Host, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := FindReusable(list, b.Toplevel, b.Inputs)
	if first == nil {
		t.Fatal("missing reuse")
	}
	if err := w.SetCurrent(list, first); err != nil {
		t.Fatal(err)
	}
	// An ordinary scripted caller stops at host2's later save failure. No batch
	// preflight/rollback is permitted to erase host1's completed current change.
	second, err := CreateFlake(root, "second", b.Toplevel, nil, Object{}, Object{}, fixedWriterTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Save(second); err == nil {
		t.Fatal("nil rollback did not fail")
	}
	writerLink(t, root+"/.confctl/generations/"+SafeHost(b.Host)+"/current", b.Name)
	writerLink(t, second.Dir+"/toplevel", b.Toplevel)
	if list.Current != first || !first.Current {
		t.Fatal("later failure rolled back reuse")
	}
}
