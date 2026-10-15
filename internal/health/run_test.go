package health

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func commandCheck(name string, timeout, cooldown int64) Check {
	return Check{Machine: name, Description: name, Argv: []string{"fixture", name}, Timing: Timing{big.NewInt(timeout), big.NewInt(cooldown)}, Command: &Command{ExpectedStatus: big.NewInt(0)}}
}

func TestRetriesStrictExpiryFirstZeroAttemptAndLogRetention(t *testing.T) {
	for _, test := range []struct {
		name              string
		timeout, cooldown int64
		statuses          []int
		advance           time.Duration
		attempts          int
		passed            bool
	}{
		{"failed then successful", 60, 3, []int{1, 0}, 3 * time.Second, 2, true},
		{"strict equality permits next attempt", 2, 1, []int{1, 1, 0}, time.Second, 3, true},
		{"cooldown precedes expiry", 1, 2, []int{1}, 2 * time.Second, 1, false},
		{"timeout zero still runs first", 0, 0, []int{1}, time.Nanosecond, 1, false},
		{"zero cooldown has no retry cap", 60, 0, []int{1, 1, 1, 1, 0}, 0, 5, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(100, 123)
			var phases []string
			var failedAttempts []int
			var cooldowns []string
			attempt := 0
			report, err := Run(context.Background(), []Check{commandCheck("m", test.timeout, test.cooldown)}, big.NewInt(1), Deps{
				Now: func() time.Time { return now },
				Cooldown: func(ctx context.Context, seconds *big.Int) error {
					cooldowns = append(cooldowns, seconds.String())
					now = now.Add(test.advance)
					return ctx.Err()
				},
				Execute: func(context.Context, Check) (CommandResult, error) {
					if attempt >= len(test.statuses) {
						t.Error("unexpected extra attempt")
						return CommandResult{}, errors.New("extra attempt")
					}
					status := test.statuses[attempt]
					attempt++
					return CommandResult{Status: status}, nil
				},
				Progress: func(event Event) {
					phases = append(phases, event.Phase)
					if event.Phase == "failed" {
						failedAttempts = append(failedAttempts, event.Result.Attempts)
						if len(event.Result.Failures) != 1 {
							t.Error("failure callback lost latest errors")
						}
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result := report.Results[0]
			if result.Attempts != test.attempts || result.Passed() != test.passed || !result.Complete || !result.HadFailure || !report.RetainLog {
				t.Fatalf("incorrect result: %#v", report)
			}
			if test.passed && len(result.Failures) != 0 {
				t.Fatal("prior errors were not cleared")
			}
			want := []string{"start"}
			failures := test.attempts
			if test.passed {
				failures--
			}
			for i := 0; i < failures; i++ {
				want = append(want, "failed")
				if failedAttempts[i] != i+1 || cooldowns[i] != big.NewInt(test.cooldown).String() {
					t.Fatal("failed-attempt/cooldown order changed")
				}
			}
			want = append(want, "done")
			if !reflect.DeepEqual(phases, want) {
				t.Fatalf("phase order %v want %v", phases, want)
			}
		})
	}
}

func TestWorkerCountsAndNoUnrunSuccess(t *testing.T) {
	huge, _ := new(big.Int).SetString("100000000000000000000000000000000000000000000", 10)
	for _, request := range []*big.Int{big.NewInt(0), big.NewInt(-1), nil} {
		report, err := Run(context.Background(), []Check{commandCheck("m", 0, 0)}, request, Deps{Execute: func(context.Context, Check) (CommandResult, error) {
			t.Fatal("invalid workers executed")
			return CommandResult{}, nil
		}})
		if err == nil || report.Results[0].Started || report.Results[0].Passed() {
			t.Fatalf("unrun check counted successful: %#v %v", report, err)
		}
	}
	if _, err := Run(context.Background(), nil, huge, Deps{}); !errors.Is(err, ErrNoChecks) {
		t.Fatal(err)
	}
	var executed atomic.Int32
	checks := []Check{commandCheck("z", 0, 0), commandCheck("a", 0, 0), commandCheck("m", 0, 0)}
	report, err := Run(context.Background(), checks, huge, Deps{Execute: func(context.Context, Check) (CommandResult, error) { executed.Add(1); return CommandResult{}, nil }})
	if err != nil || executed.Load() != 3 || len(report.Results) != 3 {
		t.Fatalf("huge request conversion: %#v %v", report, err)
	}
	for i, result := range report.Results {
		if result.Check.Machine != checks[i].Machine || !result.Passed() {
			t.Fatal("result order/success changed")
		}
	}
}

func TestProcessFailureStopsSchedulingAndJoinsOwnedChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bStarted := make(chan struct{})
	bJoined := make(chan struct{})
	sentinel := errors.New("fixture spawn/wait failure")
	var third atomic.Bool
	report, err := Run(ctx, []Check{commandCheck("a", 0, 0), commandCheck("b", 0, 0), commandCheck("c", 0, 0)}, big.NewInt(2), Deps{
		Execute: func(ctx context.Context, check Check) (CommandResult, error) {
			switch check.Machine {
			case "a":
				select {
				case <-bStarted:
					return CommandResult{}, sentinel
				case <-ctx.Done():
					return CommandResult{}, ctx.Err()
				}
			case "b":
				close(bStarted)
				defer close(bJoined)
				<-ctx.Done()
				return CommandResult{}, ctx.Err()
			default:
				third.Store(true)
				return CommandResult{}, nil
			}
		},
	})
	if !errors.Is(err, sentinel) || third.Load() || report.Results[2].Started || report.Results[2].Passed() {
		t.Fatalf("scheduling/partial result: %#v %v", report, err)
	}
	select {
	case <-bJoined:
	default:
		t.Fatal("owned running check was not joined")
	}
	if !report.Results[0].Started || report.Results[0].Attempts != 1 || report.Results[0].Complete || !errors.Is(report.Results[1].Err, context.Canceled) {
		t.Fatalf("failure metadata: %#v", report)
	}
}

func TestPredicateFailureDoesNotCancelUnrelatedCheck(t *testing.T) {
	var clock atomic.Int64
	report, err := Run(context.Background(), []Check{commandCheck("failed", 0, 0), commandCheck("passed", 0, 0)}, big.NewInt(2), Deps{
		Now:      func() time.Time { return time.Unix(clock.Add(1), 0) },
		Cooldown: func(ctx context.Context, _ *big.Int) error { return ctx.Err() },
		Execute: func(ctx context.Context, check Check) (CommandResult, error) {
			if err := ctx.Err(); err != nil {
				return CommandResult{}, err
			}
			if check.Machine == "failed" {
				return CommandResult{Status: 1}, nil
			}
			return CommandResult{}, nil
		},
	})
	if err != nil || report.Results[0].Passed() || !report.Results[0].Complete || !report.Results[1].Passed() {
		t.Fatalf("predicate failure canceled other work: %#v %v", report, err)
	}
}

func TestCancellationAndArbitraryTimingDoNotAddDeadlines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := Run(ctx, []Check{commandCheck("m", 0, 0)}, big.NewInt(1), Deps{Execute: func(context.Context, Check) (CommandResult, error) {
		t.Fatal("canceled context executed")
		return CommandResult{}, nil
	}})
	if !errors.Is(err, context.Canceled) || report.Results[0].Started || report.Results[0].Passed() {
		t.Fatal(report, err)
	}
	huge, _ := new(big.Int).SetString("100000000000000000000000000000", 10)
	if expired(time.Unix(0, 0), huge, time.Unix(100, 0)) {
		t.Fatal("large timeout overflowed")
	}
	if err := sleepSeconds(ctx, huge); !errors.Is(err, context.Canceled) {
		t.Fatal("large cooldown was not interruptible", err)
	}
	// Timeout is checked after failure/cooldown only, never during a command.
	now := time.Unix(0, 0)
	report, err = Run(context.Background(), []Check{commandCheck("m", 0, 0)}, big.NewInt(1), Deps{Now: func() time.Time { return now }, Execute: func(context.Context, Check) (CommandResult, error) {
		now = now.Add(time.Hour)
		return CommandResult{}, nil
	}})
	if err != nil || !report.Results[0].Passed() {
		t.Fatal("successful long attempt acquired a child deadline", report, err)
	}
}

func TestCancellationDuringCooldownKeepsFailureAndStopsLaterWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executed := 0
	report, err := Run(ctx, []Check{commandCheck("first", 60, 3), commandCheck("later", 60, 3)}, big.NewInt(1), Deps{
		Execute: func(context.Context, Check) (CommandResult, error) { executed++; return CommandResult{Status: 1}, nil },
		Progress: func(event Event) {
			if event.Phase == "failed" {
				cancel()
			}
		},
		Cooldown: func(ctx context.Context, _ *big.Int) error { <-ctx.Done(); return ctx.Err() },
	})
	if !errors.Is(err, context.Canceled) || executed != 1 || !report.RetainLog || report.Results[0].Complete || len(report.Results[0].Failures) != 1 || report.Results[1].Started || report.Results[1].Passed() {
		t.Fatalf("cooldown cancellation lost partial work: %#v, %v", report, err)
	}
}
