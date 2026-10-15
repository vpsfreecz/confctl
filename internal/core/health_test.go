package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/health"
)

// Synthetic evaluated-module source: defaults are explicit, never filled in by
// the test or adapter. No Nix evaluation or Ruby capture establishes this input.
const healthCommandDefaults = `"description":"","exitStatus":0,"standardOutput":{"match":null,"include":[],"exclude":[]},"standardError":{"match":null,"include":[],"exclude":[]},"timeout":60,"cooldown":3`

func healthCommandJSON(argv ...string) string {
	args, _ := json.Marshal(argv)
	return `{"command":` + string(args) + `,` + healthCommandDefaults + `}`
}

func healthMachineJSON(name, spin, healthChecks string) string {
	return fmt.Sprintf(`{"name":%q,"key":"key","clusterName":"cluster","metaConfig":{"managed":true,"spin":%q,"host":{"target":"localhost","fqdn":"m.example"},"healthChecks":%s,"enabled":false,"number":9007199254740993,"arr":[1,"x"],"object":{"z":2,"a":1}}}`, name, spin, healthChecks)
}

func healthFixtureMachines(t *testing.T, entries ...string) []Machine {
	t.Helper()
	fields := make([]string, len(entries))
	for i, entry := range entries {
		fields[i] = fmt.Sprintf(`"entry%d":%s`, i, entry)
	}
	machines, err := DecodeMachines([]byte("{" + strings.Join(fields, ",") + "}"))
	if err != nil {
		t.Fatal(err)
	}
	return machines
}

func TestHealthOrderedSchemaSubstitutionAndConstructionCount(t *testing.T) {
	builder := healthCommandJSON("fixture", "{name}", "{key}|{machineKey}|{flakeKey}", "{host.fqdn}", "{number}", "{enabled}", "{absent}", "{checks}", "{arr}", "{object}", ";$(literal)")
	property := `{"property":"SystemState","value":"running","timeout":60,"cooldown":3}`
	checks := `{"builderCommands":[` + builder + `],"systemd":{"enable":true,"systemProperties":[` + property + `],"unitProperties":{"z*.service":[` + property + `],"a.service":[` + property + `]}},"machineCommands":[` + healthCommandJSON("fixture", "{checks}") + `]}`
	engine := &Engine{Context: context.Background(), Inventory: healthFixtureMachines(t, healthMachineJSON("fleet/a", "nixos", checks))}
	plan, err := engine.PlanHealth(HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Machines) != 1 || len(plan.Checks) != 5 || plan.Workers.Int64() != 5 {
		t.Fatalf("plan %#v", plan)
	}
	wantArgv := []string{"fixture", "fleet/a", "key|key|key", "m.example", "9007199254740993", "false", "", "0", `[1, "x"]`, `{"z" => 2, "a" => 1}`, ";$(literal)"}
	if !reflect.DeepEqual(plan.Checks[0].Argv, wantArgv) || plan.Checks[0].Remote || plan.Checks[0].Description != ShellJoin(wantArgv) {
		t.Fatalf("substitution/fallback: %#v", plan.Checks[0])
	}
	for i, want := range [][]string{{"systemctl", "show"}, {"systemctl", "show", "z*.service"}, {"systemctl", "show", "a.service"}, {"fixture", "4"}} {
		if !reflect.DeepEqual(plan.Checks[i+1].Argv, want) || !plan.Checks[i+1].Remote {
			t.Fatalf("source order[%d]: %#v", i+1, plan.Checks[i+1])
		}
	}
	if plan.Checks[0].Timeout.Int64() != 60 || plan.Checks[0].Cooldown.Int64() != 3 || plan.Checks[0].Command.ExpectedStatus.Sign() != 0 || plan.Checks[0].Command.Stdout.Match != nil {
		t.Fatal("evaluated defaults changed")
	}
}

