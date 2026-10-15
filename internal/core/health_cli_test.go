package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/health"
)

func healthInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"health-check"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func healthCLIEngine(t *testing.T, entries ...string) *Engine {
	t.Helper()
	return &Engine{Root: t.TempDir(), Context: context.Background(), Color: "never",
		Inventory:     healthFixtureMachines(t, entries...),
		SettingsCache: map[string]any{"list": map[string]any{"columns": []any{"name", "host.target"}}}}
}

func healthImmediateCommand(argv ...string) string {
	command := healthCommandJSON(argv...)
	command = strings.Replace(command, `"timeout":60`, `"timeout":0`, 1)
	return strings.Replace(command, `"cooldown":3`, `"cooldown":0`, 1)
}

func healthScript(t *testing.T, path, body string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!"+sh+"\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestHealthCLISelectionConfirmationAndReturnPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, input, wantErr string
		yes                  bool
	}{
		{"yes-option", "", "", true},
		{"confirmed", "invalid\n\nY\n", "", false},
		{"declined", "n\n", "Aborted", false},
		{"exhausted", "", "end of file reached", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := healthImmediateCommand("fixture", "literal arg")
			engine := healthCLIEngine(t,
				healthMachineJSON("selected", "nixos", `{"builderCommands":[`+command+`]}`),
				healthMachineJSON("other", "nixos", `{"unknown":[]}`))
			args := []string{"selected"}
			if tc.yes {
				args = []string{"--yes", "selected"}
			}
			calls := 0
			var out bytes.Buffer
			result, err := engine.healthCheck(healthInvocation(t, args...), strings.NewReader(tc.input), &out, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
				calls++
				return Result{ExitCode: 0}, nil
			})
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr || calls != 0 || len(result.Report.Results) != 0 {
					t.Fatal("confirmation executed checks or lost refusal", err, calls, result)
				}
			} else if err != nil || calls != 1 || !result.Report.Results[0].Passed() {
				t.Fatal(result, calls, err)
			}
			if tc.yes {
				if strings.Contains(out.String(), "Continue?") || strings.Contains(out.String(), "CHECKS") {
					t.Fatal("--yes prompted")
				}
			} else if !strings.Contains(out.String(), "CHECKS") || !strings.Contains(out.String(), "1 checks in total") || !strings.Contains(out.String(), "Continue?") || strings.Contains(out.String(), "other") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestHealthCLINoChecksAndInvalidWorkersBeforeEffects(t *testing.T) {
	for _, args := range [][]string{{}, {"missing"}, {"-a", "name=absent"}, {"-t", "absent"}} {
		engine := healthCLIEngine(t, healthMachineJSON("empty", "nixos", `{"builderCommands":[]}`))
		var out bytes.Buffer
		_, err := engine.HealthCheck(healthInvocation(t, args...), strings.NewReader("y\n"), &out)
		if !errors.Is(err, health.ErrNoChecks) || out.Len() != 0 {
			t.Fatal(err, out.String())
		}
	}
	for _, workers := range []string{"0", "-3"} {
		// An uncached Engine without flake.nix must refuse the worker count
		// before inventory loading, confirmation, or process execution.
		engine := &Engine{Root: t.TempDir(), Context: context.Background()}
		var out bytes.Buffer
		_, err := engine.HealthCheck(healthInvocation(t, "--max-jobs="+workers), strings.NewReader("y\n"), &out)
		if err == nil || err.Error() != "max-jobs must be greater than zero" || out.Len() != 0 {
			t.Fatal(err, out.String())
		}
	}
}

