package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

type buildStatusDeliveryFixture struct {
	comparisonDeliveryFixture
	top string
}

func buildStatusDelivery(t *testing.T, hosts ...string) buildStatusDeliveryFixture {
	t.Helper()
	ms := make([]Machine, len(hosts))
	for i, host := range hosts {
		ms[i] = connectionMachine(host, host, host+".invalid", true)
	}
	f := buildStatusDeliveryFixture{comparisonDeliveryFixture: comparisonDelivery(t, ms...)}
	f.top = filepath.Join(f.root, "top")
	if err := os.Mkdir(f.top, 0700); err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	plan.WriteByte('{')
	outputs := map[string]map[string]string{}
	for i, host := range hosts {
		if i != 0 {
			plan.WriteByte(',')
		}
		fmt.Fprintf(&plan, "%q:{\"key\":%q,\"inputs\":{}}", host, host)
		outputs[host] = map[string]string{"toplevel": f.top, "autoRollback": "/synthetic/rollback"}
	}
	plan.WriteByte('}')
	f.write(t, "plan.json", plan.String())
	b, _ := json.Marshal(outputs)
	f.write(t, "outputs.json", string(b))
	f.write(t, "target.json", `{"os":"fresh-revision"}`)
	f.write(t, "deployed.json", `{"os":"fresh-revision"}`)
	f.tool(t, "nix", `key="${@: -1}"
printf '%s\0' "$@" >> "$HOME/nix-args"
printf '\n' >> "$HOME/nix-args"
printf 'nix:%s:%s\n' "$1" "$key" >> "$TRACE"
case "$key" in
 .#confctl.settings) printf '%s\n' '{"list":{"columns":["name","managed"]},"nix":{"maxJobs":"7","impureEval":false,"legacyNixPath":false}}';;
 .#confctl.machinesJson) printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$PWD";;
 .#confctl.machineKeys) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < keys.json;;
 .#confctl.buildPlan) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < plan.json;;
 .#confctl.build) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < outputs.json;;
 .#confctl.inputsInfo.*)
  [[ ${FAIL_METADATA:-0} == 1 ]] && exit 23
  while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < target.json;;
 *)
  if [[ "$1" == build ]]; then
   printf 'system-build\n' >> "$TRACE"
   [[ ${FAIL_BUILD:-0} == 1 ]] && { printf 'build failed\n' >&2; exit 7; }
   printf 'null\n'
  else printf 'unexpected evaluation\n' >&2; exit 98; fi;;
esac
`)
	f.tool(t, "ssh", `request="${@: -1}"
host="${@: -2:1}"
printf 'ssh:%s:%s\n' "$host" "$request" >> "$TRACE"
[[ "$request" == "${FAIL_QUERY:-}" ]] && exit 255
case "$request" in
 'cat /proc/uptime') printf '600 0\n';;
 'realpath /run/current-system') printf '%s\n' "${CURRENT_TOP:-$PWD/top}";;
 'bash --norc')
  while IFS= read -r line; do :; done
  printf '%s/top\n/nix/var/nix/profiles/system-1-link;%s/top;0\n' "$PWD" "$PWD";;
 'cat /etc/confctl/inputs-info.json')
  while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < deployed.json;;
 *) exit 98;;
esac
`)
	return f
}

func (f buildStatusDeliveryFixture) write(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f buildStatusDeliveryFixture) seed(t *testing.T, host string) {
	t.Helper()
	// Reuse requires no root-dir demand. Actual Main never writes host GC roots
	// in these fixtures; new saves are exercised below with explicit private roots.
	seedCoreGeneration(t, f.root, host, "selected", coreGenerationRecord(f.top, "2026-10-15T10:00:00Z", `{"os":"stored-revision"}`))
	if err := os.Symlink("selected", filepath.Join(f.root, ".confctl/generations", generation.SafeHost(host), "current")); err != nil {
		t.Fatal(err)
	}
}

func (f buildStatusDeliveryFixture) run(t *testing.T, args []string, input string, env ...string) (int, string, string) {
	t.Helper()
	cmd := f.command(t, args, env...)
	cmd.Stdin = strings.NewReader(input)
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	code := comparisonDeliveryExit(t, cmd.Run())
	return code, out.String(), stderr.String()
}

