package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/health"
)

func TestHealthTimingRoundIncludesCooldownAndJoinedWork(t *testing.T) {
	e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthCommandJSON("fixture", "literal arg")+`]}`))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Context = ctx
	start := time.Unix(1000, 0)
	var elapsed atomic.Int64
	e.timing = newInvocationTiming(start, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })
	entered, release := make(chan struct{}), make(chan struct{})
	type outcome struct {
		result HealthResult
		err    error
	}
	done := make(chan outcome, 1)
	inv := healthInvocation(t, "--yes")
	attempts, cooldowns := 0, 0
	var out bytes.Buffer
	deps := health.Deps{Now: func() time.Time { return start }, Cooldown: func(context.Context, *big.Int) error {
		cooldowns++
		if s := e.timing.Snapshot(); !s.Seen[timingHealth] || s.Phases[timingHealth] != 2*time.Second {
			t.Error("cooldown was outside the admitted health round", s)
		}
		elapsed.Add(int64(3 * time.Second))
		return nil
	}}
	go func() {
		result, err := e.healthCheck(inv, strings.NewReader(""), &out, io.Discard, deps, func(ctx context.Context, root string, argv []string, input []byte) (Result, error) {
			if root != e.Root || !reflect.DeepEqual(argv, []string{"fixture", "literal arg"}) || input != nil {
				t.Error("timing changed the health process binding", root, argv, input)
			}
			attempts++
			elapsed.Add(int64(2 * time.Second))
			if attempts == 1 {
				return Result{ExitCode: 7}, nil
			}
			close(entered)
			select {
			case <-release:
				return Result{}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		})
		done <- outcome{result, err}
	}()
	select {
	case <-entered:
	case result := <-done:
		t.Fatal("health round returned before reaching held retry", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	elapsed.Add(int64(4 * time.Second))
	if s := e.timing.Snapshot(); !s.Seen[timingHealth] || s.Phases[timingHealth] != 11*time.Second || s.Work != 11*time.Second {
		t.Error("health phase ended before the actual worker joined", s)
	}
	select {
	case result := <-done:
		t.Fatal("health round completed while its process was held", result)
	default:
	}
	close(release)
	result := <-done
	if result.err != nil || attempts != 2 || cooldowns != 1 || !result.result.RetainLog || !result.result.Report.Results[0].Passed() {
		t.Fatal("timing changed retry or retained-log behavior", result, attempts, cooldowns)
	}
	elapsed.Add(int64(5 * time.Second))
	if s := e.timing.Snapshot(); s.Phases[timingHealth] != 11*time.Second || s.Other != 5*time.Second {
		t.Fatal("completed round kept accruing during caller/choice time", s)
	}
	// The same owner is used for deployment's later admitted retry subsets.
	plan, err := e.PlanHealth(HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	next, err := e.runHealthPresented(plan, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
		elapsed.Add(int64(2 * time.Second))
		return Result{}, nil
	}, io.Discard, io.Discard)
	if s := e.timing.Snapshot(); err != nil || !next.Report.Results[0].Passed() || s.Phases[timingHealth] != 13*time.Second || s.Other != 5*time.Second {
		t.Fatal("later health round included the inter-round gap", next, err, s)
	}
}

func TestHealthTimingErrorAndCancellationEndRound(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "executor-error"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthCommandJSON("fixture")+`]}`))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.Context = ctx
			start := time.Unix(1000, 0)
			var elapsed atomic.Int64
			e.timing = newInvocationTiming(start, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })
			cause := errors.New("health executor failure")
			if canceled {
				cause = context.Canceled
			}
			result, err := e.healthCheck(healthInvocation(t, "--yes"), strings.NewReader(""), io.Discard, io.Discard, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
				elapsed.Add(int64(3 * time.Second))
				if canceled {
					cancel()
				}
				return Result{}, cause
			})
			if !errors.Is(err, cause) || len(result.Report.Results) != 1 || result.Report.Results[0].Complete || !errors.Is(result.Report.Results[0].Err, cause) {
				t.Fatal("timing replaced the incomplete round's cause", result, err)
			}
			elapsed.Add(int64(2 * time.Second))
			if s := e.timing.Snapshot(); !s.Seen[timingHealth] || s.Phases[timingHealth] != 3*time.Second || s.Other != 2*time.Second {
				t.Fatal("failed/canceled round timer leaked past join", s)
			}
		})
	}
}

type healthTimingReadFunc func([]byte) (int, error)

func (f healthTimingReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestHealthTimingInitialConfirmationKeepsQueuedInputAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, input, wantErr string
		yes, canceled        bool
	}{
		{name: "queued-invalid-then-confirmed", input: "invalid\nY\nTAIL\n"},
		{name: "declined", input: "n\nTAIL\n", wantErr: "Aborted"},
		{name: "eof", wantErr: "end of file reached"},
		{name: "canceled", input: "y\n", canceled: true},
		{name: "yes-bypasses-reader", input: "TAIL\n", yes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthCommandJSON("fixture")+`]}`))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.Context = ctx
			start := time.Unix(1000, 0)
			var elapsed atomic.Int64
			e.timing = newInvocationTiming(start, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })
			reads, calls := 0, 0
			source := strings.NewReader(tc.input)
			input := bufio.NewReader(healthTimingReadFunc(func(p []byte) (int, error) {
				reads++
				elapsed.Add(int64(2 * time.Second))
				if tc.canceled {
					cancel()
				}
				return source.Read(p)
			}))
			var args []string
			if tc.yes {
				args = []string{"--yes"}
			}
			result, err := e.healthCheck(healthInvocation(t, args...), input, io.Discard, io.Discard, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) {
				calls++
				elapsed.Add(int64(3 * time.Second))
				return Result{}, nil
			})
			if tc.canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("prompt lost cancellation identity", err)
				}
			} else if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatal("prompt error changed", err)
				}
			} else if err != nil || len(result.Report.Results) != 1 || !result.Report.Results[0].Passed() {
				t.Fatal(result, err)
			}
			wantPrompt, wantHealth := 2*time.Second, time.Duration(0)
			if tc.yes {
				wantPrompt = 0
			}
			if tc.wantErr == "" && !tc.canceled {
				wantHealth = 3 * time.Second
				if calls != 1 {
					t.Fatal("confirmation changed health dispatch", calls)
				}
			} else if calls != 0 {
				t.Fatal("refused/failed prompt dispatched health", calls)
			}
			if s := e.timing.Snapshot(); s.Prompt != wantPrompt || s.Phases[timingHealth] != wantHealth || s.Seen[timingHealth] != (calls == 1) || s.Other != 0 {
				t.Fatal("prompt wait leaked into health work or changed queued reads", s)
			}
			if tc.yes {
				if reads != 0 {
					t.Fatal("--yes read input", reads)
				}
			} else if reads != 1 {
				t.Fatal("confirmation replaced or reread the borrowed input", reads)
			}
			if strings.Contains(tc.input, "TAIL") {
				if tail, err := input.ReadString('\n'); err != nil || tail != "TAIL\n" {
					t.Fatal("confirmation consumed queued caller input", tail, err)
				}
			}
		})
	}
}
