package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/build"
	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

type buildFixture struct {
	e              *Engine
	roots, top     string
	clock          time.Time
	rootCalls      int
	calls          []string
	plan, metadata string
	outputs        string
}

func newBuildFixture(t *testing.T) *buildFixture {
	t.Helper()
	root := t.TempDir()
	f := &buildFixture{e: &Engine{Root: root, Context: context.Background(), SettingsCache: map[string]any{"nix": map[string]any{"maxJobs": "global"}, "list": map[string]any{"columns": []any{"name"}}}}, roots: t.TempDir(), top: filepath.Join(root, "top"), clock: time.Date(2026, 10, 15, 10, 20, 30, 123, time.UTC)}
	if err := os.Mkdir(f.top, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/synthetic-linux-6.12/bzImage", filepath.Join(f.top, "kernel")); err != nil {
		t.Fatal(err)
	}
	f.plan = `{"one":{"key":"k1","inputs":{"nixpkgs":"/synthetic/input"}},"two":{"key":"k2","inputs":{"nixpkgs":"/synthetic/input"}}}`
	f.outputs = fmt.Sprintf(`{"k1":{"toplevel":%q,"autoRollback":"/synthetic/rollback"},"k2":{"toplevel":%q,"autoRollback":"/synthetic/rollback"}}`, f.top, f.top)
	f.metadata = `{"k1":{"z":{"n":9007199254740993,"f":1.2300e+4},"a":false,"z":{"n":9007199254740995,"f":-0}},"k2":{"second":"value"}}`
	return f
}

func (f *buildFixture) run(argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
	if input != nil {
		return Result{}, fmt.Errorf("build input changed from nil")
	}
	installable := argv[len(argv)-1]
	if argv[1] == "build" {
		f.calls = append(f.calls, "build")
		if chunk != nil {
			if err := chunk(nil, []byte("building '/synthetic/build.drv'\n")); err != nil {
				return Result{}, err
			}
		}
		return Result{ExitCode: 0, Stdout: []byte("null")}, nil
	}
	f.calls = append(f.calls, installable)
	var output string
	switch installable {
	case ".#confctl.settings":
		output = `{"_module":{"ignored":true},"nix":{"impureEval":false,"legacyNixPath":false}}`
	case ".#confctl.buildPlan":
		output = f.plan
	case ".#confctl.build":
		output = f.outputs
	case ".#confctl.inputsInfo":
		output = f.metadata
	case ".#confctl.machineKeys":
		output = `{"one":"k1","two":"k2"}`
	default:
		return Result{}, fmt.Errorf("unexpected installable %s", installable)
	}
	return Result{ExitCode: 0, Stdout: []byte(output)}, nil
}

func (f *buildFixture) deps() buildDeps {
	return buildDeps{Run: f.run, Now: func() time.Time { return f.clock }, RootDir: func() (string, error) { f.rootCalls++; return f.roots, nil }}
}

