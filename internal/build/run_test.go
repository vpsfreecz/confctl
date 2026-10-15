package build

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/generation"
)

type evalCall struct {
	installable string
	apply       *string
}
type fixture struct {
	t              *testing.T
	root, roots    string
	at             time.Time
	writer         *generation.Writer
	c              Callbacks
	rows, settings generation.Object
	evals          map[string]any
	keys           map[string]string
	trace          []string
	requests       []BuildRequest
	evalCalls      []evalCall
	clockCalls     int
}

func newFixture(t *testing.T, hosts ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir(), roots: t.TempDir(), at: time.Date(2026, 10, 14, 12, 13, 14, 123456789, time.FixedZone("local", 3600)), evals: map[string]any{}, keys: map[string]string{}}
	var err error
	f.writer, err = generation.NewWriter(f.root, func() (string, error) { return f.roots, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range hosts {
		key := "m_" + host + "_01234567"
		f.keys[host] = key
		f.rows = append(f.rows, generation.Member{Name: host, Value: object("key", key, "inputs", object("nixpkgs", "/nix/store/input"))})
		f.addEvalRow(".#confctl.build", key, object("toplevel", "/nix/store/"+host+"-top", "autoRollback", "/nix/store/"+host+"-rollback"))
		f.addEvalRow(".#confctl.inputsInfo", key, object("nixpkgs", object("rev", nil, "lastModified", json.Number("9007199254740993"), "array", []any{false, nil, "literal"})))
	}
	f.c = Callbacks{
		BuildPlan: func() (generation.Object, error) { f.trace = append(f.trace, "plan"); return f.rows, nil },
		Settings:  func() (generation.Object, error) { f.trace = append(f.trace, "settings"); return f.settings, nil },
		MachineKey: func(host string) (string, error) {
			f.trace = append(f.trace, "key "+host)
			key, ok := f.keys[host]
			if !ok {
				return "", errors.New("unknown machine")
			}
			return key, nil
		},
		// A synthetic literal provider tests the consumer's call/argv boundary.
		// This is not a Nix serializer or an encoding equivalence claim.
		KeyLiteral: func(key string) (string, error) {
			f.trace = append(f.trace, "quote "+key)
			return strconv.Quote(key), nil
		},
		Eval: func(installable string, apply *string) (any, error) {
			f.trace = append(f.trace, "eval "+installable)
			if apply != nil {
				copied := *apply
				apply = &copied
			}
			f.evalCalls = append(f.evalCalls, evalCall{installable, apply})
			v, ok := f.evals[installable]
			if !ok {
				return nil, errors.New("unscripted eval " + installable)
			}
			if err, ok := v.(error); ok {
				return nil, err
			}
			return v, nil
		},
		Build: func(request BuildRequest, feed func([]byte, []byte)) ([]byte, error) {
			f.trace = append(f.trace, "build")
			f.requests = append(f.requests, request)
			return []byte(`[{"outputs":{"out":"/non-authoritative"}}]`), nil
		},
		DecodeBuild: func(bytes []byte) (any, error) {
			f.trace = append(f.trace, "decode")
			var v any
			d := json.NewDecoder(strings.NewReader(string(bytes)))
			d.UseNumber()
			err := d.Decode(&v)
			return v, err
		},
		Load: func(host string) (*generation.BuildList, error) {
			f.trace = append(f.trace, "load "+host)
			return generation.LoadBuildList(f.root, host, generation.Options{})
		},
		Create: func(host, top string, rollback *string, inputs, info generation.Object, at time.Time) (*generation.Build, error) {
			f.trace = append(f.trace, "create "+host)
			return generation.CreateFlake(f.root, host, top, rollback, inputs, info, at)
		},
		Save: func(b *generation.Build) error { f.trace = append(f.trace, "save "+b.Host); return f.writer.Save(b) },
		SetCurrent: func(l *generation.BuildList, b *generation.Build) error {
			f.trace = append(f.trace, "current "+b.Host)
			return f.writer.SetCurrent(l, b)
		},
		Now: func() time.Time { f.trace = append(f.trace, "clock"); f.clockCalls++; return f.at },
	}
	return f
}
func (f *fixture) addEvalRow(name, key string, row any) {
	o, _ := f.evals[name].(generation.Object)
	for i := range o {
		if o[i].Name == key {
			o[i].Value = row
			f.evals[name] = o
			return
		}
	}
	f.evals[name] = append(o, generation.Member{Name: key, Value: row})
}
func (f *fixture) row(host string, row generation.Object) {
	for i := range f.rows {
		if f.rows[i].Name == host {
			f.rows[i].Value = row
			return
		}
	}
	f.t.Fatal("missing host")
}
func (f *fixture) old(host string, inputs generation.Object) *generation.Build {
	f.t.Helper()
	top := "/nix/store/" + host + "-top"
	rollback := "/old-rollback"
	b, err := generation.CreateFlake(f.root, host, top, &rollback, inputs, object("old", true), f.at.Add(-24*time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.writer.Save(b); err != nil {
		f.t.Fatal(err)
	}
	return b
}
func names(rows []HostGeneration) []string {
	n := []string{}
	for _, row := range rows {
		n = append(n, row.Host)
	}
	return n
}
func checkLink(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil || got != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}
func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

func TestRunRealWriterReusedBeforeNewAndLazyMetadata(t *testing.T) {
	f := newFixture(t, "a", "b", "c", "d")
	oldB := f.old("b", object("nixpkgs", "/nix/store/input"))
	oldD := f.old("d", object("nixpkgs", "/nix/store/input"))
	// Reuse does not repair roots or old rollback links, and it ignores a
	// non-nil invalid rollback value just as the source find/current path does.
	missingRoot := f.roots + "/" + generation.RootName("b", oldB.Name, "toplevel")
	if err := os.Remove(missingRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldB.Dir + "/auto_rollback"); err != nil {
		t.Fatal(err)
	}
	f.addEvalRow(".#confctl.build", f.keys["b"], object("toplevel", oldB.Toplevel, "autoRollback", false))
	out, err := Run([]string{"a", "b", "c", "d"}, f.c)
	if err != nil || !out.Complete {
		t.Fatalf("%#v %v", out, err)
	}
	if !reflect.DeepEqual(names(out.Generations), []string{"b", "d", "a", "c"}) {
		t.Fatal(names(out.Generations))
	}
	if f.clockCalls != 1 {
		t.Fatal(f.clockCalls)
	}
	wantTrace := []string{"plan", "settings", "clock", "build", "decode", "quote " + f.keys["a"], "quote " + f.keys["b"], "quote " + f.keys["c"], "quote " + f.keys["d"], "eval .#confctl.build", "load a", "load b", "current b", "load c", "load d", "current d", "quote " + f.keys["a"], "quote " + f.keys["c"], "eval .#confctl.inputsInfo", "create a", "save a", "current a", "create c", "save c", "current c"}
	if !reflect.DeepEqual(f.trace, wantTrace) {
		t.Fatalf("trace\n%v\nwant\n%v", f.trace, wantTrace)
	}
	if len(f.evalCalls) != 2 || *f.evalCalls[1].apply != `x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "m_a_01234567" "m_c_01234567" ])` {
		t.Fatal(f.evalCalls)
	}
	absent(t, missingRoot)
	absent(t, oldB.Dir+"/auto_rollback")
	for _, row := range out.Generations {
		b := row.Generation
		checkLink(t, f.root+"/.confctl/generations/"+row.Host+"/current", b.Name)
		if row.Host == "b" || row.Host == "d" {
			continue
		}
		if b.Name != "2026-10-14--12-13-14" || b.Toplevel != "/nix/store/"+row.Host+"-top" {
			t.Fatal(b)
		}
		if b.Date.Fraction().RatString() != "123456789/1000000000" || b.Date.OffsetSeconds != 3600 {
			t.Fatal(b.Date)
		}
		loaded, err := generation.LoadBuild(f.root, row.Host, b.Name, generation.Options{})
		if err != nil || loaded.Date.Fraction().Sign() != 0 || value(loaded.Raw, "date") != "2026-10-14T12:13:14+01:00" || !reflect.DeepEqual(loaded.InputsInfo, b.InputsInfo) {
			t.Fatalf("readback %v %v", loaded, err)
		}
		checkLink(t, b.Dir+"/toplevel", b.Toplevel)
		checkLink(t, b.Dir+"/nixpkgs.input", "/nix/store/input")
		checkLink(t, f.roots+"/"+generation.RootName(row.Host, b.Name, "toplevel"), b.Dir+"/toplevel")
	}
	if out.Generations[0].Generation.Name != oldB.Name || out.Generations[1].Generation.Name != oldD.Name {
		t.Fatal("reused names changed")
	}
}

func TestRunAllReuseAvoidsCreateSaveMetadata(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.old("a", object("nixpkgs", "/nix/store/input"))
	f.old("b", object("nixpkgs", "/nix/store/input"))
	delete(f.evals, ".#confctl.inputsInfo")
	f.c.Create = func(string, string, *string, generation.Object, generation.Object, time.Time) (*generation.Build, error) {
		t.Fatal("create on reuse")
		return nil, nil
	}
	f.c.Save = func(*generation.Build) error { t.Fatal("save on reuse"); return nil }
	out, err := Run([]string{"a", "b"}, f.c)
	if err != nil || !out.Complete || len(out.Generations) != 2 || len(f.evalCalls) != 1 {
		t.Fatalf("%#v %v", out, err)
	}
}

func TestRunDuplicateKeysKeepInstallablesAndDeduplicateApply(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.row("a", object("key", false, "machineKey", "shared", "inputs", false))
	f.row("b", object("key", nil, "machineKey", false, "flakeKey", "shared", "inputs", nil))
	f.addEvalRow(".#confctl.build", "shared", object("toplevel", "/shared-top", "autoRollback", "/shared-rollback"))
	f.addEvalRow(".#confctl.inputsInfo", "shared", object())
	out, err := Run([]string{"a", "b"}, f.c)
	if err != nil || !out.Complete {
		t.Fatal(err)
	}
	want := []string{".#confctl.build.shared.toplevel", ".#confctl.build.shared.autoRollback", ".#confctl.build.shared.toplevel", ".#confctl.build.shared.autoRollback"}
	if !reflect.DeepEqual(f.requests[0].Installables, want) {
		t.Fatal(f.requests)
	}
	for _, call := range f.evalCalls {
		if *call.apply != `x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "shared" ])` {
			t.Fatal(*call.apply)
		}
	}
	for _, row := range out.Generations {
		if len(row.Generation.Inputs) != 0 {
			t.Fatal(row.Generation.Inputs)
		}
	}
}

func TestRunEmptyStringKeyAndOwnerLiteral(t *testing.T) {
	f := newFixture(t, "a")
	f.row("a", object("key", "", "machineKey", "unused", "inputs", object()))
	f.addEvalRow(".#confctl.build", "", object("toplevel", "/top", "autoRollback", "/rollback"))
	f.addEvalRow(".#confctl.inputsInfo", "", object())
	f.c.KeyLiteral = func(key string) (string, error) {
		if key != "" {
			t.Fatal(key)
		}
		return `"owner-literal"`, nil
	}
	out, err := Run([]string{"a"}, f.c)
	if err != nil || !out.Complete {
		t.Fatal(err)
	}
	if f.requests[0].Installables[0] != ".#confctl.build..toplevel" {
		t.Fatal(f.requests)
	}
	for _, call := range f.evalCalls {
		if *call.apply != `x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "owner-literal" ])` {
			t.Fatal(*call.apply)
		}
	}
}

func TestRunLegacyGroupingMappedKeysFallbackAndOneClock(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	f.settings = object("nix", object("legacyNixPath", true, "impureEval", true, "legacyNixPathMap", []any{"vpsadmin", "nixpkgs", "nixpkgs", "absent"}))
	first := object("vpsadmin", "/admin", "nixpkgs", "/nix")
	f.row("a", object("key", "alias-a", "inputs", first))
	f.row("b", object("key", "alias-b", "inputs", object("nixpkgs", "/different")))
	f.row("c", object("inputs", object("nixpkgs", "/nix", "vpsadmin", "/admin")))
	f.addEvalRow(".#confctl.inputs", f.keys["a"], false)
	f.addEvalRow(".#confctl.inputs", f.keys["c"], first)
	f.addEvalRow(".#confctl.inputs", f.keys["b"], object("nixpkgs", "/different"))
	f.evals[".#confctl.inputs."+f.keys["a"]] = object("vpsadmin", "/admin", "nixpkgs", "/nix", "empty", "", "nonstring", json.Number("1"))
	for _, host := range []string{"a", "b"} {
		f.addEvalRow(".#confctl.build", "alias-"+host, object("toplevel", "/"+host+"-top", "autoRollback", "/"+host+"-rollback"))
		f.addEvalRow(".#confctl.inputsInfo", "alias-"+host, false)
		f.evals[".#confctl.inputsInfo."+f.keys[host]] = object("fallback", host)
	}
	out, err := Run([]string{"a", "b", "c"}, f.c)
	if err != nil || !out.Complete {
		t.Fatalf("%v; trace %v", err, f.trace)
	}
	if !reflect.DeepEqual(names(out.Generations), []string{"a", "c", "b"}) || f.clockCalls != 1 {
		t.Fatal(out, f.clockCalls)
	}
	if !reflect.DeepEqual(f.requests[0].LegacyArgs, []string{"-I", "vpsadmin=/admin", "-I", "nixpkgs=/nix", "-I", "nixpkgs=/nix"}) || !reflect.DeepEqual(f.requests[1].LegacyArgs, []string{"-I", "nixpkgs=/different", "-I", "nixpkgs=/different"}) {
		t.Fatal(f.requests)
	}
	if f.evalCalls[0].installable != ".#confctl.inputs" || *f.evalCalls[0].apply != `x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "m_a_01234567" "m_c_01234567" ])` {
		t.Fatal(f.evalCalls)
	}
	for _, row := range out.Generations {
		if row.Generation.Name != "2026-10-14--12-13-14" {
			t.Fatal(row.Generation)
		}
	}
}

func TestRunLegacyRequirementsAndConflictStopBeforeBuild(t *testing.T) {
	for _, mode := range []string{"impure", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "a", "b")
			f.settings = object("nix", object("legacyNixPath", true, "impureEval", mode != "impure"))
			f.addEvalRow(".#confctl.inputs", f.keys["a"], object("nixpkgs", "/one"))
			f.addEvalRow(".#confctl.inputs", f.keys["b"], object("nixpkgs", "/two"))
			out, err := Run([]string{"a", "b"}, f.c)
			want := "legacyNixPath requires impureEval"
			if mode == "conflict" {
				want = "legacyNixPath requires consistent input nixpkgs across hosts; build hosts separately"
			}
			if err == nil || err.Error() != want || out.Complete || len(f.requests) != 0 || f.clockCalls != 1 {
				t.Fatalf("%v %#v", err, out)
			}
		})
	}
}

