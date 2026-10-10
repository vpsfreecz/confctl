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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Source-derived I1 inputs, kept separate from the immutable compatibility
// observations. Original characterization is a lead-owned acceptance gate.
const inputsTestLock = `{"root":"other","nodes":{"root":{"inputs":{"z":"absent","a":"node",".hidden":"node","slash/name":"node"}},"other":{"inputs":{"wrong":"node"}},"node":{"locked":{"type":"git","rev":"123456789","url":"git+https://repo"},"original":{"ref":"main"}}}}`
const inputsSettings = `{"nix":{"maxJobs":"7","impureEval":true},"list":{"columns":["name"]}}`

type inputsOriginalReference struct {
	SourceRevision string `json:"source_revision"`
	HelpExtracts   []struct {
		Case, Asset, SHA256 string
		ExecutableSHA256    string `json:"executable_sha256"`
	} `json:"help_extracts"`
	FailureLogs   map[string]string `json:"failure_logs"`
	FailureErrors map[string]string `json:"failure_errors"`
}

func inputsReference(t *testing.T) inputsOriginalReference {
	t.Helper()
	var r inputsOriginalReference
	b, err := os.ReadFile("testdata/inputs_reference.json")
	if err != nil || json.Unmarshal(b, &r) != nil || r.SourceRevision != "cc40679d267165aecfa569128438bb55fe910268" || len(r.HelpExtracts) != 2 {
		t.Fatal("invalid original inputs reference", err)
	}
	return r
}

func TestInputsActualHelpReferenceAndNoEffects(t *testing.T) {
	for _, record := range inputsReference(t).HelpExtracts {
		want, err := os.ReadFile("testdata/" + record.Asset)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(want)) != record.SHA256 || record.ExecutableSHA256 != "eae4297146b9790b87f2e9935f3ea516ef5e1d047b683b64f9a57cf33c3c85de" {
			t.Fatal("original help bytes/identity changed", record, err)
		}
		path := []string{"inputs", "ls"}
		if record.Case == "inputs-channel-help" {
			path = []string{"inputs", "channel", "ls"}
		}
		for _, argv := range [][]string{append(append([]string{}, path...), "--help", "--unknown"), append([]string{"help"}, path...)} {
			code, stdout, stderr, _ := actualCLI(t, argv, "")
			if code != 0 || stdout != string(want) || stderr != "" {
				t.Fatal(argv, code, stdout, stderr)
			}
		}
	}
}

type inputsNixRule struct {
	Argv           []string
	Stdout, Stderr string
	Exit           int
}

// Every request crosses the actual external process boundary. The fixture
// rejects a different argv or unexpected request instead of replaying a row in
// place of the evaluator. Call records live outside configuration state.
func TestInputsNixHelper(t *testing.T) {
	if os.Getenv("CONFCTL_INPUTS_NIX_HELPER") != "1" {
		return
	}
	var argv []string
	for i, arg := range os.Args {
		if arg == "--" {
			argv = os.Args[i+1:]
			break
		}
	}
	path := os.Getenv("CONFCTL_INPUTS_CALLS")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	var calls [][]string
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var call []string
		if err = json.Unmarshal(line, &call); err != nil {
			panic(err)
		}
		calls = append(calls, call)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	err = json.NewEncoder(f).Encode(argv)
	_ = f.Close()
	if err != nil {
		panic(err)
	}
	var rules []inputsNixRule
	if err = json.Unmarshal([]byte(os.Getenv("CONFCTL_INPUTS_RULES")), &rules); err != nil {
		panic(err)
	}
	index := len(calls)
	if index >= len(rules) || !reflect.DeepEqual(argv, rules[index].Argv) {
		fmt.Fprintf(os.Stderr, "unexpected inputs fixture request #%d: %q\n", index, argv)
		os.Exit(97)
	}
	r := rules[index]
	fmt.Print(r.Stdout)
	fmt.Fprint(os.Stderr, r.Stderr)
	os.Exit(r.Exit)
}

