package hooks_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func withoutHookTierSelectors(env []string) []string {
	var out []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "CONFCTL_HOOK_TOOLS", "CONFCTL_HOOK_MODULE_FIXTURE", "CONFCTL_HOOK_MISSING_CONTENT_TOOLS":
			continue
		}
		out = append(out, entry)
	}
	return out
}

func TestInstalledChildEnvironmentDropsOnlyTierSelectors(t *testing.T) {
	// Unknown hook controls must still reach production's strict refusal. The
	// poisoned Go controls exercise isolation in the installed dependency tier.
	want := []string{"GOPROXY=https://poison.invalid,direct", "GOMODCACHE=/operator/poison", "CONFCTL_HOOK_CONTEXT=untrusted", "CONFCTL_HOOK_UNKNOWN=untrusted", "HOME=/fixture"}
	env := append([]string{"CONFCTL_HOOK_TOOLS=/fixture/launcher", "CONFCTL_HOOK_MODULE_FIXTURE=/fixture/modules", "CONFCTL_HOOK_MISSING_CONTENT_TOOLS=/fixture/missing"}, want...)
	if got := withoutHookTierSelectors(env); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// Selected only after the installed dev-tools package is built. Exercise the
// real launcher -> RunHook -> Lefthook -> lower checker path in a private repo.
func TestInstalledHookTrustCommitAndRestore(t *testing.T) {
	launcher := os.Getenv("CONFCTL_HOOK_TOOLS")
	if launcher == "" {
		t.Skip("installed hook tier not selected")
	}
	if !filepath.IsAbs(launcher) {
		t.Fatal("CONFCTL_HOOK_TOOLS must be an absolute executable")
	}
	root := t.TempDir()
	home := t.TempDir()
	var env []string
	for _, e := range withoutHookTierSelectors(os.Environ()) {
		if !strings.HasPrefix(e, "GIT_") && !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	env = append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	command := func(success bool, program string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(program, args...)
		cmd.Dir = root
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if success && err != nil || !success && err == nil {
			t.Fatalf("%s %q: %v\n%s", program, args, err, output)
		}
		return output
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	command(true, git, "init", "--template=")
	command(true, git, "config", "user.name", "Fixture")
	command(true, git, "config", "user.email", "fixture@example.test")
	candidate := strings.TrimSuffix(string(command(true, launcher, "prepare")), "\n")
	policy, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(root, "lefthook.yml")
	if err := os.WriteFile(policyPath, policy, 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "add", "--", "lefthook.yml")
	// Selector removal belongs only to this test's child environment. Passing
	// one explicitly still refuses before trust publication or installation.
	config, err := os.ReadFile(filepath.Join(root, ".git/config"))
	if err != nil {
		t.Fatal(err)
	}
	candidateInfo, err := os.Lstat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CONFCTL_HOOK_TOOLS", "CONFCTL_HOOK_MODULE_FIXTURE", "CONFCTL_HOOK_MISSING_CONTENT_TOOLS", "CONFCTL_HOOK_CONTEXT"} {
		for _, args := range [][]string{{"trust", "--yes"}, {"run", "pre-commit"}} {
			cmd := exec.Command(launcher, args...)
			cmd.Dir = root
			cmd.Env = append(append([]string(nil), env...), key+"=untrusted")
			output, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte("forbidden-control: \""+key+"\"")) {
				t.Fatalf("explicit %s was not refused: %v\n%s", key, err, output)
			}
			for _, name := range []string{"confctl-hooks-trust.json", "confctl-hooks/hooks", "confctl-hooks/install.json"} {
				if _, err := os.Lstat(filepath.Join(root, ".git", name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refusal published trust or installed hooks", name, err)
				}
			}
			candidateBytes, err := os.ReadFile(candidate)
			if err != nil || !bytes.Equal(candidateBytes, policy) {
				t.Fatal("refusal changed prepared policy bytes", err)
			}
			info, err := os.Lstat(candidate)
			if err != nil || info.Mode() != candidateInfo.Mode() {
				t.Fatal("refusal changed prepared policy mode", err)
			}
			got, err := os.ReadFile(filepath.Join(root, ".git/config"))
			if err != nil || !bytes.Equal(got, config) {
				t.Fatal("refusal changed Git config", err)
			}
		}
	}
	command(true, launcher, "trust", "--yes")
	command(true, launcher, "install")
	message := filepath.Join(root, "message")
	if err := os.WriteFile(message, []byte("Accept hook policy\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "commit", "-F", message)
	// A valid staged Nix file plus an invalid unstaged edit proves detached
	// staged-content validation and no formatter/restaging of the working file.
	file := filepath.Join(root, "literal name.nix")
	if err := os.WriteFile(file, []byte("{ }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "add", "--", "literal name.nix")
	unstaged := []byte("invalid unstaged nix\n")
	if err := os.WriteFile(file, unstaged, 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "commit", "-F", message)
	got, _ := os.ReadFile(file)
	if !bytes.Equal(got, unstaged) {
		t.Fatal("hook rewrote working content")
	}
	// Stale policy and a staged/working mismatch both reject without refresh.
	changed := append(append([]byte(nil), policy...), []byte("# unapproved\n")...)
	if err := os.WriteFile(policyPath, changed, 0644); err != nil {
		t.Fatal(err)
	}
	command(false, launcher, "run", "pre-commit")
	command(false, launcher, "trust", "--yes")
	command(true, git, "add", "--", "lefthook.yml")
	command(false, launcher, "run", "pre-commit")
	// An unsupported arbitrary policy cannot be approved even explicitly.
	command(false, launcher, "trust", "--yes")
	if err := os.WriteFile(policyPath, policy, 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "add", "--", "lefthook.yml")
	command(true, launcher, "trust", "--yes")
	command(true, launcher, "run", "pre-commit")
	command(true, launcher, "restore")
	command(false, git, "config", "--get", "core.hooksPath")
}

func TestInstalledOfflineGoModules(t *testing.T) {
	fixture := os.Getenv("CONFCTL_HOOK_MODULE_FIXTURE")
	if fixture == "" {
		t.Skip("offline module package fixture not selected")
	}
	launcher := os.Getenv("CONFCTL_HOOK_TOOLS")
	missing := os.Getenv("CONFCTL_HOOK_MISSING_CONTENT_TOOLS")
	if !filepath.IsAbs(launcher) || !filepath.IsAbs(missing) {
		t.Fatal("declared fixture tool outputs required")
	}
	root := t.TempDir()
	home := t.TempDir()
	var env []string
	for _, e := range withoutHookTierSelectors(os.Environ()) {
		key, _, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(key, "GIT_") && !strings.HasPrefix(key, "GO") && key != "HOME" {
			env = append(env, e)
		}
	}
	env = append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GOPROXY=https://poison.invalid,direct", "GOPATH=/operator/poison", "GOMODCACHE=/operator/poison", "GOSUMDB=poison.invalid", "GONOPROXY=*", "GOPRIVATE=*", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=auto")
	command := func(success bool, program string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(program, args...)
		cmd.Dir = root
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if success && err != nil || !success && err == nil {
			t.Fatalf("%s %q: %v\n%s", program, args, err, output)
		}
		return output
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	command(true, git, "init", "--template=")
	command(true, git, "config", "user.name", "Fixture")
	command(true, git, "config", "user.email", "fixture@example.test")
	for name, path := range map[string]string{"root": ".", "extensions": "extensions"} {
		err := filepath.WalkDir(filepath.Join(fixture, name), func(source string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(filepath.Join(fixture, name), source)
			if err != nil {
				return err
			}
			target := filepath.Join(root, path, rel)
			if entry.IsDir() {
				return os.MkdirAll(target, 0755)
			}
			data, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	candidate := strings.TrimSuffix(string(command(true, launcher, "prepare")), "\n")
	policy, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lefthook.yml"), policy, 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "add", "--", ".")
	command(true, launcher, "trust", "--yes")
	command(true, launcher, "install")
	message := filepath.Join(root, "message")
	if err := os.WriteFile(message, []byte("Exercise offline module\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Every hook receives fresh empty caches. This commit needs the external
	// module even though all ambient Go settings point at unavailable paths.
	command(true, git, "commit", "-F", message)
	file := filepath.Join(root, "extensions/extension_test.go")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(original, []byte("\n// Staged extension change.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "add", "--", "extensions/extension_test.go")
	unstaged := []byte("uncompilable working extension\n")
	if err := os.WriteFile(file, unstaged, 0644); err != nil {
		t.Fatal(err)
	}
	// A changed immutable proxy/tool output refuses the old receipt. Explicit
	// retrust permits the new output, whose missing content still fails offline.
	command(false, missing, "run", "pre-commit")
	command(true, missing, "trust", "--yes")
	command(false, missing, "run", "pre-commit")
	command(true, launcher, "trust", "--yes")
	command(true, git, "commit", "-F", message)
	got, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(got, unstaged) {
		t.Fatal("partial staging was changed", err)
	}
	// Manifest-only edits reach semantic preflight without a formatter call.
	mod := filepath.Join(root, "extensions/go.mod")
	data, err := os.ReadFile(mod)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mod, append(data, []byte("\n// New dependency binding required.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	command(true, git, "add", "--", "extensions/go.mod")
	command(false, launcher, "run", "pre-commit")
	command(true, launcher, "restore")
}
