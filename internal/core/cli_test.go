package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func TestCLIStage1Subprocess(t *testing.T) {
	if os.Getenv("CONFCTL_STAGE1_SUBPROCESS") != "1" {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("CONFCTL_STAGE1_ARGV")), &argv); err != nil {
		panic(err)
	}
	os.Exit(Main(context.Background(), argv))
}

type connectionCLIResult struct {
	code                                int
	out, stderr, trace, args, log, root string
}

func connectionCLI(t *testing.T, argv []string, input string, ms []Machine, toolBody string, catMode os.FileMode, flake bool, config string) connectionCLIResult {
	t.Helper()
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if flake {
		if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{ outputs = _: {}; }\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var inventory strings.Builder
	inventory.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			inventory.WriteByte(',')
		}
		key, _ := json.Marshal(m.Name)
		record, _ := json.Marshal(map[string]any{"name": m.Name, "key": m.Key, "carrier": m.CarrierName, "metaConfig": m.Attributes})
		fmt.Fprintf(&inventory, "%s:%s", key, record)
	}
	inventory.WriteByte('}')
	if err := os.WriteFile(filepath.Join(root, "machines.json"), []byte(inventory.String()), 0600); err != nil {
		t.Fatal(err)
	}
	bash, bashErr := exec.LookPath("bash")
	if bashErr != nil {
		t.Fatal("declared Bash required for confined connection tests", bashErr)
	}
	bash, bashErr = filepath.Abs(bash)
	if bashErr != nil {
		t.Fatal(bashErr)
	}
	nix := `case "$*" in
  'eval --json --no-write-lock-file --no-update-lock-file --option max-jobs auto .#confctl.settings') label=settings-auto ;;
  'eval --json --no-write-lock-file --no-update-lock-file --option max-jobs 7 .#confctl.settings') label=settings-seven ;;
  'build --json --no-link --impure --no-write-lock-file --no-update-lock-file --option max-jobs 7 .#confctl.machinesJson') label=machines ;;
  *) printf 'unexpected Nix argv: %s\n' "$*" >&2; exit 98 ;;
esac
printf '%s\n' "$label" >> "$TRACE"
if [[ "$label" == machines ]]; then
  printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$PWD"
else
  printf '%s\n' '{"list":{"columns":["name","spin","managed","host.target"]},"nix":{"maxJobs":"7","impureEval":true,"legacyNixPath":false}}'
fi
`
	if err := os.WriteFile(filepath.Join(tools, "nix"), []byte("#!"+bash+"\n"+nix), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ssh", "cat", "nix-shell", "getent"} {
		if name == "cat" && catMode == 0 {
			continue
		}
		body := "printf '%s\\n' " + name + " >> \"$TRACE\"\nprintf '%s\\0' \"$@\" >> \"$ARGS\"\n" + toolBody
		mode := os.FileMode(0700)
		if name == "cat" {
			mode = catMode
		}
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!"+bash+"\n"+body), mode); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(argv)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + tools, "HOME=" + root, "TMPDIR=" + root, "LANG=C.UTF-8", "NO_COLOR=1", "PAGER=", "CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV=" + string(b), "TRACE=" + filepath.Join(root, "trace"), "ARGS=" + filepath.Join(root, "args"), "CONFCTL_SSH_CONFIG=" + config}
	cmd.Stdin = strings.NewReader(input)
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	trace, _ := os.ReadFile(filepath.Join(root, "trace"))
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	logs, err := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
	if err != nil || len(logs) > 1 {
		t.Fatal(logs, err)
	}
	log := ""
	if len(logs) == 1 {
		b, err := os.ReadFile(logs[0])
		if err != nil {
			t.Fatal(err)
		}
		log = string(b)
	}
	return connectionCLIResult{code, out.String(), stderr.String(), string(trace), string(args), log, root}
}