func TestRunLaterInvalidOutputPreservesEarlierReuse(t *testing.T) {
	for i, bad := range []any{nil, false, object("toplevel", nil, "autoRollback", "/rollback"), object("toplevel", "/top")} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newFixture(t, "a", "b", "c")
			old := f.old("a", object("nixpkgs", "/nix/store/input"))
			f.addEvalRow(".#confctl.build", f.keys["b"], bad)
			out, err := Run([]string{"a", "b", "c"}, f.c)
			if err == nil || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"a"}) {
				t.Fatalf("%v %#v", err, out)
			}
			checkLink(t, f.root+"/.confctl/generations/a/current", old.Name)
			absent(t, f.root+"/.confctl/generations/c")
			if len(f.evalCalls) != 1 {
				t.Fatal("metadata fetched after output failure")
			}
		})
	}
}

func TestRunSaveRootFailureRetainsRealPrefix(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	old := f.old("a", object("nixpkgs", "/nix/store/input"))
	name := f.at.Format("2006-01-02--15-04-05")
	collision := f.roots + "/" + generation.RootName("b", name, "auto_rollback")
	if err := os.Symlink("/pre-existing", collision); err != nil {
		t.Fatal(err)
	}
	out, err := Run([]string{"a", "b", "c"}, f.c)
	if err == nil || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"a"}) {
		t.Fatalf("%v %#v", err, out)
	}
	checkLink(t, f.root+"/.confctl/generations/a/current", old.Name)
	dir := f.root + "/.confctl/generations/b/" + name
	if _, err := generation.LoadBuild(f.root, "b", name, generation.Options{}); err != nil {
		t.Fatal("record not retained", err)
	}
	checkLink(t, dir+"/toplevel", "/nix/store/b-top")
	checkLink(t, f.roots+"/"+generation.RootName("b", name, "toplevel"), dir+"/toplevel")
	checkLink(t, collision, "/pre-existing")
	absent(t, f.roots+"/"+generation.RootName("b", name, "input.nixpkgs"))
	absent(t, f.root+"/.confctl/generations/b/current")
	absent(t, f.root+"/.confctl/generations/c")
}

