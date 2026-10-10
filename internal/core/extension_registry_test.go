package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/internal/cli"
)

func genericTestRegistry(t *testing.T) Registry {
	t.Helper()
	// This is a declaration input, not a second parser or command inventory.
	r, err := decodeRegistry([]byte(`{"schema":1,"extensions":[{"id":"example","protocol":{"major":1,"minor":0},"argv":["/missing/handler"],"groups":[{"path":["example"],"description":"Example group"}],"commands":[{"path":["example","run"],"handler":"capture","description":"Capture parsed values","options":[
 {"key":"missing","names":["missing"],"kind":"integer"},
 {"key":"missing-switch","names":["missing-switch"],"kind":"switch","negatable":true},
 {"key":"missing-list","names":["missing-list"],"kind":"string","multiple":true},
 {"key":"null","names":["null"],"kind":"integer","default":null},
 {"key":"null-text","names":["null-text"],"kind":"string","default":null},
 {"key":"false","names":["false"],"kind":"switch","default":false,"negatable":true},
 {"key":"zero","names":["z","zero"],"kind":"integer","default":0},
 {"key":"empty","names":["empty"],"kind":"string","default":""},
 {"key":"empty-list","names":["empty-list"],"kind":"string","multiple":true,"default":[]},
 {"key":"huge","names":["n","huge"],"kind":"integer","default":92233720368547758081234567890},
 {"key":"negative","names":["negative"],"kind":"integer","default":-92233720368547758091234567890},
 {"key":"list","names":["l","list"],"kind":"string","multiple":true,"default":["default"]}],"arguments":[{"name":"required","required":true},{"name":"tail","variadic":true}]}],"hooks":[{"event":"rediscover.after-write","handler":"capture","order":0}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Extensions[0].Commands[0].Options = append(r.Extensions[0].Commands[0].Options, ext.Option{Key: "missing-text", Names: []string{"missing-text"}, Kind: "string"})
	return r
}

func writeTestRegistry(t *testing.T, r Registry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "registry.json")
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func replaceAuthority(env *[]string, key string, value *string) {
	var filtered []string
	for _, v := range *env {
		if !strings.HasPrefix(v, key+"=") {
			filtered = append(filtered, v)
		}
	}
	if value != nil {
		filtered = append(filtered, key+"="+*value)
	}
	*env = filtered
}

func TestActualCLIBoundRegistryHelpAndFailures(t *testing.T) {
	p := writeTestRegistry(t, genericTestRegistry(t))
	for _, tc := range []struct {
		name  string
		setup func(string, string, *[]string)
	}{
		{"valid", nil},
		{"unrelated", func(root, _ string, _ *[]string) {
			if err := os.WriteFile(filepath.Join(root, "unrelated"), []byte("allowed"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"root-symlink", func(root, _ string, env *[]string) {
			alias := filepath.Join(filepath.Dir(root), "root-alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", &alias)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, stderr, _ := actualCLIWithSetup(t, []string{"example", "run", "--help"}, p, tc.setup)
			if code != 0 || stderr != "" || !strings.Contains(out, "Capture parsed values") || !strings.Contains(out, "<required> [tail...]") {
				t.Fatal(code, out, stderr)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		setup func(string, string, *[]string)
	}{
		{"partial-registry", func(_, _ string, env *[]string) { replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", nil) }},
		{"partial-root", func(_, _ string, env *[]string) { replaceAuthority(env, "CONFCTL_EXTENSION_REGISTRY", nil) }},
		{"empty", func(_, _ string, env *[]string) {
			empty := ""
			replaceAuthority(env, "CONFCTL_EXTENSION_REGISTRY", &empty)
			replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", &empty)
		}},
		{"wrong-root", func(root, _ string, env *[]string) {
			parent := filepath.Dir(root)
			replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", &parent)
		}},
		{"stale", func(root, _ string, _ *[]string) {
			if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"source-symlink", func(root, _ string, _ *[]string) {
			if err := os.Rename(filepath.Join(root, "flake.nix"), filepath.Join(root, "target")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("target", filepath.Join(root, "flake.nix")); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(root, _ string, _ *[]string) {
			if err := os.Remove(filepath.Join(root, "flake.nix")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "flake.nix"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"no-sources", func(_ string, path string, _ *[]string) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := decodeRegistry(b)
			if err != nil {
				t.Fatal(err)
			}
			r.BoundSources = nil
			b, _ = json.Marshal(r)
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, stderr, _ := actualCLIWithSetup(t, []string{"--help"}, p, tc.setup)
			if code != 1 || out != "" || stderr == "" {
				t.Fatal(code, out, stderr)
			}
		})
	}
	// A version-only request keeps the pre-registry observed failure even when the
	// explicit authority pair is invalid.
	code, out, stderr, _ := actualCLIWithSetup(t, []string{"--version"}, p, func(_, _ string, env *[]string) { replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", nil) })
	if code != 1 || out != "" || stderr != "confctl: version unknown\nerror: confctl: version unknown\n" {
		t.Fatal(code, out, stderr)
	}
	code, out, stderr, _ = actualCLI(t, []string{"example", "run"}, p)
	if code != 64 || out != "" || !strings.Contains(stderr, "missing required argument") {
		t.Fatal("arity had effects", code, out, stderr)
	}
}

func TestBoundRegistryCloneAndFiniteSources(t *testing.T) {
	r := genericTestRegistry(t)
	roots := []string{t.TempDir(), t.TempDir()}
	r = bindTestRegistry(t, roots[0], r)
	b, err := os.ReadFile(filepath.Join(roots[0], "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(roots[1], "flake.nix"), b, 0600); err != nil {
		t.Fatal(err)
	}
	p := writeTestRegistry(t, r)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	for _, root := range roots {
		if err = os.Chdir(root); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFCTL_EXTENSION_ROOT", root)
		t.Setenv("CONFCTL_EXTENSION_REGISTRY", p)
		if _, err = LoadRegistry(); err != nil {
			t.Fatal("identical clone refused", err)
		}
		_ = os.WriteFile(filepath.Join(root, "unrelated"), []byte("not bound"), 0600)
		if _, err = LoadRegistry(); err != nil {
			t.Fatal(err)
		}
	}
	root := roots[1]
	for _, path := range []string{"", ".", "..", "../flake.nix", "/flake.nix", "./flake.nix", "x/../flake.nix"} {
		source := r.BoundSources[0]
		source.Path = path
		if err = validateBoundSources(root, []ext.BoundSource{source}); err == nil {
			t.Fatal("unsafe bound path accepted", path)
		}
	}
	if err = validateBoundSources(root, append(r.BoundSources, r.BoundSources...)); err == nil {
		t.Fatal("duplicate source accepted")
	}
	if err = os.Mkdir(filepath.Join(root, "actual"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "actual", "flake.nix"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("actual", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	source := r.BoundSources[0]
	source.Path = "linked/flake.nix"
	if err = validateBoundSources(root, []ext.BoundSource{source}); err == nil {
		t.Fatal("symlink parent accepted")
	}
}

func TestGeneralDeclarationsAndInvalidDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Registry)
	}{
		{"shared-root", func(r *Registry) {
			other := r.Extensions[0]
			other.ID = "other"
			other.Commands = nil
			r.Extensions = append(r.Extensions, other)
		}},
		{"duplicate-id", func(r *Registry) { r.Extensions = append(r.Extensions, r.Extensions[0]) }},
		{"leaf-parent", func(r *Registry) {
			r.Extensions[0].Commands = append(r.Extensions[0].Commands, ext.Command{Path: []string{"example", "run", "child"}, Handler: "capture"})
		}},
		{"help-root", func(r *Registry) { r.Extensions[0].Groups[0].Path = []string{"help"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := genericTestRegistry(t)
			tc.edit(&r)
			code, out, stderr, _ := actualCLI(t, []string{"--help"}, writeTestRegistry(t, r))
			if code != 1 || out != "" || stderr == "" {
				t.Fatal(code, out, stderr)
			}
		})
	}
	for _, body := range []string{`"1"`, `1.0`, `1e3`, `true`, `[]`, `{}`, `"0x10"`} {
		r := genericTestRegistry(t)
		r.Extensions[0].Commands[0].Options[0].Default = json.RawMessage(body)
		code, out, stderr, _ := actualCLI(t, []string{"--help"}, writeTestRegistry(t, r))
		if code != 1 || out != "" || stderr == "" {
			t.Fatal("invalid integer default", body, code, out, stderr)
		}
	}
	for _, o := range []ext.Option{
		{Key: "x", Names: []string{"x"}, Kind: "switch", Default: json.RawMessage(`null`)},
		{Key: "x", Names: []string{"x"}, Kind: "string", Multiple: true, Default: json.RawMessage(`null`)},
		{Key: "x", Names: []string{"x"}, Kind: "string", Multiple: true, Default: json.RawMessage(`[1]`)},
		{Key: "x", Names: []string{"x"}, Kind: "string", Default: json.RawMessage(`"bad"`), Choices: []string{"good"}},
		{Key: "x", Names: []string{"x"}, Kind: "number"},
	} {
		r := genericTestRegistry(t)
		r.Extensions[0].Commands[0].Options = []ext.Option{o}
		if _, _, err := commandRegistry(r); err == nil {
			t.Fatal("invalid default/type accepted", o)
		}
	}
}

// This child runs the actual public SDK and records the values it receives after
// the supervisor's encoded Run crosses FD3/4. No linked-handler substitute.
func TestRegistrySDKHelper(t *testing.T) {
	if os.Getenv("CONFCTL_REGISTRY_SDK_HELPER") != "1" {
		return
	}
	ext.Serve(map[string]ext.Handler{"capture": func(_ context.Context, _ *ext.Client, in ext.Invocation) error {
		for _, key := range []string{"huge", "negative", "zero", "missing"} {
			if value, exists := in.Options[key]; exists && value != nil {
				if _, ok := value.(json.Number); !ok {
					return fmt.Errorf("SDK lost JSON number type for %s: %T", key, value)
				}
			}
		}
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		if err := os.WriteFile(os.Getenv("CONFCTL_REGISTRY_SDK_CAPTURE"), b, 0600); err != nil {
			return err
		}
		if os.Getenv("CONFCTL_REGISTRY_SDK_FAIL") == "1" {
			return fmt.Errorf("requested handler failure")
		}
		return nil
	}}, nil)
}

func TestGenericCommandAndHookSDKPresenceAndNumbers(t *testing.T) {
	r := genericTestRegistry(t)
	r.Extensions[0].Argv = []string{os.Args[0], "-test.run=^TestRegistrySDKHelper$"}
	registry, registered, err := commandRegistry(r)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		argv    []string
		changes map[string]any
	}{
		{[]string{"example", "run", "required"}, nil},
		{[]string{"example", "run", "--huge", "+92233720368547758081234567891", "required"}, map[string]any{"huge": json.Number("92233720368547758081234567891")}},
		{[]string{"example", "run", "-n+0X1_A", "--negative", "-92233720368547758091234567891", "--zero", "0", "--no-false", "--empty=", "-lone", "-ltwo", "--missing", "9007199254740993", "--no-missing-switch", "--missing-list=", "--missing-text=", "--null-text", "null", "required", "tail", "--help"}, map[string]any{"huge": json.Number("26"), "negative": json.Number("-92233720368547758091234567891"), "missing": json.Number("9007199254740993"), "null-text": "null", "missing-switch": false, "missing-text": "", "missing-list": []any{""}, "list": []any{"default", "one", "two"}}},
	}
	for _, tc := range cases {
		inv, err := registry.Parse(tc.argv)
		if err != nil {
			t.Fatal(err)
		}
		if err = checkExtensionArity(inv); err != nil {
			t.Fatal(err)
		}
		bound := registered["example run"]
		want := map[string]any{"null": nil, "null-text": nil, "false": false, "zero": json.Number("0"), "empty": "", "empty-list": []any{}, "huge": json.Number("92233720368547758081234567890"), "negative": json.Number("-92233720368547758091234567890"), "list": []any{"default"}}
		for key, value := range tc.changes {
			want[key] = value
		}
		for _, hook := range []bool{false, true} {
			root := t.TempDir()
			capture := filepath.Join(t.TempDir(), "received.json")
			t.Setenv("CONFCTL_REGISTRY_SDK_HELPER", "1")
			t.Setenv("CONFCTL_REGISTRY_SDK_CAPTURE", capture)
			t.Setenv("CONFCTL_REGISTRY_SDK_FAIL", "0")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			e := &Engine{Root: root, Context: ctx}
			var code int
			if hook {
				code, err = e.HooksFrom(r, "rediscover.after-write", []string{"selected"}, inv)
			} else {
				code, err = e.Invoke(bound.Registration, bound.Command.Handler, commandInvocation(root, bound, inv, tc.argv))
			}
			cancel()
			if err != nil || code != 0 {
				t.Fatal("SDK invocation", hook, code, err)
			}
			raw, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			var received ext.Invocation
			if err = d.Decode(&received); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(received.Options, want) || !reflect.DeepEqual(received.Arguments, inv.Args) || !reflect.DeepEqual(received.OriginCommand, inv.Command.Path) || !reflect.DeepEqual(received.RawArgv, tc.argv) {
				t.Fatalf("hook=%t options=%#v want=%#v context=%#v", hook, received.Options, want, received)
			}
			if hook && (received.Event != "rediscover.after-write" || !reflect.DeepEqual(received.SelectedNames, []string{"selected"})) {
				t.Fatal(received)
			}
			for _, key := range []string{"huge", "negative", "zero"} {
				if !bytes.Contains(raw, []byte(`"`+key+`":`+want[key].(json.Number).String())) {
					t.Fatal("quoted/rounded wire integer", string(raw))
				}
			}
		}
	}
	// Default-free initialization is still typed but does not create wire presence.
	inv, err := registry.Parse([]string{"example", "run", "required"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Options["missing-switch"].Value.Kind != cli.Boolean || inv.Options["missing-list"].Value.Kind != cli.TextList || inv.Options["missing"].Value.Kind != cli.Null {
		t.Fatal(inv.Options)
	}
	for _, key := range []string{"missing", "missing-switch", "missing-list", "missing-text"} {
		if _, exists := invocationOptions(inv)[key]; exists {
			t.Fatal("absent declaration default emitted", key)
		}
	}
	// Mutating borrowed copies and one parsed invocation must not change defaults.
	commands := registry.Commands()
	for i := range commands {
		for j := range commands[i].Options {
			o := &commands[i].Options[j]
			if o.Default.Kind == cli.Number {
				o.Default.Integer.SetInt64(7)
			}
			if o.Default.Kind == cli.TextList && len(o.Default.Strings) > 0 {
				o.Default.Strings[0] = "mutated"
			}
			o.DefaultPresent = false
		}
	}
	value := inv.Options["huge"]
	value.Value.Integer.SetInt64(9)
	inv.Options["huge"] = value
	inv.Options["list"].Value.Strings[0] = "mutated"
	fresh, err := registry.Parse([]string{"example", "run", "required"})
	if err != nil {
		t.Fatal(err)
	}
	if invocationOptions(fresh)["huge"] != json.Number("92233720368547758081234567890") || !reflect.DeepEqual(invocationOptions(fresh)["list"], []string{"default"}) {
		t.Fatal("mutable default leaked", invocationOptions(fresh))
	}
	projected := invocationOptions(fresh)
	projected["list"].([]string)[0] = "mapped mutation"
	if fresh.Options["list"].Value.Strings[0] != "default" {
		t.Fatal("projection retained mutable input storage")
	}
	for _, argv := range [][]string{{"example", "run", "--missing", "null", "required"}, {"example", "run", "--zero", "08", "required"}, {"example", "run", "--", "--zero", "null"}} {
		parsed, err := registry.Parse(argv)
		if argv[2] == "--" {
			if err != nil || !reflect.DeepEqual(parsed.Args, []string{"--zero", "null"}) {
				t.Fatal(parsed, err)
			}
		} else if err == nil {
			t.Fatal("invalid integer CLI accepted", argv)
		}
	}
}

func TestGeneralRegistryMetadataOwnsHelp(t *testing.T) {
	r := genericTestRegistry(t)
	reg := &r.Extensions[0]
	reg.Groups = append(reg.Groups, ext.Group{Path: []string{"example", "nested"}, Description: "Nested custom description"})
	reg.Commands[0].Path = []string{"example", "nested", "invoke"}
	reg.Commands[0].Description = "Custom command description"
	reg.Commands[0].Options[9].Description = "Custom arbitrary-precision default"
	reg.Commands[0].Options[9].Metavar = "count"
	code, out, stderr, _ := actualCLI(t, []string{"help", "example", "nested", "invoke"}, writeTestRegistry(t, r))
	if code != 0 || stderr != "" || !strings.Contains(out, "Custom command description") || !strings.Contains(out, "--huge=count") || !strings.Contains(out, "92233720368547758081234567890") || !strings.Contains(out, "<required> [tail...]") {
		t.Fatal(code, out, stderr)
	}
	code, out, stderr, _ = actualCLI(t, []string{"example", "nested", "--help"}, writeTestRegistry(t, r))
	if code != 0 || stderr != "" || !strings.Contains(out, "Nested custom description") || !strings.Contains(out, "invoke - Custom command description") {
		t.Fatal(code, out, stderr)
	}
}

func TestActualGenericCLIDispatchAndLog(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			root := t.TempDir()
			r := genericTestRegistry(t)
			r = bindTestRegistry(t, root, r)
			r.Extensions[0].Argv = []string{os.Args[0], "-test.run=^TestRegistrySDKHelper$"}
			path := writeTestRegistry(t, r)
			capture := filepath.Join(t.TempDir(), "received.json")
			argv := []string{"-c", "never", "example", "run", "--huge", "+0X1_A", "required"}
			a, _ := json.Marshal(argv)
			t.Setenv("CONFCTL_REGISTRY_SDK_HELPER", "1")
			t.Setenv("CONFCTL_REGISTRY_SDK_CAPTURE", capture)
			t.Setenv("CONFCTL_EXTENSION_REGISTRY", path)
			t.Setenv("CONFCTL_EXTENSION_ROOT", root)
			t.Setenv("CONFCTL_STAGE1_SUBPROCESS", "1")
			t.Setenv("CONFCTL_STAGE1_ARGV", string(a))
			if failure {
				t.Setenv("CONFCTL_REGISTRY_SDK_FAIL", "1")
			} else {
				t.Setenv("CONFCTL_REGISTRY_SDK_FAIL", "0")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
			cmd.Dir = root
			cmd.Env = os.Environ()
			raw, err := cmd.CombinedOutput()
			if failure {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !bytes.Contains(raw, []byte("requested handler failure")) {
					t.Fatal(string(raw), err)
				}
			} else if err != nil || len(raw) != 0 {
				t.Fatal(string(raw), err)
			}
			data, err := os.ReadFile(capture)
			if err != nil || !bytes.Contains(data, []byte(`"huge":26`)) {
				t.Fatal(string(data), err)
			}
			logs, err := filepath.Glob(filepath.Join(root, ".confctl", "logs", "*.log"))
			if err != nil {
				t.Fatal(err)
			}
			if !failure {
				if len(logs) != 0 {
					t.Fatal("successful log retained", logs)
				}
				return
			}
			if len(logs) != 1 {
				t.Fatal("failure log missing", logs)
			}
			data, err = os.ReadFile(logs[0])
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := bytes.Cut(data, []byte("\n"))
			d := json.NewDecoder(bytes.NewReader(line))
			d.UseNumber()
			var logged ext.Invocation
			if err = d.Decode(&logged); err != nil {
				t.Fatal(string(data), err)
			}
			if logged.Options["huge"] != json.Number("26") || logged.Options["false"] != false || logged.Options["empty"] != "" || logged.Options["zero"] != json.Number("0") || !reflect.DeepEqual(logged.RawArgv, argv) {
				t.Fatal("generic log lost typed values", logged)
			}
		})
	}
}

func TestOptionSetAndBuiltinDefaultPresence(t *testing.T) {
	r := genericTestRegistry(t)
	r.Extensions[0].Commands[0].OptionSets = []string{"machine-filter", "confirmation"}
	compiled, _, err := commandRegistry(r)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := compiled.Parse([]string{"example", "run", "required"})
	if err != nil {
		t.Fatal(err)
	}
	opts := invocationOptions(inv)
	for _, key := range []string{"yes", "show-trace"} {
		if value, exists := opts[key]; !exists || value != false {
			t.Fatal(key, opts)
		}
	}
	for _, key := range []string{"attr", "tag"} {
		if !reflect.DeepEqual(opts[key], []string{}) {
			t.Fatal(key, opts)
		}
	}
	if _, exists := opts["managed"]; exists {
		t.Fatal("option sets added managed", opts)
	}
	inv, err = cli.BuiltinRegistry().Parse([]string{"health-check"})
	if err != nil {
		t.Fatal(err)
	}
	if invocationOptions(inv)["max-jobs"] != json.Number("5") {
		t.Fatal("builtin integer default presence lost", invocationOptions(inv))
	}
	inv, err = cli.BuiltinRegistry().Parse([]string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := invocationOptions(inv)["max-jobs"]; exists {
		t.Fatal("string Nix count acquired default")
	}
	inv, err = cli.BuiltinRegistry().Parse([]string{"build", "-j010"})
	if err != nil {
		t.Fatal(err)
	}
	if invocationOptions(inv)["max-jobs"] != "010" {
		t.Fatal("string Nix count converted", invocationOptions(inv))
	}
}