func TestTestConnectionMainHelpAndExecution(t *testing.T) {
	// Complete help bytes from accepted help-long/short/command original records.
	const help = "NAME\n    test-connection - Test SSH connection\n\nSYNOPSIS\n\n    confctl [global options] test-connection [command options] [machine-pattern]\n\nCOMMAND OPTIONS\n    -a, --attr=arg - Filter by attribute (may be used more than once, default:\n                     none)\n    --managed=arg  - Filter (un)managed machines (default: none)\n    -t, --tag=arg  - Filter by tag (may be used more than once, default: none)\n    -y, --[no-]yes - Assume the answer to confirmations is yes\n\n"
	for _, argv := range [][]string{{"test-connection", "--help"}, {"test-connection", "-h"}, {"help", "test-connection"}} {
		r := connectionCLI(t, argv, "", nil, "exit 99\n", 0, false, "")
		if r.code != 0 || r.out != help || r.stderr != "" || r.trace != "" || r.log != "" {
			t.Fatal(r)
		}
	}
	r := connectionCLI(t, []string{"test-connection", "--managed", "invalid", "--help"}, "", nil, "exit 99\n", 0, false, "")
	if r.code != 64 || r.out != help || r.stderr != "error: invalid argument: --managed invalid\n\n" || r.trace != "" || r.log != "" {
		t.Fatal(r)
	}
	r = connectionCLI(t, []string{"test-connection", "--unknown", "--help"}, "", nil, "exit 99\n", 0, false, "")
	if r.code != 64 || r.out != help || r.stderr != "error: Unknown option --unknown\n\n" || r.trace != "" || r.log != "" {
		t.Fatal(r)
	}
	r = connectionCLI(t, []string{"test-connection", "--help", "--unknown"}, "", nil, "exit 99\n", 0, false, "")
	if r.code != 0 || r.out != help || r.stderr != "" || r.trace != "" || r.log != "" {
		t.Fatal(r)
	}
	for _, argv := range [][]string{{"ls", "--unknown"}, {"--unknown", "test-connection"}} {
		r = connectionCLI(t, argv, "", nil, "exit 99\n", 0, false, "")
		if r.code != 64 || r.stderr != "error: invalid option: --unknown\n\n" || r.trace != "" || r.log != "" {
			t.Fatal("other/default parse presentation changed", r)
		}
	}
	r = connectionCLI(t, []string{"test-connection", "--managed"}, "", nil, "exit 99\n", 0, false, "")
	if r.code != 64 || strings.Contains(r.stderr, "Unknown option") || r.trace != "" || r.log != "" {
		t.Fatal("missing argument transformed", r)
	}
	one := connectionMachine("one", "one", "one.example", true)
	for _, tc := range []struct {
		argv          []string
		input, suffix string
		code          int
		calls         bool
	}{
		{[]string{"test-connection", "-y", "one", "ignored", "--help"}, "", "", 0, true},
		{[]string{"test-connection", "--no-yes", "one"}, "\x00 Y \n", "", 0, true},
		{[]string{"test-connection", "one"}, "\nother\ny\n", "", 0, true},
		{[]string{"test-connection", "one"}, "n", "error: Aborted\n", 1, false},
		{[]string{"test-connection", "one"}, "", "error: end of file reached\n", 1, false},
		{[]string{"test-connection", "one"}, "y", "", 0, true},
	} {
		r := connectionCLI(t, tc.argv, tc.input, []Machine{one}, "printf 'not-numeric'; printf stderr >&2; exit 0\n", 0700, true, "")
		wantTrace := "settings-auto\nsettings-seven\nmachines\n"
		if tc.calls {
			wantTrace += "ssh\n"
		}
		if r.code != tc.code || r.trace != wantTrace || tc.code == 0 && (r.log != "" || r.stderr != "") || tc.code != 0 && (r.log == "" || !strings.HasSuffix(r.stderr, tc.suffix)) {
			t.Fatal(r)
		}
		if tc.calls && !strings.HasSuffix(r.out, "\nResult: 1 successful, 0 failed\n\nFailed machines:\n") || !tc.calls && strings.Contains(r.out, "Result:") {
			t.Fatal(r.out)
		}
		if !strings.Contains(strings.Join(tc.argv, " "), "-y") && !strings.HasPrefix(r.out, "Test SSH connection to the following machines:\nNAME   SPIN    MANAGED   HOST.TARGET\none    nixos   true      one.example\n\nContinue? [y/N]: ") {
			t.Fatal(r.out)
		}
	}
	for _, flake := range []bool{false, true} {
		r := connectionCLI(t, []string{"test-connection", "-a", "malformed", "-y"}, "", []Machine{one}, "exit 99\n", 0, flake, "")
		wantCode := 1
		wantTrace := ""
		if flake {
			wantCode = 64
			wantTrace = "settings-auto\nsettings-seven\nmachines\n"
		}
		if r.code != wantCode || r.trace != wantTrace || r.log == "" {
			t.Fatal(r)
		}
		if flake && (r.out != help || !strings.HasSuffix(r.stderr, "error: Invalid filter ''\n\n")) || !flake && (r.out != "" || !strings.Contains(r.stderr, "has no flake.nix")) {
			t.Fatal(r)
		}
		if !strings.HasPrefix(r.log, `{command: ["test-connection"],`+"\n") || strings.Contains(r.log, "ParentKey") {
			t.Fatal(r.log)
		}
	}
}