func TestRunCurrentFailureKeepsRecordAndInMemoryMutation(t *testing.T) {
	f := newFixture(t, "a", "b")
	if err := os.MkdirAll(f.root+"/.confctl/generations/a/current", 0700); err != nil {
		t.Fatal(err)
	}
	var list *generation.BuildList
	load := f.c.Load
	f.c.Load = func(host string) (*generation.BuildList, error) {
		loaded, err := load(host)
		if host == "a" {
			list = loaded
		}
		return loaded, err
	}
	out, err := Run([]string{"a", "b"}, f.c)
	if err == nil || out.Complete || len(out.Generations) != 0 || list.Current == nil || !list.Current.Current || len(list.Builds) != 1 {
		t.Fatalf("%v %#v %#v", err, out, list)
	}
	if _, err := generation.LoadBuild(f.root, "a", list.Current.Name, generation.Options{}); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(f.root + "/.confctl/generations/a/current.new-*")
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	checkLink(t, paths[0], list.Current.Name)
	absent(t, f.root+"/.confctl/generations/b")
}

func TestRunPendingOnlyMetadataFallbacksAndLaterCreateFailure(t *testing.T) {
	f := newFixture(t, "a", "b", "c", "d")
	f.old("b", object("nixpkgs", "/nix/store/input"))
	f.addEvalRow(".#confctl.inputsInfo", f.keys["a"], generation.Object{})
	f.addEvalRow(".#confctl.inputsInfo", f.keys["c"], nil)
	f.evals[".#confctl.inputsInfo."+f.keys["c"]] = errors.New("finite fallback failure")
	out, err := Run([]string{"a", "b", "c", "d"}, f.c)
	if err == nil || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"b", "a"}) {
		t.Fatalf("%v %#v", err, out)
	}
	if len(f.evalCalls) != 3 || f.evalCalls[2].installable != ".#confctl.inputsInfo."+f.keys["c"] || f.evalCalls[2].apply != nil {
		t.Fatal(f.evalCalls)
	}
	if *f.evalCalls[1].apply != `x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ "m_a_01234567" "m_c_01234567" "m_d_01234567" ])` {
		t.Fatal(*f.evalCalls[1].apply)
	}
	checkLink(t, f.root+"/.confctl/generations/a/current", "2026-10-14--12-13-14")
	absent(t, f.root+"/.confctl/generations/c")
	absent(t, f.root+"/.confctl/generations/d")
}