func inputsEvalArgv(installable string, impure bool, max string) []string {
	a := []string{"eval", "--json"}
	if impure {
		a = append(a, "--impure")
	}
	return append(a, "--no-write-lock-file", "--no-update-lock-file", "--option", "max-jobs", max, installable)
}

func inputsSettingsRules() []inputsNixRule {
	return []inputsNixRule{
		{Argv: inputsEvalArgv(".#confctl.settings", false, "auto"), Stdout: inputsSettings},
		{Argv: inputsEvalArgv(".#confctl.settings", false, "7"), Stdout: inputsSettings},
	}
}

func inputsTools(t *testing.T, rules []inputsNixRule) (string, string) {
	t.Helper()
	tools, marker := configurationTools(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!" + sh + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestInputsNixHelper$", "--"}) + " \"$@\"\n"
	if err = os.WriteFile(filepath.Join(tools, "nix"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(tools, "calls.jsonl")
	b, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFCTL_INPUTS_NIX_HELPER", "1")
	t.Setenv("CONFCTL_INPUTS_RULES", string(b))
	t.Setenv("CONFCTL_INPUTS_CALLS", calls)
	t.Setenv("CONFCTL_INPUTS_OTHER_TOOL", marker)
	return tools, calls
}

func inputsCalls(t *testing.T, path string, rules []inputsNixRule) {
	t.Helper()
	if b, err := os.ReadFile(os.Getenv("CONFCTL_INPUTS_OTHER_TOOL")); !os.IsNotExist(err) {
		t.Fatal("inputs command used another tool", string(b), err)
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && len(rules) == 0 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		var argv []string
		if err = json.Unmarshal(line, &argv); err != nil {
			t.Fatal(err)
		}
		got = append(got, argv)
	}
	want := [][]string{}
	for _, r := range rules {
		want = append(want, r.Argv)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("inputs Nix chronology/argv changed", got, want)
	}
}

func inputsCLI(t *testing.T, root string, argv []string, rules []inputsNixRule) (int, string, string) {
	t.Helper()
	tools, calls := inputsTools(t, rules)
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	// Test containment only, not a production command deadline. Race-instrumented
	// serial process fixtures have their ordinary shutdown work within this budget.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
	cmd.Dir = root
	env := os.Environ()
	replaceAuthority(&env, "CONFCTL_EXTENSION_ROOT", nil)
	replaceAuthority(&env, "CONFCTL_EXTENSION_REGISTRY", nil)
	cmd.Env = append(env, "CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV="+string(b), "PATH="+tools+":"+os.Getenv("PATH"), "PWD="+root)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("inputs test process deadline", ctx.Err(), stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	inputsCalls(t, calls, rules)
	return code, stdout.String(), stderr.String()
}

func TestInputsRootMainFilesystemAndPattern(t *testing.T) {
	for _, argv := range [][]string{{"inputs", "ls", "a"}, {"inputs", "ls", "{a}", "ignored", "--help"}} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		configurationFile(t, root, "flake.lock", inputsTestLock)
		code, stdout, stderr := inputsCLI(t, root, argv, nil)
		want := "INPUT   TYPE   REF    REV        URL\na       git    main   12345678   https://repo\n"
		if code != 0 || stdout != want || stderr != "" {
			t.Fatal(argv, code, stdout, stderr)
		}
		if configurationRead(t, root, "flake.lock") != inputsTestLock || configurationRead(t, root, "flake.nix") != "{}" {
			t.Fatal("root reader mutated configuration")
		}
		if paths, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log")); len(paths) != 0 {
			t.Fatal("successful inputs log retained", paths)
		}
	}
	for _, tc := range []struct{ lock, pattern, want string }{
		{`{"nodes":{"root":{"inputs":{}}}}`, "*", "INPUT   TYPE   REF   REV   URL\n"},
		{inputsTestLock, ".hidden", "INPUT     TYPE   REF    REV        URL\n.hidden   git    main   12345678   https://repo\n"},
		{inputsTestLock, "slash/*", "INPUT        TYPE   REF    REV        URL\nslash/name   git    main   12345678   https://repo\n"},
	} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		configurationFile(t, root, "flake.lock", tc.lock)
		code, stdout, stderr := inputsCLI(t, root, []string{"inputs", "ls", tc.pattern}, nil)
		if code != 0 || stdout != tc.want || stderr != "" {
			t.Fatal(tc, code, stdout, stderr)
		}
	}
}