func TestHealthSelectionBeforeSchemaAndNoChecks(t *testing.T) {
	valid := healthMachineJSON("selected", "nixos", `{"builderCommands":[`+healthCommandJSON("fixture")+`]}`)
	unmanaged := strings.Replace(healthMachineJSON("unmanaged", "nixos", `{"unsupported":0}`), `"managed":true`, `"managed":false`, 1)
	carried := strings.Replace(healthMachineJSON("carried", "nixos", `{"unsupported":0}`), `"key":"key"`, `"carrier":"carrier","key":"key"`, 1)
	noTarget := strings.Replace(healthMachineJSON("no-target", "nixos", `{"unsupported":0}`), `"target":"localhost"`, `"target":null`, 1)
	noChecks := healthMachineJSON("empty", "vpsadminos", `{"builderCommands":[],"machineCommands":[],"systemd":{"enable":true,"invalid":"ignored for other spin"}}`)
	engine := &Engine{Context: context.Background(), Inventory: healthFixtureMachines(t, unmanaged, carried, valid, noTarget, noChecks)}
	plan, err := engine.PlanHealth(HealthRequest{})
	if err != nil || len(plan.Machines) != 1 || plan.Machines[0].Name != "selected" {
		t.Fatal(plan, err)
	}
	for _, request := range []HealthRequest{{Pattern: healthTestText("absent")}, {Attrs: []string{"name=empty"}}, {Tags: []string{"missing"}}} {
		if _, err := engine.PlanHealth(request); !errors.Is(err, health.ErrNoChecks) {
			t.Fatal(err)
		}
	}
	if _, err := engine.PlanHealth(HealthRequest{Attrs: []string{"malformed"}}); err == nil {
		t.Fatal("invalid selection accepted")
	}
	for _, workers := range []*big.Int{big.NewInt(0), big.NewInt(-1)} {
		// No cached inventory and no flake: validation must precede loading it.
		engine := &Engine{Context: context.Background(), Root: t.TempDir()}
		if _, err := engine.PlanHealth(HealthRequest{Workers: workers}); err == nil || err.Error() != "max-jobs must be greater than zero" {
			t.Fatalf("worker validation ran late: %v", err)
		}
	}
}

func healthTestText(value string) *string { return &value }

func TestHealthMalformedMetadataIsTypedAndNeverInventsDefaults(t *testing.T) {
	for _, checks := range []string{
		`{"unknown":[]}`, `{"builderCommands":null}`, `{"builderCommands":[{"command":["fixture"]}]}`,
		`{"builderCommands":[` + strings.Replace(healthCommandJSON("fixture"), `"timeout":60`, `"timeout":-1`, 1) + `]}`,
		`{"builderCommands":[` + strings.Replace(healthCommandJSON("fixture"), `"match":null`, `"match":false`, 1) + `]}`,
		`{"builderCommands":[` + strings.Replace(healthCommandJSON("fixture"), `"description":""`, `"description":null`, 1) + `]}`,
		`{"builderCommands":[` + strings.Replace(healthCommandJSON("fixture"), `"command":["fixture"]`, `"command":[]`, 1) + `]}`,
		`{"systemd":{"enable":true,"systemProperties":[],"unitProperties":{"empty.service":[]}}}`,
		`{"systemd":{"enable":null}}`,
	} {
		engine := &Engine{Context: context.Background(), Inventory: healthFixtureMachines(t, healthMachineJSON("m", "nixos", checks))}
		_, err := engine.PlanHealth(HealthRequest{})
		var schema *health.SchemaError
		if !errors.As(err, &schema) {
			t.Fatalf("malformed metadata accepted/untyped: %s: %v", checks, err)
		}
	}
	m := Machine{Name: "missing-raw", Attributes: map[string]any{"healthChecks": map[string]any{}}}
	if _, err := m.healthSpecs(); err == nil {
		t.Fatal("unordered map fallback invented source order")
	}
	checks := `{"systemd":{"enable":false,"invalid":null},"builderCommands":[],"machineCommands":"skip"}`
	m = healthFixtureMachines(t, strings.Replace(healthMachineJSON("m", "nixos", checks), `"target":"localhost"`, `"target":null`, 1))[0]
	if got, err := m.healthSpecs(); err != nil || len(got) != 0 {
		t.Fatal("disabled/non-runnable groups decoded", got, err)
	}
}

