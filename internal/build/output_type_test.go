package build

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/generation"
)

// These changed-expectation fixtures exercise the accepted new-record type
// admission exception, not historical Ruby observations. Nil remains the
// original early gate; reuse ignores non-nil rollback, including these types.
var unsupportedTargets = []struct {
	name  string
	value any
	kind  string
}{
	{"false", false, "boolean"}, {"true", true, "boolean"},
	{"number", json.Number("9007199254740993"), "number"},
	{"array", []any{"unexposed raw text"}, "array"},
	{"object", object("private", "unexposed raw text"), "object"},
}

func TestOutputTypeReusableNonNilTargetsRemainUnused(t *testing.T) {
	for _, target := range unsupportedTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newFixture(t, "a")
			old := f.old("a", object("nixpkgs", "/nix/store/input"))
			f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", old.Toplevel, "autoRollback", target.value))
			f.c.Create = func(string, string, *string, generation.Object, generation.Object, time.Time) (*generation.Build, error) {
				t.Fatal("unused constructor")
				return nil, nil
			}
			f.c.Save = func(*generation.Build) error { t.Fatal("unused Save"); return nil }
			out, err := Run([]string{"a"}, f.c)
			if err != nil || !out.Complete || len(out.Generations) != 1 || len(f.evalCalls) != 1 {
				t.Fatal(err, out, f.evalCalls)
			}
			checkLink(t, old.Dir+"/auto_rollback", "/old-rollback")
			checkLink(t, f.root+"/.confctl/generations/a/current", old.Name)
		})
	}
}

func TestOutputTypeChangedExpectationNewTargetAfterConstructorBeforeSave(t *testing.T) {
	for _, target := range unsupportedTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newFixture(t, "a")
			f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", "/nix/store/a-top", "autoRollback", target.value))
			create := f.c.Create
			calls := 0
			f.c.Create = func(host, top string, rollback *string, inputs, info generation.Object, at time.Time) (*generation.Build, error) {
				calls++
				if rollback != nil {
					t.Fatal("unsupported value coerced")
				}
				return create(host, top, rollback, inputs, info, at)
			}
			f.c.Save = func(*generation.Build) error { t.Fatal("malformed target reached Save"); return nil }
			f.c.SetCurrent = func(*generation.BuildList, *generation.Build) error {
				t.Fatal("temporary object exposed to list")
				return nil
			}
			out, err := Run([]string{"a"}, f.c)
			want := "a (m_a_01234567): autoRollback build output must be a string (got " + target.kind + ")"
			if err == nil || err.Error() != want || out.Complete || len(out.Generations) != 0 || calls != 1 {
				t.Fatalf("%v %#v calls%d", err, out, calls)
			}
			wantTrace := []string{"plan", "settings", "clock", "build", "decode", "quote " + f.keys["a"], "eval .#confctl.build", "load a", "quote " + f.keys["a"], "eval .#confctl.inputsInfo", "create a"}
			if !reflect.DeepEqual(f.trace, wantTrace) {
				t.Fatal(f.trace, wantTrace)
			}
			absent(t, f.root+"/.confctl")
			entries, err := os.ReadDir(f.roots)
			if err != nil || len(entries) != 0 {
				t.Fatal(entries, err)
			}
		})
	}
}

func TestOutputNilGatePrecedesEvenReusableList(t *testing.T) {
	for i, output := range []any{nil, object("toplevel", nil, "autoRollback", "/rollback"), object("toplevel", "/nix/store/a-top", "autoRollback", nil)} {
		f := newFixture(t, "a")
		old := f.old("a", object("nixpkgs", "/nix/store/input"))
		f.addEvalRow(".#confctl.build", f.keys["a"], output)
		f.c.Load = func(string) (*generation.BuildList, error) {
			t.Fatalf("case%d loaded before nil gate", i)
			return nil, nil
		}
		out, err := Run([]string{"a"}, f.c)
		if err == nil || out.Complete || len(out.Generations) != 0 || len(f.evalCalls) != 1 {
			t.Fatal(err, out)
		}
		absent(t, f.root+"/.confctl/generations/a/current")
		checkLink(t, old.Dir+"/auto_rollback", "/old-rollback")
	}
}

func TestOutputTypeMetadataErrorsWin(t *testing.T) {
	for _, mode := range []string{"batch", "nil-fallback", "false-fallback"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, "a")
			f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", "/top", "autoRollback", false))
			failure := errors.New("metadata provider failure")
			if mode == "batch" {
				f.evals[".#confctl.inputsInfo"] = failure
			} else {
				var entry any
				if mode == "false-fallback" {
					entry = false
				}
				f.addEvalRow(".#confctl.inputsInfo", f.keys["a"], entry)
				f.evals[".#confctl.inputsInfo."+f.keys["a"]] = failure
			}
			f.c.Create = func(string, string, *string, generation.Object, generation.Object, time.Time) (*generation.Build, error) {
				t.Fatal("constructor before failed metadata")
				return nil, nil
			}
			out, err := Run([]string{"a"}, f.c)
			if !errors.Is(err, failure) || out.Complete || len(out.Generations) != 0 || strings.Contains(err.Error(), "must be a string") {
				t.Fatal(err, out)
			}
			absent(t, f.root+"/.confctl")
		})
	}
}

