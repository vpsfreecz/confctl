package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func inputsUpdateReference(t *testing.T) inputsOriginalReference {
	t.Helper()
	var r inputsOriginalReference
	b, err := os.ReadFile("testdata/inputs_update_reference.json")
	if err != nil || json.Unmarshal(b, &r) != nil || r.SourceRevision != "cc40679d267165aecfa569128438bb55fe910268" || len(r.HelpExtracts) != 3 {
		t.Fatal("invalid update original reference", err)
	}
	return r
}
func updateHelpPath(id string) []string {
	if strings.Contains(id, "channel") {
		return []string{"inputs", "channel", "update"}
	}
	if strings.Contains(id, "machine") {
		return []string{"inputs", "machine", "update"}
	}
	return []string{"inputs", "update"}
}
func TestInputsUpdateHelpOriginalAndNoEffects(t *testing.T) {
	for _, record := range inputsUpdateReference(t).HelpExtracts {
		want, err := os.ReadFile("testdata/" + record.Asset)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(want)) != record.SHA256 {
			t.Fatal(record, err)
		}
		path := updateHelpPath(record.Case)
		for _, argv := range [][]string{append(append([]string{}, path...), "--help", "--unknown"), append([]string{"help"}, path...)} {
			code, out, stderr, _ := actualCLI(t, argv, "")
			if code != 0 || out != string(want) || stderr != "" {
				t.Fatal(argv, code, out, stderr)
			}
		}
	}
}
func TestInputsUpdateArityFlakeAndExactPP(t *testing.T) {
	for _, tc := range []struct {
		id   string
		args []string
	}{
		{"inputs-update-root-missing-name", nil},
		{"inputs-update-channel-extra-argument", []string{"zeta", "os", "extra"}},
		{"inputs-update-machine-extra-argument", []string{"alpha/site", "os", "extra"}},
	} {
		root := t.TempDir()
		argv := append(updateHelpPath(tc.id), tc.args...)
		code, out, stderr := inputsCLI(t, root, argv, nil)
		if code != 1 || out != "" || !strings.Contains(stderr, "has no flake.nix") {
			t.Fatal("flake-before-arity", tc, code, out, stderr)
		}
		root = t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		code, out, stderr = inputsCLI(t, root, argv, nil)
		ref := inputsUpdateReference(t)
		wantHelp, _ := cli.BuiltinRegistry().Help(updateHelpPath(tc.id), 80)
		if code != 64 || out != wantHelp || !strings.HasSuffix(stderr, "error: "+ref.FailureErrors[tc.id]) {
			t.Fatal(tc, code, out, stderr)
		}
		logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
		if len(logs) != 1 {
			t.Fatal(logs)
		}
		wantLog := strings.ReplaceAll(ref.FailureLogs[tc.id], "${PARENT_ADDRESS:1}", "0x0000000000000000")
		if got := configurationRead(t, "/", logs[0]); got != wantLog {
			t.Fatal("PP layout/order", got, wantLog)
		}
		if _, err := os.Stat(filepath.Join(root, "flake.lock")); !os.IsNotExist(err) {
			t.Fatal("arity mutated lock", err)
		}
	}
}
func inputsUpdatedTestLock() string {
	return strings.ReplaceAll(inputsTestLock, "123456789", "abcdef987654321")
}
func TestInputsUpdateRootActualProcessAndRetainedWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		argv   []string
		exit   int
		stderr string
		want   string
	}{
		{"duplicates", []string{"inputs", "update", "a", "a"}, 0, "", "Updated inputs (2):\n  a: 12345678 -> abcdef98\n  a: 12345678 -> abcdef98\n"},
		{"failure", []string{"inputs", "update", "a"}, 73, "refused", ""},
		{"warning", []string{"inputs", "update", "a"}, 0, "unable to download 'https://api.github.com/repos/x': offline; using cached version", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			configurationFile(t, root, "flake.nix", "{}")
			configurationFile(t, root, "flake.lock", inputsTestLock)
			argv := append([]string{"flake", "update"}, tc.argv[2:]...)
			next := inputsUpdatedTestLock()
			rules := []inputsNixRule{{Argv: argv, Exit: tc.exit, Stderr: tc.stderr, LockWrite: &next}}
			code, out, stderr := inputsCLI(t, root, tc.argv, rules)
			wantCode := 0
			if tc.name != "duplicates" {
				wantCode = 1
			}
			if code != wantCode || out != tc.want || code == 0 && stderr != "" {
				t.Fatal(code, out, stderr)
			}
			if configurationRead(t, root, "flake.lock") != next {
				t.Fatal("post-Nix bytes lost")
			}
			logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
			if code != 0 {
				if len(logs) != 1 {
					t.Fatal(logs)
				}
				log := configurationRead(t, "/", logs[0])
				if strings.Contains(log, "cd ") || !strings.Contains(log, "Running nix flake update a") {
					t.Fatal("updater log topology", log)
				}
			} else if len(logs) != 0 {
				t.Fatal(logs)
			}
		})
	}
}