func TestInputsChannelMainOrderingAndNonobject(t *testing.T) {
	for _, tc := range []struct {
		selector []string
		channels string
		rows     []string
	}{
		{nil, `{"z":{"os":"a","tool":"z"},"a":{"tool":"z","os":"a"}}`, []string{"a", "tool", "z", "a", "os", "a", "z", "os", "a", "z", "tool", "z"}},
		{[]string{"{z,a,z}"}, `{"z":{"os":"a","tool":"z"},"a":{"tool":"z","os":"a"}}`, []string{"z", "os", "a", "z", "tool", "z", "a", "tool", "z", "a", "os", "a", "z", "os", "a", "z", "tool", "z"}},
		{[]string{"{*}"}, `{"z":{"os":"a"}}`, nil},
		{nil, `[]`, nil},
		{nil, `{"empty":null}`, nil},
	} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		configurationFile(t, root, "flake.lock", inputsTestLock)
		rules := append(inputsSettingsRules(), inputsNixRule{Argv: inputsEvalArgv(".#confctl.channels", true, "7"), Stdout: tc.channels})
		argv := append([]string{"inputs", "channel", "ls"}, tc.selector...)
		code, stdout, stderr := inputsCLI(t, root, argv, rules)
		if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "CHANNEL   ROLE   INPUT   REV") {
			t.Fatal(argv, code, stdout, stderr)
		}
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")[1:]
		var got []string
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) != 5 {
				t.Fatal("missing row data", line)
			}
			got = append(got, fields[:3]...)
			if fields[2] == "z" && (fields[3] != "-" || fields[4] != "-") || fields[2] == "a" && (fields[3] != "12345678" || fields[4] != "https://repo") {
				t.Fatal("lock display differs", fields)
			}
		}
		if !reflect.DeepEqual(got, tc.rows) || configurationRead(t, root, "flake.lock") != inputsTestLock {
			t.Fatal("channel rows/order/state changed", got, tc.rows)
		}
	}
}

func TestInputsFlakeBeforeArityAndRetainedFailureLog(t *testing.T) {
	root := t.TempDir()
	argv := []string{"inputs", "channel", "ls", "alpha", "extra"}
	code, stdout, stderr := inputsCLI(t, root, argv, nil)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "has no flake.nix") || strings.Contains(stderr, "usage:") {
		t.Fatal("flake did not win", code, stdout, stderr)
	}
	root = t.TempDir()
	configurationFile(t, root, "flake.nix", "{}")
	code, stdout, stderr = inputsCLI(t, root, argv, nil)
	help, _ := cli.BuiltinRegistry().Help([]string{"inputs", "channel", "ls"}, 80)
	if code != 64 || stdout != help || !strings.HasSuffix(stderr, "error: usage: confctl inputs channel ls [channel-pattern]\n\n") {
		t.Fatal(code, stdout, stderr)
	}
	paths, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*-confctl-inputs-channel-ls.log"))
	if len(paths) != 1 {
		t.Fatal("failed command lost full-path log", paths)
	}
	log := configurationRead(t, "/", paths[0])
	wantLog := strings.ReplaceAll(inputsReference(t).FailureLogs["inputs-channel-extra"], "${PARENT_ADDRESS:1}", "0x0000000000000000")
	if log != wantLog || strings.Contains(log, "Running ") {
		t.Fatal("failed input log lost context or started Nix", log)
	}
	if _, err := os.Stat(filepath.Join(root, "flake.lock")); !os.IsNotExist(err) {
		t.Fatal("arity read/created a lock", err)
	}
}

