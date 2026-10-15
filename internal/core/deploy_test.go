package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/health"
)

func deployInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"deploy"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}
func deployTestMachine(name string) Machine {
	host := name + ".invalid"
	return Machine{Name: name, Managed: true, Target: Target{Host: &host, Port: 22}, Profile: "/nix/var/nix/profiles/system", Attributes: map[string]any{"autoRollback": map[string]any{"enable": false}}}
}
func deployTestGeneration(name string) *generation.Build {
	return &generation.Build{Name: name, Toplevel: "/fixture/" + name}
}

func TestDeployDeliveryPrepareRebindAndOrders(t *testing.T) {
	for _, one := range []bool{false, true} {
		t.Run(map[bool]string{false: "bulk", true: "one-by-one"}[one], func(t *testing.T) {
			a, b := deployTestMachine("a"), deployTestMachine("b")
			var trace []string
			calls := 0
			d := deployCommandDeps{
				Machines: func() ([]Machine, error) {
					calls++
					if calls == 1 {
						return []Machine{a, b}, nil
					}
					fresh := a
					fresh.Profile = "/fresh/profile"
					return []Machine{b, fresh}, nil
				},
				Prepare: func(names []string) (int, error) {
					trace = append(trace, "prepare:"+strings.Join(names, ","))
					return 0, nil
				},
				Build: func(names []string) ([]deploy.Target, error) {
					trace = append(trace, "build:"+strings.Join(names, ","))
					return []deploy.Target{{Host: "b", Generation: deployTestGeneration("gb")}, {Host: "a", Generation: deployTestGeneration("ga")}}, nil
				},
				Operations: func(_ context.Context, all, selected []Machine, _ commandLine, _ io.Writer, _ cli.Invocation) deploy.Deps {
					if selected[0].Name != "a" || selected[1].Name != "b" || selected[0].Profile != "/fresh/profile" || all[0].Name != "b" {
						t.Fatal("hook rebind lost selected identity/order/fresh metadata")
					}
					return deploy.Deps{
						QueryTarget:         func(_ context.Context, m deploy.Machine) (deploy.Status, error) { return deploy.Status{}, nil },
						AutoRollbackDefault: func(deploy.Machine) (bool, error) { return false, nil },
						Copy: func(_ context.Context, m deploy.Machine, _ []string, _ func(deploy.CopyProgress)) (bool, error) {
							trace = append(trace, "copy:"+m.Name)
							return true, nil
						},
						Activate: func(_ context.Context, m deploy.Machine, _ *generation.Build, _ deploy.Action) (bool, error) {
							trace = append(trace, "activate:"+m.Name)
							return true, nil
						},
						SetProfile: func(_ context.Context, m deploy.Machine, _ string) (bool, error) {
							trace = append(trace, "profile:"+m.Name)
							return true, nil
						},
					}
				},
			}
			args := []string{"--yes", "--no-health-checks", "--max-concurrent-copy", "1"}
			if one {
				args = append(args, "--one-by-one")
			}
			e := &Engine{Context: context.Background()}
			report, err := e.deployCommand(deployInvocation(t, args...), strings.NewReader(""), io.Discard, d)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"prepare:a,b", "build:a,b", "copy:a", "copy:b", "activate:b", "profile:b", "activate:a", "profile:a"}
			if one {
				want = []string{"prepare:a,b", "build:a,b", "copy:b", "activate:b", "profile:b", "copy:a", "activate:a", "profile:a"}
			}
			if !reflect.DeepEqual(trace, want) || len(report.Outcomes) != 2 || report.Outcomes[0].Target.Host != "b" {
				t.Fatalf("source order differs: %v", trace)
			}
		})
	}
}