func TestInputsUpdateCompletedExitErrorMainOriginalStderr(t *testing.T) {
	for _, tc := range []struct{ id, sha256 string }{
		{"inputs-update-nonmatching-failure", "233ea1a6821f54b3209698bd872aede86b0cc8316299c43054b8a77d66b11103"},
		{"inputs-update-experimental-second-failure", "a4e705639b5bf0064a1991ce4e49b3b0e8bb91aca2ba9599dd812d0d80209bc8"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			// The repository-root module/package includes these immutable original
			// records. Drive actual Main and finite processes from their requests;
			// do not derive the expected diagnostic from ExitError.Error().
			b, err := os.ReadFile("../../tests/compat/stage3-i2/expected/" + tc.id + ".json")
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(b)) != tc.sha256 {
				t.Fatal("original failed-update reference changed", err)
			}
			var reference struct {
				SourceRevision string `json:"source_revision"`
				Stdout, Stderr string
				Exit           int
				Before, After  map[string]struct{ Text string }
				Events         []struct {
					Tool           string
					Argv           []string
					Stdout, Stderr string
					Exit           int
					Writes         map[string]struct{ Text string }
				}
			}
			if err = json.Unmarshal(b, &reference); err != nil || reference.SourceRevision != "cc40679d267165aecfa569128438bb55fe910268" || reference.Exit != 1 {
				t.Fatal("invalid original failed-update reference", err)
			}
			root := t.TempDir()
			configurationFile(t, root, "flake.nix", reference.Before["flake.nix"].Text)
			configurationFile(t, root, "flake.lock", reference.Before["flake.lock"].Text)
			var rules []inputsNixRule
			for _, event := range reference.Events {
				if event.Tool != "nix" {
					t.Fatal("unexpected original failed-update tool", event.Tool)
				}
				rule := inputsNixRule{Argv: event.Argv, Stdout: event.Stdout, Stderr: event.Stderr, Exit: event.Exit}
				if write, ok := event.Writes["flake.lock"]; ok {
					text := write.Text
					rule.LockWrite = &text
				}
				rules = append(rules, rule)
			}
			code, out, stderr := inputsCLI(t, root, []string{"inputs", "update", "a"}, rules)
			logs, err := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
			if err != nil || len(logs) != 1 {
				t.Fatal("failed update did not retain exactly one log", logs, err)
			}
			info, err := os.Lstat(logs[0])
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				t.Fatal("failed update log is not a retained regular file", err)
			}
			const logPath = "${ROOT}/.confctl/logs/${LOG_TIME:1}-confctl-inputs-update.log"
			if strings.Count(reference.Stderr, logPath) != 1 {
				t.Fatal("original stderr log-path contract changed")
			}
			// Bind only the original observation's explicit log path to this
			// actual retained file; compare every other byte, including newlines.
			wantStderr := strings.Replace(reference.Stderr, logPath, logs[0], 1)
			if code != reference.Exit || out != reference.Stdout || stderr != wantStderr {
				t.Fatalf("failed update display: exit=%d stdout=%q stderr=%q; want exit=%d stdout=%q stderr=%q", code, out, stderr, reference.Exit, reference.Stdout, wantStderr)
			}
			wantLock := reference.After["flake.lock"].Text
			if wantLock == reference.Before["flake.lock"].Text || configurationRead(t, root, "flake.lock") != wantLock || configurationRead(t, root, "flake.nix") != reference.Before["flake.nix"].Text {
				t.Fatal("failed update lost written lock or mutated flake")
			}
			log := configurationRead(t, "/", logs[0])
			for _, rule := range rules {
				if !strings.Contains(log, "Running "+ShellJoin(append([]string{"nix"}, rule.Argv...))+"\n") || !strings.Contains(log, "\t"+rule.Stderr) {
					t.Fatal("failed update log lost actual request/diagnostic", rule.Argv, log)
				}
			}
		})
	}
}

