package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCandidateRegistryPreparationBindsActualBytesWithoutConfigEffects(t *testing.T) {
	root := t.TempDir()
	template := filepath.Join(root, "template.json")
	text := `{"schema":1,"bound_sources":[],"extensions":[{"id":"fixture","argv":["/missing"],"protocol":{"major":1,"minor":0}}]}`
	if err := os.WriteFile(template, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	_ = json.Unmarshal([]byte(text), &original)
	seen := map[string]bool{}
	for _, flake := range []string{"fixture one", "fixture two ${ROOT}"} {
		run := t.TempDir()
		config := filepath.Join(run, "config")
		_ = os.Mkdir(config, 0700)
		if err := WriteFiles(config, map[string]File{"flake.nix": {Text: flake}, "scripts/original.rb": {Text: "unchanged"}}); err != nil {
			t.Fatal(err)
		}
		before, err := Snapshot(config)
		if err != nil {
			t.Fatal(err)
		}
		path, err := prepareCandidateRegistry(template, config, run)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var prepared map[string]json.RawMessage
		_ = json.Unmarshal(b, &prepared)
		for key, value := range original {
			if key == "bound_sources" {
				continue
			}
			var x, y any
			_ = json.Unmarshal(value, &x)
			_ = json.Unmarshal(prepared[key], &y)
			if !reflect.DeepEqual(x, y) {
				t.Fatal("template field changed", key)
			}
		}
		var sources []struct{ Path, SHA256 string }
		_ = json.Unmarshal(prepared["bound_sources"], &sources)
		actual, _ := os.ReadFile(filepath.Join(config, "flake.nix"))
		digest := fmt.Sprintf("%x", sha256.Sum256(actual))
		if len(sources) != 1 || sources[0].Path != "flake.nix" || sources[0].SHA256 != digest || seen[digest] {
			t.Fatal("binding did not use distinct actual bytes", sources)
		}
		seen[digest] = true
		after, err := Snapshot(config)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("preparation changed configuration", err)
		}
		var sidecar map[string]string
		sb, err := os.ReadFile(filepath.Join(run, "candidate-registry-preparation.json"))
		if err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(sb, &sidecar)
		if sidecar["template_sha256"] != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) || sidecar["prepared_sha256"] != fmt.Sprintf("%x", sha256.Sum256(b)) || sidecar["flake_sha256"] != digest {
			t.Fatal(sidecar)
		}
		_ = os.WriteFile(filepath.Join(config, "flake.nix"), []byte("edited declared bytes"), 0600)
		changed, _ := os.ReadFile(filepath.Join(config, "flake.nix"))
		if sources[0].SHA256 == fmt.Sprintf("%x", sha256.Sum256(changed)) {
			t.Fatal("old preparation silently rebound edit")
		}
	}
}

func TestRegistryPreparationHelper(t *testing.T) {
	if os.Getenv("COMPAT_REGISTRY_PREPARATION_HELPER") != "1" {
		return
	}
	fmt.Printf("%s\n%s\n", os.Getenv("CONFCTL_EXTENSION_REGISTRY"), os.Getenv("CONFCTL_EXTENSION_ROOT"))
	os.Exit(0)
}

func TestRunPreparesOnlyCandidateExtensionCases(t *testing.T) {
	template := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(template, []byte(`{"schema":1,"bound_sources":[],"extensions":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"discover_netbootable.rb", "runtime_kernels.rb"} {
		if err := os.WriteFile(filepath.Join(source, "scripts", name), []byte("original script bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COMPAT_SITE_SOURCE", source)
	for _, tc := range []struct {
		name, registry string
		extensions     bool
		prepared       bool
	}{
		{"candidate", template, true, true},
		{"ordinary", template, false, false},
		{"oracle", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{ID: "registry-" + tc.name, DeadlineMS: 3000, Argv: []string{"-test.run=^TestRegistryPreparationHelper$"}, Extensions: tc.extensions, Env: map[string]string{"COMPAT_REGISTRY_PREPARATION_HELPER": "1", "GORACE": "atexit_sleep_ms=0"}, Files: map[string]File{"flake.nix": {Text: "actual ${ROOT} bytes\n"}}}
			o, root, err := Run(context.Background(), c, os.Args[0], "/nonexistent", t.TempDir(), tc.registry, "")
			if err != nil || o.Exit != 0 {
				t.Fatal(err, o)
			}
			path := filepath.Join(root, "candidate-registry.json")
			if tc.prepared {
				if o.Stdout != path+"\n"+o.ConfigRoot+"\n" {
					t.Fatal("prepared authority not passed", o.Stdout)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var r struct {
					BoundSources []struct{ Path, SHA256 string } `json:"bound_sources"`
				}
				if err = json.Unmarshal(b, &r); err != nil {
					t.Fatal(err)
				}
				if len(r.BoundSources) != 1 || r.BoundSources[0].SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(o.Before["flake.nix"].Text))) {
					t.Fatal("binding not from before snapshot", r)
				}
			} else {
				if o.Stdout != "\n\n" {
					t.Fatal("candidate authority on unprepared path", o.Stdout)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("unrequested preparation", err)
				}
			}
			if !reflect.DeepEqual(o.Before, o.After) || len(o.Events) != 0 {
				t.Fatal("preparation mutated/traced configuration", o)
			}
			if tc.extensions && o.Before["scripts/discover_netbootable.rb"].Text != "original script bytes\n" {
				t.Fatal("script copy changed")
			}
		})
	}
}
