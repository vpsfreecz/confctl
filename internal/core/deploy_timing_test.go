package core

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/health"
)

func TestDeployTimingOperationCalls(t *testing.T) {
	for _, operation := range []string{"copy", "activate", "profile", "carried-profile"} {
		for _, mode := range []string{"success", "nonzero", "error", "cancel", "nil-ledger"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				start := time.Unix(100, 0)
				now := start
				e := &Engine{Context: ctx}
				if mode != "nil-ledger" {
					e.timing = newInvocationTiming(start, func() time.Time { return now })
				}
				phase := timingDeployment
				if operation == "copy" {
					phase = timingCopy
				}
				m, carrier := deployTestMachine("node"), deployTestMachine("carrier")
				if operation == "carried-profile" {
					m.CarrierName = &carrier.Name
				}
				cause := errors.New("synthetic deployment transport error")
				calls := 0
				ops := deployProcessOps{Run: func(got context.Context, argv []string, input []byte, _ []string, logged bool, _ func([]byte, []byte) error) (Result, error) {
					calls++
					if got != ctx || input != nil || !logged || len(argv) == 0 {
						t.Fatal("timing changed original process binding", argv, input, logged)
					}
					now = now.Add(2 * time.Second)
					if s := e.timing.Snapshot(); e.timing != nil && (!s.Seen[phase] || s.Phases[phase] != 2*time.Second || s.Work != 2*time.Second || s.Prompt != 0) {
						t.Fatal("real operation ran outside its timing interval", s)
					}
					switch mode {
					case "error":
						return Result{}, cause
					case "cancel":
						cancel()
						return Result{}, ctx.Err()
					case "nonzero":
						return Result{ExitCode: 23}, nil
					default:
						return Result{}, nil
					}
				}}
				d := e.deployOperationsWithOps(ctx, []Machine{m, carrier}, []Machine{m}, nil, io.Discard, io.Discard, cli.Invocation{}, ops)
				var ok bool
				var err error
				switch operation {
				case "copy":
					ok, err = d.Copy(ctx, deployMachine(m), []string{"/fixture/top"}, nil)
				case "activate":
					ok, err = d.Activate(ctx, deployMachine(m), deployTestGeneration("new"), deploy.Switch)
				case "profile":
					ok, err = d.SetProfile(ctx, deployMachine(m), "/fixture/top")
				case "carried-profile":
					ok, err = d.SetCarriedProfile(ctx, deployMachine(m), "/fixture/top")
				}
				wantErr := error(nil)
				if mode == "error" {
					wantErr = cause
				} else if mode == "cancel" {
					wantErr = context.Canceled
				}
				wantOK := mode == "success" || mode == "nil-ledger"
				// Machine commands retain status==0 alongside the exact error;
				// copy returns false before interpreting that status on error.
				if operation != "copy" && (mode == "error" || mode == "cancel") {
					wantOK = true
				}
				if calls != 1 || err != wantErr || ok != wantOK {
					t.Fatal("timing changed result/error or execution count", calls, ok, err)
				}
				now = now.Add(5 * time.Second)
				s := e.timing.Snapshot()
				if e.timing == nil {
					if !reflect.DeepEqual(s, timingSnapshot{}) {
						t.Fatal("nil ledger recorded work", s)
					}
					return
				}
				if s.Phases[phase] != 2*time.Second || s.Work != 2*time.Second || s.Other != 5*time.Second {
					t.Fatal("operation timing leaked beyond joined return", s)
				}
				for other, seen := range s.Seen {
					if seen != (timingPhase(other) == phase) {
						t.Fatal("operation acquired an unrelated phase", s.Seen)
					}
				}
			})
		}
	}
}

func TestDeployTimingQueryTarget(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "error"}[fail], func(t *testing.T) {
			now := time.Unix(100, 0)
			e := &Engine{Context: context.Background(), timing: newInvocationTiming(now, func() time.Time { return now })}
			m := deployTestMachine("node")
			cause := errors.New("synthetic query transport error")
			calls := 0
			ops := deployProcessOps{Run: func(_ context.Context, _ []string, _ []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
				calls++
				now = now.Add(time.Second)
				if s := e.timing.Snapshot(); !s.Seen[timingStatusChecks] || s.Phases[timingStatusChecks] != time.Duration(calls)*time.Second {
					t.Fatal("query did not own its full call interval", s)
				}
				if fail {
					return Result{}, cause
				}
				return Result{Stdout: []byte([]string{"100 0", "/fixture/old", "/fixture/old\n" + m.Profile + "-9-link;/fixture/old;1;\n"}[calls-1])}, nil
			}}
			d := e.deployOperationsWithOps(e.Context, []Machine{m}, []Machine{m}, nil, io.Discard, io.Discard, cli.Invocation{}, ops)
			st, err := d.QueryTarget(e.Context, deployMachine(m))
			if fail && (err != cause || calls != 1) || !fail && (err != nil || calls != 3 || st.CurrentToplevel == nil || *st.CurrentToplevel != "/fixture/old" || st.Generations == nil) {
				t.Fatal("query timing changed partial results or primary error", st, calls, err)
			}
			now = now.Add(5 * time.Second)
			s := e.timing.Snapshot()
			if s.Phases[timingStatusChecks] != time.Duration(calls)*time.Second || s.Work != time.Duration(calls)*time.Second || s.Other != 5*time.Second || s.Seen[timingDeployment] {
				t.Fatal("query timing spilled into another phase or cleanup", s)
			}
		})
	}
}