func TestBuildStatusDeliveryMainModes(t *testing.T) {
	for _, tc := range []struct {
		args               []string
		build, target, top bool
		want               string
	}{
		{[]string{"build", "-y", "--max-jobs=", "--cores=auto", "--show-trace"}, true, false, false, "Built generations"},
		{[]string{"status", "-y"}, true, true, true, "ok"},
		{[]string{"status", "-y", "-gnone"}, false, true, false, "ok"},
		{[]string{"status", "-y", "-gselected"}, false, false, true, "outdated"},
		{[]string{"status", "-y", "-gcurrent"}, false, false, true, "outdated"},
		{[]string{"status", "-y", "-g0"}, false, false, true, "outdated"},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			f := buildStatusDelivery(t, "node")
			f.seed(t, "node")
			before, err := os.ReadFile(filepath.Join(f.root, ".confctl/generations/node/selected/generation.json"))
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr := f.run(t, tc.args, "")
			trace := f.text("trace")
			if code != 0 || stderr != "" || !strings.Contains(out, tc.want) || strings.Contains(out, "Continue?") || len(f.logs(t)) != 0 || (strings.Count(trace, "system-build\n") == 1) != tc.build || strings.Contains(trace, "inputsInfo.node") != tc.target || strings.Contains(trace, "realpath") != tc.top {
				t.Fatal(code, out, stderr, trace)
			}
			if tc.target && tc.build && strings.Index(trace, "system-build\n") > strings.Index(trace, "inputsInfo.node") {
				t.Fatal("status did not refresh current metadata after build", trace)
			}
			if tc.args[0] == "build" {
				args := f.text("nix-args")
				if !strings.Contains(args, "--option\x00max-jobs\x00\x00") || !strings.Contains(args, "--option\x00cores\x00auto\x00") || !strings.Contains(args, "--show-trace\x00") || strings.Contains(trace, "ssh:") {
					t.Fatal("build options or selection reached the wrong adapter", args, trace)
				}
			}
			after, err := os.ReadFile(filepath.Join(f.root, ".confctl/generations/node/selected/generation.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("reuse rewrote stored metadata", err)
			}
		})
	}
}

func TestBuildStatusDeliveryMainConfirmationsAndErrors(t *testing.T) {
	for _, command := range []string{"build", "status"} {
		for _, input := range []string{"y\n", "n\n", ""} {
			t.Run(command+"/"+input, func(t *testing.T) {
				f := buildStatusDelivery(t, "node")
				f.seed(t, "node")
				code, out, stderr := f.run(t, []string{command}, input)
				accepted := input == "y\n"
				if strings.Count(out, "Continue?") != 1 || (code == 0) != accepted || (strings.Count(f.text("trace"), "system-build\n") == 1) != accepted || (len(f.logs(t)) == 1) == accepted || !accepted && !strings.Contains(stderr, "error:") {
					t.Fatal(code, out, stderr, f.text("trace"))
				}
			})
		}
		for _, extra := range [][]string{{"--help"}, {"--unknown"}, {"-y", "-a", "bad"}, {"-y", "missing"}} {
			t.Run(command+"/"+strings.Join(extra, "/"), func(t *testing.T) {
				f := buildStatusDelivery(t, "node")
				code, out, stderr := f.run(t, append([]string{command}, extra...), "")
				want := 64
				if extra[0] == "--help" {
					want = 0
				} else if extra[len(extra)-1] == "missing" {
					want = 1
				}
				if code != want || strings.Contains(f.text("trace"), "system-build") || strings.Contains(f.text("trace"), "ssh:") || want == 64 && !strings.Contains(out, "SYNOPSIS") || want == 1 && !strings.Contains(stderr, "No machines") {
					t.Fatal(code, out, stderr, f.text("trace"))
				}
			})
		}
	}
}

func TestBuildStatusDeliveryMainMissingAndEmptyGeneration(t *testing.T) {
	for _, input := range []string{"y\ny\n", "y\nn\n"} {
		f := buildStatusDelivery(t, "node", "missing")
		f.seed(t, "node")
		code, out, stderr := f.run(t, []string{"status", "-gselected"}, input)
		accepted := input == "y\ny\n"
		if (code == 0) != accepted || strings.Count(out, "Continue?") != 2 || !strings.Contains(out, "will be ignored") || strings.Contains(f.text("trace"), "ssh:missing.invalid:") || strings.Contains(f.text("trace"), "system-build") || strings.Contains(f.text("trace"), "inputsInfo.") || strings.Contains(f.text("trace"), "ssh:node.invalid:") != accepted {
			t.Fatal(code, out, stderr, f.text("trace"))
		}
	}
	f := buildStatusDelivery(t, "node")
	f.seed(t, "node")
	code, out, stderr := f.run(t, []string{"status", "-y", "-g", ""}, "")
	if code != 1 || !strings.Contains(stderr, "No generation found") || strings.Contains(f.text("trace"), "system-build") || strings.Contains(f.text("trace"), "ssh:") {
		t.Fatal(code, out, stderr, f.text("trace"))
	}
}