func TestRunLaterGroupFailurePreservesEarlierGroup(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	f.settings = object("nix", object("legacyNixPath", true, "impureEval", true))
	f.row("b", object("key", f.keys["b"], "inputs", object("nixpkgs", "/other")))
	f.row("c", object("key", f.keys["c"], "inputs", object("nixpkgs", "/third")))
	for _, host := range []string{"a", "b", "c"} {
		f.addEvalRow(".#confctl.inputs", f.keys[host], object("nixpkgs", "/"+host))
	}
	build := f.c.Build
	failure := errors.New("second group build failed")
	calls := 0
	f.c.Build = func(request BuildRequest, feed func([]byte, []byte)) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, failure
		}
		return build(request, feed)
	}
	out, err := Run([]string{"a", "b", "c"}, f.c)
	if !errors.Is(err, failure) || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"a"}) || f.clockCalls != 1 || calls != 2 {
		t.Fatalf("%v %#v", err, out)
	}
	checkLink(t, f.root+"/.confctl/generations/a/current", out.Generations[0].Generation.Name)
	absent(t, f.root+"/.confctl/generations/b")
	absent(t, f.root+"/.confctl/generations/c")
}

func TestRunSaveSameSecondCollisionDoesNotAllocateOrRepair(t *testing.T) {
	f := newFixture(t, "a", "b")
	rollback := "/old-rollback"
	old, err := generation.CreateFlake(f.root, "a", "/different-top", &rollback, object("nixpkgs", "/different-input"), generation.Object{}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Save(old); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(old.Dir + "/generation.json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Run([]string{"a", "b"}, f.c)
	if err == nil || out.Complete || len(out.Generations) != 0 {
		t.Fatalf("%v %#v", err, out)
	}
	checkLink(t, old.Dir+"/toplevel", "/different-top")
	after, err := os.ReadFile(old.Dir + "/generation.json")
	if err != nil || string(after) != string(before) {
		t.Fatal("existing JSON changed", err)
	}
	absent(t, f.root+"/.confctl/generations/a/current")
	absent(t, f.root+"/.confctl/generations/b")
	entries, err := os.ReadDir(f.root + "/.confctl/generations/a")
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
}

func TestRunChildDecodeAndProgressErrorBoundaries(t *testing.T) {
	for _, mode := range []string{"child-error", "decode-error", "success"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "a")
			failure := errors.New("selected failure")
			events := []string{}
			f.c.Progress = func(e Event) { events = append(events, eventText(e)) }
			f.c.Build = func(request BuildRequest, feed func([]byte, []byte)) ([]byte, error) {
				feed([]byte("building '/stdout'\n"), []byte("building '/line'\nbuilding '/tail'"))
				if mode == "child-error" {
					return []byte(`[]`), failure
				}
				return []byte(`[]`), nil
			}
			decode := f.c.DecodeBuild
			f.c.DecodeBuild = func(bytes []byte) (any, error) {
				if mode == "child-error" {
					t.Fatal("decode after child error")
				}
				if mode == "decode-error" {
					return nil, failure
				}
				return decode(bytes)
			}
			out, err := Run([]string{"a"}, f.c)
			if mode == "success" {
				if err != nil || !out.Complete {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, failure) || out.Complete || len(f.evalCalls) != 0 {
					t.Fatal(err, out)
				}
			}
			want := []string{"build 1/0 /line"}
			if mode != "child-error" {
				want = append(want, "build 2/0 /tail")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatal(events, want)
			}
		})
	}
}

