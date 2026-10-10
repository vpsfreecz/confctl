package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestConfigurationCLIHelper(t *testing.T) {
	if os.Getenv("CONFCTL_CONFIGURATION_CLI") != "1" {
		return
	}
	mask, err := strconv.ParseUint(os.Getenv("CONFCTL_CONFIGURATION_UMASK"), 8, 32)
	if err != nil {
		panic(err)
	}
	syscall.Umask(int(mask))
	var argv []string
	if err = json.Unmarshal([]byte(os.Getenv("CONFCTL_CONFIGURATION_ARGV")), &argv); err != nil {
		panic(err)
	}
	os.Exit(Main(context.Background(), argv))
}

// The real Main runs in its own CWD/umask. Tools are failure sentinels unless a
// hook test explicitly supplies the process fixture; configuration writes stay
// available for inspection instead of using the pre-execution no-effect helper.
func configurationCLI(t *testing.T, root string, argv []string, registry, mask string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigurationCLIHelper$")
	cmd.Dir = root
	env := os.Environ()
	replaceAuthority(&env, "CONFCTL_EXTENSION_REGISTRY", nil)
	replaceAuthority(&env, "CONFCTL_EXTENSION_ROOT", nil)
	if registry != "" {
		replaceAuthority(&env, "CONFCTL_EXTENSION_REGISTRY", &registry)
		replaceAuthority(&env, "CONFCTL_EXTENSION_ROOT", &root)
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	tools, marker := configurationTools(t)
	cmd.Env = append(env, "CONFCTL_CONFIGURATION_CLI=1", "CONFCTL_CONFIGURATION_ARGV="+string(argvJSON), "CONFCTL_CONFIGURATION_UMASK="+mask, "PATH="+tools+":"+os.Getenv("PATH"), "PWD="+root)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(marker); !os.IsNotExist(err) {
		t.Fatal("configuration command unexpectedly ran a tool", argv, string(b), err)
	}
	return code, stdout.String(), stderr.String()
}

func configurationTools(t *testing.T) (string, string) {
	t.Helper()
	tools := t.TempDir()
	marker := filepath.Join(tools, "unexpected-tool")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "ssh", "nix"} {
		body := "#!" + bash + "\nprintf 'unexpected " + name + "\\n' >> " + ShellJoin([]string{marker}) + "\nexit 97\n"
		if name == "nix" && os.Getenv("CONFCTL_CONFIGURATION_NIX") == "1" {
			body = "#!" + bash + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestConfigurationNixHelper$", "--"}) + " \"$@\"\n"
		}
		if err = os.WriteFile(filepath.Join(tools, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return tools, marker
}

func configurationFile(t *testing.T, root, path, text string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func configurationRead(t *testing.T, root, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func configurationMachine(t *testing.T, root, name string) {
	t.Helper()
	configurationFile(t, root, "cluster/"+name+"/module.nix", "module "+name+"\n")
	configurationFile(t, root, "cluster/"+name+"/config.nix", "configuration "+name+"\n")
}

func TestConfigurationHelpExtractProvenance(t *testing.T) {
	var reference struct {
		SourceRevision string `json:"source_revision"`
		HelpExtracts   map[string]struct {
			CaptureCase string `json:"capture_case"`
			RawField    string `json:"raw_field"`
			SHA256      string `json:"sha256"`
			OracleSHA   string `json:"oracle_sha256"`
		} `json:"help_extracts"`
	}
	if err := json.Unmarshal([]byte(configurationRead(t, ".", "testdata/configuration_templates.json")), &reference); err != nil {
		t.Fatal(err)
	}
	if reference.SourceRevision != "cc40679d267165aecfa569128438bb55fe910268" || len(reference.HelpExtracts) != 2 {
		t.Fatal("invalid original help provenance", reference)
	}
	for command, extract := range reference.HelpExtracts {
		b := []byte(configurationRead(t, ".", "testdata/configuration_"+command+"_help.txt"))
		if extract.CaptureCase == "" || extract.RawField != "stdout.raw" || extract.OracleSHA != "eae4297146b9790b87f2e9935f3ea516ef5e1d047b683b64f9a57cf33c3c85de" || fmt.Sprintf("%x", sha256.Sum256(b)) != extract.SHA256 {
			t.Fatal("help extract differs from the accepted original capture", command, extract)
		}
	}
}

func TestConfigurationInitTemplatesAndUmask(t *testing.T) {
	var reference struct {
		SourceRevision string            `json:"source_revision"`
		Files          map[string]string `json:"files"`
	}
	if err := json.Unmarshal([]byte(configurationRead(t, ".", "testdata/configuration_templates.json")), &reference); err != nil {
		t.Fatal(err)
	}
	if reference.SourceRevision != "cc40679d267165aecfa569128438bb55fe910268" || len(reference.Files) != 8 {
		t.Fatal("invalid source-derived template reference", reference)
	}
	wantOutput := "mkdir cluster\nmkfile cluster/module-list.nix\nmkfile cluster/cluster.nix\nmkdir configs\nmkfile configs/confctl.nix\nmkdir data\nmkfile data/default.nix\nmkfile data/ssh-keys.nix\nmkdir environments\nmkfile environments/base.nix\nmkdir modules\nmkfile modules/module-list.nix\nmkfile flake.nix\n"
	for _, mask := range []string{"022", "077"} {
		t.Run(mask, func(t *testing.T) {
			root := t.TempDir()
			// All four permitted entries retain their bytes/modes; no .git is created.
			for _, p := range []string{"shell.nix", ".gitignore", ".gems/retained"} {
				configurationFile(t, root, p, "retained\n")
			}
			code, stdout, stderr := configurationCLI(t, root, []string{"init"}, "", mask)
			if code != 0 || stdout != wantOutput || stderr != "" {
				t.Fatal(code, stdout, stderr)
			}
			for path, digest := range reference.Files {
				content := configurationRead(t, root, path)
				if fmt.Sprintf("%x", sha256.Sum256([]byte(content))) != digest {
					t.Fatal("template differs from original Ruby", path, content)
				}
				info, err := os.Stat(filepath.Join(root, path))
				want := os.FileMode(0644)
				if mask == "077" {
					want = 0600
				}
				if err != nil || info.Mode().Perm() != want {
					t.Fatal(path, info, err)
				}
			}
			for _, directory := range []string{"cluster", "configs", "data", "environments", "modules"} {
				info, err := os.Stat(filepath.Join(root, directory))
				want := os.FileMode(0755)
				if mask == "077" {
					want = 0700
				}
				if err != nil || !info.IsDir() || info.Mode().Perm() != want {
					t.Fatal(directory, info, err)
				}
			}
			if logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log")); len(logs) != 0 {
				t.Fatal("successful init retained a log", logs)
			}
			for _, path := range []string{"shell.nix", ".gitignore", ".gems/retained"} {
				if configurationRead(t, root, path) != "retained\n" {
					t.Fatal("init overwrote permitted existing input", path)
				}
			}
		})
	}
	for _, blocked := range []string{".git", "other"} {
		t.Run(blocked, func(t *testing.T) {
			root := t.TempDir()
			configurationFile(t, root, blocked+"/retained", "retained")
			code, stdout, stderr := configurationCLI(t, root, []string{"init"}, "", "022")
			if code != 1 || stdout != "" || !strings.Contains(stderr, "init must be called in an empty directory") {
				t.Fatal(code, stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "cluster")); !os.IsNotExist(err) {
				t.Fatal("rejected init started writing", err)
			}
		})
	}
}

func TestConfigurationAddRenameAndArgumentOrdering(t *testing.T) {
	root := t.TempDir()
	configurationFile(t, root, "flake.nix", "{}\n")
	code, stdout, stderr := configurationCLI(t, root, []string{"add", "nested/host"}, "", "077")
	if code != 0 || stdout != "mkdir cluster/nested/host\nmkfile cluster/nested/host/module.nix\nmkfile cluster/nested/host/config.nix\nreplace cluster/cluster.nix\n" || stderr != "" {
		t.Fatal(code, stdout, stderr)
	}
	module := configurationRead(t, root, "cluster/nested/host/module.nix")
	config := configurationRead(t, root, "cluster/nested/host/config.nix")
	if !strings.Contains(module, `cluster."nested/host"`) || !strings.Contains(module, `inputs.channels = [ "nixos-unstable" ];`) || !strings.Contains(config, "../../../environments/base.nix") || !strings.Contains(config, `networking.hostName = "nested-host";`) {
		t.Fatal(module, config)
	}
	for _, path := range []string{"cluster", "cluster/nested", "cluster/nested/host"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatal("FileUtils.mkdir_p explicit chmod lost", path, info, err)
		}
	}
	code, stdout, stderr = configurationCLI(t, root, []string{"rename", "nested/host", "renamed/host"}, "", "022")
	if code != 0 || stdout != "mkdir cluster/renamed\nmv cluster/nested/host cluster/renamed/host\nreplace cluster/cluster.nix\n" || stderr != "" {
		t.Fatal(code, stdout, stderr)
	}
	if configurationRead(t, root, "cluster/renamed/host/module.nix") != module || configurationRead(t, root, "cluster/renamed/host/config.nix") != config || configurationRead(t, root, "cluster/cluster.nix") != "# This file is generated by confctl, changes will be lost\n[\n  ./renamed/host/module.nix\n]\n" {
		t.Fatal("rename rewrote embedded names or inventory")
	}
	for _, tc := range []struct {
		argv []string
		text string
	}{
		{[]string{"add"}, "missing argument <name>"},
		{[]string{"rename"}, "missing argument <old-name>"},
		{[]string{"rename", "old"}, "missing argument <new-name>"},
		{[]string{"add", "new", "--help"}, "unknown argument: --help (note that options must come before arguments)"},
		{[]string{"rename", "old", "new", "one", "two"}, "unknown arguments: one two"},
	} {
		for _, flake := range []bool{false, true} {
			root := t.TempDir()
			if flake {
				configurationFile(t, root, "flake.nix", "{}")
			}
			code, stdout, stderr := configurationCLI(t, root, tc.argv, "", "022")
			want, message := 1, "has no flake.nix"
			if flake {
				want, message = 64, tc.text
			}
			wantOut := ""
			if flake {
				wantOut = configurationRead(t, ".", "testdata/configuration_"+tc.argv[0]+"_help.txt")
			}
			if code != want || stdout != wantOut || !strings.Contains(stderr, message) || flake && !strings.HasSuffix(stderr, "error: "+tc.text+"\n\n") {
				t.Fatal(tc.argv, flake, code, stdout, stderr)
			}
			logs, err := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
			if err != nil || len(logs) != 1 || !strings.Contains(configurationRead(t, "/", logs[0]), " command_options: {},\n arguments: ") {
				t.Fatal("validation failure lost its CLI log", tc.argv, logs, err)
			}
		}
	}
}

func TestConfigurationDiscoveryLinksHiddenAndExistence(t *testing.T) {
	root := t.TempDir()
	configurationFile(t, root, "flake.nix", "{}")
	configurationMachine(t, root, "z")
	configurationMachine(t, root, ".hidden/nested")
	configurationMachine(t, root, "parent")
	configurationMachine(t, root, "parent/child")
	configurationMachine(t, root, "space name")
	configurationFile(t, root, "cluster/incomplete/module.nix", "only one")
	configurationMachine(t, root, "dangling")
	if err := os.Remove(filepath.Join(root, "cluster/dangling/config.nix")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("absent", filepath.Join(root, "cluster/dangling/config.nix")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("parent", filepath.Join(root, "cluster/alias")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	configurationFile(t, outside, "module.nix", "outside module")
	// File.exist? accepts directory leaves, and symlinks outside cluster are followed.
	if err := os.Mkdir(filepath.Join(outside, "config.nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "cluster/external")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := configurationCLI(t, root, []string{"rediscover"}, "", "022")
	if code != 0 || stdout != "replace cluster/cluster.nix\n" || stderr != "" {
		t.Fatal(code, stdout, stderr)
	}
	want := "# This file is generated by confctl, changes will be lost\n[\n  ./.hidden/nested/module.nix\n  ./alias/module.nix\n  ./alias/child/module.nix\n  ./external/module.nix\n  ./parent/module.nix\n  ./parent/child/module.nix\n  ./space name/module.nix\n  ./z/module.nix\n]\n"
	if got := configurationRead(t, root, "cluster/cluster.nix"); got != want {
		t.Fatal(got)
	}
}

func TestConfigurationFailureRetainsCompletedWritesAndLog(t *testing.T) {
	for _, rename := range []bool{false, true} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		if err := os.MkdirAll(filepath.Join(root, "cluster/cluster.nix"), 0755); err != nil {
			t.Fatal(err)
		}
		argv := []string{"add", "new"}
		if rename {
			configurationMachine(t, root, "old")
			argv = []string{"rename", "old", "new"}
		}
		code, stdout, stderr := configurationCLI(t, root, argv, "", "022")
		if code != 1 || !strings.HasSuffix(stdout, "replace cluster/cluster.nix\n") || !strings.Contains(stderr, "Log file:") {
			t.Fatal(code, stdout, stderr)
		}
		configurationRead(t, root, "cluster/new/module.nix")
		configurationRead(t, root, "cluster/new/config.nix")
		if rename {
			if _, err := os.Stat(filepath.Join(root, "cluster/old")); !os.IsNotExist(err) {
				t.Fatal("failed rediscovery undid the completed rename", err)
			}
		}
		paths, err := filepath.Glob(filepath.Join(root, "cluster/cluster.nix.new-*"))
		if err != nil || len(paths) != 1 || !regexp.MustCompile(`\.new-[0-9a-f]{6}$`).MatchString(paths[0]) || !strings.Contains(configurationRead(t, "/", paths[0]), "./new/module.nix") {
			t.Fatal("failed replacement lost partial file", paths, err)
		}
		logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
		if len(logs) != 1 || !strings.Contains(configurationRead(t, "/", logs[0]), " command_options: {},\n arguments: ") {
			t.Fatal("failure log missing original option/argument adapter", logs)
		}
	}
	root := t.TempDir()
	configurationFile(t, root, "flake.nix", "{}")
	configurationMachine(t, root, "old")
	configurationFile(t, root, "cluster/blocked", "file")
	code, stdout, stderr := configurationCLI(t, root, []string{"rename", "old", "blocked/new"}, "", "022")
	if code != 1 || stdout != "mkdir cluster/blocked\n" || !strings.Contains(stderr, "File exists") || configurationRead(t, root, "cluster/old/module.nix") != "module old\n" {
		t.Fatal("failed parent creation moved the source", code, stdout, stderr)
	}
}

func TestConfigurationRediscoveryInvalidatesWithoutEagerTools(t *testing.T) {
	root := t.TempDir()
	tools, marker := configurationTools(t)
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	configurationFile(t, root, "flake.nix", "{}")
	configurationMachine(t, root, "new")
	origin, err := cliConfigurationOrigin([]string{"rediscover"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Context: context.Background(), Inventory: []Machine{{Name: "stale"}}, SettingsCache: map[string]any{"stale": true}}
	code, err := e.Rediscover(configurationOperation{Origin: origin})
	if err != nil || code != 0 || e.Inventory != nil || e.SettingsCache != nil {
		t.Fatal(code, err, e.Inventory, e.SettingsCache)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "cluster/*.new-*")); len(files) != 0 {
		t.Fatal("successful replacement left temporary files", files)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("rediscovery evaluated inventory without a subscriber", err)
	}
}

func TestConfigurationHelpHasNoEffects(t *testing.T) {
	for _, command := range []string{"init", "add", "rename", "rediscover"} {
		code, out, stderr, _ := actualCLI(t, []string{command, "--help"}, "")
		if code != 0 || stderr != "" || !strings.Contains(out, "NAME\n    "+command) || strings.Contains(out, "Unavailable") {
			t.Fatal(command, code, out, stderr)
		}
	}
}

func TestConfigurationLiteralNamesPreserveSourceBytes(t *testing.T) {
	// Original /leading, double//slash and trailing/ observations retain lexical
	// source names/depth while rediscovery walks the physical directory names.
	for _, tc := range []struct {
		name, printed, discovered, hostname, importPath string
	}{
		{"/leading", "cluster/leading", "leading", "-leading", "../../../environments/base.nix"},
		{"double//slash", "cluster/double//slash", "double/slash", "double--slash", "../../../../environments/base.nix"},
		{"trailing/", "cluster/trailing/", "trailing", "trailing-", "../../../environments/base.nix"},
		{"nested/../peer", "cluster/nested/../peer", "peer", "nested-..-peer", "../../../../environments/base.nix"},
		{"quote\"host", "cluster/quote\"host", "quote\"host", "quote\"host", "../../environments/base.nix"},
	} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		code, stdout, stderr := configurationCLI(t, root, []string{"add", tc.name}, "", "022")
		if code != 0 || !strings.HasPrefix(stdout, "mkdir "+tc.printed+"\n") || stderr != "" {
			t.Fatal(tc, code, stdout, stderr)
		}
		module := configurationRead(t, root, "cluster/"+tc.discovered+"/module.nix")
		config := configurationRead(t, root, "cluster/"+tc.discovered+"/config.nix")
		inventory := configurationRead(t, root, "cluster/cluster.nix")
		if !strings.Contains(module, "cluster.\""+tc.name+"\"") || !strings.Contains(config, "networking.hostName = \""+tc.hostname+"\";") || !strings.Contains(config, tc.importPath) || !strings.Contains(inventory, "./"+tc.discovered+"/module.nix") {
			t.Fatal("source name was cleaned, escaped or import depth changed", tc, module, config, inventory)
		}
	}
}