func TestBuildStatusDeliveryMainPartialQueriesAndFailure(t *testing.T) {
	for _, failed := range []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc", "cat /etc/confctl/inputs-info.json"} {
		f := buildStatusDelivery(t, "node")
		f.seed(t, "node")
		code, out, stderr := f.run(t, []string{"status", "-y", "-gselected"}, "", "FAIL_QUERY="+failed)
		if code != 0 || stderr != "" || !strings.Contains(out, "outdated") || len(f.logs(t)) != 0 {
			t.Fatal("ordinary remote failure changed status exit policy", code, out, stderr)
		}
		trace := f.text("trace")
		if !strings.HasSuffix(trace, "ssh:node.invalid:"+failed+"\n") {
			t.Fatal("query continued after a failed stage", trace)
		}
	}
	f := buildStatusDelivery(t, "node")
	f.seed(t, "node")
	code, out, stderr := f.run(t, []string{"status", "-y"}, "", "FAIL_BUILD=1")
	if code != 1 || !strings.Contains(stderr, "build failed") || strings.Contains(f.text("trace"), "ssh:") || len(f.logs(t)) != 1 {
		t.Fatal("failed build reached queries or lost log", code, out, stderr, f.text("trace"))
	}
	for _, env := range []string{"CURRENT_TOP=/other-toplevel", "FAIL_METADATA=1"} {
		f := buildStatusDelivery(t, "node")
		f.seed(t, "node")
		code, out, stderr := f.run(t, []string{"status", "-y"}, "", env)
		if code != 0 || stderr != "" || !strings.Contains(out, "outdated") {
			t.Fatal("target toplevel or unknown metadata was ignored", code, out, stderr)
		}
	}
}

func TestBuildStatusDeliveryMainCurrentIsNotNewest(t *testing.T) {
	for _, selector := range []string{"current", "0"} {
		f := buildStatusDelivery(t, "node")
		f.seed(t, "node")
		seedCoreGeneration(t, f.root, "node", "older", coreGenerationRecord(f.top, "2025-01-01T00:00:00Z", `{"os":"fresh-revision"}`))
		current := filepath.Join(f.root, ".confctl/generations/node/current")
		if err := os.Remove(current); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("older", current); err != nil {
			t.Fatal(err)
		}
		code, out, stderr := f.run(t, []string{"status", "-y", "-g", selector}, "")
		if code != 0 || stderr != "" || strings.Contains(out, "outdated") != (selector == "0") || strings.Contains(f.text("trace"), "system-build") || strings.Contains(f.text("trace"), "inputsInfo.") {
			t.Fatal("current and newest lost their separate stored targets", selector, code, out, stderr)
		}
	}
}

func TestBuildStatusDeliveryMainInfrastructureError(t *testing.T) {
	f := buildStatusDelivery(t, "node")
	f.seed(t, "node")
	if err := os.Chmod(filepath.Join(f.tools, "ssh"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run(t, []string{"status", "-y", "-gnone"}, "")
	if code != 1 || !strings.Contains(stderr, "error:") || len(f.logs(t)) != 1 || strings.Contains(out, "outdated") {
		t.Fatal("infrastructure failure became an ordinary status row", code, out, stderr)
	}
}

func TestBuildStatusDeliveryStatusUsesActualWriter(t *testing.T) {
	f := newBuildFixture(t)
	f.e.Inventory = []Machine{connectionMachine("one", "k1", "one.invalid", true), connectionMachine("two", "k2", "two.invalid", true)}
	tools := filepath.Join(f.e.Root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	healthScript(t, filepath.Join(tools, "nix"), `for key do :; done
case "$key" in
 .#confctl.settings) printf '{"nix":{"maxJobs":"7"}}';;
 .#confctl.machineKeys) printf '{"one":"k1","two":"k2"}';;
 *) printf '{"os":"fresh"}';;
esac
`)
	healthScript(t, filepath.Join(tools, "ssh"), `for request do :; done
case "$request" in
 'cat /proc/uptime') printf '600 0\n';;
 'realpath /run/current-system') printf '%s/top\n' "$PWD";;
 'bash --norc') while IFS= read -r line; do :; done; printf '';;
 *) printf '{"os":"fresh"}';;
esac
`)
	if err := os.WriteFile(filepath.Join(f.e.Root, "flake.nix"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	inv, _ := cli.BuiltinRegistry().Parse([]string{"status", "-y"})
	var out bytes.Buffer
	err := f.e.statusCLI(inv, strings.NewReader(""), &out, &out, f.deps())
	builds := 0
	for _, call := range f.calls {
		if call == "build" {
			builds++
		}
	}
	if err != nil || builds != 1 || f.rootCalls != 12 || !strings.Contains(out.String(), "ok") {
		t.Fatal("status failed actual Run/Writer composition", err, f.calls, f.rootCalls, out.String())
	}
	for _, host := range []string{"one", "two"} {
		list, err := generation.LoadBuildList(f.e.Root, host, generation.Options{})
		if err != nil || len(list.Builds) != 1 || list.Current == nil {
			t.Fatal(list, err)
		}
	}
}