func TestHealthRawDuplicateReplacementAndExplicitWhitespaceDescription(t *testing.T) {
	old := healthCommandJSON("old")
	command := strings.Replace(healthCommandJSON("new", "{checks}"), `"description":""`, `"description":" "`, 1)
	checks := `{"builderCommands":[` + old + `],"machineCommands":[` + healthCommandJSON("remote", "{checks}") + `],"builderCommands":[` + command + `]}`
	machine := healthFixtureMachines(t, healthMachineJSON("m", "nixos", checks))[0]
	specs, err := machine.healthSpecs()
	if err != nil || len(specs) != 2 || !reflect.DeepEqual(specs[0].Argv, []string{"new", "0"}) || !reflect.DeepEqual(specs[1].Argv, []string{"remote", "1"}) || specs[0].Description != " " {
		t.Fatalf("duplicate replacement/order or description normalized: %#v, %v", specs, err)
	}
}

func TestHealthAdapterSeparateStreamsLiteralSSHAndRetention(t *testing.T) {
	checks := `{"machineCommands":[` + strings.Replace(healthCommandJSON("fixture", ";$(literal)", "line\narg"), `"exitStatus":0`, `"exitStatus":255`, 1) + `]}`
	machine := healthFixtureMachines(t, healthMachineJSON("m", "nixos", checks))[0]
	host := "host.example"
	machine.Target = Target{Host: &host, Port: 2222}
	engine := &Engine{Context: context.Background(), Root: t.TempDir(), Inventory: []Machine{machine}}
	plan, err := engine.PlanHealth(HealthRequest{Workers: big.NewInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFCTL_SSH_CONFIG", "literal$conf")
	var argv []string
	result, err := engine.runHealth(plan, health.Deps{}, func(ctx context.Context, root string, args []string, input []byte) (Result, error) {
		argv = append([]string(nil), args...)
		if root != engine.Root || input != nil || ctx.Err() != nil {
			t.Fatal("process context/root/input changed")
		}
		return Result{ExitCode: 255, Stdout: []byte("stdout\n"), Stderr: []byte("stderr\n")}, nil
	})
	want := []string{"ssh", "-F", "literal$conf", "-l", "root", "-p", "2222", "host.example", ShellJoin(plan.Checks[0].Argv)}
	if err != nil || !result.Report.Results[0].Passed() || !reflect.DeepEqual(argv, want) || result.RetainLog {
		t.Fatal(result, err, argv)
	}
	// Full stdout matching must fail while stderr remains independently checked.
	plan.Checks[0].Command.ExpectedStatus = big.NewInt(0)
	plan.Checks[0].Command.Stdout.Match = healthTestText("stdout")
	plan.Checks[0].Timeout = big.NewInt(0)
	now := time.Unix(0, 0)
	attempts := 0
	result, err = engine.runHealth(plan, health.Deps{Now: func() time.Time { return now }, Cooldown: func(context.Context, *big.Int) error { return nil }}, func(context.Context, string, []string, []byte) (Result, error) {
		attempts++
		if attempts == 1 {
			return Result{ExitCode: 1, Stdout: []byte("stdout\n"), Stderr: []byte("stderr\n")}, nil
		}
		return Result{ExitCode: 0, Stdout: []byte("stdout")}, nil
	})
	if err != nil || attempts != 2 || !result.Report.Results[0].Passed() || !result.RetainLog {
		t.Fatal("failed-then-success lost retention", result, err)
	}
	// This unit returns a retention requirement; it does not change CloseLog.
}

func TestHealthAdapterProcessErrorsAndSourceDiagnostics(t *testing.T) {
	engine := &Engine{Context: context.Background(), Inventory: healthFixtureMachines(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthCommandJSON("fixture", "literal$")+`]}`))}
	plan, err := engine.PlanHealth(HealthRequest{Workers: big.NewInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []error{errors.New("spawn"), errors.New("wait"), context.Canceled} {
		result, err := engine.runHealth(plan, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) { return Result{}, sentinel })
		if !errors.Is(err, sentinel) || result.Report.Results[0].Complete || result.Report.Results[0].Passed() {
			t.Fatal("process failure became success", result, err)
		}
	}
	sentinel := errors.New("non-exit wait failure")
	if result, err := engine.runHealth(plan, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
		return Result{ExitCode: 0, waitErr: sentinel}, nil
	}); !errors.Is(err, sentinel) || result.Report.Results[0].Passed() {
		t.Fatal("wait metadata was ignored", result, err)
	}
	if result, err := engine.runHealth(plan, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
		return Result{ExitCode: -1}, nil
	}); err == nil || err.Error() != "health command did not return an exit status" || result.Report.Results[0].Passed() {
		t.Fatal("absent exit status became predicate data/success", result, err)
	}
	failed := health.Result{Check: plan.Checks[0], Failures: []health.Failure{{Kind: "status", Status: 1}, {Kind: "exclude", Stream: "standard error", Fragment: ""}}}
	if got := HealthMessages(failed); got != `fixture literal\$ failed with 1 (); fixture literal\$: standard error includes ""` {
		t.Fatal(got)
	}
	pattern := "unit*"
	failed.Check = health.Check{Argv: []string{"systemctl", "show", pattern}, Pattern: &pattern}
	failed.Failures = []health.Failure{{Kind: "missing-property", Property: "Missing"}, {Kind: "property", Property: "P", Actual: "bad\r", Want: "good"}}
	if got := HealthMessages(failed); got != "unit*: property \"Missing\" not found; P=bad\r, expected good" {
		t.Fatal(got)
	}
}

