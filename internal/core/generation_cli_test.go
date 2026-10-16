package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func generationCLI(t *testing.T, argv []string, setup func(string), sshBody string) connectionCLIResult {
	t.Helper()
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	nix := `printf '%s\n' "nix ${@: -1}" >> "$TRACE"
case "${@: -1}" in
 '.#confctl.settings') printf '{"list":{"columns":["name"]},"nix":{"maxJobs":"7"}}' ;;
 '.#confctl.machinesJson') printf '[{"outputs":{"out":"%s/machines.json"}}]' "$PWD" ;;
 '.#confctl.machineKeys') printf '{"z":"z-key","a":"a-key"}' ;;
 '.#confctl.inputsInfo.'*) printf '{"role":"abcdefghijk"}' ;;
 *) exit 99 ;;
esac
`
	ssh := `remote=${@: -1}
printf '%s\n' "$remote" >> "$TRACE"
printf '%s\0' "$@" >> "$ARGS"
if [[ "$remote" == 'bash --norc' ]]; then
 while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line" >> "$PROFILE_SCRIPT"; done
fi
` + sshBody
	for name, body := range map[string]string{"nix": nix, "ssh": ssh} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!"+bash+"\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	// Unmanaged targetless z remains included locally; carried a connects through
	// carrier although that carrier is excluded by the selected pattern.
	inventory := `{"z":{"name":"z","key":"z-key","metaConfig":{"managed":false,"spin":"nixos","host":{"target":null},"tags":["fixture"]}},"a":{"name":"a","key":"a-key","carrier":"carrier","alias":"site/../logical","metaConfig":{"managed":true,"spin":"nixos","tags":["fixture"],"carrier":{"name":"site/../logical"}}},"carrier":{"name":"carrier","key":"c","metaConfig":{"managed":true,"spin":"nixos","host":{"target":"carrier.invalid","port":2222},"tags":["carrier"]}}}`
	if err := os.WriteFile(filepath.Join(root, "machines.json"), []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(root)
	}
	before := testConfigurationState(t, filepath.Join(root, ".confctl", "generations"))
	b, _ := json.Marshal(argv)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + tools, "HOME=" + root, "TMPDIR=" + root, "LANG=C.UTF-8", "NO_COLOR=1", "PAGER=", "TZ=UTC", "CONFCTL_SSH_CONFIG=literal config", "CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV=" + string(b), "TRACE=" + filepath.Join(root, "trace"), "ARGS=" + filepath.Join(root, "args"), "PROFILE_SCRIPT=" + filepath.Join(root, "script")}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if !reflect.DeepEqual(before, testConfigurationState(t, filepath.Join(root, ".confctl", "generations"))) {
		t.Fatal("CLI mutated generation records")
	}
	logs, err := filepath.Glob(filepath.Join(root, ".confctl", "logs", "*.log"))
	if err != nil || len(logs) > 1 {
		t.Fatal(logs, err)
	}
	log := ""
	if len(logs) == 1 {
		log = fixtureFile(t, filepath.Dir(logs[0]), filepath.Base(logs[0]))
	}
	return connectionCLIResult{code: code, out: out.String(), stderr: stderr.String(), trace: fixtureFile(t, root, "trace"), args: fixtureFile(t, root, "args"), log: log, root: root}
}

func generationCLISetup(t *testing.T) func(string) {
	return func(root string) {
		seedCoreGeneration(t, root, "z", "zzz", coreGenerationRecord(root+"/z", "2020-01-01T00:00:00Z", `{"zrole":"zzzzzzzzzz"}`))
		seedCoreGeneration(t, root, "a", "aaa", coreGenerationRecord(root+"/a", "2000-01-01T00:00:00Z", `{"arole":{"rev":"aaaaaaaaaa","extra":9007199254740993}}`))
	}
}

func TestGenerationListingActualCLIScopesColumnsAndCarriedQuery(t *testing.T) {
	for _, args := range [][]string{{"generation", "ls", "{z,a}"}, {"generation", "ls", "-l", "{z,a}"}, {"generation", "ls", "--remote", "a"}, {"generation", "ls", "-lr", "a"}} {
		r := generationCLI(t, args, generationCLISetup(t), `case "$remote" in
 'cat /proc/uptime') printf '600 0\n' ;;
 'bash --norc') printf '%s/a\n/nix/var/nix/profiles/confctl-site:..:logical-999999999999999999999-link;%s/a;0;/nix/store/hash-linux-remote/bzImage\n' "$PWD" "$PWD" ;;
 *) exit 99 ;;
esac
`)
		remote := strings.Contains(strings.Join(args, " "), "remote") || strings.Contains(strings.Join(args, " "), "-lr")
		if r.code != 0 || r.stderr != "" || r.log != "" {
			t.Fatal(args, r)
		}
		if remote {
			if !strings.HasSuffix(r.trace, "cat /proc/uptime\nbash --norc\n") || strings.Contains(r.trace, "inputs-info") || strings.Contains(r.trace, "realpath") || !strings.Contains(r.args, "carrier.invalid\x00") || !strings.Contains(r.args, "2222\x00") {
				t.Fatal(r)
			}
			if strings.Contains(strings.Join(args, " "), "-lr") && (!strings.Contains(r.out, "build+host") || !strings.Contains(r.out, "aaa") || !strings.Contains(r.out, "remote")) {
				t.Fatal(r)
			}
		} else {
			if !strings.HasSuffix(r.trace, "nix .#confctl.machinesJson\n") || r.args != "" || !strings.Contains(r.out, "aaa") || !strings.Contains(r.out, "zzz") {
				t.Fatal(r)
			}
			fields := strings.Fields(strings.Split(r.out, "\n")[0])
			if !reflect.DeepEqual(fields[6:], []string{"ZROLE", "AROLE"}) {
				t.Fatal(r.out)
			}
		}
	}
}