func TestBuildDeliverySaveReuseAndOrderedMetadata(t *testing.T) {
	f := newBuildFixture(t)
	var trace []string
	ui := buildPresentation{
		Evaluating: func(count int) error { trace = append(trace, fmt.Sprint("evaluate-", count)); return nil },
		GroupsPrepared: func(groups []build.Group) error {
			trace = append(trace, fmt.Sprint("groups-", len(groups)))
			return nil
		},
		GroupStart: func(i, count int, group build.Group) error { trace = append(trace, "group"); return nil },
		Progress:   func(build.Event) error { trace = append(trace, "progress"); return nil },
		Close:      func(error) error { trace = append(trace, "close"); return nil },
	}
	out, err := f.e.buildSelected([]string{"one", "two"}, buildOptions{}, ui, f.deps())
	if err != nil || !out.Complete || len(out.Generations) != 2 || f.rootCalls != 12 {
		t.Fatal(out, f.rootCalls, err)
	}
	if !reflect.DeepEqual(trace, []string{"evaluate-2", "groups-1", "group", "progress", "close"}) {
		t.Fatal("phase order", trace)
	}
	name := "2026-10-15--10-20-30"
	for _, host := range []string{"one", "two"} {
		list, err := generation.LoadBuildList(f.e.Root, host, generation.Options{})
		if err != nil || list.Current == nil || list.Current.Name != name || len(list.Builds) != 1 {
			t.Fatal(list, err)
		}
		link, err := os.Readlink(filepath.Join(f.e.Root, ".confctl/generations", host, "current"))
		if err != nil || link != name {
			t.Fatal(link, err)
		}
		root, err := os.Readlink(filepath.Join(f.roots, generation.RootName(host, name, "toplevel")))
		if err != nil || root != filepath.Join(f.e.Root, ".confctl/generations", host, name, "toplevel") {
			t.Fatal(root, err)
		}
	}
	record := out.Generations[0].Generation
	if got := record.InputsInfo; len(got) != 2 || got[0].Name != "z" || got[1].Name != "a" || buildObjectValue(got[0].Value.(generation.Object), "n") != json.Number("9007199254740995") {
		t.Fatal("ordered/exact metadata lost", got)
	}
	bytesBefore, err := os.ReadFile(filepath.Join(record.Dir, "generation.json"))
	if err != nil || !bytes.Contains(bytesBefore, []byte(`"f": -0`)) {
		t.Fatal("number token changed", err)
	}
	f.calls = nil
	deps := f.deps()
	deps.RootDir = func() (string, error) { t.Fatal("reuse looked up login/roots"); return "", nil }
	f.metadata = "invalid JSON must remain unreached"
	out, err = f.e.buildSelected([]string{"one", "two"}, buildOptions{}, buildPresentation{}, deps)
	if err != nil || !out.Complete || len(out.Generations) != 2 || strings.Contains(strings.Join(f.calls, ","), "inputsInfo") {
		t.Fatal(out, f.calls, err)
	}
	bytesAfter, _ := os.ReadFile(filepath.Join(record.Dir, "generation.json"))
	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatal("reuse rewrote record")
	}
}

func TestBuildDeliveryLateRootAndCurrentFailuresKeepPrefix(t *testing.T) {
	for _, kind := range []string{"root", "current"} {
		t.Run(kind, func(t *testing.T) {
			f := newBuildFixture(t)
			deps := f.deps()
			sentinel := errors.New("late root lookup")
			if kind == "root" {
				deps.RootDir = func() (string, error) {
					f.rootCalls++
					if f.rootCalls == 8 { // second host: mkdir happened, no root link.
						return "", sentinel
					}
					return f.roots, nil
				}
			} else {
				current := filepath.Join(f.e.Root, ".confctl/generations/two/current")
				if err := os.MkdirAll(current, 0700); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.e.buildSelected([]string{"one", "two"}, buildOptions{}, buildPresentation{}, deps)
			if err == nil || out.Complete || len(out.Generations) != 1 || out.Generations[0].Host != "one" || kind == "root" && !errors.Is(err, sentinel) {
				t.Fatal(out, err)
			}
			name := f.clock.Format("2006-01-02--15-04-05")
			for _, host := range []string{"one", "two"} {
				if data, err := os.ReadFile(filepath.Join(f.e.Root, ".confctl/generations", host, name, "generation.json")); err != nil || !bytes.Contains(data, []byte(`"mode": "flakes"`)) {
					t.Fatal("saved record lost after later error", host, err)
				}
			}
			if target, err := os.Readlink(filepath.Join(f.e.Root, ".confctl/generations/one/current")); err != nil || target != name {
				t.Fatal("earlier current was undone", target, err)
			}
			if kind == "current" {
				pending, _ := filepath.Glob(filepath.Join(f.e.Root, ".confctl/generations/two/current.new-*"))
				if len(pending) != 1 {
					t.Fatal("failed rename removed temporary link", pending)
				}
			}
		})
	}
}

func TestBuildDeliveryCLIConfirmationAndSelection(t *testing.T) {
	for _, input := range []string{"n\n", ""} {
		f := newBuildFixture(t)
		f.e.Inventory = []Machine{{Name: "one", Managed: true}}
		inv, err := cli.BuiltinRegistry().Parse([]string{"build", "one"})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = f.e.buildCLI(inv, strings.NewReader(input), &out, &out, f.deps())
		if err == nil || len(f.calls) != 0 || f.rootCalls != 0 || !strings.Contains(out.String(), "Continue?") {
			t.Fatal("refusal reached build", err, f.calls, out.String())
		}
	}
	f := newBuildFixture(t)
	f.e.Inventory = []Machine{{Name: "one", Managed: true}, {Name: "two", Managed: false}}
	inv, _ := cli.BuiltinRegistry().Parse([]string{"build", "-y", "--max-jobs=", "--cores=auto", "one"})
	var out bytes.Buffer
	if err := f.e.buildCLI(inv, strings.NewReader(""), &out, &out, f.deps()); err != nil || strings.Contains(out.String(), "Continue?") || !strings.Contains(out.String(), "one: 2026-") || len(f.calls) == 0 {
		t.Fatal(err, out.String())
	}
	inv, _ = cli.BuiltinRegistry().Parse([]string{"build", "-y", "missing"})
	if err := f.e.buildCLI(inv, strings.NewReader(""), &out, &out, f.deps()); err == nil || err.Error() != "No machines to build" {
		t.Fatal(err)
	}
}

func TestBuildDeliveryLazyLogin(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name  string
		login *string
		err   error
	}{
		{name: "nil"}, {name: "empty", login: buildText("")},
		{name: "literal", login: buildText("synthetic\n user")},
		{name: "error", err: errors.New("synthetic login failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			get := buildRootDirectory(root, func() (*string, error) { calls++; return tc.login, tc.err })
			if calls != 0 {
				t.Fatal("root closure queried login eagerly")
			}
			for i := 1; i <= 2; i++ {
				path, err := get()
				if calls != i || tc.err != nil && err != tc.err || tc.login == nil && err == nil {
					t.Fatal(calls, path, err)
				}
				if tc.login != nil && tc.err == nil {
					identity, _ := generation.ComputeRootIdentity(root, tc.login)
					if err != nil || path != identity.Directory {
						t.Fatal("login bytes changed", err)
					}
				}
			}
		})
	}
}