func TestRunCallbackFailuresStopSchedulingAndRetainCause(t *testing.T) {
	for _, stage := range []string{"plan", "settings", "key", "quote", "output", "load", "info", "create", "save", "current"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, "a", "b")
			failure := errors.New("callback failure")
			switch stage {
			case "plan":
				f.c.BuildPlan = func() (generation.Object, error) { return nil, failure }
			case "settings":
				f.c.Settings = func() (generation.Object, error) { return nil, failure }
			case "key":
				f.row("a", object())
				f.c.MachineKey = func(string) (string, error) { return "", failure }
			case "quote":
				f.c.KeyLiteral = func(string) (string, error) { return "", failure }
			case "output":
				f.evals[".#confctl.build"] = failure
			case "load":
				f.c.Load = func(string) (*generation.BuildList, error) { return nil, failure }
			case "info":
				f.evals[".#confctl.inputsInfo"] = failure
			case "create":
				f.c.Create = func(string, string, *string, generation.Object, generation.Object, time.Time) (*generation.Build, error) {
					return nil, failure
				}
			case "save":
				f.c.Save = func(*generation.Build) error { return failure }
			case "current":
				f.c.SetCurrent = func(*generation.BuildList, *generation.Build) error { return failure }
			}
			out, err := Run([]string{"a", "b"}, f.c)
			if !errors.Is(err, failure) || out.Complete || len(out.Generations) != 0 {
				t.Fatalf("%v %#v", err, out)
			}
			absent(t, f.root+"/.confctl/generations/b")
		})
	}
}