func TestDeployDeliveryRefusalHookAndAction(t *testing.T) {
	for _, kind := range []string{"decline", "hook-status", "hook-error", "bad-action", "bad-reboot", "missing-after-hook"} {
		t.Run(kind, func(t *testing.T) {
			prepared, built, calls := 0, 0, 0
			cause := errors.New("prepare failed")
			d := deployCommandDeps{Machines: func() ([]Machine, error) {
				calls++
				if kind == "missing-after-hook" && calls > 1 {
					return nil, nil
				}
				return []Machine{deployTestMachine("a")}, nil
			}, Prepare: func([]string) (int, error) {
				prepared++
				if kind == "hook-status" {
					return 7, nil
				}
				if kind == "hook-error" {
					return 0, cause
				}
				return 0, nil
			}, Build: func([]string) ([]deploy.Target, error) { built++; return nil, errors.New("must not build") }}
			args := []string{}
			input := "n\n"
			switch kind {
			case "bad-action":
				args = []string{"*", "bogus"}
			case "bad-reboot":
				args = []string{"--reboot"}
			case "decline":
			default:
				args = []string{"--yes"}
			}
			_, err := (&Engine{Context: context.Background()}).deployCommand(deployInvocation(t, args...), strings.NewReader(input), io.Discard, d)
			if err == nil || built != 0 {
				t.Fatal("refusal allowed build or returned success")
			}
			if kind == "decline" && prepared != 0 {
				t.Fatal("decline ran prepare")
			}
			if kind == "hook-status" {
				var hook *DeployHookError
				if !errors.As(err, &hook) || hook.Status != 7 {
					t.Fatal("hook status lost")
				}
			}
			if kind == "hook-error" && !errors.Is(err, cause) {
				t.Fatal("hook cause lost")
			}
			if strings.HasPrefix(kind, "bad-") {
				var argument *deployArgumentError
				if !errors.As(err, &argument) || calls != 1 || prepared != 0 {
					t.Fatal("argument validation timing changed")
				}
			}
		})
	}
}

func deploySeedRecord(t *testing.T, root, host, name string) {
	t.Helper()
	dir := filepath.Join(root, ".confctl/generations", host, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generation.json"), []byte(`{"mode":"flakes","date":"2026-10-15T10:00:00Z","toplevel":"/fixture/system","inputs":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestDeployDeliveryStoredMissingAndCopyOnly(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "bulk-refuses", true: "interactive-found-only"}[interactive], func(t *testing.T) {
			root := t.TempDir()
			deploySeedRecord(t, root, "a", "stored")
			copied, prepare := 0, 0
			d := deployCommandDeps{Machines: func() ([]Machine, error) { return []Machine{deployTestMachine("a"), deployTestMachine("b")}, nil }, Prepare: func([]string) (int, error) { prepare++; return 0, nil }, Load: func(host string) (*generation.BuildList, error) {
				return generation.LoadBuildList(root, host, generation.Options{})
			}, Build: func([]string) ([]deploy.Target, error) { t.Fatal("stored generation built"); return nil, nil }, Operations: func(context.Context, []Machine, []Machine, commandLine, io.Writer, cli.Invocation) deploy.Deps {
				return deploy.Deps{AutoRollbackDefault: func(deploy.Machine) (bool, error) { return false, nil }, Confirm: func(context.Context, deploy.Confirmation) (bool, error) { return true, nil }, Copy: func(context.Context, deploy.Machine, []string, func(deploy.CopyProgress)) (bool, error) {
					copied++
					return true, nil
				}}
			}}
			args := []string{"--yes", "--copy-only", "--generation", "stored"}
			if interactive {
				args = append(args, "--interactive")
			}
			report, err := (&Engine{Context: context.Background()}).deployCommand(deployInvocation(t, args...), strings.NewReader("y\n"), io.Discard, d)
			if prepare != 1 || len(report.Outcomes) != 1 {
				t.Fatal("prepare/resolved outcomes differ")
			}
			if interactive {
				if err != nil || copied != 1 {
					t.Fatal("interactive copy-only did not use found target", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "sorted generations length") || copied != 0 {
				t.Fatal("missing-generation sorter refusal changed", err)
			}
		})
	}
}

