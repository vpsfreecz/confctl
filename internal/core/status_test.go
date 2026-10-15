package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/generation"
)

// Source-only query seam: no evaluator/child injection is invented for Status's
// best-effort target metadata. That admission remains a separate native gate.
func logProcessQueryRefusal(t *testing.T) {
	t.Helper()
	failure := errors.New("query log refused")
	e := logProcessEngine(engineLogOps{Write: func([]byte) (int, error) { return 0, failure }})
	m := connectionMachine("logical", "key", "fake.invalid", true)
	st := Status{Machine: m}
	calls := 0
	e.queryWithInputs(&st, []Machine{m}, queryOptions{Toplevel: true, Generations: true, Inputs: true}, func([]string, []byte) (Result, error) {
		calls++
		if calls == 1 {
			return Result{ExitCode: 0, Stdout: []byte("1000 0\n")}, nil
		}
		return Result{}, e.logText("query record\n")
	}, false)
	if st.Err != failure || st.Uptime == nil || st.CurrentToplevel != nil || st.Generations != nil || st.Deployed != nil || calls != 2 {
		t.Fatal(st, calls)
	}
}

// The fake executable inspects literal SSH argv and never evaluates the remote
// command or script. All capture files and records are confined to this root.
func queryFixture(t *testing.T, body string) (*Engine, Machine, string) {
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
	script := "#!" + bash + "\n" + `remote=${@: -1}
printf '%s\n' "$remote" >> "$QUERY_TRACE"
printf '%s\0' "$@" >> "$QUERY_ARGS"
if [[ "$remote" == 'bash --norc' ]]; then
  while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line" >> "$QUERY_SCRIPT"; done
fi
` + body
	if err := os.WriteFile(filepath.Join(tools, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	t.Setenv("QUERY_TRACE", filepath.Join(root, "trace"))
	t.Setenv("QUERY_ARGS", filepath.Join(root, "args"))
	t.Setenv("QUERY_SCRIPT", filepath.Join(root, "script"))
	m := connectionMachine("logical", "key", "fake.invalid", true)
	m.Profile = "/private/profile"
	return &Engine{Root: root, Context: context.Background(), Inventory: []Machine{m}}, m, root
}
func fixtureFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenerationQuerySwitchesOrderAndPartialFields(t *testing.T) {
	for _, tc := range []struct {
		name, fail                string
		options                   queryOptions
		steps                     []string
		uptime, top, gens, inputs bool
	}{
		{"none", "", queryOptions{}, []string{"cat /proc/uptime"}, true, false, false, false},
		{"status_none", "", queryOptions{Generations: true, Inputs: true}, []string{"cat /proc/uptime", "bash --norc", "cat /etc/confctl/inputs-info.json"}, true, false, true, true},
		{"listing", "", queryOptions{Generations: true}, []string{"cat /proc/uptime", "bash --norc"}, true, false, true, false},
		{"all", "", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc", "cat /etc/confctl/inputs-info.json"}, true, true, true, true},
		{"uptime_failure", "cat /proc/uptime", queryOptions{true, true, true}, []string{"cat /proc/uptime"}, false, false, false, false},
		{"top_failure", "realpath /run/current-system", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system"}, true, false, false, false},
		{"generation_failure", "bash --norc", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc"}, true, true, false, false},
		{"inputs_failure", "cat /etc/confctl/inputs-info.json", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc", "cat /etc/confctl/inputs-info.json"}, true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUERY_FAIL", tc.fail)
			e, machine, root := queryFixture(t, `[[ "$remote" == "$QUERY_FAIL" ]] && exit 255
case "$remote" in
 'cat /proc/uptime') printf '600.5 0\n' ;;
 'realpath /run/current-system') printf '\000 \t/top/../system\n' ;;
 'bash --norc') printf '' ;;
 'cat /etc/confctl/inputs-info.json') printf '%s' '{"role":{"rev":"abcdefghijk","lastModified":9007199254740993}}' ;;
 *) exit 99 ;;
esac
`)
			st := Status{Machine: machine}
			e.query(&st, []Machine{machine}, tc.options)
			if st.Err != nil || (st.Uptime != nil) != tc.uptime || (st.CurrentToplevel != nil) != tc.top || (st.Generations != nil) != tc.gens || (st.Deployed != nil) != tc.inputs {
				t.Fatalf("%+v", st)
			}
			if st.Uptime != nil && *st.Uptime != 600.5 {
				t.Fatal(st.Uptime)
			}
			if st.CurrentToplevel != nil && *st.CurrentToplevel != "/top/../system" {
				t.Fatal(st.CurrentToplevel)
			}
			if st.Generations != nil && (len(st.Generations.Generations) != 0 || st.Generations.Host != "logical") {
				t.Fatal(st.Generations)
			}
			if st.Deployed != nil && st.Deployed["role"]["lastModified"].(json.Number).String() != "9007199254740993" {
				t.Fatal(st.Deployed)
			}
			if fixtureFile(t, root, "trace") != strings.Join(tc.steps, "\n")+"\n" {
				t.Fatal(fixtureFile(t, root, "trace"))
			}
			if tc.gens || tc.fail == "bash --norc" {
				if fixtureFile(t, root, "script") != string(generation.ProfileScript(machine.Profile)) {
					t.Fatal("script bytes changed")
				}
			}
		})
	}
}

func TestGenerationQueryCarriedRoutingInputsAndFailures(t *testing.T) {
	for _, info := range []string{`{"inputs-info":{"role":"abcdefghij"}}`, `{"inputs-info":"{\"role\":\"abcdefghij\"}"}`, `{"inputs-info":false}`, `bad JSON`, `failure`} {
		t.Run(info, func(t *testing.T) {
			t.Setenv("QUERY_INFO", info)
			e, carrier, root := queryFixture(t, `case "$remote" in
 'cat /proc/uptime') printf '1000 0\n' ;;
 'realpath /carried/alias/../profile/') printf '/carried-current\n' ;;
 'bash --norc') printf '/system\n/carried/alias/../profile/-2-link;/system;0\n' ;;
 'cat /carried/alias/../profile/machine.json') [[ "$QUERY_INFO" == failure ]] && exit 1; printf '%s' "$QUERY_INFO" ;;
 'cat /carried/alias/../profile/etc/confctl/inputs-info.json') printf '{"role":"abcdefghij"}' ;;
 *) exit 99 ;;
esac
`)
			carrier.Name = "carrier"
			carrier.Target.Port = 2222
			logical := connectionMachine("logical", "logical", "unused.invalid", false)
			logical.Profile = "/carried/alias/../profile/"
			logical.CarrierName = &carrier.Name
			t.Setenv("CONFCTL_SSH_CONFIG", "private ssh/config")
			st := Status{Machine: logical}
			e.query(&st, []Machine{logical, carrier}, queryOptions{true, true, true})
			if st.Err != nil || st.Uptime == nil || st.CurrentToplevel == nil || *st.CurrentToplevel != "/carried-current" || st.Generations == nil || st.Generations.Host != "logical" || len(st.Generations.Generations) != 1 || st.Deployed["role"]["rev"] != "abcdefghij" {
				t.Fatal(st)
			}
			args := strings.Split(fixtureFile(t, root, "args"), "\x00")
			want := []string{"-F", "private ssh/config", "-l", "root", "-p", "2222", "fake.invalid"}
			if !reflect.DeepEqual(args[:len(want)], want) || strings.Contains(fixtureFile(t, root, "args"), "unused.invalid") {
				t.Fatal(args)
			}
			if fixtureFile(t, root, "script") != string(generation.ProfileScript(logical.Profile)) {
				t.Fatal("carrier replaced logical profile")
			}
			fallback := strings.Contains(fixtureFile(t, root, "trace"), "profile/etc/confctl")
			if fallback != (info == `{"inputs-info":false}` || info == `bad JSON` || info == `failure`) {
				t.Fatal(info, fixtureFile(t, root, "trace"))
			}
		})
	}
	// Missing carrier is diagnosed without a child; targetless ordinary machines
	// remain query errors instead of silently disappearing from generation ls.
	e, m, root := queryFixture(t, "exit 99\n")
	missing := "absent"
	m.CarrierName = &missing
	st := Status{Machine: m}
	e.query(&st, []Machine{m}, queryOptions{Generations: true})
	if st.Err == nil || st.Err.Error() != "Carrier absent not found in machine list" || fixtureFile(t, root, "trace") != "" {
		t.Fatal(st)
	}
	m.CarrierName = nil
	m.Target.Host = nil
	st = Status{Machine: m}
	e.query(&st, []Machine{m}, queryOptions{Generations: true})
	if st.Err == nil || fixtureFile(t, root, "trace") != "" {
		t.Fatal(st)
	}
}

func TestGenerationQueryInfrastructureAndMalformedInputs(t *testing.T) {
	e, m, root := queryFixture(t, `case "$remote" in
 'cat /proc/uptime') printf '100 0\n' ;;
 'bash --norc') printf '/system\n/private/profile-1-link;/system;0\n' ;;
 'cat /etc/confctl/inputs-info.json') printf 'bad JSON' ;;
esac
`)
	st := Status{Machine: m}
	e.query(&st, []Machine{m}, queryOptions{Generations: true, Inputs: true})
	if st.Err != nil || st.Generations == nil || st.Deployed != nil {
		t.Fatal(st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.Context = ctx
	st = Status{Machine: m}
	e.query(&st, []Machine{m}, queryOptions{Generations: true})
	if !errors.Is(st.Err, context.Canceled) || st.Uptime != nil {
		t.Fatal(st)
	}
	e.Context = context.Background()
	if err := os.Chmod(filepath.Join(root, "tools", "ssh"), 0600); err != nil {
		t.Fatal(err)
	}
	st = Status{Machine: m}
	e.query(&st, []Machine{m}, queryOptions{Generations: true})
	if st.Err == nil || st.Uptime != nil {
		t.Fatal(st)
	}
}
