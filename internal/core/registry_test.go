package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/internal/cli"
)

func packagedRegistry(t *testing.T) Registry {
	t.Helper()
	b, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeRegistry([]byte(strings.ReplaceAll(string(b), "@SITE@", "/missing-site")))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func bindTestRegistry(t *testing.T, root string, r Registry) Registry {
	t.Helper()
	b := []byte("test configuration source\n")
	if err := os.WriteFile(filepath.Join(root, "flake.nix"), b, 0600); err != nil {
		t.Fatal(err)
	}
	r.BoundSources = []ext.BoundSource{{Path: "flake.nix", SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}}
	return r
}

func registryMain(t *testing.T, r Registry, argv []string) (int, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "configuration")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	r = bindTestRegistry(t, root, r)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "registry.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFCTL_EXTENSION_REGISTRY", path)
	t.Setenv("CONFCTL_EXTENSION_ROOT", root)
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
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 || files[0].Name() != "flake.nix" {
		t.Fatal("preexecution effects", files, err)
	}
	return code, string(b), root
}

func TestHookOnlyRegistryHelp(t *testing.T) {
	r := packagedRegistry(t)
	r.Extensions = r.Extensions[:1]
	code, out, _ := registryMain(t, r, []string{"--help"})
	want, _ := cli.BuiltinRegistry().Help(nil, 80)
	if code != 0 || out != want || strings.Contains(out, "runtime-kernels") {
		t.Fatal(code, out)
	}
}
func TestSupportedRegistryHelp(t *testing.T) {
	code, out, _ := registryMain(t, packagedRegistry(t), []string{"--help"})
	if code != 0 || !strings.Contains(out, "    runtime-kernels - Manage node runtime kernel versions\n") {
		t.Fatal(code, out)
	}
}
func TestInvalidDeclarationsRejectBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Registration)
	}{
		{"missing-parent", func(r *Registration) { r.Commands[0].Path = []string{"custom", "update"} }},
		{"empty-handler", func(r *Registration) { r.Commands[0].Handler = "" }},
		{"unknown-options", func(r *Registration) { r.Commands[0].OptionSets = []string{"custom"} }},
		{"argument-order", func(r *Registration) {
			r.Commands[0].Arguments = append(r.Commands[0].Arguments, ext.Argument{Name: "required", Required: true})
		}},
		{"variadic-position", func(r *Registration) {
			r.Commands[0].Arguments[0].Variadic = true
			r.Commands[0].Arguments = append(r.Commands[0].Arguments, ext.Argument{Name: "last"})
		}},
		{"duplicate-command", func(r *Registration) { r.Commands = append(r.Commands, r.Commands[0]) }},
		{"reserved-alias", func(r *Registration) {
			r.Commands[0].Options = []ext.Option{{Key: "custom", Names: []string{"h"}, Kind: "switch"}}
		}},
		{"alias-collision", func(r *Registration) {
			r.Commands[0].Options = []ext.Option{{Key: "custom", Names: []string{"no-yes"}, Kind: "switch"}}
		}},
		{"relative-executable", func(r *Registration) { r.Argv = []string{"relative"} }},
		{"major", func(r *Registration) { r.Protocol.Major = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := packagedRegistry(t)
			tc.edit(&r.Extensions[1])
			code, out, _ := registryMain(t, r, []string{"--help"})
			if code != 1 || out != "" {
				t.Fatal(code, out)
			}
		})
	}
}
func TestRegistryRejectsUnknownFields(t *testing.T) {
	for _, body := range []string{
		`{"schema":1,"extensions":[],"discovery":true}`,
		`{"schema":1,"extensions":[{"id":"hook","protocol":{"major":1,"minor":0},"argv":["/unused"],"dynamic_options":[]}]}`,
		`{"schema":1,"extensions":[]} {"schema":1}`,
		`{"schema":1,"extensions":[{"commands":[{"arguments":[{"name":"x","unknown":true}]}]}]}`,
		`{"schema":1,"extensions":[{"commands":[{"options":[{"key":"x","unknown":true}]}]}]}`,
		`{"schema":1,"extensions":[{"commands":[{"options":[{"key":"x","multiple":"true"}]}]}]}`,
		`{"schema":1,"extensions":[{"commands":[{"arguments":[{"name":"x","required":1}]}]}]}`,
		`{"schema":1,"extensions":[{"commands":[{"aliases":["other"]}]}]}`,
	} {
		if _, err := decodeRegistry([]byte(body)); err == nil {
			t.Fatal("accepted invalid metadata", body)
		}
	}
}