func TestHealthCLIRealNixLocalAndSSHChildren(t *testing.T) {
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local := healthImmediateCommand("local-check", "{name}", "one literal argument")
	local = strings.Replace(local, `"match":null`, `"match":"local stdout\n"`, 1)
	remote := healthImmediateCommand("remote-check", "{host.fqdn}", "a;$(literal)")
	remote = strings.Replace(remote, `"exitStatus":0`, `"exitStatus":255`, 1)
	remote = strings.Replace(remote, `"standardError":{"match":null`, `"standardError":{"match":"remote stderr\n"`, 1)
	machine := strings.Replace(healthMachineJSON("selected", "nixos", `{"builderCommands":[`+local+`],"machineCommands":[`+remote+`]}`), `"target":"localhost"`, `"target":"fake.invalid"`, 1)
	machine = strings.Replace(machine, `"host":{`, `"tags":["wanted"],"host":{`, 1)
	other := healthMachineJSON("other", "nixos", `{"unknown":[]}`)
	inventory := `{"selected":` + machine + `,"other":` + other + `}`
	settings := `{"list":{"columns":["name"]},"nix":{"maxJobs":"auto","impureEval":false}}`
	build, _ := json.Marshal([]map[string]any{{"outputs": map[string]string{"out": filepath.Join(root, "machines.json")}}})
	if err := os.WriteFile(filepath.Join(root, "machines.json"), []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	healthScript(t, filepath.Join(tools, "nix"), "case \"$1\" in\neval) printf '%s' "+quote(settings)+";;\nbuild) printf '%s' "+quote(string(build))+";;\n*) exit 99;;\nesac\n")
	healthScript(t, filepath.Join(tools, "local-check"), "[ \"$1\" = selected ] && [ \"$2\" = 'one literal argument' ] || exit 99\nprintf 'local stdout\\n'\nprintf 'local stderr\\n' >&2\n")
	sshTrace := filepath.Join(root, "ssh-args")
	healthScript(t, filepath.Join(tools, "ssh"), "printf '%s\\n' \"$@\" > "+quote(sshTrace)+"\nprintf 'remote stdout\\n'\nprintf 'remote stderr\\n' >&2\nexit 255\n")
	t.Setenv("PATH", tools)
	engine := &Engine{Root: root, Context: context.Background()}
	if err := engine.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	logPath := engine.Log.Name()
	t.Cleanup(func() { _ = engine.CloseLog(false) })
	var out bytes.Buffer
	result, err := engine.HealthCheck(healthInvocation(t, "-y", "-t", "wanted", "--max-jobs=99999999999999999999999999999999"), strings.NewReader(""), &out)
	if err != nil || len(result.Report.Results) != 2 || result.RetainLog || !result.Report.Results[0].Passed() || !result.Report.Results[1].Passed() || !strings.Contains(out.String(), "2 checks passed, 0 failed") {
		t.Fatal(result, err, out.String())
	}
	trace, err := os.ReadFile(sshTrace)
	if err != nil || !strings.Contains(string(trace), "fake.invalid") || !strings.Contains(string(trace), ShellJoin([]string{"remote-check", "m.example", "a;$(literal)"})) {
		t.Fatal("SSH adapter did not dispatch the selected literal command", string(trace), err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil || !bytes.Contains(log, []byte("local stderr")) || !bytes.Contains(log, []byte("remote stderr")) || !bytes.Contains(log, []byte("exit status 255")) {
		t.Fatal("current Engine process logging was not propagated", string(log), err)
	}
	// This is the exact shared-CLI finalization boundary handed to imp3.
	if err := engine.CloseLog(!result.RetainLog); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("successful checks retained a disposable success log", err)
	}
}

func TestHealthCLIFailedChildrenAndOrderedSummary(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "failed-check")
	healthScript(t, tool, "printf 'synthetic failure\\n' >&2\nexit 7\n")
	check := healthImmediateCommand(tool)
	engine := healthCLIEngine(t,
		healthMachineJSON("z-first", "nixos", `{"builderCommands":[`+check+`,`+check+`]}`),
		healthMachineJSON("a-second", "nixos", `{"builderCommands":[`+check+`]}`))
	if err := engine.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	path := engine.Log.Name()
	t.Cleanup(func() { _ = engine.CloseLog(false) })
	var out bytes.Buffer
	result, err := engine.HealthCheck(healthInvocation(t, "-y", "-j", "2"), strings.NewReader(""), &out)
	text := out.String()
	if err == nil || err.Error() != "Health checks failed" || !result.RetainLog || !strings.Contains(text, "0 checks passed, 3 failed") || strings.Index(text, "z-first: 2 failures") >= strings.Index(text, "a-second: 1 failures") {
		t.Fatal(result, err, text)
	}
	if err := engine.CloseLog(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failure log was removed", err)
	}
}

func TestHealthCLIRetrySuccessRetainsLog(t *testing.T) {
	engine := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
	if err := engine.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	path := engine.Log.Name()
	t.Cleanup(func() { _ = engine.CloseLog(false) })
	attempts := 0
	var out bytes.Buffer
	result, err := engine.healthCheck(healthInvocation(t, "-y"), strings.NewReader(""), &out, health.Deps{Now: func() time.Time { return time.Unix(0, 0) }, Cooldown: func(context.Context, *big.Int) error { return nil }}, func(context.Context, string, []string, []byte) (Result, error) {
		attempts++
		if attempts == 1 {
			return Result{ExitCode: 7}, nil
		}
		return Result{ExitCode: 0}, nil
	})
	if err != nil || attempts != 2 || !result.RetainLog || !strings.Contains(out.String(), "failed #1") || !strings.Contains(out.String(), "1 checks passed, 0 failed") {
		t.Fatal(result, err, out.String())
	}
	if err := engine.CloseLog(!result.RetainLog); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("successful retry erased its failed-attempt log", err)
	}
}