func TestDeployDeliveryOutputErrorJoins(t *testing.T) {
	cause := errors.New("event output failed")
	finished := make(chan struct{})
	d := deployCommandDeps{Machines: func() ([]Machine, error) { return []Machine{deployTestMachine("a")}, nil }, Prepare: func([]string) (int, error) { return 0, nil }, Build: func([]string) ([]deploy.Target, error) {
		return []deploy.Target{{Host: "a", Generation: deployTestGeneration("g")}}, nil
	}, Operations: func(context.Context, []Machine, []Machine, commandLine, io.Writer, cli.Invocation) deploy.Deps {
		return deploy.Deps{QueryTarget: func(ctx context.Context, _ deploy.Machine) (deploy.Status, error) {
			defer close(finished)
			<-ctx.Done()
			return deploy.Status{}, ctx.Err()
		}}
	}}
	// First announcement succeeds; the first Event write cancels its own Run.
	out := &deployErrorWriter{err: cause}
	_, err := (&Engine{Context: context.Background()}).deployCommand(deployInvocation(t, "--yes"), strings.NewReader(""), out, d)
	if !errors.Is(err, cause) {
		t.Fatal("event I/O error replaced by cancellation")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Run returned before owned query joined")
	}
}

type deployErrorWriter struct {
	writes int
	err    error
}

func (w *deployErrorWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, w.err
	}
	return len(b), nil
}

func TestDeployDeliveryPromptsAndWait(t *testing.T) {
	_, input := commandInput(strings.NewReader("\ninvalid\ny\nn\nr\nf\n"))
	var out bytes.Buffer
	if ok, err := deployConfirm(context.Background(), input, &out); err != nil || !ok {
		t.Fatal("blank/invalid must repeat confirmation")
	}
	if ok, err := deployConfirm(context.Background(), input, &out); err != nil || ok {
		t.Fatal("explicit decline changed")
	}
	if answer, err := deployChoice(context.Background(), input, &out, "yrsa"); err != nil || answer != "r" {
		t.Fatal("activation retry changed")
	}
	if answer, err := deployChoice(context.Background(), input, &out, "crfa"); err != nil || answer != "f" {
		t.Fatal("health retry subset changed")
	}
	if _, err := deployChoice(context.Background(), input, &out, "yn"); err == nil {
		t.Fatal("EOF selected default")
	}
	for text, want := range map[string]string{"600": "600", "999999999999999999999999": "999999999999999999999999", "x\n12\nz": "0", "\n12\n": "12", "-1_2\n0\n": "-12"} {
		v, err := deployWaitMode(text)
		if err != nil || v.Kind != deploy.WaitSeconds || v.Seconds.String() != want {
			t.Fatal("wait spelling differs", text, err)
		}
	}
	if _, err := deployWaitMode("auto"); err == nil {
		t.Fatal("invalid wait admitted")
	}
	for _, kind := range []deploy.WaitKind{deploy.NoWait, deploy.WaitForever} {
		text := "nowait"
		if kind == deploy.WaitForever {
			text = "wait"
		}
		v, err := deployWaitMode(text)
		if err != nil || v.Kind != kind {
			t.Fatal("wait mode differs")
		}
	}
}