func TestRunMissingPlanPrecedesSettingsAndClock(t *testing.T) {
	f := newFixture(t, "a")
	f.c.Settings = func() (generation.Object, error) { t.Fatal("settings before fetch failure"); return nil, nil }
	f.c.Now = func() time.Time { t.Fatal("clock before fetch failure"); return time.Time{} }
	out, err := Run([]string{"missing"}, f.c)
	if err == nil || out.Complete || !reflect.DeepEqual(f.trace, []string{"plan"}) {
		t.Fatal(err, out, f.trace)
	}
}

func TestRunLiteralTargetsSafeArgvAndPhysicalConfiguration(t *testing.T) {
	host := "carrier/host #1"
	f := newFixture(t, host)
	physical := f.root
	alias := t.TempDir() + "/configuration alias"
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	f.root = alias
	var err error
	f.writer, err = generation.NewWriter(alias, func() (string, error) { return f.roots, nil })
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir() + "/private store path"
	if err := os.Mkdir(store, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/abc-linux-6.12.12/kernel", store+"/kernel"); err != nil {
		t.Fatal(err)
	}
	literal := t.TempDir() + "/store alias ; $(unexecuted)"
	if err := os.Symlink(store, literal); err != nil {
		t.Fatal(err)
	}
	key := `key with space;$(unexecuted)"`
	f.row(host, object("key", key, "inputs", object("role with space", "/input path ; $(unexecuted)")))
	f.addEvalRow(".#confctl.build", key, object("toplevel", literal, "autoRollback", "/rollback path"))
	f.addEvalRow(".#confctl.inputsInfo", key, object("literal", json.Number("9007199254740993")))
	out, err := Run([]string{host}, f.c)
	if err != nil || !out.Complete {
		t.Fatal(err)
	}
	b := out.Generations[0].Generation
	if b.Toplevel != literal || b.KernelVersion == nil || *b.KernelVersion != "6.12.12" || !strings.HasPrefix(b.Dir, physical+"/") {
		t.Fatal(b)
	}
	if !reflect.DeepEqual(f.requests[0].Installables, []string{".#confctl.build." + key + ".toplevel", ".#confctl.build." + key + ".autoRollback"}) {
		t.Fatal("argv changed", f.requests)
	}
	checkLink(t, b.Dir+"/toplevel", literal)
	checkLink(t, b.Dir+"/role with space.input", "/input path ; $(unexecuted)")
	checkLink(t, physical+"/.confctl/generations/carrier:host #1/current", b.Name)
	checkLink(t, f.roots+"/"+generation.RootName(host, b.Name, "toplevel"), b.Dir+"/toplevel")
}

func TestRunEmptyInputRetainsSourceOrdinaryVersusLegacyGroups(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			f := newFixture(t)
			f.settings = object("nix", object("legacyNixPath", legacy))
			out, err := Run(nil, f.c)
			if err != nil || !out.Complete || len(out.Generations) != 0 || f.clockCalls != 1 || len(f.evalCalls) != 0 {
				t.Fatal(err, out)
			}
			wantGroups := 1
			if legacy {
				wantGroups = 0
			}
			if len(out.Groups) != wantGroups || len(f.requests) != wantGroups {
				t.Fatal(out, f.requests)
			}
			if !legacy && (len(f.requests[0].Installables) != 0 || len(f.requests[0].LegacyArgs) != 0) {
				t.Fatal(f.requests)
			}
		})
	}
}