func TestDeployTimingRollbackError(t *testing.T) {
	now := time.Unix(100, 0)
	e := &Engine{Context: context.Background(), timing: newInvocationTiming(now, func() time.Time { return now })}
	m := deployTestMachine("node")
	cause := errors.New("synthetic check-file failure")
	ops := deployProcessOps{CheckFile: func() (string, error) {
		now = now.Add(2 * time.Second)
		if s := e.timing.Snapshot(); s.Phases[timingDeployment] != 2*time.Second {
			t.Fatal("rollback admission did not start deployment timing", s)
		}
		return "", cause
	}, Run: func(context.Context, []string, []byte, []string, bool, func([]byte, []byte) error) (Result, error) {
		t.Fatal("check-file failure admitted activation")
		return Result{}, nil
	}}
	d := e.deployOperationsWithOps(e.Context, []Machine{m}, []Machine{m}, nil, io.Discard, io.Discard, cli.Invocation{}, ops)
	if ok, err := d.ActivateWithRollback(e.Context, deployMachine(m), deployTestGeneration("new"), deploy.Switch); ok || err != cause {
		t.Fatal("rollback timing changed admission error identity", ok, err)
	}
	now = now.Add(time.Second)
	if s := e.timing.Snapshot(); s.Phases[timingDeployment] != 2*time.Second || s.Other != time.Second {
		t.Fatal("rollback error left timing active", s)
	}
}

func TestDeployTimingRebootWait(t *testing.T) {
	for _, mode := range []string{"nowait", "wait", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Unix(100, 0)
			e := &Engine{Context: ctx, timing: newInvocationTiming(now, func() time.Time { return now })}
			m := deployTestMachine("node")
			reads, reboots, sleeps := 0, 0, 0
			ops := deployProcessOps{Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error {
				sleeps++
				now = now.Add(300 * time.Millisecond)
				if s := e.timing.Snapshot(); !s.Seen[timingRebootWait] || s.Phases[timingRebootWait] != 3300*time.Millisecond {
					t.Fatal("reboot wait excluded its poll/sleep interval", s)
				}
				if mode == "cancel" {
					cancel()
				}
				return ctx.Err()
			}, Run: func(_ context.Context, argv []string, _ []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
				now = now.Add(time.Second)
				if argv[len(argv)-1] == "reboot" {
					reboots++
					return Result{ExitCode: 1}, nil // Existing reboot status policy.
				}
				reads++
				if reads == 1 {
					return Result{Stdout: []byte("100 0")}, nil
				}
				if reads == 2 {
					return Result{ExitCode: 1}, nil
				}
				return Result{Stdout: []byte("1 0")}, nil
			}}
			d := e.deployOperationsWithOps(ctx, []Machine{m}, []Machine{m}, nil, io.Discard, io.Discard, cli.Invocation{}, ops)
			wait := deploy.WaitMode{Kind: deploy.WaitForever}
			if mode == "nowait" {
				wait.Kind = deploy.NoWait
			}
			err := d.RebootAndWait(ctx, deployMachine(m), wait)
			want := time.Second
			if mode != "nowait" {
				want = 4300 * time.Millisecond
				if mode == "cancel" {
					want -= time.Second
				}
			}
			if reboots != 1 || mode == "nowait" && (reads != 0 || sleeps != 0) || mode == "wait" && (reads != 3 || sleeps != 1) || mode == "cancel" && (err != context.Canceled || reads != 2 || sleeps != 1) || mode != "cancel" && err != nil {
				t.Fatal("timing changed reboot effects or cancellation", reads, reboots, sleeps, err)
			}
			now = now.Add(time.Second)
			if s := e.timing.Snapshot(); s.Phases[timingRebootWait] != want || s.Work != want || s.Other != time.Second || s.Prompt != 0 {
				t.Fatal("reboot timing ended outside the actual callback", s)
			}
		})
	}
}