func TestHealthCLIParsedOptionLog(t *testing.T) {
	engine := &Engine{Root: t.TempDir(), Context: context.Background()}
	if err := engine.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.CloseLog(false) })
	inv := healthInvocation(t, "-y", "--max-jobs=99999999999999999999999999999999", "-a", "spin=nixos", "node*")
	if err := engine.LogHealthCLI(inv); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(engine.Log.Name())
	if err != nil {
		t.Fatal(err)
	}
	var logged struct {
		Command []string                   `json:"command"`
		Options map[string]json.RawMessage `json:"options"`
		Args    []string                   `json:"arguments"`
	}
	if err := json.Unmarshal(data, &logged); err != nil || strings.Join(logged.Command, " ") != "health-check" || strings.Join(logged.Args, " ") != "node*" || string(logged.Options["yes"]) != "true" || string(logged.Options["max-jobs"]) != "99999999999999999999999999999999" || string(logged.Options["attr"]) != `["spin=nixos"]` {
		t.Fatal("parsed invocation log lost option meaning", err)
	}
}

func TestHealthCLIStartupAndCancellationPartialResults(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		engine := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand(filepath.Join(t.TempDir(), "absent-tool"))+`]}`))
		var out bytes.Buffer
		result, err := engine.HealthCheck(healthInvocation(t, "-y"), strings.NewReader(""), &out)
		if err == nil || len(result.Report.Results) != 1 || result.Report.Results[0].Passed() || !strings.Contains(out.String(), "1 incomplete") {
			t.Fatal(result, err, out.String())
		}
	})
	t.Run("cancel-joined-workers", func(t *testing.T) {
		check := healthImmediateCommand("fixture")
		engine := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+check+`,`+check+`,`+check+`]}`))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		engine.Context = ctx
		entered := make(chan struct{}, 2)
		var mu sync.Mutex
		calls, exited := 0, 0
		var result HealthResult
		var err error
		var out bytes.Buffer
		done := make(chan struct{})
		go func() {
			defer close(done)
			result, err = engine.healthCheck(healthInvocation(t, "-y", "-j", "2"), strings.NewReader(""), &out, health.Deps{}, func(ctx context.Context, _ string, _ []string, _ []byte) (Result, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				entered <- struct{}{}
				<-ctx.Done()
				mu.Lock()
				exited++
				mu.Unlock()
				return Result{}, ctx.Err()
			})
		}()
		<-entered
		<-entered
		cancel()
		<-done
		if !errors.Is(err, context.Canceled) || calls != 2 || exited != 2 || result.Report.Results[2].Started || !strings.Contains(out.String(), "3 incomplete") {
			t.Fatal("cancellation did not join bounded workers", result, err, calls, exited)
		}
	})
}

func TestHealthCLIProcessCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := healthImmediateCommand(os.Args[0], "-test.run=^TestHealthCancelChild$", "--", "health-cancel-child")
	engine := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+command+`]}`))
	engine.Context = ctx
	t.Setenv("CONFCTL_HEALTH_CANCEL_CHILD", "yes")
	if err := engine.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.CloseLog(false) })
	var ready sync.Once
	detach, err := engine.attachLog(func(chunk []byte) error {
		if bytes.Contains(chunk, []byte("health child ready")) {
			ready.Do(cancel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	var out bytes.Buffer
	result, err := engine.HealthCheck(healthInvocation(t, "-y"), strings.NewReader(""), &out)
	if !errors.Is(err, context.Canceled) || result.Report.Results[0].Passed() || result.Report.Results[0].Attempts != 1 {
		t.Fatal("actual child cancellation became success or timed out", result, err)
	}
}

func TestHealthCancelChild(t *testing.T) {
	if os.Getenv("CONFCTL_HEALTH_CANCEL_CHILD") != "yes" || os.Args[len(os.Args)-1] != "health-cancel-child" {
		return
	}
	fmt.Fprintln(os.Stdout, "health child ready")
	// The parent cancels only after receiving this child's real logged output.
	// This finite fallback bounds a broken cancellation path in the test child.
	<-time.After(30 * time.Second)
	os.Exit(99)
}

type healthFailWriter struct{ err error }

func (w healthFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHealthCLIOutputFailureBeforeCommands(t *testing.T) {
	engine := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
	sentinel := fmt.Errorf("synthetic output failure")
	called := false
	_, err := engine.healthCheck(healthInvocation(t, "-y"), strings.NewReader(""), healthFailWriter{sentinel}, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
		called = true
		return Result{}, nil
	})
	if !errors.Is(err, sentinel) || called {
		t.Fatal(err, called)
	}
	if err := healthWrite(io.Discard, ""); err != nil {
		t.Fatal(err)
	}
}