func buildText(value string) *string { return &value }

func TestBuildDeliveryActualFakeNixAndLog(t *testing.T) {
	f := newBuildFixture(t)
	tools := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	body := "#!" + sh + "\nfor last do :; done\ncase \"$1:$last\" in\n" +
		"eval:.#confctl.settings) printf '%s' '{\"nix\":{\"impureEval\":false,\"legacyNixPath\":false}}';;\n" +
		"eval:.#confctl.buildPlan) printf '%s' " + quote(f.plan) + ";;\n" +
		"eval:.#confctl.build) printf '%s' " + quote(f.outputs) + ";;\n" +
		"eval:.#confctl.inputsInfo) printf '%s' " + quote(f.metadata) + ";;\n" +
		"build:*) printf \"building '/synthetic.drv'\\n\" >&2; printf 'null';;\n" +
		"*) printf 'unexpected fake Nix request' >&2; exit 99;;\nesac\n"
	if err := os.WriteFile(filepath.Join(tools, "nix"), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	if err := f.e.OpenLog("build"); err != nil {
		t.Fatal(err)
	}
	defer f.e.CloseLog(false)
	deps := f.deps()
	deps.Run = nil // Real tagged process/log adapter, fake Nix only.
	var events []build.Event
	out, err := f.e.buildSelected([]string{"one", "two"}, buildOptions{}, buildPresentation{Progress: func(event build.Event) error { events = append(events, event); return nil }}, deps)
	if err != nil || !out.Complete || len(out.Generations) != 2 || len(events) != 1 || events[0].Path != "/synthetic.drv" {
		t.Fatal(out, events, err)
	}
	log, err := os.ReadFile(f.e.Log.Name())
	if err != nil || bytes.Count(log, []byte("building '/synthetic.drv'")) != 1 || bytes.Count(log, []byte("Finished in")) != 5 {
		t.Fatal("actual tagged log lost or replayed output", string(log), err)
	}
}

func TestBuildDeliveryBuildJSONAdmissionBeforeOutputs(t *testing.T) {
	for _, body := range []string{"null", "false", "1.20e+2", `"text"`, "[]", "{}", "true", "bad JSON", "null false"} {
		t.Run(body, func(t *testing.T) {
			f := newBuildFixture(t)
			deps := f.deps()
			deps.Run = func(argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
				if argv[1] == "build" {
					f.calls = append(f.calls, "build")
					return Result{ExitCode: 0, Stdout: []byte(body)}, nil
				}
				return f.run(argv, input, chunk)
			}
			out, err := f.e.buildSelected([]string{"one"}, buildOptions{}, buildPresentation{}, deps)
			bad := body == "bad JSON" || body == "null false"
			if bad {
				if err == nil || out.Complete || f.rootCalls != 0 || containsBuildArg(f.calls, ".#confctl.build") {
					t.Fatal("invalid build stdout reached authoritative outputs/state", out, f.calls, err)
				}
			} else if err != nil || !out.Complete {
				t.Fatal("valid discarded JSON root refused", body, err)
			}
		})
	}
}

func TestBuildDeliveryLoginFailureAfterRecordBeforeRoots(t *testing.T) {
	f := newBuildFixture(t)
	name := "synthetic-login"
	loginCalls := 0
	resolve := buildRootDirectory(f.e.Root, func() (*string, error) {
		loginCalls++
		if loginCalls == 2 {
			return nil, nil
		}
		return &name, nil
	})
	deps := f.deps()
	deps.RootDir = func() (string, error) {
		// Exercise the actual nullable identity at each demand while keeping
		// the physical root destination independently confined to TempDir.
		if _, err := resolve(); err != nil {
			return "", err
		}
		return f.roots, nil
	}
	out, err := f.e.buildSelected([]string{"one"}, buildOptions{}, buildPresentation{}, deps)
	if err == nil || !strings.Contains(err.Error(), "login identity is missing") || out.Complete || loginCalls != 2 {
		t.Fatal(out, loginCalls, err)
	}
	if _, err := os.Stat(filepath.Join(f.e.Root, ".confctl/generations/one", f.clock.Format("2006-01-02--15-04-05"), "generation.json")); err != nil {
		t.Fatal("late login refusal lost already closed record", err)
	}
	entries, err := os.ReadDir(f.roots)
	if err != nil || len(entries) != 0 {
		t.Fatal("second-demand login refusal created a root", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(f.e.Root, ".confctl/generations/one/current")); !os.IsNotExist(err) {
		t.Fatal("failed Save set current", err)
	}
}

func TestBuildDeliveryGlobalOptionsAndGroupErrorOrder(t *testing.T) {
	for _, max := range []*string{nil, buildText(""), buildText("auto")} {
		f := newBuildFixture(t)
		deps := f.deps()
		deps.Run = func(argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
			want := "global"
			if max != nil {
				want = *max
			}
			for i, arg := range argv {
				if arg == "max-jobs" && i+1 < len(argv) {
					if argv[i+1] != want {
						t.Fatal("instance settings replaced global/explicit option", argv)
					}
					return f.run(argv, input, chunk)
				}
			}
			t.Fatal("max-jobs option omitted")
			return Result{}, nil
		}
		if _, err := f.e.buildSelected([]string{"one"}, buildOptions{MaxJobs: max}, buildPresentation{}, deps); err != nil {
			t.Fatal(err)
		}
	}
	f := newBuildFixture(t)
	deps := f.deps()
	sentinel := errors.New("group preparation failed")
	deps.Now = func() time.Time { t.Fatal("clock sampled after preparation error"); return time.Time{} }
	_, err := f.e.buildSelected([]string{"one"}, buildOptions{}, buildPresentation{GroupsPrepared: func([]build.Group) error { return sentinel }}, deps)
	if !errors.Is(err, sentinel) || f.rootCalls != 0 || containsBuildArg(f.calls, "build") {
		t.Fatal("group preparation error advanced effects", err, f.calls)
	}
	f = newBuildFixture(t)
	closed := 0
	out, err := f.e.buildSelected([]string{"one"}, buildOptions{}, buildPresentation{GroupStart: func(int, int, build.Group) error { return nil }, Close: func(error) error { closed++; return sentinel }}, f.deps())
	if !out.Complete || !errors.Is(err, sentinel) || closed != 1 {
		t.Fatal("group close failure lost complete outcome or retried", out, closed, err)
	}
}