func TestHealthExistingEngineProcessLocalFixture(t *testing.T) {
	argv := []string{os.Args[0], "-test.run=^TestHealthProcessHelper$", "--", "fixture"}
	command := strings.Replace(healthCommandJSON(argv...), `"exitStatus":0`, `"exitStatus":7`, 1)
	command = strings.Replace(command, `"match":null`, `"match":"body\n"`, 1)
	engine := &Engine{Context: context.Background(), Root: t.TempDir(), Inventory: healthFixtureMachines(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+command+`]}`))}
	t.Setenv("CONFCTL_HEALTH_FIXTURE", "yes")
	if err := os.WriteFile(filepath.Join(engine.Root, "fixture-root"), []byte("root"), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(engine.Root, "fixture.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	engine.Log = log
	engine.logOwner = nativeEngineLog(log)
	plan, err := engine.PlanHealth(HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunHealth(plan, nil)
	if err != nil || !result.Report.Results[0].Passed() {
		t.Fatal(result, err)
	}
	data, err := os.ReadFile(log.Name())
	if err != nil || !strings.Contains(string(data), "body") || !strings.Contains(string(data), "diagnostic") || !strings.Contains(string(data), "exit status 7") {
		t.Fatal("existing process logging missing", string(data), err)
	}
}

func TestHealthProcessHelper(t *testing.T) {
	if os.Getenv("CONFCTL_HEALTH_FIXTURE") != "yes" || len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" || os.Args[len(os.Args)-1] != "fixture" {
		return
	}
	if data, err := os.ReadFile("fixture-root"); err != nil || string(data) != "root" {
		os.Exit(99)
	}
	fmt.Fprint(os.Stdout, "body\n")
	fmt.Fprint(os.Stderr, "diagnostic\n")
	os.Exit(7)
}