func TestDeployDeliveryHealthRetainsFailures(t *testing.T) {
	machine := deployTestMachine("a")
	g := deployTestGeneration("g")
	rounds := 0
	d := deployCommandDeps{Machines: func() ([]Machine, error) { return []Machine{machine}, nil }, Prepare: func([]string) (int, error) { return 0, nil }, Build: func([]string) ([]deploy.Target, error) { return []deploy.Target{{Host: "a", Generation: g}}, nil }, Operations: func(context.Context, []Machine, []Machine, commandLine, io.Writer, cli.Invocation) deploy.Deps {
		return deploy.Deps{QueryTarget: func(context.Context, deploy.Machine) (deploy.Status, error) { return deploy.Status{}, nil }, AutoRollbackDefault: func(deploy.Machine) (bool, error) { return false, nil }, Copy: func(context.Context, deploy.Machine, []string, func(deploy.CopyProgress)) (bool, error) {
			return true, nil
		}, Activate: func(context.Context, deploy.Machine, *generation.Build, deploy.Action) (bool, error) {
			return true, nil
		}, Checks: func([]deploy.Machine) ([]health.Check, error) {
			return []health.Check{{Machine: "a", Argv: []string{"fixture"}, Timing: health.Timing{Timeout: big.NewInt(0), Cooldown: big.NewInt(0)}, Command: &health.Command{ExpectedStatus: big.NewInt(0)}}}, nil
		}, RunHealth: func(ctx context.Context, _ []deploy.Machine, checks []health.Check) (health.Report, error) {
			rounds++
			return health.Run(ctx, checks, big.NewInt(1), health.Deps{Execute: func(context.Context, health.Check) (health.CommandResult, error) {
				return health.CommandResult{Status: 1}, nil
			}})
		}, ChooseActivation: func(context.Context, deploy.Target, bool) (deploy.ActivationChoice, error) {
			return deploy.ActivationContinue, nil
		},
			ChooseHealth: func(context.Context, []deploy.Machine, health.Report) (deploy.HealthChoice, error) {
				return deploy.HealthContinue, nil
			}, Confirm: func(context.Context, deploy.Confirmation) (bool, error) { return true, nil }}
	}}
	report, err := (&Engine{Context: context.Background()}).deployCommand(deployInvocation(t, "--yes", "--interactive", "--keep-going", "*", "test"), strings.NewReader(""), io.Discard, d)
	if err != nil || rounds != 1 || !report.RetainLog {
		t.Fatal("normal health failure/continue lost log retention", err)
	}
}

func TestDeployDeliveryConcretePartialEffects(t *testing.T) {
	for _, failure := range []string{"copy", "activation", "profile"} {
		t.Run(failure, func(t *testing.T) {
			machines := []Machine{deployTestMachine("a"), deployTestMachine("b")}
			var effects []string
			ops := deployProcessOps{Run: func(_ context.Context, argv []string, input []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
				command := argv[len(argv)-1]
				host := strings.TrimSuffix(argv[len(argv)-2], ".invalid")
				if argv[0] == "nix-copy-closure" {
					host = strings.TrimSuffix(strings.TrimPrefix(argv[2], "root@"), ".invalid")
					effects = append(effects, "copy:"+host)
				} else {
					switch {
					case command == "cat /proc/uptime":
						return Result{Stdout: []byte("100 0")}, nil
					case strings.HasPrefix(command, "realpath "):
						return Result{Stdout: []byte("/old/top\n")}, nil
					case command == "bash --norc":
						if string(input) != string(generation.ProfileScript(machines[0].Profile)) {
							t.Error("query did not use the existing profile protocol")
						}
						return Result{Stdout: []byte("/old/top\n" + machines[0].Profile + "-1-link;/old/top;1;\n")}, nil
					case strings.Contains(command, "/bin/switch-to-configuration switch"):
						effects = append(effects, "activation:"+host)
					case strings.HasPrefix(command, "nix-env "):
						effects = append(effects, "profile:"+host)
					default:
						t.Errorf("unexpected command %q", argv)
						return Result{}, errors.New("unexpected scripted command")
					}
				}
				if effects[len(effects)-1] == failure+":b" {
					return Result{ExitCode: 1}, nil
				}
				return Result{}, nil
			}}
			e := &Engine{Context: context.Background()}
			d := deployCommandDeps{
				Machines: func() ([]Machine, error) { return machines, nil },
				Prepare:  func([]string) (int, error) { return 0, nil },
				Build: func([]string) ([]deploy.Target, error) {
					return []deploy.Target{{Host: "a", Generation: deployTestGeneration("a")}, {Host: "b", Generation: deployTestGeneration("b")}}, nil
				},
				Operations: func(ctx context.Context, all, selected []Machine, input commandLine, out io.Writer, inv cli.Invocation) deploy.Deps {
					return e.deployOperationsWithOps(ctx, all, selected, input, out, inv, ops)
				},
			}
			report, err := e.deployCommand(deployInvocation(t, "--yes", "--no-health-checks", "--max-concurrent-copy", "1"), strings.NewReader(""), io.Discard, d)
			if err == nil || len(report.Outcomes) != 2 {
				t.Fatal("ordinary failure lost its partial report", err)
			}
			want := []string{"copy:a", "copy:b"}
			if failure != "copy" {
				want = append(want, "activation:a", "profile:a", "activation:b")
				if failure == "profile" {
					want = append(want, "profile:b")
				}
				if report.Outcomes[0].Profile.State != deploy.Succeeded || report.Outcomes[1].Copy.State != deploy.Succeeded {
					t.Fatal("earlier successful effects were lost")
				}
			} else if report.Outcomes[1].Copy.State != deploy.Failed || report.Outcomes[0].Activation.State != deploy.NotReached {
				t.Fatal("copy failure permitted activation")
			}
			if !reflect.DeepEqual(effects, want) {
				t.Fatalf("effects=%v want=%v", effects, want)
			}
		})
	}
}

