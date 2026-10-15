package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

func mutationInvocation(t *testing.T, command string, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"generation", command}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func mutationMachine(name string) Machine {
	target := name + ".invalid"
	return Machine{Name: name, Profile: "/nix/var/nix/profiles/system", Target: Target{Host: &target, Port: 22}, Attributes: map[string]any{}}
}

func mutationSeed(t *testing.T, root, host, name string) (string, string) {
	t.Helper()
	top := filepath.Join(root, "store-"+host+"-"+name)
	if err := os.Mkdir(top, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(top+"/sentinel", []byte(name), 0600); err != nil {
		t.Fatal(err)
	}
	date := "2020-01-01T00:00:00Z"
	if name == "new" {
		date = "2021-01-01T00:00:00Z"
	}
	dir := seedCoreGeneration(t, root, host, name, coreGenerationRecord(top, date, `{}`))
	if err := os.Symlink(top, dir+"/toplevel"); err != nil {
		t.Fatal(err)
	}
	return dir, top
}

func mutationFixture(t *testing.T) (*Engine, generationMutationDeps, string, string) {
	t.Helper()
	root, roots := t.TempDir(), t.TempDir()
	old, _ := mutationSeed(t, root, "node", "old")
	newDir, _ := mutationSeed(t, root, "node", "new")
	if err := os.Symlink("new", filepath.Dir(newDir)+"/current"); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Context: context.Background(), Inventory: []Machine{mutationMachine("node")}}
	d := generationMutationDeps{
		Roots: func() (string, error) { return roots, nil },
		Load: func(host string) (*generation.BuildList, error) {
			return generation.LoadBuildList(root, host, generation.Options{})
		},
		Statuses: func([]Machine, []Machine) ([]Status, error) { return nil, nil },
		Delete:   func(Machine, *generation.Host) error { t.Fatal("unexpected remote deletion"); return nil },
		GC:       func([]gcTask, *big.Int) error { t.Fatal("unexpected GC"); return nil },
		Settings: func() (map[string]any, error) {
			policy := func() map[string]any {
				return map[string]any{"min": json.Number("0"), "max": json.Number("0"), "maxAge": json.Number("0"), "collectGarbage": false}
			}
			return map[string]any{"buildGenerations": policy(), "hostGenerations": policy()}, nil
		},
		Now: func() generation.Timestamp {
			t.Fatal("count-driven deletion should not sample time")
			return generation.Timestamp{}
		},
	}
	return e, d, old, roots
}

func TestGenerationMutationDeclineAndEmptyHaveNoEffects(t *testing.T) {
	for _, command := range []string{"rm", "rotate"} {
		t.Run(command, func(t *testing.T) {
			e, d, _, _ := mutationFixture(t)
			d.Roots = func() (string, error) { t.Fatal("roots reached before confirmation"); return "", nil }
			before := testConfigurationState(t, e.Root)
			var out bytes.Buffer
			inv := mutationInvocation(t, command, "node")
			var err error
			if command == "rm" {
				err = e.removeGenerations(inv, strings.NewReader("n\n"), &out, d)
			} else {
				err = e.rotateGenerations(inv, strings.NewReader("n\n"), &out, d)
			}
			if err == nil || err.Error() != "Aborted" || !reflect.DeepEqual(before, testConfigurationState(t, e.Root)) {
				t.Fatal(out.String(), err)
			}
		})
	}
	e, d, _, _ := mutationFixture(t)
	d.Load = func(string) (*generation.BuildList, error) { t.Fatal("unselected reader"); return nil, nil }
	var out bytes.Buffer
	if err := e.removeGenerations(mutationInvocation(t, "rm", "--yes", "missing"), nil, &out, d); err != nil || out.String() != "No generations found\n" {
		t.Fatal(out.String(), err)
	}
}