func TestInputsUpdateCommitActualInheritedBoundary(t *testing.T) {
	t.Setenv("CONFCTL_INPUTS_ALLOW_GIT", "1")
	for _, failed := range []bool{false, true} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		configurationFile(t, root, "flake.lock", inputsTestLock)
		next := inputsUpdatedTestLock()
		exit := 0
		if failed {
			exit = 9
		}
		rules := []inputsNixRule{{Argv: []string{"flake", "update", "a"}, LockWrite: &next}, {Argv: []string{"commit", "-m", "inputs: update a to abcdef98\n\na: 12345678 -> abcdef98", "flake.lock"}, Stdout: "commit output\n", Stderr: "commit diagnostic\n", Exit: exit}}
		code, out, stderr := inputsCLI(t, root, []string{"inputs", "update", "--commit", "--no-changelog", "a"}, rules)
		if failed {
			if code != 1 || out != "commit output\n" || !strings.HasSuffix(stderr, "error: git commit failed\n") {
				t.Fatal(code, out, stderr)
			}
			logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
			if len(logs) != 1 {
				t.Fatal(logs)
			}
			log := configurationRead(t, "/", logs[0])
			if strings.Contains(log, "Running git") || strings.Contains(log, "commit output") || strings.Contains(log, "commit diagnostic") {
				t.Fatal("inherited commit was captured/logged", log)
			}
		} else if code != 0 || out != "commit output\nUpdated inputs (1):\n  a: 12345678 -> abcdef98\n" || stderr != "commit diagnostic\n" {
			t.Fatal(code, out, stderr)
		}
		if configurationRead(t, root, "flake.lock") != next {
			t.Fatal("commit failure rolled back Nix")
		}
	}
}
func TestInputsUpdateChannelAndMachineActualOrderedRequests(t *testing.T) {
	for _, machine := range []bool{false, true} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		configurationFile(t, root, "flake.lock", inputsTestLock)
		next := inputsUpdatedTestLock()
		rules := inputsSettingsRules()
		argv := []string{"inputs", "channel", "update", "{z,a,z}"}
		want := "Updating os in z -> abcdef98\nUpdating os in a -> abcdef98\nUpdating os in z -> abcdef98\n"
		if machine {
			argv = []string{"inputs", "machine", "update", "key-alpha", "os"}
			rules = append(rules, inputsNixRule{Argv: inputsEvalArgv(".#confctl.machineKeys", true, "7"), Stdout: `{"alpha/site":"key-alpha"}`}, inputsNixRule{Argv: inputsEvalArgv(".#confctl.inputsInfo.key-alpha", true, "7"), Stdout: `{"os":{"input":"a"}}`})
			want = "Updating os in key-alpha -> abcdef98\n"
		} else {
			rules = append(rules, inputsNixRule{Argv: inputsEvalArgv(".#confctl.channels", true, "7"), Stdout: `{"a":{"os":"a"},"z":{"os":"a"}}`})
		}
		rules = append(rules, inputsNixRule{Argv: []string{"flake", "update", "a"}, LockWrite: &next})
		code, out, stderr := inputsCLI(t, root, argv, rules)
		if code != 0 || out != want || stderr != "" {
			t.Fatal(argv, code, out, stderr)
		}
	}
}

func TestInputsUpdateLogParsedAliasDefaultsOriginal(t *testing.T) {
	var reference struct {
		PP []struct {
			Case         string
			Argv         []string
			Text, SHA256 string
		} `json:"pp_extracts"`
	}
	b, err := os.ReadFile("testdata/inputs_update_reference.json")
	if err != nil || json.Unmarshal(b, &reference) != nil || len(reference.PP) != 5 {
		t.Fatal(err)
	}
	for _, record := range reference.PP {
		if fmt.Sprintf("%x", sha256.Sum256([]byte(record.Text))) != record.SHA256 {
			t.Fatal("PP extract changed", record.Case)
		}
		inv, err := cli.BuiltinRegistry().Parse(record.Argv)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(t.TempDir(), "log"))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Log: f, Color: inv.Globals["color"].Value.String}
		e.LogInputUpdateCLI(inv)
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(f.Name())
		want := strings.ReplaceAll(record.Text, "${PARENT_ADDRESS:1}", "0x0000000000000000")
		if err != nil || string(got) != want {
			t.Fatal(record.Case, string(got), want, err)
		}
	}
}