func TestTestConnectionMainCompletedAndSpawn(t *testing.T) {
	// Exact accepted original255 and signal15/2/9 presentation; executable
	// scripts are synthetic finite test stimuli, not original observations.
	for _, local := range []bool{false, true} {
		for _, sig := range []string{"TERM", "INT", "KILL"} {
			host := "signal.example"
			command := `ssh -l root -o ConnectTimeout\=3 -o ServerAliveInterval\=3 -o ServerAliveCountMax\=1 signal.example cat\ /proc/uptime`
			if local {
				host, command = "localhost", "cat /proc/uptime"
			}
			ms := []Machine{connectionMachine("signalled", "first", host, true), connectionMachine("later", "later", "later.example", true)}
			body := "if [[ \"$0\" == */cat || \"${@: -2:1}\" == signal.example ]]; then printf 'signal stdout\\n'; printf 'signal stderr\\n' >&2; kill -" + sig + " $$; fi\nexit 0\n"
			r := connectionCLI(t, []string{"test-connection", "-y"}, "", ms, body, 0700, true, "")
			status := map[string]int{"TERM": 15, "INT": 2, "KILL": 9}[sig]
			want := fmt.Sprintf("Unable to connect to signalled: Running `%s` failed with\n  exit status: %d\n  stdout: signal stdout\n  stderr: signal stderr\n\n\nResult: 1 successful, 1 failed\n\nFailed machines:\n  signalled\n", command, status)
			first := "ssh\n"
			if local {
				first = "cat\n"
			}
			if r.code != 0 || r.out != want || r.stderr != "" || r.log != "" || r.trace != "settings-auto\nsettings-seven\nmachines\n"+first+"ssh\n" {
				t.Fatal(sig, local, r)
			}
		}
	}
	ms := []Machine{connectionMachine("b-first", "b", "b.example", true), connectionMachine("a-second", "a", "a.example", true)}
	r := connectionCLI(t, []string{"test-connection", "-y"}, "", ms, "exit 255\n", 0700, true, "")
	if r.code != 0 || r.stderr != "" || r.log != "" || strings.Count(r.out, "exit status: 255\n") != 2 || !strings.HasSuffix(r.out, "\nResult: 0 successful, 2 failed\n\nFailed machines:\n  b-first\n  a-second\n") {
		t.Fatal(r)
	}
	for _, mode := range []os.FileMode{0, 0600} {
		ms := []Machine{connectionMachine("local", "local", "localhost", true), connectionMachine("later", "later", "later.example", true)}
		r := connectionCLI(t, []string{"test-connection", "-y"}, "", ms, "exit 99\n", mode, true, "")
		if r.code != 1 || r.out != "" || !strings.HasSuffix(r.stderr, "error: No such file or directory - cat\n") || r.trace != "settings-auto\nsettings-seven\nmachines\n" || !strings.HasSuffix(r.log, "Running cat /proc/uptime\n") {
			t.Fatal(r)
		}
	}
}