func TestDeployDeliveryHealthWorkerAdmissionAndRetries(t *testing.T) {
	for _, scenario := range []struct {
		name, workers, answers                         string
		checks, interactive, disabled, fail, wantError bool
		wantA, wantB, rounds                           int
	}{
		{name: "absent", checks: true, wantA: 1, wantB: 1, rounds: 1},
		{name: "base-prefixed", workers: "0x2", checks: true, wantA: 1, wantB: 1, rounds: 1},
		{name: "huge", workers: "9999999999999999999999", checks: true, wantA: 1, wantB: 1, rounds: 1},
		{name: "auto", workers: "auto", checks: true, wantError: true},
		{name: "zero", workers: "0", checks: true, wantError: true},
		{name: "no-checks", workers: "auto"},
		{name: "disabled", workers: "auto", checks: true, disabled: true},
		{name: "decline", workers: "auto", checks: true, interactive: true, answers: "y\ny\nn\n"},
		{name: "retry-failed", workers: "1", checks: true, interactive: true, fail: true, answers: "y\ny\ny\nf\n", wantA: 2, wantB: 1, rounds: 2},
		{name: "retry-all", workers: "1", checks: true, interactive: true, fail: true, answers: "y\ny\ny\nr\n", wantA: 2, wantB: 2, rounds: 2},
		{name: "keep-going", workers: "1", checks: true, fail: true, wantA: 1, wantB: 1, rounds: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			checks := `{"builderCommands":[]}`
			if scenario.checks {
				command := func(name string) string {
					return strings.ReplaceAll(strings.ReplaceAll(healthCommandJSON("fixture", name), `"timeout":60`, `"timeout":0`), `"cooldown":3`, `"cooldown":0`)
				}
				checks = `{"builderCommands":[` + command("a") + `,` + command("b") + `]}`
			}
			m := healthFixtureMachines(t, healthMachineJSON("m", "nixos", checks))[0]
			m.Attributes["autoRollback"] = map[string]any{"enable": false}
			var mu sync.Mutex
			calls := map[string]int{}
			activations := 0
			ops := deployProcessOps{Run: func(_ context.Context, argv []string, _ []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
				switch argv[0] {
				case "cat":
					return Result{Stdout: []byte("100 0")}, nil
				case "realpath":
					return Result{Stdout: []byte("/old/top")}, nil
				case "bash":
					return Result{}, nil
				case "/fixture/g/bin/switch-to-configuration":
					activations++
					return Result{}, nil
				case "fixture":
					mu.Lock()
					defer mu.Unlock()
					calls[argv[1]]++
					if scenario.fail && argv[1] == "a" && calls["a"] == 1 {
						return Result{ExitCode: 1}, nil
					}
					return Result{}, nil
				}
				return Result{}, fmt.Errorf("unexpected scripted command %q", argv)
			}}
			e := &Engine{Context: context.Background()}
			d := deployCommandDeps{
				Machines: func() ([]Machine, error) { return []Machine{m}, nil },
				Prepare:  func([]string) (int, error) { return 0, nil },
				Build: func([]string) ([]deploy.Target, error) {
					return []deploy.Target{{Host: "m", Generation: deployTestGeneration("g")}}, nil
				},
				Operations: func(ctx context.Context, all, selected []Machine, input commandLine, out io.Writer, inv cli.Invocation) deploy.Deps {
					return e.deployOperationsWithOps(ctx, all, selected, input, out, inv, ops)
				},
			}
			args := []string{"--yes"}
			if scenario.workers != "" {
				args = append(args, "--max-jobs", scenario.workers)
			}
			if scenario.interactive {
				args = append(args, "--interactive")
			}
			if scenario.disabled {
				args = append(args, "--no-health-checks")
			}
			if scenario.name == "keep-going" {
				args = append(args, "--keep-going")
			}
			args = append(args, "*", "test")
			report, err := e.deployCommand(deployInvocation(t, args...), strings.NewReader(scenario.answers), io.Discard, d)
			if (err != nil) != scenario.wantError || calls["a"] != scenario.wantA || calls["b"] != scenario.wantB {
				t.Fatalf("calls=%v err=%v report=%#v", calls, err, report)
			}
			if !scenario.wantError && len(report.HealthRounds) != scenario.rounds {
				t.Fatalf("health rounds=%d want=%d", len(report.HealthRounds), scenario.rounds)
			}
			if scenario.fail && !report.RetainLog {
				t.Fatal("later success/keep-going erased failure log retention")
			}
			if scenario.wantError && report.Outcomes[0].Activation.State != deploy.Succeeded {
				t.Fatal("health worker refusal occurred before activation")
			}
			if scenario.wantError {
				if activations != 1 || report.Outcomes[0].Health.State != deploy.NotReached || report.Outcomes[0].Health.Err != nil {
					t.Fatal("worker refusal lost completed activation or marked unstarted health unknown")
				}
				if len(report.HealthRounds) != 1 || len(report.HealthRounds[0].Results) != 2 {
					t.Fatalf("worker refusal lost submitted checks: %#v", report.HealthRounds)
				}
				wantChecks, checkErr := m.healthSpecs()
				if checkErr != nil {
					t.Fatal(checkErr)
				}
				for i, result := range report.HealthRounds[0].Results {
					if !reflect.DeepEqual(result, health.Result{Check: wantChecks[i]}) || result.Passed() {
						t.Fatalf("worker refusal lost ordered unstarted check %d: %#v", i, result)
					}
				}
				var binding *deploy.BindingError
				if errors.As(err, &binding) || report.RetainLog {
					t.Fatal("worker admission was misreported as a binding or predicate failure")
				}
				if scenario.name == "auto" {
					var parse *cli.ParseError
					if !errors.As(err, &parse) {
						t.Fatal("primary Integer parser error was lost", err)
					}
				}
			}
		})
	}
}