type deployTimingRead func([]byte) (int, error)

func (read deployTimingRead) Read(p []byte) (int, error) { return read(p) }

func TestDeployTimingPromptLinePropagation(t *testing.T) {
	now := time.Unix(100, 0)
	e := &Engine{Context: context.Background(), timing: newInvocationTiming(now, func() time.Time { return now })}
	m := deployTestMachine("node")
	chunks := []string{"invalid\n", "y\n", "invalid\ny\nr\nf\n"}
	reads := 0
	input := deployTimingRead(func(p []byte) (int, error) {
		if reads >= len(chunks) {
			t.Fatal("timing added an input read or lost the queued suffix")
		}
		chunk := chunks[reads]
		reads++
		now = now.Add(2 * time.Second)
		if s := e.timing.Snapshot(); s.Prompt != time.Duration(reads)*2*time.Second || s.Work != 0 {
			t.Fatal("actual prompt read was not the timing boundary", s)
		}
		return copy(p, chunk), nil
	})
	deps := deployCommandDeps{
		Machines: func() ([]Machine, error) { return []Machine{m}, nil },
		Prepare:  func([]string) (int, error) { now = now.Add(5 * time.Second); return 0, nil },
		Build: func([]string) ([]deploy.Target, error) {
			return []deploy.Target{{Host: m.Name, Generation: deployTestGeneration("new")}}, nil
		},
		Operations: func(ctx context.Context, all, selected []Machine, line commandLine, out, stderr io.Writer, inv cli.Invocation) deploy.Deps {
			// These are the real production choice adapters receiving the same
			// once-wrapped reader as the initial deployment confirmation.
			d := e.deployOperationsWithOps(ctx, all, selected, line, out, stderr, inv, deployProcessOps{})
			if ok, err := d.Confirm(ctx, deploy.Confirmation{}); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if choice, err := d.ChooseActivation(ctx, deploy.Target{}, true); err != nil || choice != deploy.ActivationRetry {
				t.Fatal(choice, err)
			}
			if choice, err := d.ChooseHealth(ctx, nil, health.Report{}); err != nil || choice != deploy.HealthRetryFailed {
				t.Fatal(choice, err)
			}
			return deploy.Deps{
				QueryTarget:         func(context.Context, deploy.Machine) (deploy.Status, error) { return deploy.Status{}, nil },
				AutoRollbackDefault: func(deploy.Machine) (bool, error) { return false, nil },
				Copy: func(context.Context, deploy.Machine, []string, func(deploy.CopyProgress)) (bool, error) {
					return true, nil
				},
			}
		},
	}
	if _, err := e.deployCommand(deployInvocation(t, "--copy-only"), input, io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if s := e.timing.Snapshot(); reads != 3 || s.Prompt != 6*time.Second || s.Other != 6*time.Second || s.Work != 0 {
		t.Fatal("prompts measured rendering/setup or lost buffered answers", reads, s)
	}
}

func TestDeployTimingPromptReadFailure(t *testing.T) {
	for _, mode := range []string{"refusal", "eof", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Unix(100, 0)
			e := &Engine{Context: ctx, timing: newInvocationTiming(now, func() time.Time { return now })}
			cause := errors.New("synthetic prompt read error")
			input := deployTimingRead(func(p []byte) (int, error) {
				now = now.Add(2 * time.Second)
				switch mode {
				case "refusal":
					return copy(p, "n\n"), nil
				case "eof":
					return 0, io.EOF
				case "cancel":
					cancel()
					return 0, ctx.Err()
				default:
					return 0, cause
				}
			})
			deps := deployCommandDeps{Machines: func() ([]Machine, error) { return []Machine{deployTestMachine("node")}, nil }, Prepare: func([]string) (int, error) {
				t.Fatal("failed/refused prompt admitted effects")
				return 0, nil
			}}
			_, err := e.deployCommand(deployInvocation(t), input, io.Discard, io.Discard, deps)
			if err == nil || mode == "error" && err != cause || mode == "cancel" && err != context.Canceled || mode == "refusal" && err.Error() != "Aborted" || mode == "eof" && !strings.Contains(err.Error(), "end of file") {
				t.Fatal("timing changed prompt failure identity", err)
			}
			now = now.Add(time.Second)
			if s := e.timing.Snapshot(); s.Prompt != 2*time.Second || s.Work != 0 || s.Other != time.Second {
				t.Fatal("failed prompt left timing active", s)
			}
		})
	}
}