func TestTestConnectionMainSelectionFailureAndRepeatedHost(t *testing.T) {
	one := connectionMachine("one", "one", "one.example", true)
	for _, tc := range []struct {
		ms   []Machine
		argv []string
	}{
		{[]Machine{}, []string{"test-connection"}},
		{[]Machine{one}, []string{"test-connection", "-y", "missing*"}},
		{[]Machine{connectionMachine("collision", "different", "excluded", true), connectionMachine("survivor", "collision", "survivor", true)}, []string{"test-connection", "-y", "-a", "name!=collision", "collision"}},
	} {
		r := connectionCLI(t, tc.argv, "y\n", tc.ms, "exit 99\n", 0, true, "")
		if r.code != 1 || r.out != "" || !strings.HasSuffix(r.stderr, "error: No machines to test\n") || r.trace != "settings-auto\nsettings-seven\nmachines\n" || r.log == "" {
			t.Fatal(r)
		}
	}
	// Invalid decoded evaluator output wins over the later malformed attr.
	bad := one
	bad.Attributes = nil
	r := connectionCLI(t, []string{"test-connection", "-a", "malformed", "-y"}, "", []Machine{bad}, "exit 99\n", 0, true, "")
	if r.code != 1 || r.out != "" || !strings.HasSuffix(r.stderr, "error: one: missing metaConfig\n") || strings.Contains(r.stderr, "Invalid filter") || r.trace != "settings-auto\nsettings-seven\nmachines\n" {
		t.Fatal(r)
	}
	ms := []Machine{connectionMachine("first", "first", "shared.example", true), connectionMachine("second", "second", "shared.example", true)}
	r = connectionCLI(t, []string{"test-connection", "-y"}, "", ms, "exit 0\n", 0700, true, "")
	request := []string{"-l", "root", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1", "shared.example", "cat /proc/uptime"}
	if r.code != 0 || r.out != "\nResult: 2 successful, 0 failed\n\nFailed machines:\n" || r.trace != "settings-auto\nsettings-seven\nmachines\nssh\nssh\n" || r.args != strings.Repeat(strings.Join(request, "\x00")+"\x00", 2) {
		t.Fatal(r)
	}
}

func TestTestConnectionMainRetainedLargeLogAndQuotedArgs(t *testing.T) {
	for _, size := range []int{32768, 65536, 65537} {
		ms := []Machine{connectionMachine("first", "first", "large.example", true), connectionMachine("local", "local", "localhost", true)}
		// Bash builtin printf only, no host cat/profile/process tools.
		payload := strings.Repeat("P", 32768)
		if size == 65536 {
			payload += strings.Repeat("S", 32768)
		}
		if size == 65537 {
			payload += "M" + strings.Repeat("S", 32768)
		}
		body := "printf 'retained stdout\\n'; printf '%s' '" + payload + "' >&2; exit 7\n"
		r := connectionCLI(t, []string{"test-connection", "-y"}, "", ms, body, 0, true, "")
		if r.code != 1 || !strings.HasSuffix(r.stderr, "error: No such file or directory - cat\n") || strings.Contains(r.out, "Result:") || strings.Count(r.log, "] Finished") != 4 || !strings.HasSuffix(r.log, "Running cat /proc/uptime\n") {
			t.Fatal("large stop/log", size, r.code, r.stderr)
		}
		var captured strings.Builder
		chunks := 0
		for _, line := range strings.Split(r.log, "\n") {
			_, data, ok := strings.Cut(line, "] \t")
			if !ok || data == "" || strings.Trim(data, "PMS") != "" {
				continue
			}
			if len(data) > 16384 {
				t.Fatal("actual read bound lost", len(data))
			}
			captured.WriteString(data)
			chunks++
		}
		if captured.String() != payload || chunks < 2 || strings.Contains(r.log, "... omitting") {
			t.Fatal("full target chunk log truncated", size, captured.Len(), chunks)
		}
		if size == 65537 && !strings.Contains(r.out, strings.Repeat("P", 32768)+"\n... omitting 1 bytes ...\n"+strings.Repeat("S", 32768)) || size <= 65536 && !strings.Contains(r.out, payload) {
			t.Fatal("diagnostic bound", size)
		}
	}
	m := connectionMachine("quoted", "quoted", "target%with'quote", true)
	m.Target.Port = 2222
	m.Attributes["host"].(map[string]any)["port"] = 2222
	r := connectionCLI(t, []string{"test-connection", "-y"}, "", []Machine{m}, "printf 'data-not-uptime\\n'; printf 'separate error\\n' >&2; exit 7\n", 0700, true, "private ssh%quote'/config")
	wantArgs := []string{"-F", "private ssh%quote'/config", "-l", "root", "-p", "2222", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1", "target%with'quote", "cat /proc/uptime"}
	if r.code != 0 || r.stderr != "" || r.log != "" || r.args != strings.Join(wantArgs, "\x00")+"\x00" || !strings.Contains(r.out, `private\ ssh\%quote\'/config`) {
		t.Fatal(r)
	}
}

func actualCLI(t *testing.T, argv []string, registry string) (int, string, string, string) {
	return actualCLIWithSetup(t, argv, registry, nil)
}

func actualCLIWithSetup(t *testing.T, argv []string, registry string, setup func(string, string, *[]string)) (int, string, string, string) {
	t.Helper()
	root := t.TempDir()
	configuration := filepath.Join(root, "configuration")
	tools := filepath.Join(root, "tools")
	for _, p := range []string{configuration, tools} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("declared Bash required for process sentinels", err)
	}
	marker := filepath.Join(root, "unexpected-process")
	for _, tool := range []string{"nix", "git", "ssh", "extension"} {
		if err := os.WriteFile(filepath.Join(tools, tool), []byte("#!"+bash+"\nprintf 'unexpected child\\n' >> \"$CONFCTL_STAGE1_SENTINEL\"\nexit 97\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(argv)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
	cmd.Dir = configuration
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "CONFCTL_EXTENSION_REGISTRY=") && !strings.HasPrefix(value, "CONFCTL_EXTENSION_ROOT=") {
			env = append(env, value)
		}
	}
	if registry != "" {
		// Only test input is bound here. Invalid JSON is retained for rejection cases.
		rb, readErr := os.ReadFile(registry)
		if readErr == nil {
			if r, decodeErr := decodeRegistry(rb); decodeErr == nil {
				r = bindTestRegistry(t, configuration, r)
				rb, _ = json.Marshal(r)
				registry = filepath.Join(root, "registry.json")
				if err = os.WriteFile(registry, rb, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		env = append(env, "CONFCTL_EXTENSION_REGISTRY="+registry, "CONFCTL_EXTENSION_ROOT="+configuration)
	}
	if setup != nil {
		setup(configuration, registry, &env)
	}
	before := testConfigurationState(t, configuration)
	cmd.Env = append(env, "CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV="+string(b), "CONFCTL_STAGE1_SENTINEL="+marker, "PATH="+tools+":"+os.Getenv("PATH"), "PAGER=", "HOME="+root)
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		var ok bool
		var exit *exec.ExitError
		exit, ok = err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if _, err = os.Stat(marker); err == nil {
		t.Fatal("pre-execution path started an external tool", argv)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	after := testConfigurationState(t, configuration)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("pre-execution path changed configuration", argv, before, after)
	}
	return code, out.String(), stderr.String(), configuration
}

type stage1Reference struct {
	Exit                 int
	Stdout, Stderr, Text string
	StdoutAsset          string `json:"stdout_asset"`
	StdoutSha256         string `json:"stdout_sha256"`
}

func referenceRecord(t *testing.T, id string) stage1Reference {
	t.Helper()
	b, err := os.ReadFile("testdata/stage1-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var references struct {
		ContractRevision string `json:"contract_revision"`
		Records          map[string]stage1Reference
	}
	if err = json.Unmarshal(b, &references); err != nil {
		t.Fatal(err)
	}
	o, ok := references.Records[id]
	if !ok || references.ContractRevision != "cc40679d267165aecfa569128438bb55fe910268" {
		t.Fatal("missing original Ruby reference", id)
	}
	if o.StdoutAsset != "" {
		b, err = os.ReadFile(o.StdoutAsset)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(b)) != o.StdoutSha256 {
			t.Fatal("original help asset changed", o.StdoutAsset, err)
		}
		o.Stdout = string(b)
	}
	return o
}

func TestActualCLIReferenceHelpVersionAndParserError(t *testing.T) {
	for _, tc := range []struct {
		id       string
		argv     []string
		registry string
	}{
		{"parser-help", []string{"--help"}, ""},
		{"parser-help", []string{"-h"}, ""},
		{"parser-help", []string{"help"}, ""},
		{"parser-version", []string{"--version"}, "/no/such/registry"},
		{"list-invalid-managed", []string{"ls", "--managed", "bad"}, ""},
	} {
		code, out, stderr, _ := actualCLI(t, tc.argv, tc.registry)
		want := referenceRecord(t, tc.id)
		if code != want.Exit || out != want.Stdout || stderr != want.Stderr {
			t.Fatalf("%s: exit%d stdout%q stderr%q", tc.id, code, out, stderr)
		}
	}
}

func TestActualCLICommandHelpPrecedenceHasNoEffects(t *testing.T) {
	// Reuse the original ls help bytes, including their captured provenance.
	want := referenceRecord(t, "list-invalid-managed")
	for _, argv := range [][]string{
		{"ls", "--help", "--managed", "bad"},
		{"ls", "-h", "--unknown"},
		{"ls", "-HhZ"},
		{"ls", "-h", "--output"},
	} {
		code, out, stderr, _ := actualCLI(t, argv, "")
		if code != 0 || stderr != "" || out != want.Stdout {
			t.Fatal(argv, code, out, stderr)
		}
	}
	code, out, stderr, _ := actualCLI(t, []string{"ls", "--managed", "bad", "--help"}, "")
	if code != want.Exit || out != want.Stdout || stderr != want.Stderr {
		t.Fatal("earlier parse error lost", code, out, stderr)
	}
}

func TestActualCLIIntegerParsingHasNoEffects(t *testing.T) {
	help, err := cli.BuiltinRegistry().Help([]string{"health-check"}, 80)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		argv        []string
		code        int
		out, stderr string
	}{
		{[]string{"health-check", "-j9223372036854775808", "--help"}, 0, help, ""},
		{[]string{"health-check", "-j-9223372036854775809", "--help"}, 0, help, ""},
		{[]string{"health-check", "-j0x10", "--help"}, 0, help, ""},
		{[]string{"health-check", "-j08"}, 64, help, "error: invalid argument: -j 08\n\n"},
		{[]string{"health-check", "-j08", "--help"}, 64, help, "error: invalid argument: -j 08\n\n"},
		{[]string{"health-check", "--help", "-j08"}, 0, help, ""},
		{[]string{"health-check", "-j0_10", "--help"}, 0, help, ""},
	} {
		code, out, stderr, _ := actualCLI(t, tc.argv, "")
		if code != tc.code || out != tc.out || stderr != tc.stderr {
			t.Fatal(tc.argv, code, out, stderr)
		}
	}
}

func TestActualCLIGlobalErrorsRespectRegistryAuthority(t *testing.T) {
	p := writeTestRegistry(t, genericTestRegistry(t))
	help := referenceRecord(t, "parser-help").Stdout
	version := referenceRecord(t, "parser-version")
	for _, authority := range []struct {
		name     string
		registry string
		setup    func(string, string, *[]string)
		stderr   string
	}{
		{name: "absent"},
		{name: "valid", registry: p},
		{
			name: "partial-registry", registry: p,
			setup:  func(_, _ string, env *[]string) { replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", nil) },
			stderr: "extension registry requires nonempty CONFCTL_EXTENSION_REGISTRY and CONFCTL_EXTENSION_ROOT\n",
		},
		{
			name: "partial-root", registry: p,
			setup:  func(_, _ string, env *[]string) { replaceAuthority(env, "CONFCTL_EXTENSION_REGISTRY", nil) },
			stderr: "extension registry requires nonempty CONFCTL_EXTENSION_REGISTRY and CONFCTL_EXTENSION_ROOT\n",
		},
		{
			name: "empty", registry: p,
			setup: func(_, _ string, env *[]string) {
				empty := ""
				replaceAuthority(env, "CONFCTL_EXTENSION_REGISTRY", &empty)
				replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", &empty)
			},
			stderr: "extension registry requires nonempty CONFCTL_EXTENSION_REGISTRY and CONFCTL_EXTENSION_ROOT\n",
		},
		{
			name: "wrong-root", registry: p,
			setup: func(root, _ string, env *[]string) {
				parent := filepath.Dir(root)
				replaceAuthority(env, "CONFCTL_EXTENSION_ROOT", &parent)
			},
			stderr: "extension root differs from current directory\n",
		},
		{
			name: "stale", registry: p,
			setup: func(root, _ string, _ *[]string) {
				if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			stderr: "stale bound source \"flake.nix\"\n",
		},
	} {
		t.Run(authority.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, message string
				argv          []string
			}{
				{"unknown", "invalid option: --bad", []string{"--bad"}},
				{"missing-value", "missing argument: --color", []string{"--color"}},
				{"invalid-prefix-version", "invalid option: --bad", []string{"--bad", "--version"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					wantCode, wantOut, wantErr := 64, help, "error: "+tc.message+"\n\n"
					if authority.stderr != "" {
						wantCode, wantOut, wantErr = 1, "", authority.stderr
					}
					code, out, stderr, _ := actualCLIWithSetup(t, tc.argv, authority.registry, authority.setup)
					if code != wantCode || out != wantOut || stderr != wantErr {
						t.Fatalf("argv %q: exit=%d stdout=%q stderr=%q; want exit=%d stdout=%q stderr=%q", tc.argv, code, out, stderr, wantCode, wantOut, wantErr)
					}
				})
			}
			for _, argv := range [][]string{{"--version"}, {"--color", "never", "--version"}} {
				code, out, stderr, _ := actualCLIWithSetup(t, argv, authority.registry, authority.setup)
				if code != version.Exit || out != version.Stdout || stderr != version.Stderr {
					t.Fatal("valid version-only bypass changed", argv, code, out, stderr)
				}
			}
		})
	}
}

func TestActualCLIRegistryFailuresHaveNoEffects(t *testing.T) {
	for _, body := range []string{
		`not JSON`,
		`{"schema":2,"extensions":[]}`,
		`{"schema":1,"extensions":[],"discovery":true}`,
		`{"schema":1,"extensions":[]} {"schema":1}`,
	} {
		p := filepath.Join(t.TempDir(), "registry.json")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		code, out, stderr, _ := actualCLI(t, []string{"--help"}, p)
		if code != 1 || out != "" || stderr == "" {
			t.Fatal(body, code, out, stderr)
		}
	}
}

func TestActualCLIUnavailableLeavesHaveNoEffects(t *testing.T) {
	r := cli.BuiltinRegistry()
	for _, c := range r.Leaves() {
		if c.Availability.Mode != cli.Unavailable {
			continue
		}
		t.Run(strings.Join(c.Path, "/"), func(t *testing.T) {
			code, out, stderr, _ := actualCLI(t, c.Path, "")
			if code != 2 || out != "" || !strings.Contains(stderr, "unavailable") {
				t.Fatal(code, out, stderr)
			}
		})
	}

}

func TestActualCLINestedAndExtensionHelp(t *testing.T) {
	r := syntheticSiteRegistry(t)
	for i := range r.Extensions {
		r.Extensions[i].Argv = []string{"/no/such/extension"}
	}
	p := filepath.Join(t.TempDir(), "registry.json")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		argv     []string
		contains string
	}{
		{[]string{"help", "inputs", "channel", "set"}, "--[no-]allow_shared"},
		{[]string{"inputs", "channel", "--help"}, "Available"},
		{[]string{"status", "-h"}, "--generation"},
		{[]string{"help", "runtime-kernels", "update"}, "Update runtime kernels"},
		{[]string{"runtime-kernels", "--help"}, "Available"},
		{[]string{"runtime-kernels", "update", "--help"}, "--[no-]show-trace"},
		{[]string{"runtime-kernels", "update", "-h", "--managed", "all"}, "--[no-]show-trace"},
	} {
		code, out, stderr, _ := actualCLI(t, tc.argv, p)
		if code != 0 || stderr != "" || !strings.Contains(out, tc.contains) {
			t.Fatal(tc, code, out, stderr)
		}
	}
	code, out, _, _ := actualCLI(t, []string{"runtime-kernels", "update", "first", "second"}, p)
	if code != 64 || out != "" {
		t.Fatal("extension arity guard changed", code, out)
	}
	for _, argv := range [][]string{{"help", "missing"}, {"help", "--availability"}, {"inputs", "missing"}, {"runtime-kernels", "update", "--managed", "all"}} {
		code, _, _, _ := actualCLI(t, argv, p)
		if code != 64 {
			t.Fatal(argv, code)
		}
	}
}

func TestRegistryCollisionUsesBuiltinTree(t *testing.T) {
	for _, c := range cli.BuiltinRegistry().Children(nil) {
		r := syntheticSiteRegistry(t)
		r.Extensions[1].Commands[0].Path = []string{c.Path[0], "update"}
		b, _ := json.Marshal(r)
		p := filepath.Join(t.TempDir(), "registry.json")
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := commandRegistry(r); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatal(c.Path, err)
		}
		code, _, _, _ := actualCLI(t, []string{"--help"}, p)
		if code != 1 {
			t.Fatal(c.Path, code)
		}
	}
}

func TestParsedAdapterPreservesLogAndExtensionValues(t *testing.T) {
	registrations := syntheticSiteRegistry(t)
	r, registered, err := commandRegistry(registrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"ls", "-o", "", "-a", "one", "-a", "two", "target", "-o", "ignored"}, {"runtime-kernels", "update", "-a", "one", "-t", "two", "-y", "--no-yes", "--show-trace", "target"}} {
		inv, err := r.Parse(argv)
		if err != nil {
			t.Fatal(err)
		}
		o := handlerOptions(inv)
		if argv[0] == "ls" {
			if !o.HasOutput || o.Output != "" || !reflect.DeepEqual(o.Attrs, []string{"one", "two"}) || !reflect.DeepEqual(o.Args, []string{"target", "-o", "ignored"}) {
				t.Fatal(o)
			}
		} else if o.Yes || !o.Trace || !reflect.DeepEqual(o.Attrs, []string{"one"}) || !reflect.DeepEqual(o.Tags, []string{"two"}) || !reflect.DeepEqual(o.Args, []string{"target"}) {
			t.Fatal(o)
		}
		if argv[0] == "runtime-kernels" {
			bound := registered[strings.Join(inv.Command.Path, " ")]
			in := extensionInvocation("/disposable", bound.Registration, bound.Command, o, argv)
			wantOptions := map[string]any{"yes": false, "attr": []string{"one"}, "tag": []string{"two"}, "show-trace": true}
			if in.Root != "/disposable" || in.ExtensionID != bound.Registration.ID || !reflect.DeepEqual(in.CommandPath, []string{"runtime-kernels", "update"}) || !reflect.DeepEqual(in.OriginCommand, in.CommandPath) || !reflect.DeepEqual(in.Options, wantOptions) || !reflect.DeepEqual(in.Arguments, []string{"target"}) || !reflect.DeepEqual(in.RawArgv, argv) {
				t.Fatal("schema1 adapter changed", in)
			}
		}
		f, err := os.Create(filepath.Join(t.TempDir(), "log"))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Log: f, logOwner: nativeEngineLog(f), Color: "auto"}
		if err := e.LogCLI(argv[0], o, argv); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if argv[0] == "runtime-kernels" {
			if !strings.Contains(string(b), "#<GLI::Command::ParentKey:0x0000000000000000> => {}") || !strings.Contains(string(b), "\"show-trace\" => true") {
				t.Fatal(string(b))
			}
		} else if !strings.Contains(string(b), "\"o\" => \"\",\n   \"output\" => \"\"") || !strings.Contains(string(b), "arguments: [\"target\", \"-o\", \"ignored\"]") {
			t.Fatal(string(b))
		}
	}
}