func TestInputsMissingCorruptLockAndNoNix(t *testing.T) {
	for _, content := range []*string{nil, inputsStringPtr("bad json")} {
		root := t.TempDir()
		configurationFile(t, root, "flake.nix", "{}")
		if content != nil {
			configurationFile(t, root, "flake.lock", *content)
		}
		code, stdout, stderr := inputsCLI(t, root, []string{"inputs", "ls"}, nil)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "Log file:") || !strings.Contains(stderr, "error:") {
			t.Fatal(code, stdout, stderr)
		}
		id := "inputs-root-missing-lock"
		if content != nil {
			id = "inputs-root-corrupt-lock"
		}
		reference := inputsReference(t)
		wantError := strings.ReplaceAll(reference.FailureErrors[id], "${ROOT}", root)
		if !strings.HasSuffix(stderr, "error: "+wantError) {
			t.Fatal("original error bytes differ", id, stderr, wantError)
		}
		paths, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*-confctl-inputs-ls.log"))
		if len(paths) != 1 || configurationRead(t, "/", paths[0]) != strings.ReplaceAll(reference.FailureLogs[id], "${PARENT_ADDRESS:1}", "0x0000000000000000") {
			t.Fatal("original root PP failure context differs", paths)
		}
		if content != nil && configurationRead(t, root, "flake.lock") != *content {
			t.Fatal("corrupt lock replaced")
		}
	}
}

func inputsStringPtr(s string) *string { return &s }

func TestInputsMachineResolverNixAdapterAndFallback(t *testing.T) {
	root := t.TempDir()
	configurationFile(t, root, "flake.nix", "{}")
	rules := inputsSettingsRules()
	keys := inputsEvalArgv(".#confctl.machineKeys", true, "7")
	rules = append(rules,
		inputsNixRule{Argv: keys, Stderr: "unknown option --no-update-lock-file", Exit: 1},
		inputsNixRule{Argv: []string{"eval", "--json", "--impure", "--no-write-lock-file", "--option", "max-jobs", "7", ".#confctl.machineKeys"}, Stdout: `{"alpha/site":"key-alpha"}`},
		inputsNixRule{Argv: inputsEvalArgv(".#confctl.inputsInfo.key-alpha", true, "7"), Stdout: `{"os":{"input":"nixpkgs"}}`},
		inputsNixRule{Argv: inputsEvalArgv(".#confctl.inputsInfo.key-alpha", true, "7"), Stdout: `{"os":{"input":"nixpkgs"}}`},
	)
	tools, calls := inputsTools(t, rules)
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &Engine{Root: root, Context: ctx}
	r, err := e.inputResolver()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha/site", "key-alpha"} {
		if input, err := r.Resolve(name, "os"); err != nil || input != "nixpkgs" {
			t.Fatal(input, err)
		}
	}
	if _, err = r.Resolve("alpha*", "os"); err == nil || err.Error() != `Unknown machine "alpha*"` {
		t.Fatal(err)
	}
	inputsCalls(t, calls, rules)
}

func TestInputsHelpAndMutatorsRemainNoEffect(t *testing.T) {
	for _, path := range [][]string{{"inputs", "ls"}, {"inputs", "channel", "ls"}} {
		code, stdout, stderr, _ := actualCLI(t, append(append([]string{}, path...), "--help"), "")
		if code != 0 || stderr != "" || !strings.Contains(stdout, "NAME\n    ls - ") || strings.Contains(stdout, "Unavailable") {
			t.Fatal(path, code, stdout, stderr)
		}
	}
	for _, leaf := range cli.BuiltinRegistry().Leaves() {
		if leaf.Path[0] == "inputs" && leaf.Handler != cli.InputsReadHandler {
			code, stdout, stderr, _ := actualCLI(t, append(append([]string{}, leaf.Path...), "a", "os", "rev"), "")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "unavailable") {
				t.Fatal(leaf.Path, code, stdout, stderr)
			}
		}
	}
}