func TestInputsUpdateAllAndMetadataOnlyMain(t *testing.T) {
	for _, all := range []bool{false, true} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		configurationFile(t, root, "flake.lock", inputsTestLock)
		next := strings.ReplaceAll(inputsTestLock, "https://repo", "https://metadata-only")
		args := []string{"inputs", "update", "--commit", "a"}
		request := []string{"flake", "update", "a"}
		if all {
			args = []string{"inputs", "update", "--all", "--commit", "ignored"}
			request = []string{"flake", "update", ".hidden", "a", "slash/name", "z"}
		}
		code, out, stderr := inputsCLI(t, root, args, []inputsNixRule{{Argv: request, LockWrite: &next}})
		if code != 0 || out != "No changes.\n" || stderr != "" || configurationRead(t, root, "flake.lock") != next {
			t.Fatal(args, code, out, stderr)
		}
	}
	for _, lock := range []*string{nil, inputsStringPtr(`{"nodes":{"root":{"inputs":{}}}}`), inputsStringPtr("bad json")} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		if lock != nil {
			configurationFile(t, root, "flake.lock", *lock)
		}
		code, out, stderr := inputsCLI(t, root, []string{"inputs", "update", "--all", "ignored"}, nil)
		if code != 1 || out != "" || !strings.Contains(stderr, "error:") {
			t.Fatal(code, out, stderr)
		}
	}
}

func TestInputsMirrorCacheRubyJoinSpellingWithoutCreation(t *testing.T) {
	// Actual pinned Ruby stdlib receipt: stage3-i2-ruby-cache-join.json. These tests
	// inspect only strings, especially the empty XDG /confctl case.
	for _, tc := range []struct{ base, want string }{{"", "/confctl"}, {"/", "/confctl"}, {"relative//cache/", "relative//cache/confctl"}} {
		t.Setenv("XDG_CACHE_HOME", tc.base)
		got, err := inputMirrorCache()
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	t.Setenv("HOME", "/private//home")
	t.Setenv("XDG_CACHE_HOME", "unused")
	if err := os.Unsetenv("XDG_CACHE_HOME"); err != nil {
		t.Fatal(err)
	}
	got, err := inputMirrorCache()
	if err != nil || got != "/private//home/.cache/confctl" {
		t.Fatal(got, err)
	}
}

func TestInputsUpdateLogExplicitEncounterOrderAndAliasGrouping(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		rows  []string
	}{
		{[]string{"--all", "--no-editor", "--downgrade", "--commit", "--no-all", "--no-downgrade", "--no-commit"}, []string{`"all" => false`, `"editor" => false`, `"d" => false`, `"downgrade" => false`, `"commit" => false`, `"changelog" => true`}},
		{[]string{"--no-changelog", "-d", "--all", "--editor", "--no-downgrade", "--changelog"}, []string{`"changelog" => true`, `"d" => false`, `"downgrade" => false`, `"all" => true`, `"editor" => true`, `"commit" => false`}},
		{[]string{"--commit", "--no-commit", "--editor", "-d", "--no-downgrade"}, []string{`"commit" => false`, `"editor" => true`, `"d" => false`, `"downgrade" => false`, `"changelog" => true`, `"all" => false`}},
	} {
		inv, err := cli.BuiltinRegistry().Parse(append(append([]string{"inputs", "update"}, tc.flags...), "a"))
		if err != nil {
			t.Fatal(tc.flags, err)
		}
		declarationOrder := make([]string, len(inv.Command.Options))
		for i, spec := range inv.Command.Options {
			declarationOrder[i] = spec.Key
		}
		f, err := os.Create(filepath.Join(t.TempDir(), "log"))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Log: f, Color: inv.Globals["color"].Value.String}
		e.LogInputUpdateCLI(inv)
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		text, previous := string(b), -1
		for _, row := range tc.rows {
			index := strings.Index(text, row)
			if strings.Count(text, row) != 1 || index <= previous {
				t.Fatal("PP insertion/final value changed", tc.flags, row, text)
			}
			previous = index
		}
		if !strings.Contains(text, "\"d\" => false,\n   \"downgrade\" => false") || strings.Index(text, "#<GLI::Command::ParentKey:") <= previous {
			t.Fatal("alias group or parent placement changed", text)
		}
		for i, key := range declarationOrder {
			if inv.Command.Options[i].Key != key {
				t.Fatal("log reordered invocation metadata", inv.Command.Options)
			}
		}
	}
}