func TestDeployDeliveryPublicStoredCopy(t *testing.T) {
	root := t.TempDir()
	deploySeedRecord(t, root, "a", "stored")
	recordPath := filepath.Join(root, ".confctl/generations/a/stored/generation.json")
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(root, "nix-copy-closure")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DEPLOY_TEST_LEDGER\"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ledger := filepath.Join(root, "copy-argv")
	t.Setenv("DEPLOY_TEST_LEDGER", ledger)
	input, err := os.CreateTemp(root, "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.CreateTemp(root, "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	e := &Engine{Root: root, Context: context.Background(), Inventory: []Machine{deployTestMachine("a")}}
	if err := e.OpenLog("deploy"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseLog(false)
	report, err := e.Deploy(deployInvocation(t, "--yes", "--copy-only", "--generation", "stored"), Registry{}, input, output, output)
	if err == nil || !reflect.DeepEqual(report.FailedCopies, []string{"a"}) || report.Outcomes[0].Activation.State != deploy.NotReached {
		t.Fatal("public stored copy did not return the real partial failure", err)
	}
	args, err := os.ReadFile(ledger)
	if err != nil || string(args) != "--to\nroot@a.invalid\n/fixture/system\n" {
		t.Fatalf("public copy did not reach the fake tool with literal arguments: %q, %v", args, err)
	}
	after, err := os.ReadFile(recordPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stored deployment mutated its generation record")
	}
}