func TestLogCLIOriginalPPReference(t *testing.T) {
	r, registered, err := commandRegistry(syntheticSiteRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"runtime-kernels", "update", "-y"}
	inv, err := r.Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	o := handlerOptions(inv)
	bound := registered[strings.Join(inv.Command.Path, " ")]
	in := extensionInvocation("/disposable", bound.Registration, bound.Command, o, argv)
	// Empty arrays remain the existing adapter's nil slices, rather than becoming
	// new protocol values. The wire schema and the aliases in LogCLI stay intact.
	if !o.Yes || o.Attrs != nil || o.Tags != nil || o.Args != nil || !reflect.DeepEqual(in.Options["attr"], []string(nil)) || !reflect.DeepEqual(in.Options["tag"], []string(nil)) || in.Arguments != nil {
		t.Fatal(o, in)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Log: f, logOwner: nativeEngineLog(f), Color: inv.Globals["color"].Value.String}
	if err := e.LogCLI("runtime-kernels", o, argv); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := referenceRecord(t, "site-kernels-corrupt-log").Text
	want = strings.ReplaceAll(want, "${PARENT_ADDRESS:1}", "0x0000000000000000")
	if string(b) != want {
		t.Fatalf("PP log changed:\n%s\nwant:\n%s", b, want)
	}
}

func testConfigurationState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += target
		} else if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf("%x", sha256.Sum256(b))
		}
		state[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
