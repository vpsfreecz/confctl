package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func packagedRegistry(t *testing.T) Registry {
	t.Helper()
	r, err := ReadRegistry("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func registryMain(t *testing.T, r Registry, argv []string) (int, string, string) {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, "registry.json")
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFCTL_EXTENSION_REGISTRY", path)
	root := filepath.Join(base, "configuration")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	f, err := os.Create(filepath.Join(base, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old; f.Close() }()
	code := Main(context.Background(), argv)
	b, err = os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b), root
}

func TestHookOnlyRegistryHelp(t *testing.T) {
	r := packagedRegistry(t)
	r.Extensions = r.Extensions[:1]
	code, out, root := registryMain(t, r, []string{"--help"})
	expected, _ := cli.BuiltinRegistry().Help(nil, 80)
	if code != 0 || out != expected || strings.Contains(out, "runtime-kernels") {
		t.Fatal(code, out)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatal("help caused configuration effects", files, err)
	}
}

func TestSupportedRegistryHelp(t *testing.T) {
	code, out, _ := registryMain(t, packagedRegistry(t), []string{"--help"})
	if code != 0 || !strings.Contains(out, "    runtime-kernels - Manage node runtime kernel versions\n") {
		t.Fatal(code, out)
	}
}

func TestUnsupportedRegistryShapesRejectBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Registration)
	}{
		{"path", func(r *Registration) { r.Commands[0].Path = []string{"custom", "update"} }},
		{"handler", func(r *Registration) { r.Commands[0].Handler = "custom.update" }},
		{"description", func(r *Registration) { r.Commands[0].Description = "Different help" }},
		{"options", func(r *Registration) { r.Commands[0].OptionSets = []string{"custom"} }},
		{"required-argument", func(r *Registration) { r.Commands[0].Arguments[0]["required"] = true }},
		{"extra-argument-field", func(r *Registration) { r.Commands[0].Arguments[0]["variadic"] = true }},
		{"extra-argument", func(r *Registration) {
			r.Commands[0].Arguments = append(r.Commands[0].Arguments, map[string]any{"name": "extra", "required": false})
		}},
		{"group", func(r *Registration) { r.Groups[0].Description = "Different group" }},
		{"hook-only-group", func(r *Registration) { r.Commands = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := packagedRegistry(t)
			tc.edit(&r.Extensions[1])
			// Neither Nix nor the extension executable may start on rejection.
			for i := range r.Extensions {
				r.Extensions[i].Argv = []string{"/no/such/extension"}
			}
			code, out, root := registryMain(t, r, []string{"runtime-kernels", "update", "-y"})
			files, err := os.ReadDir(root)
			if code != 1 || out != "" || err != nil || len(files) != 0 {
				t.Fatal("unsupported registration caused effects", code, out, files, err)
			}
		})
	}
}

func TestRegistryRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	for _, b := range []string{
		`{"schema":1,"extensions":[],"discovery":true}`,
		`{"schema":1,"extensions":[{"id":"hook","protocol":{"major":1,"minor":0},"argv":["unused"],"dynamic_options":[]}]}`,
		`{"schema":1,"extensions":[]} {"schema":1}`,
	} {
		if err := os.WriteFile(p, []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRegistry(p); err == nil {
			t.Fatal("accepted unsupported metadata", b)
		}
	}
}
