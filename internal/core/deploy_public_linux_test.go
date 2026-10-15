package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	ext "github.com/vpsfreecz/confctl/extension"
)

// A finite SDK peer records the actual hook payload and returns an explicit
// status. It runs only as a child through the production FD3/4 supervisor.
func TestDeployPublicHookHelper(t *testing.T) {
	if os.Getenv("CONFCTL_DEPLOY_PUBLIC_HOOK") != "1" {
		return
	}
	input, output := os.NewFile(3, "rpc-in"), os.NewFile(4, "rpc-out")
	decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
	decoder.UseNumber()
	var message ext.Message
	if err := decoder.Decode(&message); err != nil || message.Method != "initialize" {
		t.Fatal(message, err)
	}
	b, _ := json.Marshal(ext.InitResult{Protocol: ext.Version{Major: 1}})
	if err := encoder.Encode(ext.Message{JSONRPC: "2.0", ID: message.ID, Result: b}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&message); err != nil || message.Method != "run" {
		t.Fatal(message, err)
	}
	var run ext.Run
	d := json.NewDecoder(bytes.NewReader(message.Params))
	d.UseNumber()
	if err := d.Decode(&run); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(run.Context)
	if err := os.WriteFile(filepath.Join(run.Context.Root, "hook.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(run.Context.Root, "trace"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hook\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(run.Context.Root, "machines.after.json")); err == nil {
		if err := os.WriteFile(filepath.Join(run.Context.Root, "machines.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	code := 0
	if value := os.Getenv("DEPLOY_HOOK_STATUS"); value != "" {
		code, err = strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	b, _ = json.Marshal(ext.RunResult{ExitCode: code})
	if err := encoder.Encode(ext.Message{JSONRPC: "2.0", ID: message.ID, Result: b}); err != nil {
		t.Fatal(err)
	}
	os.Exit(code)
}

type deployPublicFixture struct {
	buildStatusDeliveryFixture
	hooks Registry
}

func deployPublic(t *testing.T, hosts ...string) deployPublicFixture {
	t.Helper()
	f := deployPublicFixture{buildStatusDeliveryFixture: buildStatusDelivery(t, hosts...)}
	for _, host := range hosts {
		f.seed(t, host) // Build mode reuses these; no production root/login demand.
	}
	f.hooks = bindTestRegistry(t, f.root, Registry{Schema: 1, Extensions: []Registration{{
		ID: "fixture.deploy", Protocol: ext.Version{Major: 1},
		Argv:  []string{os.Args[0], "-test.run=^TestDeployPublicHookHelper$"},
		Hooks: []Hook{{Event: "deploy.prepare", Handler: "prepare"}},
	}}})
	b, _ := json.Marshal(f.hooks)
	f.write(t, "registry.json", string(b))
	f.tool(t, "nix-copy-closure", `host="${2#root@}"
printf 'copy:%s\n' "$host" >> "$TRACE"
printf '%s\0' "$@" >> "$HOME/copy-args"
printf "copying 1 paths\ncopying path '%s'\n" "${@: -1}" >&2
[[ "$host" == "${FAIL_COPY_HOST:-}" ]] && exit 23
printf 'copied:%s\n' "$host" >> "$HOME/effects"
`)
	f.tool(t, "ssh", `request="${@: -1}"
host="${@: -2:1}"
printf 'ssh:%s:%s\n' "$host" "$request" >> "$TRACE"
case "$request" in
 'cat /proc/uptime') printf '600 0\n';;
 'realpath /run/current-system') printf '/fixture/previous\n';;
 'bash --norc') while IFS= read -r line; do :; done
  printf '/fixture/previous\n/nix/var/nix/profiles/system-1-link;/fixture/previous;0\n';;
 *'/bin/switch-to-configuration '*)
  [[ "$host" == "${FAIL_ACTIVATE_HOST:-}" ]] && exit 23
  printf 'activated:%s:%s\n' "$host" "${request##* }" >> "$HOME/effects";;
 'nix-env -p '*) printf 'profile:%s\n' "$host" >> "$HOME/effects";;
 *) printf 'unexpected request: %s\n' "$request" >&2; exit 98;;
esac
`)
	f.tool(t, "health-probe", `printf 'health\n' >> "$TRACE"
if [[ ${HEALTH_MODE:-success} == fail ]] || [[ ${HEALTH_MODE:-success} == once && ! -f "$HOME/health-failed" ]]; then
 printf 'failure detail\n' >&2
 printf 'failed\n' > "$HOME/health-failed"
 exit 7
fi
printf 'healthy\n'
`)
	return f
}

func (f deployPublicFixture) run(t *testing.T, args []string, input string, env ...string) (int, string, string) {
	t.Helper()
	env = append([]string{"CONFCTL_DEPLOY_PUBLIC_HOOK=1", "CONFCTL_EXTENSION_REGISTRY=" + filepath.Join(f.root, "registry.json"), "CONFCTL_EXTENSION_ROOT=" + f.root}, env...)
	return f.buildStatusDeliveryFixture.run(t, args, input, env...)
}

func (f deployPublicFixture) hook(t *testing.T) ext.Invocation {
	t.Helper()
	var in ext.Invocation
	d := json.NewDecoder(strings.NewReader(f.text("hook.json")))
	d.UseNumber()
	if err := d.Decode(&in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestDeployPublicPrepareActionsAndFreshInventory(t *testing.T) {
	for _, action := range []string{"", "boot", "test", "dry-activate"} {
		t.Run(action, func(t *testing.T) {
			f := deployPublic(t, "node")
			fresh := strings.ReplaceAll(f.text("machines.json"), "node.invalid", "fresh.invalid")
			f.write(t, "machines.after.json", fresh)
			before := testConfigurationState(t, filepath.Join(f.root, ".confctl/generations"))
			args := []string{"deploy", "-y", "-gselected", "--copy-only", "--max-concurrent-copy=0x2", "node"}
			want := action
			if action == "" {
				want = "switch"
			} else {
				args = append(args, action)
			}
			code, out, stderr := f.run(t, args, "")
			in, trace := f.hook(t), f.text("trace")
			if code != 0 || stderr != "" || in.Action != want || in.Event != "deploy.prepare" || !reflect.DeepEqual(in.SelectedNames, []string{"node"}) || !reflect.DeepEqual(in.OriginCommand, []string{"deploy"}) || !reflect.DeepEqual(in.RawArgv, args) || !reflect.DeepEqual(in.Arguments, args[5:]) || in.Options["copy-only"] != true || in.Options["max-concurrent-copy"] != json.Number("2") {
				t.Fatal(code, out, stderr, in, trace)
			}
			if strings.Count(trace, "hook\n") != 1 || !strings.Contains(trace, "copy:fresh.invalid") || strings.Contains(trace, "ssh:") || strings.Contains(trace, "system-build") || strings.Index(trace, "hook\n") > strings.Index(trace, "copy:") || len(f.logs(t)) != 0 {
				t.Fatal("hook/refresh/copy-only order changed", trace)
			}
			if !reflect.DeepEqual(before, testConfigurationState(t, filepath.Join(f.root, ".confctl/generations"))) {
				t.Fatal("stored copy changed generation state")
			}
		})
	}
}

func TestDeployPublicBuildAndOneConfirmation(t *testing.T) {
	f := deployPublic(t, "node")
	code, out, stderr := f.run(t, []string{"deploy", "--copy-only"}, "y\n")
	trace := f.text("trace")
	if code != 0 || stderr != "" || strings.Count(out, "Continue?") != 1 || strings.Count(trace, "hook\n") != 1 || strings.Count(trace, "system-build\n") != 1 || strings.Index(trace, "hook\n") > strings.Index(trace, "system-build\n") || !strings.Contains(trace, "copy:node.invalid") || len(f.logs(t)) != 0 {
		t.Fatal(code, out, stderr, trace)
	}
}

func TestDeployPublicRefusalUsageAndHookStatus(t *testing.T) {
	for _, tc := range []struct {
		args                   []string
		input, env, diagnostic string
		code                   int
	}{
		{[]string{"deploy"}, "n\n", "", "Aborted", 1},
		{[]string{"deploy"}, "", "", "end of file reached", 1},
		{[]string{"deploy", "--help"}, "", "", "", 0},
		{[]string{"deploy", "--unknown"}, "", "", "invalid option", 64},
		{[]string{"deploy", "--max-concurrent-copy=auto"}, "", "", "invalid argument", 64},
		{[]string{"deploy", "-y", "node", "invalid"}, "", "", "invalid action", 64},
		{[]string{"deploy", "--reboot", "-y"}, "", "", "only with switch-action boot", 64},
		{[]string{"deploy", "-y", "missing"}, "", "", "No machines", 1},
		{[]string{"deploy", "-y", "--copy-only"}, "", "DEPLOY_HOOK_STATUS=73", "status 73", 73},
	} {
		f := deployPublic(t, "node")
		var env []string
		if tc.env != "" {
			env = []string{tc.env}
		}
		code, out, stderr := f.run(t, tc.args, tc.input, env...)
		trace := f.text("trace")
		if code != tc.code || !strings.Contains(stderr, tc.diagnostic) || strings.Contains(trace, "copy:") || strings.Contains(trace, "ssh:") || strings.Contains(trace, "system-build") || tc.env == "" && strings.Contains(trace, "hook\n") {
			t.Fatal(tc.args, code, out, stderr, trace)
		}
		if tc.code == 64 && !strings.Contains(out, "SYNOPSIS") {
			t.Fatal(out)
		}
		if tc.code == 73 && len(f.logs(t)) != 1 {
			t.Fatal("hook failure lost log")
		}
	}
}

func TestDeployPublicActionsAndPartialEffects(t *testing.T) {
	for _, action := range []string{"switch", "boot", "test", "dry-activate"} {
		f := deployPublic(t, "node")
		code, out, stderr := f.run(t, []string{"deploy", "-y", "-gselected", "--disable-auto-rollback", "--no-health-checks", "node", action}, "")
		effects := f.text("effects")
		if code != 0 || stderr != "" || !strings.Contains(effects, "activated:node.invalid:"+action) || strings.Contains(effects, "profile:") != (action == "switch" || action == "boot") || len(f.logs(t)) != 0 {
			t.Fatal(code, out, stderr, effects)
		}
	}
	for _, phase := range []string{"COPY", "ACTIVATE"} {
		f := deployPublic(t, "first", "second")
		args := []string{"deploy", "-y", "-gselected", "--disable-auto-rollback", "--no-health-checks", "--max-concurrent-copy=1"}
		if phase == "ACTIVATE" {
			args = append(args, "--one-by-one")
		}
		code, out, stderr := f.run(t, args, "", "FAIL_"+phase+"_HOST=second.invalid")
		effects := f.text("effects")
		if code != 1 || !strings.Contains(stderr, "failed") || !strings.Contains(effects, "copied:first.invalid") || strings.Contains(effects, "profile:second.invalid") || len(f.logs(t)) != 1 || phase == "COPY" && strings.Contains(effects, "activated:") || phase == "ACTIVATE" && !strings.Contains(effects, "profile:first.invalid") {
			t.Fatal(phase, code, out, stderr, effects)
		}
	}
}

func TestDeployPublicHealthFailureRetryAndLog(t *testing.T) {
	for _, mode := range []string{"success", "fail", "keep-going", "retry"} {
		f := deployPublic(t, "node")
		var inventory map[string]map[string]any
		if err := json.Unmarshal([]byte(f.text("machines.json")), &inventory); err != nil {
			t.Fatal(err)
		}
		var command any
		if err := json.Unmarshal([]byte(healthImmediateCommand("health-probe", "literal token")), &command); err != nil {
			t.Fatal(err)
		}
		inventory["node"]["metaConfig"].(map[string]any)["healthChecks"] = map[string]any{"builderCommands": []any{command}}
		b, _ := json.Marshal(inventory)
		f.write(t, "machines.json", string(b))
		args := []string{"deploy", "-y", "-gselected", "--disable-auto-rollback"}
		input, healthMode, want := "", mode, 0
		if mode == "fail" {
			want = 1
		}
		if mode == "keep-going" {
			args = append(args, "--keep-going")
			healthMode = "fail"
		}
		if mode == "retry" {
			args = append(args, "--interactive")
			input = "y\ny\ny\nr\n"
			healthMode = "once"
		}
		code, out, stderr := f.run(t, args, input, "HEALTH_MODE="+healthMode)
		if code != want || (len(f.logs(t)) == 1) != (mode != "success") || !strings.Contains(f.text("trace"), "health\n") {
			t.Fatal(mode, code, out, stderr, f.text("trace"))
		}
		if mode == "retry" && strings.Count(f.text("trace"), "health\n") != 2 {
			t.Fatal("shared input/retry did not reach second health round", out, stderr)
		}
		if mode != "success" {
			log, err := os.ReadFile(f.logs(t)[0])
			if err != nil || !bytes.Contains(log, []byte("failure detail")) {
				t.Fatal(string(log), err)
			}
		}
	}
}

func TestDeployPublicNoOriginHookKeepsContext(t *testing.T) {
	f := deployPublic(t, "node")
	t.Setenv("CONFCTL_DEPLOY_PUBLIC_HOOK", "1")
	e := &Engine{Root: f.root, Context: context.Background()}
	if code, err := e.Hooks(f.hooks, "deploy.prepare", []string{"node"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	in := f.hook(t)
	if in.Action != "switch" || len(in.OriginCommand) != 0 || !reflect.DeepEqual(in.SelectedNames, []string{"node"}) {
		t.Fatal(in)
	}
}

func TestDeployPublicLogIncludesParsedOptions(t *testing.T) {
	f := deployPublic(t, "node")
	args := []string{"deploy", "-y", "--generation=", "--max-concurrent-copy=0x10", "--max-jobs=auto", "node", "test"}
	code, out, stderr := f.run(t, args, "", "DEPLOY_HOOK_STATUS=73")
	if code != 73 || len(f.logs(t)) != 1 {
		t.Fatal(code, out, stderr)
	}
	b, err := os.ReadFile(f.logs(t)[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`command: ["deploy"]`, `"max-concurrent-copy" => 16`, `"max-jobs" => "auto"`, `"generation" => ""`, `arguments: ["node", "test"]`} {
		if !bytes.Contains(b, []byte(field)) {
			t.Fatal("deploy log lost parsed invocation value", field, string(b))
		}
	}
}