func TestGenerationMutationUnifiedLocalBeforeFailedRemote(t *testing.T) {
	e, d, old, roots := mutationFixture(t)
	builds, err := d.Load("node")
	if err != nil {
		t.Fatal(err)
	}
	g := builds.Builds[0]
	rootLink := roots + "/" + generation.RootName(g.Host, g.Name, "toplevel")
	if err := os.Symlink(g.Toplevel, rootLink); err != nil {
		t.Fatal(err)
	}
	id, _ := new(big.Int).SetString("999999999999999999999999999999", 10)
	d.Statuses = func([]Machine, []Machine) ([]Status, error) {
		return []Status{{Machine: e.Inventory[0], Generations: &generation.HostList{Generations: []*generation.Host{{Host: "node", ID: id, Profile: "literal profile", Toplevel: &g.Toplevel, Date: g.Date}}}}}, nil
	}
	boom := errors.New("remote failure after local removal")
	d.Delete = func(_ Machine, host *generation.Host) error {
		if host.ID.String() != id.String() {
			t.Fatal(host.ID)
		}
		for _, p := range []string{old, rootLink} {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("remote ran before local deletion", p, err)
			}
		}
		return boom
	}
	var out bytes.Buffer
	err = e.removeGenerations(mutationInvocation(t, "rm", "-lr", "--yes", "node", "old"), nil, &out, d)
	if !errors.Is(err, boom) {
		t.Fatal(out.String(), err)
	}
	current := filepath.Dir(old) + "/current"
	if text, err := os.Readlink(current); err != nil || text != "new" {
		t.Fatal(text, err)
	}
	if b, err := os.ReadFile(g.Toplevel + "/sentinel"); err != nil || string(b) != "old" {
		t.Fatal("store target modified", err)
	}
}

func TestGenerationMutationLocalFailurePreventsRemoteAndLaterRecords(t *testing.T) {
	e, d, old, roots := mutationFixture(t)
	if err := os.Mkdir(old+"/unexpected", 0700); err != nil {
		t.Fatal(err)
	}
	builds, _ := d.Load("node")
	g := builds.Builds[0]
	d.Statuses = func([]Machine, []Machine) ([]Status, error) {
		return []Status{{Machine: e.Inventory[0], Generations: &generation.HostList{Generations: []*generation.Host{{Host: "node", ID: big.NewInt(1), Toplevel: &g.Toplevel, Date: g.Date}}}}}, nil
	}
	var out bytes.Buffer
	err := e.removeGenerations(mutationInvocation(t, "rm", "-lr", "--yes", "node"), nil, &out, d)
	if err == nil {
		t.Fatal("expected nonempty rmdir failure")
	}
	if _, err := os.Stat(filepath.Dir(old) + "/new/generation.json"); err != nil {
		t.Fatal("later record touched", err)
	}
	if _, err := os.Lstat(old + "/generation.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed prefix missing", err)
	}
	if members, err := os.ReadDir(roots); err != nil || len(members) != 0 {
		t.Fatal(members, err)
	}
}

func TestGenerationMutationCurrentRemovalDoesNotRepairLink(t *testing.T) {
	e, d, old, _ := mutationFixture(t)
	var out bytes.Buffer
	if err := e.removeGenerations(mutationInvocation(t, "rm", "--yes", "node", "current"), nil, &out, d); err != nil {
		t.Fatal(err)
	}
	if text, err := os.Readlink(filepath.Dir(old) + "/current"); err != nil || text != "new" {
		t.Fatal(text, err)
	}
	list, err := d.Load("node")
	if err != nil || list.RequireResolvedCurrent() == nil {
		t.Fatal("fresh reader must report dangling current", err)
	}
	if _, err := os.Stat(old + "/generation.json"); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationMutationEmptyPostGCDiffersBetweenRemoveAndRotate(t *testing.T) {
	for _, command := range []string{"rm", "rotate"} {
		t.Run(command, func(t *testing.T) {
			e, d, old, _ := mutationFixture(t)
			e.Inventory[0].Target.Host = nil
			calls := 0
			d.GC = func(tasks []gcTask, workers *big.Int) error {
				calls++
				if len(tasks) != 0 || workers.String() != "-1" {
					t.Fatal(tasks, workers)
				}
				return nil
			}
			var out bytes.Buffer
			inv := mutationInvocation(t, command, "-lr", "--yes", "--max-concurrent-gc=-1", "node")
			var err error
			if command == "rm" {
				inv.Args = append(inv.Args, "old")
				err = e.removeGenerations(inv, nil, &out, d)
			} else {
				err = e.rotateGenerations(inv, nil, &out, d)
			}
			want := 0
			if command == "rm" {
				want = 1
			}
			if err != nil || calls != want {
				t.Fatal(out.String(), calls, err)
			}
			if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("prior removal missing", err)
			}
		})
	}
}

