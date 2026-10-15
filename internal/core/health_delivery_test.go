package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Reuse the real Main subprocess and disposable Nix/command fixture. These
// tests exercise dispatch and final log disposal, rather than calling the
// health adapter or simulating its return value.
func TestHealthDeliveryMainHelpAndUsage(t *testing.T) {
	help, err := cli.BuiltinRegistry().Help([]string{"health-check"}, 80)
	if err != nil || strings.Contains(help, "Unavailable") {
		t.Fatal(help, err)
	}
	for _, args := range [][]string{{"health-check", "--help"}, {"help", "health-check"}, {"health-check", "--help", "--bad"}} {
		r := connectionCLI(t, args, "", nil, "exit 99\n", 0, false, "")
		if r.code != 0 || r.out != help || r.stderr != "" || r.trace != "" || r.log != "" {
			t.Fatal(r)
		}
	}
	for _, tc := range []struct {
		args []string
		text string
	}{
		{[]string{"--unknown"}, "invalid option"},
		{[]string{"-j08"}, "invalid argument"},
		{[]string{"-j0"}, "max-jobs must be greater than zero"},
		{[]string{"-j-999999999999999999999999999"}, "max-jobs must be greater than zero"},
	} {
		r := connectionCLI(t, append([]string{"health-check"}, tc.args...), "", nil, "exit 99\n", 0, false, "")
		if r.code != 64 || r.out != help || !strings.Contains(r.stderr, tc.text) || r.trace != "" {
			t.Fatal(r)
		}
	}
	ms := healthFixtureMachines(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("cat")+`]}`))
	r := connectionCLI(t, []string{"health-check", "-y", "-a", "bad"}, "", ms, "exit 99\n", 0700, true, "")
	if r.code != 64 || r.out != help || !strings.Contains(r.stderr, "Invalid filter") || strings.Contains(r.trace, "cat\n") {
		t.Fatal(r)
	}
}

func TestHealthDeliveryMainExecutionAndLogRetention(t *testing.T) {
	for _, tc := range []struct {
		name, input, body, diagnostic string
		yes, retain                   bool
		code                          int
	}{
		{name: "success", yes: true, body: "printf 'checked stdout\\n'\nprintf 'checked stderr\\n' >&2\n"},
		{name: "confirmed", input: "\nY\n", body: "exit 0\n"},
		{name: "refused", input: "n\n", code: 1, retain: true, diagnostic: "Aborted"},
		{name: "eof", code: 1, retain: true, diagnostic: "end of file reached"},
		{name: "predicate-failure", yes: true, body: "printf 'failure detail\\n' >&2\nexit 7\n", code: 1, retain: true, diagnostic: "Health checks failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := healthImmediateCommand("cat", "one literal argument", ";$(literal)")
			ms := healthFixtureMachines(t, healthMachineJSON("selected", "nixos", `{"builderCommands":[`+command+`]}`))
			args := []string{"health-check"}
			if tc.yes {
				args = append(args, "-y")
			}
			args = append(args, "--max-jobs=99999999999999999999999999999999", "selected")
			r := connectionCLI(t, args, tc.input, ms, tc.body, 0700, true, "")
			if r.code != tc.code || (r.log != "") != tc.retain || !strings.Contains(r.stderr, tc.diagnostic) {
				t.Fatal(r)
			}
			if tc.code == 0 && (!strings.Contains(r.out, "1 checks passed, 0 failed") || r.stderr != "") {
				t.Fatal(r)
			}
			if tc.diagnostic == "Aborted" || tc.diagnostic == "end of file reached" {
				if strings.Contains(r.trace, "cat\n") {
					t.Fatal("refusal dispatched a check", r)
				}
			} else if r.args != "one literal argument\x00;$(literal)\x00" {
				t.Fatal("health dispatch lost literal arguments", r)
			}
			if tc.yes == strings.Contains(r.out, "Continue?") {
				t.Fatal("confirmation policy changed", r)
			}
			if tc.retain {
				assertHealthDeliveryLog(t, r)
			}
		})
	}
}

func assertHealthDeliveryLog(t *testing.T, r connectionCLIResult) {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(r.root, ".confctl/logs/*-confctl-health-check.log"))
	if err != nil || len(logs) != 1 {
		t.Fatal("health log name or retention changed", logs, err)
	}
	var logged struct {
		Command []string                   `json:"command"`
		Options map[string]json.RawMessage `json:"options"`
		Args    []string                   `json:"arguments"`
	}
	first := strings.SplitN(r.log, "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &logged); err != nil || strings.Join(logged.Command, " ") != "health-check" || len(logged.Args) != 1 || logged.Args[0] != "selected" || string(logged.Options["max-jobs"]) != "99999999999999999999999999999999" {
		t.Fatal("parsed health metadata was not logged", first, err)
	}
}

func TestHealthDeliveryMainSuccessfulRetryRetainsLog(t *testing.T) {
	command := strings.Replace(healthCommandJSON("cat"), `"cooldown":3`, `"cooldown":0`, 1)
	ms := healthFixtureMachines(t, healthMachineJSON("selected", "nixos", `{"builderCommands":[`+command+`]}`))
	body := `if [ ! -f "$PWD/first-attempt" ]; then
  printf 'attempted' > "$PWD/first-attempt"
  printf 'first failure detail\n' >&2
  exit 7
fi
printf 'retry succeeded\n'
`
	r := connectionCLI(t, []string{"health-check", "-y", "--max-jobs=99999999999999999999999999999999", "selected"}, "", ms, body, 0700, true, "")
	if r.code != 0 || r.stderr != "" || strings.Count(r.trace, "cat\n") != 2 || !strings.Contains(r.out, "failed #1") || !strings.Contains(r.out, "1 checks passed, 0 failed") || !strings.Contains(r.log, "first failure detail") || !strings.Contains(r.log, "retry succeeded") {
		t.Fatal("successful retry lost its earlier failure or exit policy", r)
	}
	assertHealthDeliveryLog(t, r)
	if _, err := os.Stat(filepath.Join(r.root, "first-attempt")); err != nil {
		t.Fatal(err)
	}
}

func TestHealthDeliveryMainNoChecksAndStartupError(t *testing.T) {
	for _, tc := range []struct{ name, checks, diagnostic string }{
		{"no-checks", `{"builderCommands":[]}`, "No runnable machines to check or no health checks configured"},
		{"startup", `{"builderCommands":[` + healthImmediateCommand("absent-health-tool") + `]}`, "absent-health-tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := healthFixtureMachines(t, healthMachineJSON("selected", "nixos", tc.checks))
			r := connectionCLI(t, []string{"health-check", "-y", "selected"}, "", ms, "exit 99\n", 0, true, "")
			if r.code != 1 || r.log == "" || !strings.Contains(r.stderr, tc.diagnostic) || strings.Contains(r.out, "1 checks passed") {
				t.Fatal(r)
			}
		})
	}
}
