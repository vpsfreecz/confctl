package generation

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRootIdentityRealpathAndInjectedLogin(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "configuration")
	if err := os.Mkdir(config, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(config, alias); err != nil {
		t.Fatal(err)
	}
	login := "fixture-user"
	readOnly(t, root, func() {
		a, err := ComputeRootIdentity(config, &login)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ComputeRootIdentity(alias, &login)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatal(a, b)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(config)))
		if a.Hash != hash || a.ShortHash != hash[:8] || a.Directory != filepath.Join("/nix/var/nix/gcroots/per-user", login, "confctl-"+hash[:8]) {
			t.Fatal(a)
		}
		if _, err := ComputeRootIdentity(config, nil); err == nil {
			t.Fatal("missing login was inferred")
		}
	})
	if got := RootName("nested/host", "name", "input.nixpkgs"); got != "nested:host-generation-name-input.nixpkgs" {
		t.Fatal(got)
	}
	if SafeHost("a/b:c") != "a:b:c" {
		t.Fatal("changed host escaping")
	}
}

func changeTestDirectory(t *testing.T, path string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Error(err)
		}
	})
}

func TestReviewedSymlinkParentConfigurationIdentityAndTrees(t *testing.T) {
	root := t.TempDir()
	outer := root + "/outer"
	physical := root + "/actual"
	if err := os.MkdirAll(physical+"/config", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outer, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical+"/config", outer+"/link"); err != nil {
		t.Fatal(err)
	}
	seed(t, outer, "host", "lexical", record(root+"/missing", "2026-10-08T12:00:00Z"))
	seed(t, physical, "host", "physical", record(root+"/missing", "2026-10-08T12:01:00Z"))
	changeTestDirectory(t, root)
	login := "fixture-user"
	for _, config := range []string{outer + "/link/..", "outer/link/.."} {
		readOnly(t, root, func() {
			got, err := ComputeRootIdentity(config, &login)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := ComputeRootIdentity(physical, &login)
			if err != nil {
				t.Fatal(err)
			}
			wrong, err := ComputeRootIdentity(outer, &login)
			if err != nil {
				t.Fatal(err)
			}
			if got != expected || got.Hash == wrong.Hash {
				t.Fatalf("physical %+v expected %+v lexical %+v", got, expected, wrong)
			}
			list, err := LoadBuildList(config, "host", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Builds) != 1 || list.Current.Name != "physical" {
				t.Fatalf("wrong generation tree: %+v", list)
			}
			build, err := LoadBuild(config, "host", "physical", Options{})
			if err != nil || build == nil {
				t.Fatal(build, err)
			}
		})
	}
}