func TestGenerationListingActualCLIFailureWarningsAndAvailability(t *testing.T) {
	for _, tc := range []struct {
		argv             []string
		setup            func(string)
		body, diagnostic string
		code             int
	}{
		{[]string{"generation", "ls", "-lr", "a", "0"}, func(root string) {
			generationCLISetup(t)(root)
			seedCoreGeneration(t, root, "a", "legacy", `{"mode":"swpins"}`)
		}, "case \"$remote\" in 'cat /proc/uptime') printf '600 0\\n';; 'bash --norc') printf '';; esac\n", "ambiguous", 1},
		{[]string{"generation", "ls", "-l", "a", "old"}, func(root string) {
			generationCLISetup(t)(root)
			if err := os.Symlink("absent", filepath.Join(root, ".confctl", "generations", "a", "current")); err != nil {
				t.Fatal(err)
			}
		}, "exit 99\n", "Cannot select current generation for a", 1},
		{[]string{"generation", "ls", "-r", "z"}, generationCLISetup(t), "exit 99\n", "machine z has no target", 1},
		{[]string{"generation", "ls", "-a", "invalid"}, generationCLISetup(t), "exit 99\n", "Invalid filter ''", 64},
	} {
		r := generationCLI(t, tc.argv, tc.setup, tc.body)
		if r.code != tc.code || !strings.Contains(r.stderr, tc.diagnostic) || !strings.Contains(r.stderr, "Log file:") || r.log == "" || !strings.Contains(r.log, `command: ["generation", "ls"]`) || !strings.Contains(r.log, "#<GLI::Command::ParentKey:") {
			t.Fatal(tc.argv, r)
		}
		if strings.Contains(tc.diagnostic, "ambiguous") && (!strings.Contains(r.stderr, "Skipping generation") || !strings.Contains(r.log, "Skipping generation") || !strings.Contains(r.trace, "bash --norc")) {
			t.Fatal(r)
		}
	}
	for _, argv := range [][]string{{"generation", "ls", "--help"}, {"help", "generation", "ls"}, {"generation", "--help"}, {"generation", "ls", "--help", "--bad"}} {
		r := generationCLI(t, argv, generationCLISetup(t), "exit 99\n")
		if r.code != 0 || r.stderr != "" || r.trace != "" || r.log != "" || !strings.Contains(r.out, "generation") || len(argv) > 2 && strings.Contains(r.out, "Unavailable in") {
			t.Fatal(r)
		}
	}
	for _, argv := range [][]string{{"generation", "rm"}, {"generation", "rotate"}} {
		r := generationCLI(t, argv, generationCLISetup(t), "exit 99\n")
		if r.code != 2 || r.out != "" || r.trace != "" || r.log != "" || !strings.Contains(r.stderr, "unavailable") {
			t.Fatal(r)
		}
	}
	// Ordinary remote failures are handled empty listings; successful empty
	// profile output remains a present empty list in the private query tests.
	r := generationCLI(t, []string{"generation", "ls", "-r", "a"}, generationCLISetup(t), "exit 255\n")
	if r.code != 0 || r.stderr != "" || r.log != "" || len(strings.Split(strings.TrimSuffix(r.out, "\n"), "\n")) != 1 {
		t.Fatal(r)
	}
}

func TestGenerationListingLogOptionValues(t *testing.T) {
	root := t.TempDir()
	log, err := os.Create(filepath.Join(root, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	e := &Engine{Context: context.Background(), Log: log, logOwner: nativeEngineLog(log), Color: "never"}
	if err := e.LogGenerationCLI(generationInvocation(t, "-r", "-a", "managed=false", "-a", "spin=nixos", "-t", "fixture", "-l", "--no-remote", "node", "")); err != nil {
		t.Fatal(err)
	}
	text := fixtureFile(t, root, "log")
	for _, part := range []string{`"r" => false`, `"remote" => false`, `"attr" => ["managed=false", "spin=nixos"]`, `"tag" => ["fixture"]`, `"local" => true`, `arguments: ["node", ""]`} {
		if !strings.Contains(text, part) {
			t.Fatal(part, text)
		}
	}
	if strings.Index(text, `"r"`) > strings.Index(text, `"a"`) || strings.Index(text, `"a"`) > strings.Index(text, `"l"`) {
		t.Fatal("option encounter order changed", text)
	}
}

func TestGenerationStatusNoneActualCLI(t *testing.T) {
	r := generationCLI(t, []string{"status", "-gnone", "-y", "a"}, generationCLISetup(t), `case "$remote" in
 'cat /proc/uptime') printf '600 0\n' ;;
 'bash --norc') printf '%s/a\n/nix/var/nix/profiles/confctl-site:..:logical-1-link;%s/a;0\n' "$PWD" "$PWD" ;;
 'cat /nix/var/nix/profiles/confctl-site:..:logical/machine.json') printf '{"inputs-info":{"role":"abcdefghijk"}}' ;;
 *) exit 99 ;;
esac
`)
	if r.code != 0 || r.stderr != "" || r.log != "" || !strings.Contains(r.out, "1:1") || !strings.Contains(r.out, "ok") || strings.Contains(r.trace, "realpath") || !strings.Contains(r.trace, "inputsInfo.a-key") || !strings.HasSuffix(r.trace, "cat /proc/uptime\nbash --norc\ncat /nix/var/nix/profiles/confctl-site:..:logical/machine.json\n") {
		t.Fatal(r)
	}
}