func TestOutputTypeRealConstructorErrorWins(t *testing.T) {
	f := newFixture(t, "a")
	file := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(file, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", file, "autoRollback", object()))
	f.c.Save = func(*generation.Build) error { t.Fatal("Save after constructor failure"); return nil }
	out, err := Run([]string{"a"}, f.c)
	var pathError *os.PathError
	if !errors.As(err, &pathError) || pathError.Op != "readlink" || strings.Contains(err.Error(), "must be a string") || out.Complete || len(out.Generations) != 0 {
		t.Fatal(err, out)
	}
	if f.trace[len(f.trace)-1] != "create a" || len(f.evalCalls) != 2 {
		t.Fatal(f.trace)
	}
	absent(t, f.root+"/.confctl")
}

func TestOutputTypeLaterReusePrecedesEarlierPendingAdmission(t *testing.T) {
	f := newFixture(t, "a", "b")
	old := f.old("b", object("nixpkgs", "/nix/store/input"))
	f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", "/a-top", "autoRollback", false))
	out, err := Run([]string{"a", "b"}, f.c)
	if err == nil || !strings.Contains(err.Error(), "got boolean") || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"b"}) {
		t.Fatal(err, out)
	}
	checkLink(t, f.root+"/.confctl/generations/b/current", old.Name)
	absent(t, f.root+"/.confctl/generations/a")
	wantSuffix := []string{"load a", "load b", "current b", "quote " + f.keys["a"], "eval .#confctl.inputsInfo", "create a"}
	if !reflect.DeepEqual(f.trace[len(f.trace)-len(wantSuffix):], wantSuffix) {
		t.Fatal(f.trace)
	}
}

func TestOutputTypeValidPrefixMalformedMiddleStopsLaterPending(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	f.addEvalRow(".#confctl.build", f.keys["b"], object("toplevel", "/b-top", "autoRollback", []any{}))
	out, err := Run([]string{"a", "b", "c"}, f.c)
	if err == nil || !strings.Contains(err.Error(), "got array") || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"a"}) {
		t.Fatal(err, out)
	}
	checkLink(t, f.root+"/.confctl/generations/a/current", out.Generations[0].Generation.Name)
	absent(t, f.root+"/.confctl/generations/b")
	absent(t, f.root+"/.confctl/generations/c")
	if f.trace[len(f.trace)-1] != "create b" || len(f.evalCalls) != 2 {
		t.Fatal(f.trace)
	}
}

func TestOutputTypeChangedExpectationAdmissionOutranksUnattemptedCollision(t *testing.T) {
	f := newFixture(t, "a")
	rollback := "/prior-rollback"
	old, err := generation.CreateFlake(f.root, "a", "/prior-top", &rollback, object(), object(), f.at)
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
	f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", "/new-top", "autoRollback", false))
	f.c.Save = func(*generation.Build) error { t.Fatal("collision attempted before admission"); return nil }
	out, err := Run([]string{"a"}, f.c)
	if err == nil || !strings.Contains(err.Error(), "got boolean") || out.Complete || len(out.Generations) != 0 {
		t.Fatal(err, out)
	}
	after, err := os.ReadFile(old.Dir + "/generation.json")
	if err != nil || string(after) != string(before) {
		t.Fatal("preexisting record changed", err)
	}
	checkLink(t, old.Dir+"/toplevel", "/prior-top")
	checkLink(t, old.Dir+"/auto_rollback", "/prior-rollback")
	absent(t, f.root+"/.confctl/generations/a/current")
}

func TestOutputTypeAllStringsReachUnchangedWriter(t *testing.T) {
	for _, target := range []string{"", "../rollback", "/missing-target", "nul\x00target", "invalid\xffutf8"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t, "a")
			f.addEvalRow(".#confctl.build", f.keys["a"], object("toplevel", "/nix/store/a-top", "autoRollback", target))
			create, save := f.c.Create, f.c.Save
			created, saved := 0, 0
			f.c.Create = func(host, top string, rollback *string, inputs, info generation.Object, at time.Time) (*generation.Build, error) {
				created++
				if rollback == nil || *rollback != target {
					t.Fatal("string changed")
				}
				return create(host, top, rollback, inputs, info, at)
			}
			f.c.Save = func(b *generation.Build) error { saved++; return save(b) }
			out, err := Run([]string{"a"}, f.c)
			if created != 1 || saved != 1 || (err != nil && strings.Contains(err.Error(), "must be a string")) {
				t.Fatal(err, created, saved)
			}
			dir := f.root + "/.confctl/generations/a/2026-10-14--12-13-14"
			checkLink(t, dir+"/toplevel", "/nix/store/a-top")
			if target == "../rollback" || target == "/missing-target" {
				if err != nil || !out.Complete {
					t.Fatal(err, out)
				}
				checkLink(t, dir+"/auto_rollback", target)
			} else {
				if err == nil || out.Complete || len(out.Generations) != 0 {
					t.Fatal(err, out)
				}
				absent(t, f.root+"/.confctl/generations/a/current")
			}
		})
	}
}