func TestGenerationMutationRotationPlansBeforeEffectsRemoteBeforeLocal(t *testing.T) {
	for _, badLocal := range []bool{false, true} {
		t.Run(fmt.Sprint(badLocal), func(t *testing.T) {
			e, d, old, _ := mutationFixture(t)
			builds, _ := d.Load("node")
			g := builds.Builds[0]
			d.Statuses = func([]Machine, []Machine) ([]Status, error) {
				return []Status{{Machine: e.Inventory[0], Generations: &generation.HostList{Generations: []*generation.Host{{Host: "node", ID: big.NewInt(3), Toplevel: &g.Toplevel, Date: g.Date}}}}}, nil
			}
			deleted := 0
			d.Delete = func(Machine, *generation.Host) error {
				deleted++
				if _, err := os.Stat(old + "/generation.json"); err != nil {
					t.Fatal("local removed before remote rotation", err)
				}
				return nil
			}
			boom := errors.New("unresolved local current")
			if badLocal {
				d.Load = func(string) (*generation.BuildList, error) {
					return &generation.BuildList{Host: "node", CurrentError: boom}, nil
				}
			}
			var out bytes.Buffer
			err := e.rotateGenerations(mutationInvocation(t, "rotate", "-lr", "--yes", "--no-gc", "node"), nil, &out, d)
			if badLocal {
				if err == nil || !strings.Contains(err.Error(), boom.Error()) || deleted != 0 {
					t.Fatal(deleted, err)
				}
			} else if err != nil || deleted != 1 {
				t.Fatal(out.String(), deleted, err)
			}
		})
	}
}

func TestGenerationMutationPolicyAndCarrierPostGC(t *testing.T) {
	carrier := mutationMachine("carrier")
	carrier.Attributes["buildGenerations"] = map[string]any{"collectGarbage": false}
	a, b := mutationMachine("a"), mutationMachine("b")
	a.CarrierName, b.CarrierName = &carrier.Name, &carrier.Name
	global := map[string]any{"buildGenerations": map[string]any{"min": json.Number("5"), "max": json.Number("30"), "maxAge": json.Number("999999999999999999999999")}, "hostGenerations": map[string]any{"collectGarbage": true}}
	a.Attributes["buildGenerations"] = map[string]any{"min": json.Number("0"), "max": false}
	policy, err := generationRotationPolicy(a, global, "buildGenerations")
	if err != nil || policy.Min.String() != "0" || policy.Max.String() != "30" || policy.MaxAgeSeconds.String() != "999999999999999999999999" {
		t.Fatal(policy, err)
	}
	tasks, err := generationGCTargets([]Machine{b, a}, []Machine{a, carrier, b}, map[string]bool{"a": true, "b": true}, true, global)
	if err != nil || len(tasks) != 1 || tasks[0].Host != "carrier" {
		t.Fatal(tasks, err)
	}
	global["hostGenerations"].(map[string]any)["collectGarbage"] = false
	tasks, err = generationGCTargets([]Machine{a}, []Machine{a, carrier}, map[string]bool{"a": true}, true, global)
	if err != nil || len(tasks) != 0 {
		t.Fatal(tasks, err)
	}
	carrier.Attributes["buildGenerations"].(map[string]any)["collectGarbage"] = true
	tasks, err = generationGCTargets([]Machine{a}, []Machine{a, carrier}, map[string]bool{"a": true}, true, global)
	if err != nil || len(tasks) != 1 {
		t.Fatal(tasks, err)
	}
}

func TestGenerationMutationRemoteLiteralCommandAndFailure(t *testing.T) {
	root := t.TempDir()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nix-env", "carrier-env"} {
		if err := os.WriteFile(root+"/"+name, []byte("#!"+bash+"\nprintf '%s\\0' \"${0##*/}\" \"$@\" > \"$TRACE\"\nexit 23\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root)
	t.Setenv("TRACE", root+"/trace")
	carrier := mutationMachine("carrier")
	carrier.Target.Localhost = true
	e := &Engine{Context: context.Background(), Root: root, Inventory: []Machine{carrier}}
	id, _ := new(big.Int).SetString("999999999999999999999999999999", 10)
	h := &generation.Host{ID: id, Profile: "profile with ; literal"}
	for _, carried := range []bool{false, true} {
		m := carrier
		if carried {
			m.CarrierName = &carrier.Name
		}
		err := e.removeHostGeneration(m, h)
		if err == nil || !strings.Contains(err.Error(), "exit status 23") {
			t.Fatal(err)
		}
		b, err := os.ReadFile(root + "/trace")
		command := "nix-env"
		if carried {
			command = "carrier-env"
		}
		if err != nil || string(b) != command+"\x00-p\x00"+h.Profile+"\x00--delete-generations\x00"+id.String()+"\x00" {
			t.Fatal(string(b), err)
		}
	}
}
