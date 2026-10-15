package deploy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/health"
)

type ledger struct {
	mu    sync.Mutex
	calls []string
}

func (l *ledger) add(s string) { l.mu.Lock(); defer l.mu.Unlock(); l.calls = append(l.calls, s) }
func (l *ledger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}
func machineNames(ms []Machine) string {
	names := []string{}
	for _, m := range ms {
		names = append(names, m.Name)
	}
	return strings.Join(names, ",")
}
func withoutQueries(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !strings.HasPrefix(x, "query:") {
			out = append(out, x)
		}
	}
	return out
}

func script(l *ledger) Deps {
	return Deps{
		QueryTarget:         func(_ context.Context, m Machine) (Status, error) { l.add("query:" + m.Name); return Status{}, nil },
		CarrierName:         func(m Machine) (string, error) { l.add("carrier:" + m.Name); return "carrier", nil },
		AutoRollbackDefault: func(m Machine) (bool, error) { l.add("default:" + m.Name); return false, nil },
		Copy: func(_ context.Context, m Machine, paths []string, _ func(CopyProgress)) (bool, error) {
			l.add("copy:" + m.Name + ":" + strings.Join(paths, "|"))
			return true, nil
		},
		Activate: func(_ context.Context, m Machine, _ *generation.Build, a Action) (bool, error) {
			l.add("activate:" + m.Name + ":" + string(a))
			return true, nil
		},
		ActivateWithRollback: func(_ context.Context, m Machine, _ *generation.Build, a Action) (bool, error) {
			l.add("rollback:" + m.Name + ":" + string(a))
			return true, nil
		},
		SetProfile: func(_ context.Context, m Machine, top string) (bool, error) {
			l.add("profile:" + m.Name + ":" + top)
			return true, nil
		},
		SetCarriedProfile: func(_ context.Context, m Machine, top string) (bool, error) {
			l.add("carried-profile:" + m.Name + ":" + top)
			return true, nil
		},
		RebootAndWait: func(_ context.Context, m Machine, _ WaitMode) error { l.add("reboot:" + m.Name); return nil },
		Checks:        func(ms []Machine) ([]health.Check, error) { l.add("checks:" + machineNames(ms)); return nil, nil },
		Confirm: func(_ context.Context, c Confirmation) (bool, error) {
			host := machineNames(c.Scope)
			if c.Target != nil {
				host = c.Target.Host
			}
			l.add(fmt.Sprintf("confirm:%d:%s", c.Kind, host))
			return true, nil
		},
		ChooseActivation: func(_ context.Context, t Target, retry bool) (ActivationChoice, error) {
			l.add(fmt.Sprintf("choose:%s:%t", t.Host, retry))
			return ActivationContinue, nil
		},
		WithActivation: func(_ context.Context, t Target, body func() error) error {
			l.add("openA:" + t.Host)
			err := body()
			if err == nil {
				l.add("flushA:" + t.Host)
			}
			l.add("closeA:" + t.Host)
			return err
		},
		WithBulkCopy: func(_ context.Context, _ []Target, body func() error) error {
			l.add("openB")
			err := body()
			if err == nil {
				l.add("flushB")
			}
			l.add("closeB")
			return err
		},
	}
}

func runPlan(t *testing.T, in Input, d Deps) (Report, error) {
	t.Helper()
	return Run(context.Background(), prepared(t, in), d)
}
func expectCalls(t *testing.T, l *ledger, want ...string) {
	t.Helper()
	if got := withoutQueries(l.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls=%q want=%q", got, want)
	}
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("private barrier watchdog")
		var zero T
		return zero
	}
}

func TestOrdersFilteringAndLazyAccess(t *testing.T) {
	in := inputFor("a", "b", "c", "missing")
	in.Targets = []Target{in.Targets[2], in.Targets[0], in.Targets[1]}
	in.Options.HealthChecks = true
	l := &ledger{}
	d := script(l)
	d.QueryTarget = func(_ context.Context, m Machine) (Status, error) {
		l.add("query:" + m.Name)
		if m.Name == "a" {
			return Status{CurrentToplevel: text("/literal/a"), Generations: &generation.HostList{Generations: []*generation.Host{{Current: true, Toplevel: text("/literal/a")}}}}, nil
		}
		return Status{}, nil
	}
	var skips []string
	d.Event = func(e Event) {
		if e.Kind == CurrentSkipped {
			skips = append(skips, e.Host)
		}
	}
	report, err := runPlan(t, in, d)
	if err != nil {
		t.Fatal(err)
	}
	expectCalls(t, l, "openB", "copy:b:/literal/b", "copy:c:/literal/c", "flushB", "closeB", "openA:c", "activate:c:switch", "profile:c:/literal/c", "flushA:c", "closeA:c", "openA:b", "activate:b:switch", "profile:b:/literal/b", "flushA:b", "closeA:b", "checks:b,c")
	if !reflect.DeepEqual(skips, []string{"a"}) || report.Outcomes[0].Target.Host != "c" || !report.Outcomes[1].SkippedCurrent || report.Outcomes[2].Target.Host != "b" {
		t.Fatal(report, skips)
	}
	// All-current returns without even looking up any copy/health/rollback binding.
	d = Deps{QueryTarget: func(_ context.Context, m Machine) (Status, error) {
		top := "/literal/" + m.Name
		return Status{CurrentToplevel: &top, Generations: &generation.HostList{Generations: []*generation.Host{{Current: true, Toplevel: &top}}}}, nil
	}}
	if _, err := runPlan(t, in, d); err != nil {
		t.Fatal(err)
	}
	// Copy-only never queries status; missing machine bindings remain in bulk sort.
	for _, mode := range []string{"bulk", "interactive", "one"} {
		t.Run(mode, func(t *testing.T) {
			in := inputFor("a", "missing")
			in.Targets = in.Targets[:1]
			in.Options.CopyOnly = true
			in.Options.Interactive = mode == "interactive"
			in.Options.OneByOne = mode == "one"
			l := &ledger{}
			d := script(l)
			d.QueryTarget = nil
			report, err := runPlan(t, in, d)
			if mode == "bulk" {
				if err == nil || err.Error() != "programming error: sorted generations length != original generations" || len(l.snapshot()) != 0 {
					t.Fatal(report, err, l.snapshot())
				}
			} else if err != nil || report.Outcomes[0].Copy.State != Succeeded {
				t.Fatal(report, err)
			}
		})
	}
}

func TestCarrierRoundRobinAndCopyRouting(t *testing.T) {
	in := inputFor("a1", "s1", "b1", "a2", "s2", "a3", "b2")
	in.Options.CopyOnly = true
	for i := range in.Machines {
		if !strings.HasPrefix(in.Machines[i].Name, "s") {
			in.Machines[i].Carried = true
		}
	}
	in.Machines[0].Localhost = true
	// Deliberately unrelated returned generation order.
	in.Targets = []Target{in.Targets[6], in.Targets[5], in.Targets[4], in.Targets[3], in.Targets[2], in.Targets[1], in.Targets[0]}
	l := &ledger{}
	d := script(l)
	d.CarrierName = func(m Machine) (string, error) { l.add("carrier:" + m.Name); return m.Name[:1], nil }
	report, err := runPlan(t, in, d)
	if err != nil {
		t.Fatal(err)
	}
	expectCalls(t, l, "carrier:a1", "carrier:b1", "carrier:a2", "carrier:a3", "carrier:b2", "openB", "copy:s1:/literal/s1", "copy:s2:/literal/s2", "copy:a1:/literal/a1", "copy:b1:/literal/b1", "copy:a2:/literal/a2", "copy:b2:/literal/b2", "copy:a3:/literal/a3", "flushB", "closeB")
	if report.Outcomes[6].Copy.State != Noop {
		t.Fatal(report)
	}
	d = script(&ledger{})
	boom := errors.New("carrier failure")
	d.CarrierName = func(Machine) (string, error) { return "", boom }
	called := false
	d.Copy = func(context.Context, Machine, []string, func(CopyProgress)) (bool, error) {
		called = true
		return true, nil
	}
	if _, err := runPlan(t, in, d); !errors.Is(err, boom) || called {
		t.Fatal(err, called)
	}
}

func TestCopyRollbackPredicateAndDecline(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		disable, enable, def bool
		helper               *string
		want                 []string
		defaults             int
	}{
		{"disable-wins", true, true, true, text("helper"), []string{"/literal/a"}, 0},
		{"explicit-enable-empty", false, true, false, text(""), []string{"/literal/a", ""}, 0},
		{"default-before-nil", false, false, true, nil, []string{"/literal/a"}, 1},
		{"default-false", false, false, false, text("helper"), []string{"/literal/a"}, 1},
		{"default-true", false, false, true, text("helper"), []string{"/literal/a", "helper"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := inputFor("a")
			in.Options.CopyOnly = true
			in.Options.DisableAutoRollback = tc.disable
			in.Options.EnableAutoRollback = tc.enable
			in.Targets[0].Generation.AutoRollback = tc.helper
			in.Machines[0].Localhost = true
			d := Deps{}
			calls := 0
			d.AutoRollbackDefault = func(Machine) (bool, error) { calls++; return tc.def, nil }
			d.Copy = func(_ context.Context, m Machine, paths []string, _ func(CopyProgress)) (bool, error) {
				if !m.Localhost || !reflect.DeepEqual(paths, tc.want) {
					t.Fatal(m, paths)
				}
				return true, nil
			}
			report, err := runPlan(t, in, d)
			if err != nil || calls != tc.defaults || report.Outcomes[0].Copy.State != Noop {
				t.Fatal(report, err, calls)
			}
		})
	}
	in := inputFor("a")
	in.Options.CopyOnly = true
	in.Options.Interactive = true
	in.Options.DisableAutoRollback = false
	seen := []EventKind{}
	d := Deps{Confirm: func(context.Context, Confirmation) (bool, error) { return false, nil }, Event: func(e Event) { seen = append(seen, e.Kind) }}
	report, err := runPlan(t, in, d)
	if err != nil || report.Outcomes[0].Copy.State != Skipped || !reflect.DeepEqual(seen, []EventKind{CopyingToHost, CopySkipped}) {
		t.Fatal(report, err, seen)
	}
}

func TestBulkNormalFailuresDrainInCompletionOrder(t *testing.T) {
	in := inputFor("a", "b", "c")
	in.Options.CopyWorkers = integer(2)
	started := make(chan string, 3)
	a, b := make(chan struct{}), make(chan struct{})
	finished := make(chan string, 3)
	d := script(&ledger{})
	d.Copy = func(_ context.Context, m Machine, _ []string, _ func(CopyProgress)) (bool, error) {
		started <- m.Name
		switch m.Name {
		case "a":
			<-a
		case "b":
			<-b
		}
		return m.Name == "c", nil
	}
	d.Event = func(e Event) {
		if e.Kind == CopyFinished {
			finished <- e.Host
		}
	}
	d.Activate = nil
	type result struct {
		report Report
		err    error
	}
	done := make(chan result, 1)
	go func() { report, err := runPlan(t, in, d); done <- result{report, err} }()
	first := []string{receive(t, started), receive(t, started)}
	if !(first[0] == "a" && first[1] == "b" || first[0] == "b" && first[1] == "a") {
		t.Fatal(first)
	}
	close(b)
	if receive(t, finished) != "b" {
		t.Fatal("B did not complete first")
	}
	if receive(t, started) != "c" {
		t.Fatal("normal false cancelled queue")
	}
	if receive(t, finished) != "c" {
		t.Fatal("C")
	}
	close(a)
	if receive(t, finished) != "a" {
		t.Fatal("A")
	}
	got := receive(t, done)
	if got.err == nil || !reflect.DeepEqual(got.report.FailedCopies, []string{"b", "a"}) {
		t.Fatal(got)
	}
	for _, o := range got.report.Outcomes {
		if o.Activation.State != NotReached {
			t.Fatal(o)
		}
	}
}

func TestCopyInfrastructureStopsCancelsAndJoins(t *testing.T) {
	in := inputFor("a", "b", "c")
	in.Options.CopyOnly = true
	in.Options.CopyWorkers = integer(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 3)
	releaseA := make(chan struct{})
	joinB := make(chan struct{})
	cancelledB := make(chan struct{})
	boom := errors.New("copy transport")
	d := Deps{Copy: func(ctx context.Context, m Machine, _ []string, _ func(CopyProgress)) (bool, error) {
		started <- m.Name
		if m.Name == "a" {
			<-releaseA
			return false, boom
		}
		<-ctx.Done()
		close(cancelledB)
		<-joinB
		return false, ctx.Err()
	}}
	type result struct {
		r Report
		e error
	}
	done := make(chan result, 1)
	go func() { r, e := Run(ctx, prepared(t, in), d); done <- result{r, e} }()
	receive(t, started)
	receive(t, started)
	close(releaseA)
	receive(t, cancelledB)
	select {
	case got := <-done:
		t.Fatal("returned before callback join", got)
	default:
	}
	select {
	case name := <-started:
		t.Fatal("new dequeue", name)
	default:
	}
	close(joinB)
	got := receive(t, done)
	if !errors.Is(got.e, boom) || got.r.Outcomes[0].Copy.State != Unknown || got.r.Outcomes[1].Copy.State != Unknown || got.r.Outcomes[2].Copy.State != NotReached {
		t.Fatal(got)
	}
}

func TestQueryFiveBoundAndInfrastructureJoin(t *testing.T) {
	in := inputFor("a", "b", "c", "d", "e", "f")
	started := make(chan string, 6)
	release := make(chan struct{})
	finish := make(chan struct{})
	cancelled := make(chan string, 4)
	boom := errors.New("query transport")
	d := Deps{QueryTarget: func(ctx context.Context, m Machine) (Status, error) {
		started <- m.Name
		if m.Name == "a" {
			<-release
			return Status{}, boom
		}
		<-ctx.Done()
		cancelled <- m.Name
		<-finish
		return Status{}, ctx.Err()
	}}
	type result struct {
		r Report
		e error
	}
	done := make(chan result, 1)
	go func() { r, e := runPlan(t, in, d); done <- result{r, e} }()
	first := map[string]bool{}
	for i := 0; i < 5; i++ {
		first[receive(t, started)] = true
	}
	if len(first) != 5 || first["f"] {
		t.Fatal(first)
	}
	close(release)
	for i := 0; i < 4; i++ {
		receive(t, cancelled)
	}
	select {
	case x := <-done:
		t.Fatal("unjoined", x)
	default:
	}
	select {
	case x := <-started:
		t.Fatal("dequeued sixth", x)
	default:
	}
	close(finish)
	got := receive(t, done)
	if !errors.Is(got.e, boom) || got.r.Outcomes[5].Query.State != NotReached {
		t.Fatal(got)
	}
	for _, o := range got.r.Outcomes {
		if o.Copy.State != NotReached {
			t.Fatal("copy before joined queries", o)
		}
	}
}

func TestExternalCancellationStopsCopyQueue(t *testing.T) {
	in := inputFor("a", "b", "c")
	in.Options.CopyOnly = true
	in.Options.CopyWorkers = integer(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	joined := make(chan struct{})
	var calls atomic.Int32
	d := Deps{Copy: func(ctx context.Context, _ Machine, _ []string, _ func(CopyProgress)) (bool, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(joined)
		return false, ctx.Err()
	}}
	type result struct {
		r Report
		e error
	}
	done := make(chan result, 1)
	go func() { r, e := Run(ctx, prepared(t, in), d); done <- result{r, e} }()
	receive(t, started)
	cancel()
	got := receive(t, done)
	receive(t, joined)
	if !errors.Is(got.e, context.Canceled) || calls.Load() != 1 || got.r.Outcomes[0].Copy.State != Unknown || got.r.Outcomes[1].Copy.State != NotReached {
		t.Fatal(got, calls.Load())
	}
}

func TestActivationDryRetryProfilesAndPartialEffects(t *testing.T) {
	for _, action := range []Action{Boot, Switch, Test, DryActivate} {
		t.Run(string(action), func(t *testing.T) {
			in := inputFor("a")
			in.Options.Action = action
			in.Options.Interactive = true
			in.Options.DryActivateFirst = true
			l := &ledger{}
			d := script(l)
			choices := 0
			d.ChooseActivation = func(_ context.Context, _ Target, retry bool) (ActivationChoice, error) {
				if !retry {
					t.Fatal("retry unavailable")
				}
				choices++
				l.add("choice")
				if choices == 1 {
					return ActivationRetry, nil
				}
				return ActivationContinue, nil
			}
			report, err := runPlan(t, in, d)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"confirm:0:a", "copy:a:/literal/a", "openA:a", "activate:a:dry-activate", "choice", "activate:a:dry-activate", "choice", "activate:a:" + string(action)}
			if action == Boot || action == Switch {
				want = append(want, "profile:a:/literal/a")
			}
			want = append(want, "flushA:a", "closeA:a")
			expectCalls(t, l, want...)
			if report.Outcomes[0].Dry.State != Succeeded || report.Outcomes[0].Activation.State != Succeeded {
				t.Fatal(report)
			}
		})
	}
	in := inputFor("a", "b")
	l := &ledger{}
	d := script(l)
	d.SetProfile = func(_ context.Context, m Machine, _ string) (bool, error) {
		l.add("profile-failed:" + m.Name)
		return false, nil
	}
	report, err := runPlan(t, in, d)
	if err == nil || report.Outcomes[0].Activation.State != Succeeded || report.Outcomes[0].Profile.State != Failed || report.Outcomes[1].Activation.State != NotReached {
		t.Fatal(report, err)
	}
	expectCalls(t, l, "openB", "copy:a:/literal/a", "copy:b:/literal/b", "flushB", "closeB", "openA:a", "activate:a:switch", "profile-failed:a", "closeA:a")
	// Copy and activation independently read the rollback default; no caching.
	in = inputFor("a")
	in.Options.DisableAutoRollback = false
	in.Targets[0].Generation.AutoRollback = text("")
	l = &ledger{}
	d = script(l)
	n := 0
	d.AutoRollbackDefault = func(Machine) (bool, error) { n++; return n == 2, nil }
	report, err = runPlan(t, in, d)
	if err != nil || n != 2 || report.Outcomes[0].Activation.State != Succeeded {
		t.Fatal(report, err, n)
	}
	expectCalls(t, l, "openB", "copy:a:/literal/a", "flushB", "closeB", "openA:a", "rollback:a:switch", "profile:a:/literal/a", "flushA:a", "closeA:a")
}

func TestCarriedActivationAndScopeErrors(t *testing.T) {
	for _, action := range []Action{Boot, Switch, Test, DryActivate} {
		t.Run(string(action), func(t *testing.T) {
			in := inputFor("a")
			in.Machines[0].Carried = true
			in.Options.Action = action
			in.Options.OneByOne = true
			in.Options.Interactive = true
			in.Options.DryActivateFirst = true
			l := &ledger{}
			d := script(l)
			d.Activate = nil
			d.AutoRollbackDefault = nil
			d.ChooseActivation = nil
			report, err := runPlan(t, in, d)
			if err != nil || report.Outcomes[0].Activation.State != Noop {
				t.Fatal(report, err)
			}
			want := []string{"confirm:0:a", "copy:a:/literal/a", "openA:a"}
			if action == Switch {
				want = append(want, "carried-profile:a:/literal/a")
			}
			want = append(want, "flushA:a", "closeA:a")
			expectCalls(t, l, want...)
		})
	}
	boom := errors.New("release failed")
	in := inputFor("a")
	d := script(&ledger{})
	d.WithActivation = func(_ context.Context, _ Target, body func() error) error {
		if err := body(); err != nil {
			return err
		}
		return boom
	}
	report, err := runPlan(t, in, d)
	if !errors.Is(err, boom) || report.Outcomes[0].Profile.State != Succeeded {
		t.Fatal(report, err)
	}
	d = script(&ledger{})
	d.WithActivation = func(context.Context, Target, func() error) error { return nil }
	_, err = runPlan(t, in, d)
	var bindingError *BindingError
	if !errors.As(err, &bindingError) {
		t.Fatal(err)
	}
	d = script(&ledger{})
	d.WithBulkCopy = func(_ context.Context, _ []Target, body func() error) error { _ = body(); return body() }
	report, err = runPlan(t, in, d)
	if !errors.As(err, &bindingError) || report.Outcomes[0].Copy.State != Succeeded {
		t.Fatal(report, err)
	}
}

func TestBulkAndSequentialRebootHealthDifferences(t *testing.T) {
	for _, one := range []bool{false, true} {
		for _, reason := range []string{"localhost", "carried", "decline"} {
			t.Run(fmt.Sprintf("%t/%s", one, reason), func(t *testing.T) {
				in := inputFor("a", "b")
				in.Options.Action = Boot
				in.Options.Reboot = true
				in.Options.HealthChecks = true
				in.Options.OneByOne = one
				in.Options.Interactive = true
				in.Options.Wait = WaitMode{WaitSeconds, new(big.Int).Lsh(integer(1), 200)}
				if reason == "localhost" {
					in.Machines[0].Localhost = true
					in.Machines[0].Carried = true
				}
				if reason == "carried" {
					in.Machines[0].Carried = true
					in.Machines[0].Runnable = false
				}
				l := &ledger{}
				d := script(l)
				d.Confirm = func(_ context.Context, c Confirmation) (bool, error) {
					if c.Kind == ConfirmReboot && c.Target.Host == "a" && reason == "decline" {
						return false, nil
					}
					return true, nil
				}
				var scopes []string
				d.Checks = func(ms []Machine) ([]health.Check, error) { scopes = append(scopes, machineNames(ms)); return nil, nil }
				d.RebootAndWait = func(_ context.Context, m Machine, wait WaitMode) error {
					if wait.Seconds.Cmp(in.Options.Wait.Seconds) != 0 {
						t.Fatal("narrowed wait")
					}
					wait.Seconds.SetInt64(-1)
					return nil
				}
				report, err := runPlan(t, in, d)
				if err != nil {
					t.Fatal(err)
				}
				if reason == "localhost" && report.Outcomes[0].Reboot.Reason != "localhost" {
					t.Fatal(report)
				}
				want := []string{"a,b"}
				if reason == "carried" {
					want = []string{"b"}
				}
				if one {
					want = []string{"b"}
				}
				if !reflect.DeepEqual(scopes, want) {
					t.Fatal(scopes, want)
				}
				if in.Options.Wait.Seconds.Sign() < 0 {
					t.Fatal("borrowed wait mutated")
				}
			})
		}
	}
}

func commandCheck(machine, description string) health.Check {
	return health.Check{Machine: machine, Description: description, Argv: []string{"fake"}, Timing: health.Timing{Timeout: integer(0), Cooldown: integer(0)}, Command: &health.Command{ExpectedStatus: integer(0)}}
}
func healthResults(checks []health.Check, failed ...int) health.Report {
	r := health.Report{Results: make([]health.Result, len(checks))}
	for i, c := range checks {
		r.Results[i] = health.Result{Check: c, Started: true, Complete: true, Attempts: 1}
		for _, index := range failed {
			if i == index {
				r.Results[i].Failures = []health.Failure{{Kind: "fixture"}}
				r.Results[i].HadFailure = true
				r.RetainLog = true
			}
		}
	}
	return r
}

func TestHealthRetryIndicesScopeAndRetainedLog(t *testing.T) {
	in := inputFor("a", "carried", "b")
	in.Options.Interactive = true
	in.Options.HealthChecks = true
	in.Machines[1].Carried = true
	in.Machines[1].Runnable = false
	l := &ledger{}
	d := script(l)
	checks := []health.Check{commandCheck("a", "duplicate"), commandCheck("a", "duplicate"), commandCheck("b", "last")}
	d.Checks = func(ms []Machine) ([]health.Check, error) {
		if machineNames(ms) != "a,b" {
			t.Fatal(ms)
		}
		return checks, nil
	}
	var rounds [][]string
	d.RunHealth = func(_ context.Context, scope []Machine, submitted []health.Check) (health.Report, error) {
		if machineNames(scope) != "a,carried,b" {
			t.Fatal(scope)
		}
		names := []string{}
		for _, c := range submitted {
			names = append(names, c.Description)
		}
		rounds = append(rounds, names)
		switch len(rounds) {
		case 1:
			return healthResults(submitted, 1, 2), nil
		case 2:
			return healthResults(submitted, 0), nil
		default:
			return healthResults(submitted), nil
		}
	}
	choices := 0
	d.ChooseHealth = func(context.Context, []Machine, health.Report) (HealthChoice, error) {
		choices++
		if choices == 1 {
			return HealthRetryFailed, nil
		}
		return HealthRetryAll, nil
	}
	var indices [][]int
	d.Event = func(e Event) {
		if e.Kind == HealthRoundStarted {
			indices = append(indices, e.Indices)
		}
	}
	report, err := runPlan(t, in, d)
	if err != nil || !report.RetainLog || len(report.HealthRounds) != 3 || !reflect.DeepEqual(indices, [][]int{{0, 1, 2}, {1, 2}, {0, 1, 2}}) {
		t.Fatal(report, err, indices)
	}
	if !reflect.DeepEqual(rounds, [][]string{{"duplicate", "duplicate", "last"}, {"duplicate", "last"}, {"duplicate", "duplicate", "last"}}) {
		t.Fatal(rounds)
	}
	for _, i := range []int{0, 2} {
		if report.Outcomes[i].Health.State != Succeeded {
			t.Fatal(report)
		}
	}
}

func TestHealthFailureChoicesBindingsAndMalformedReports(t *testing.T) {
	for _, name := range []string{"no-checks", "decline", "continue", "abort", "keep-going", "failure", "infrastructure", "incomplete", "wrong-check", "wrong-count", "bad-choice"} {
		t.Run(name, func(t *testing.T) {
			in := inputFor("a")
			in.Options.HealthChecks = true
			in.Options.Interactive = name != "keep-going" && name != "failure" && name != "infrastructure"
			in.Options.KeepGoing = name == "keep-going" || name == "infrastructure"
			d := script(&ledger{})
			check := commandCheck("a", "check")
			d.Checks = func([]Machine) ([]health.Check, error) {
				if name == "no-checks" {
					return nil, nil
				}
				return []health.Check{check}, nil
			}
			boom := errors.New("health infrastructure")
			runs := 0
			d.RunHealth = func(_ context.Context, _ []Machine, cs []health.Check) (health.Report, error) {
				runs++
				report := healthResults(cs, 0)
				switch name {
				case "infrastructure":
					return report, boom
				case "incomplete":
					report.Results[0].Complete = false
				case "wrong-check":
					report.Results[0].Check.Description = "foreign"
				case "wrong-count":
					report.Results = nil
				}
				return report, nil
			}
			d.Confirm = func(_ context.Context, c Confirmation) (bool, error) {
				return !(name == "decline" && c.Kind == ConfirmHealth), nil
			}
			d.ChooseHealth = func(context.Context, []Machine, health.Report) (HealthChoice, error) {
				switch name {
				case "continue":
					return HealthContinue, nil
				case "bad-choice":
					return HealthChoice(250), nil
				default:
					return HealthAbort, nil
				}
			}
			report, err := runPlan(t, in, d)
			wantError := name != "no-checks" && name != "decline" && name != "continue" && name != "keep-going"
			if (err != nil) != wantError {
				t.Fatal(name, report, err)
			}
			if name == "no-checks" || name == "decline" {
				if runs != 0 {
					t.Fatal(runs)
				}
			} else if !report.RetainLog {
				t.Fatal("lost log on error/choice", report)
			}
			if name == "infrastructure" && !errors.Is(err, boom) {
				t.Fatal(err)
			}
			if strings.HasPrefix(name, "wrong-") || name == "incomplete" {
				if report.Outcomes[0].Health.State != Unknown {
					t.Fatal(report)
				}
			}
		})
	}
}

// Compose the reviewed runner without commands or real clock/cooldown waits.
// Equal duplicate checks retain their separate submitted indices.
func TestReviewedHealthRunCompositionWithFakeClock(t *testing.T) {
	in := inputFor("a")
	in.Options.HealthChecks = true
	in.Options.Interactive = true
	d := script(&ledger{})
	duplicate := commandCheck("a", "duplicate")
	duplicate.Timeout = integer(2)
	checks := []health.Check{duplicate, duplicate, commandCheck("a", "timeout-zero")}
	d.Checks = func([]Machine) ([]health.Check, error) { return checks, nil }
	now := time.Unix(0, 0)
	round := 0
	var calls [][]int
	var submittedIndices [][]int
	d.Event = func(e Event) {
		if e.Kind == HealthRoundStarted {
			submittedIndices = append(submittedIndices, e.Indices)
		}
	}
	d.RunHealth = func(ctx context.Context, _ []Machine, cs []health.Check) (health.Report, error) {
		round++
		attempts := make([]int, len(cs))
		index := -1
		report, err := health.Run(ctx, cs, integer(1), health.Deps{
			Progress: func(e health.Event) {
				if e.Phase == "start" {
					index = e.Index
				}
			},
			Execute: func(_ context.Context, _ health.Check) (health.CommandResult, error) {
				attempts[index]++
				status := 0
				if round == 1 && (index != 0 || attempts[index] == 1) {
					status = 1
				}
				return health.CommandResult{Status: status}, nil
			}, Now: func() time.Time { return now }, Cooldown: func(context.Context, *big.Int) error { now = now.Add(time.Second); return nil },
		})
		calls = append(calls, attempts)
		return report, err
	}
	d.ChooseHealth = func(context.Context, []Machine, health.Report) (HealthChoice, error) { return HealthRetryFailed, nil }
	report, err := runPlan(t, in, d)
	if err != nil || !report.RetainLog || len(report.HealthRounds) != 2 {
		t.Fatal(report, err, calls)
	}
	if !reflect.DeepEqual(calls, [][]int{{2, 3, 1}, {1, 1}}) || !reflect.DeepEqual(submittedIndices, [][]int{{0, 1, 2}, {1, 2}}) {
		t.Fatal(calls, submittedIndices)
	}
	first := report.HealthRounds[0]
	if !first.Results[0].Passed() || !first.Results[0].HadFailure || first.Results[1].Passed() || first.Results[2].Passed() {
		t.Fatal(first)
	}
	for _, result := range report.HealthRounds[1].Results {
		if !result.Passed() || result.HadFailure {
			t.Fatal(result)
		}
	}
	if report.Outcomes[0].Health.State != Succeeded {
		t.Fatal(report)
	}
}

func TestEventSerializationAndProgressOwnership(t *testing.T) {
	in := inputFor("a", "b")
	in.Options.CopyOnly = true
	in.Options.CopyWorkers = integer(2)
	entered := make(chan struct{})
	release := make(chan struct{})
	startProgress := make(chan struct{})
	attempted := make(chan struct{}, 2)
	var once sync.Once
	var active atomic.Int32
	var unknownTotals atomic.Int32
	var bad atomic.Bool
	d := Deps{Copy: func(_ context.Context, _ Machine, _ []string, progress func(CopyProgress)) (bool, error) {
		current, total := integer(1), integer(2)
		attempted <- struct{}{}
		<-startProgress
		progress(CopyProgress{Path: "before-total", Current: current})
		progress(CopyProgress{Path: "literal", Current: current, Total: total})
		if current.Int64() != 1 || total.Int64() != 2 {
			bad.Store(true)
		}
		return true, nil
	}, Event: func(e Event) {
		if active.Add(1) != 1 {
			bad.Store(true)
		}
		defer active.Add(-1)
		if e.Kind == CopyAdvanced {
			once.Do(func() { close(entered); <-release })
			e.Progress.Current.SetInt64(99)
			if e.Progress.Total == nil {
				unknownTotals.Add(1)
			} else {
				e.Progress.Total.SetInt64(99)
			}
		}
	}}
	done := make(chan error, 1)
	go func() { _, err := runPlan(t, in, d); done <- err }()
	receive(t, attempted)
	receive(t, attempted)
	close(startProgress)
	receive(t, entered)
	close(release)
	if err := receive(t, done); err != nil || bad.Load() || unknownTotals.Load() != 2 {
		t.Fatal(err, bad.Load(), unknownTotals.Load())
	}
}

func TestSequentialCopyFailuresRetainEarlierEffects(t *testing.T) {
	for _, one := range []bool{false, true} {
		t.Run(fmt.Sprint(one), func(t *testing.T) {
			in := inputFor("a", "b", "c")
			in.Options.OneByOne = one
			in.Options.Interactive = true
			in.Options.Action = Boot
			in.Options.Reboot = true
			in.Options.HealthChecks = true
			l := &ledger{}
			d := script(l)
			d.Copy = func(_ context.Context, m Machine, _ []string, _ func(CopyProgress)) (bool, error) {
				l.add("copy:" + m.Name)
				return m.Name != "b", nil
			}
			d.Checks = func(ms []Machine) ([]health.Check, error) {
				l.add("checks:" + machineNames(ms))
				return []health.Check{commandCheck(ms[0].Name, "check")}, nil
			}
			d.RunHealth = func(_ context.Context, ms []Machine, cs []health.Check) (health.Report, error) {
				l.add("health:" + machineNames(ms))
				return healthResults(cs), nil
			}
			report, err := runPlan(t, in, d)
			if err == nil || report.Outcomes[1].Copy.State != Failed || report.Outcomes[2].Copy.State != NotReached || !reflect.DeepEqual(report.FailedCopies, []string{"b"}) {
				t.Fatal(report, err)
			}
			want := []string{"confirm:0:a", "copy:a"}
			if one {
				want = append(want, "openA:a", "choose:a:false", "activate:a:boot", "profile:a:/literal/a", "flushA:a", "closeA:a", "confirm:1:a", "reboot:a", "checks:a", "confirm:2:a", "health:a")
			}
			want = append(want, "confirm:0:b", "copy:b")
			expectCalls(t, l, want...)
			if one && report.Outcomes[0].Health.State != Succeeded {
				t.Fatal(report)
			}
			if report.Outcomes[1].Activation.State != NotReached || report.Outcomes[2].Reboot.State != NotReached {
				t.Fatal(report)
			}
		})
	}
}

func TestCopyAndActivationSkipsLimitLaterScope(t *testing.T) {
	for _, one := range []bool{false, true} {
		t.Run(fmt.Sprint(one), func(t *testing.T) {
			in := inputFor("a", "b", "c")
			in.Options.OneByOne = one
			in.Options.Interactive = true
			in.Options.Action = Boot
			in.Options.Reboot = true
			in.Options.HealthChecks = true
			l := &ledger{}
			d := script(l)
			d.Confirm = func(_ context.Context, c Confirmation) (bool, error) {
				return c.Kind != ConfirmCopy || c.Target.Host != "a", nil
			}
			d.ChooseActivation = func(_ context.Context, t Target, _ bool) (ActivationChoice, error) {
				if t.Host == "b" {
					return ActivationSkip, nil
				}
				return ActivationContinue, nil
			}
			report, err := runPlan(t, in, d)
			if err != nil {
				t.Fatal(report, err)
			}
			if report.Outcomes[0].Copy.State != Skipped || report.Outcomes[1].Activation.State != Skipped || report.Outcomes[0].Reboot.State != NotReached || report.Outcomes[1].Reboot.State != NotReached {
				t.Fatal(report)
			}
			if !strings.Contains(strings.Join(l.snapshot(), "|"), "checks:c") || strings.Contains(strings.Join(l.snapshot(), "|"), "reboot:b") {
				t.Fatal(l.snapshot())
			}
			want := []string{}
			if one {
				want = append(want, "copy:b:/literal/b", "openA:b", "flushA:b", "closeA:b", "copy:c:/literal/c", "openA:c", "activate:c:boot", "profile:c:/literal/c", "flushA:c", "closeA:c", "reboot:c", "checks:c")
			} else {
				want = append(want, "copy:b:/literal/b", "copy:c:/literal/c", "openA:b", "flushA:b", "closeA:b", "openA:c", "activate:c:boot", "profile:c:/literal/c", "flushA:c", "closeA:c", "reboot:c", "checks:c")
			}
			expectCalls(t, l, want...)
		})
	}
}

func TestCarriedFailureAndHealthAbortPreserveRealPrefix(t *testing.T) {
	in := inputFor("a", "b")
	in.Machines[0].Carried = true
	in.Options.OneByOne = true
	l := &ledger{}
	d := script(l)
	profile := "old"
	d.SetCarriedProfile = func(_ context.Context, _ Machine, top string) (bool, error) {
		profile = top
		l.add("profile-changed-callback-failed")
		return false, nil
	}
	report, err := runPlan(t, in, d)
	if err == nil || profile != "/literal/a" || report.Outcomes[0].Profile.State != Failed || report.Outcomes[0].Activation.State != Noop || report.Outcomes[1].Copy.State != NotReached {
		t.Fatal(report, err, profile)
	}
	expectCalls(t, l, "copy:a:/literal/a", "openA:a", "profile-changed-callback-failed", "closeA:a")
	in = inputFor("a", "b")
	in.Options.Action = Boot
	in.Options.Reboot = true
	in.Options.HealthChecks = true
	in.Options.Interactive = true
	l = &ledger{}
	d = script(l)
	d.Checks = func(ms []Machine) ([]health.Check, error) {
		l.add("checks:" + machineNames(ms))
		return []health.Check{commandCheck("a", "bad")}, nil
	}
	d.RunHealth = func(_ context.Context, _ []Machine, cs []health.Check) (health.Report, error) {
		l.add("health-failed")
		return healthResults(cs, 0), nil
	}
	d.ChooseHealth = func(context.Context, []Machine, health.Report) (HealthChoice, error) {
		l.add("health-abort")
		return HealthAbort, nil
	}
	report, err = runPlan(t, in, d)
	if err == nil || !report.RetainLog || report.Outcomes[0].Reboot.State != Succeeded || report.Outcomes[1].Reboot.State != Succeeded || report.Outcomes[0].Health.State != Failed {
		t.Fatal(report, err)
	}
	expectCalls(t, l, "confirm:0:a", "copy:a:/literal/a", "confirm:0:b", "copy:b:/literal/b", "openA:a", "choose:a:false", "activate:a:boot", "profile:a:/literal/a", "flushA:a", "closeA:a", "openA:b", "choose:b:false", "activate:b:boot", "profile:b:/literal/b", "flushA:b", "closeA:b", "confirm:1:a", "reboot:a", "confirm:1:b", "reboot:b", "checks:a,b", "confirm:2:a,b", "health-failed", "health-abort")
}

func TestCancellationBetweenCompletedActionsStopsNextEffect(t *testing.T) {
	in := inputFor("a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &ledger{}
	d := script(l)
	d.Activate = func(context.Context, Machine, *generation.Build, Action) (bool, error) {
		l.add("activated-before-cancel")
		cancel()
		return true, nil
	}
	report, err := Run(ctx, prepared(t, in), d)
	if !errors.Is(err, context.Canceled) || report.Outcomes[0].Activation.State != Succeeded || report.Outcomes[0].Profile.State != NotReached || report.Outcomes[1].Activation.State != NotReached {
		t.Fatal(report, err)
	}
	expectCalls(t, l, "openB", "copy:a:/literal/a", "copy:b:/literal/b", "flushB", "closeB", "openA:a", "activated-before-cancel", "closeA:a")
}

func TestReachedBindingsAndInvalidActivationChoice(t *testing.T) {
	for _, name := range []string{"query", "copy", "rollback-default", "activate", "profile", "reboot", "checks", "run-health", "retry-without-dry", "bad-choice"} {
		t.Run(name, func(t *testing.T) {
			in := inputFor("a")
			d := script(&ledger{})
			switch name {
			case "query":
				d.QueryTarget = nil
			case "copy":
				d.Copy = nil
			case "rollback-default":
				in.Options.DisableAutoRollback = false
				d.AutoRollbackDefault = nil
			case "activate":
				d.Activate = nil
			case "profile":
				d.SetProfile = nil
			case "reboot":
				in.Options.Action = Boot
				in.Options.Reboot = true
				d.RebootAndWait = nil
			case "checks":
				in.Options.HealthChecks = true
				d.Checks = nil
			case "run-health":
				in.Options.HealthChecks = true
				d.Checks = func([]Machine) ([]health.Check, error) { return []health.Check{commandCheck("a", "check")}, nil }
			default:
				in.Options.Interactive = true
				d.ChooseActivation = func(context.Context, Target, bool) (ActivationChoice, error) {
					if name == "retry-without-dry" {
						return ActivationRetry, nil
					}
					return ActivationChoice(255), nil
				}
			}
			_, err := runPlan(t, in, d)
			var binding *BindingError
			if !errors.As(err, &binding) {
				t.Fatal(name, err)
			}
		})
	}
}

// A query callback can return an ordinary partial status. Even a completed
// current candidate is not skipped until the last owned query has joined.
func TestPartialStatusAndQueryDecisionBarrier(t *testing.T) {
	in := inputFor("a", "b")
	ready := make(chan string, 2)
	release := make(chan struct{})
	var decisions atomic.Int32
	d := script(&ledger{})
	d.QueryTarget = func(_ context.Context, m Machine) (Status, error) {
		ready <- m.Name
		if m.Name == "b" {
			<-release
			return Status{CurrentToplevel: text("/literal/b")}, nil
		}
		return Status{CurrentToplevel: text("/literal/a"), Generations: &generation.HostList{Generations: []*generation.Host{{Current: true, Toplevel: text("/literal/a")}}}}, nil
	}
	d.Event = func(e Event) {
		if e.Kind == CurrentSkipped || e.Kind == CopyingToHost {
			decisions.Add(1)
		}
	}
	p := prepared(t, in)
	type result struct {
		r Report
		e error
	}
	done := make(chan result, 1)
	go func() { r, e := Run(context.Background(), p, d); done <- result{r, e} }()
	receive(t, ready)
	receive(t, ready)
	if decisions.Load() != 0 {
		t.Fatal("decision before query join")
	}
	close(release)
	got := receive(t, done)
	if got.e != nil || !got.r.Outcomes[0].SkippedCurrent || got.r.Outcomes[1].SkippedCurrent || got.r.Outcomes[1].Activation.State != Succeeded {
		t.Fatal(got)
	}
}

// This is the real health owner with only fake execution and clocks. Its
// ordered partial rows must survive a fatal transport or cancellation result.
func TestHealthPartialReportFromReviewedRunner(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			in := inputFor("A", "B", "C")
			in.Options.HealthChecks = true
			in.Options.KeepGoing = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			boom := errors.New("fake B transport")
			wantErr := boom
			if cancelled {
				wantErr = context.Canceled
			}
			l := &ledger{}
			d := script(l)
			checks := []health.Check{commandCheck("A", "A"), commandCheck("B", "B"), commandCheck("C", "C")}
			checks[0].Timeout = integer(2)
			d.Checks = func(ms []Machine) ([]health.Check, error) { l.add("checks:" + machineNames(ms)); return checks, nil }
			now := time.Unix(0, 0)
			calls := map[string]int{}
			var raw health.Report
			d.RunHealth = func(ctx context.Context, _ []Machine, cs []health.Check) (health.Report, error) {
				var err error
				raw, err = health.Run(ctx, cs, integer(1), health.Deps{
					Execute: func(_ context.Context, c health.Check) (health.CommandResult, error) {
						calls[c.Machine]++
						l.add("health:" + c.Machine)
						if c.Machine == "B" {
							if cancelled {
								cancel()
								return health.CommandResult{}, nil
							}
							return health.CommandResult{}, boom
						}
						status := 0
						if c.Machine == "A" && calls["A"] == 1 {
							status = 1
						}
						return health.CommandResult{Status: status}, nil
					}, Now: func() time.Time { return now }, Cooldown: func(context.Context, *big.Int) error { now = now.Add(time.Second); return nil },
				})
				return raw, err
			}
			report, err := Run(ctx, prepared(t, in), d)
			if !errors.Is(err, wantErr) || !report.RetainLog || len(report.HealthRounds) != 1 || !reflect.DeepEqual(report.HealthRounds[0], raw) {
				t.Fatal(report, err, raw)
			}
			if !reflect.DeepEqual(calls, map[string]int{"A": 2, "B": 1}) {
				t.Fatal(calls)
			}
			if !raw.Results[0].Passed() || !raw.Results[0].HadFailure || !raw.Results[1].Started || raw.Results[1].Complete || !errors.Is(raw.Results[1].Err, wantErr) || raw.Results[2].Started || raw.Results[2].Complete {
				t.Fatal(raw)
			}
			for i, want := range []State{Succeeded, Unknown, NotReached} {
				o := report.Outcomes[i]
				if o.Health.State != want || o.Copy.State != Succeeded || o.Activation.State != Succeeded || o.Profile.State != Succeeded {
					t.Fatal(i, o, want)
				}
			}
			if !errors.Is(report.Outcomes[1].Health.Err, wantErr) {
				t.Fatal(report.Outcomes[1])
			}
			expectCalls(t, l, "openB", "copy:A:/literal/A", "copy:B:/literal/B", "copy:C:/literal/C", "flushB", "closeB",
				"openA:A", "activate:A:switch", "profile:A:/literal/A", "flushA:A", "closeA:A",
				"openA:B", "activate:B:switch", "profile:B:/literal/B", "flushA:B", "closeA:B",
				"openA:C", "activate:C:switch", "profile:C:/literal/C", "flushA:C", "closeA:C",
				"checks:A,B,C", "health:A", "health:A", "health:B")
		})
	}
}

func TestHealthUnstartedRetryPreservesOriginalIndexResults(t *testing.T) {
	for _, retry := range []HealthChoice{HealthRetryAll, HealthRetryFailed} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			in := inputFor("A", "B", "C")
			in.Options.HealthChecks = true
			in.Options.Interactive = true
			l := &ledger{}
			d := script(l)
			checks := []health.Check{commandCheck("A", "A"), commandCheck("B", "B"), commandCheck("C", "C")}
			d.Checks = func([]Machine) ([]health.Check, error) { return checks, nil }
			boom := errors.New("fake retry transport")
			round := 0
			now := time.Unix(0, 0)
			choices := 0
			var raw []health.Report
			var indices [][]int
			var executions []string
			d.Event = func(e Event) {
				if e.Kind == HealthRoundStarted {
					indices = append(indices, e.Indices)
				}
			}
			d.RunHealth = func(ctx context.Context, _ []Machine, cs []health.Check) (health.Report, error) {
				round++
				r, err := health.Run(ctx, cs, integer(1), health.Deps{
					Execute: func(_ context.Context, c health.Check) (health.CommandResult, error) {
						executions = append(executions, fmt.Sprintf("%d:%s", round, c.Machine))
						if round == 2 {
							return health.CommandResult{}, boom
						}
						status := 0
						if c.Machine == "B" || retry == HealthRetryFailed && c.Machine == "A" {
							status = 1
						}
						return health.CommandResult{Status: status}, nil
					}, Now: func() time.Time { return now }, Cooldown: func(context.Context, *big.Int) error { now = now.Add(time.Second); return nil },
				})
				raw = append(raw, r)
				return r, err
			}
			d.ChooseHealth = func(context.Context, []Machine, health.Report) (HealthChoice, error) { choices++; return retry, nil }
			report, err := runPlan(t, in, d)
			if !errors.Is(err, boom) || choices != 1 || !report.RetainLog || len(report.HealthRounds) != 2 || !reflect.DeepEqual(report.HealthRounds, raw) {
				t.Fatal(report, err, choices)
			}
			wantIndices := [][]int{{0, 1, 2}, {0, 1, 2}}
			if retry == HealthRetryFailed {
				wantIndices[1] = []int{0, 1}
			}
			if !reflect.DeepEqual(indices, wantIndices) || !reflect.DeepEqual(executions, []string{"1:A", "1:B", "1:C", "2:A"}) {
				t.Fatal(indices, executions)
			}
			for i, want := range []State{Unknown, Failed, Succeeded} {
				if report.Outcomes[i].Health.State != want {
					t.Fatal(i, report.Outcomes[i], want)
				}
			}
			if !errors.Is(report.Outcomes[0].Health.Err, boom) || raw[1].Results[1].Started || raw[1].RetainLog {
				t.Fatal(raw, report.Outcomes)
			}
			// All deployment effects still occurred once before either health round.
			for _, host := range []string{"A", "B", "C"} {
				n := 0
				for _, call := range l.snapshot() {
					if call == "profile:"+host+":/literal/"+host {
						n++
					}
				}
				if n != 1 {
					t.Fatal(host, l.snapshot())
				}
			}
		})
	}
}

// Injected partial shapes exercise the finite callback binding, including a
// legitimate before-start cancellation and an indeterminate started row.
func TestHealthCrediblePartialFlags(t *testing.T) {
	in := inputFor("A", "B", "C")
	in.Options.HealthChecks = true
	d := script(&ledger{})
	checks := []health.Check{commandCheck("A", "A"), commandCheck("B", "B"), commandCheck("C", "C")}
	d.Checks = func([]Machine) ([]health.Check, error) { return checks, nil }
	boom := errors.New("round error")
	d.RunHealth = func(context.Context, []Machine, []health.Check) (health.Report, error) {
		return health.Report{RetainLog: true, Results: []health.Result{
			{Check: checks[0], Started: true, Complete: true, Attempts: 1, HadFailure: true, Failures: []health.Failure{{Kind: "injected"}}},
			{Check: checks[1], Started: true},
			{Check: checks[2], Err: context.Canceled},
		}}, boom
	}
	report, err := runPlan(t, in, d)
	if !errors.Is(err, boom) || !report.RetainLog {
		t.Fatal(report, err)
	}
	for i, want := range []State{Failed, Unknown, NotReached} {
		if report.Outcomes[i].Health.State != want {
			t.Fatal(i, report.Outcomes[i], want)
		}
	}
	if !errors.Is(report.Outcomes[1].Health.Err, boom) {
		t.Fatal(report)
	}
}

func TestHealthMalformedPartialReportsRefuseSubmittedRows(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		for _, shape := range []string{"count", "identity", "complete-unstarted", "complete-error", "complete-no-attempt", "unstarted-attempt", "unstarted-failure", "incomplete"} {
			t.Run(fmt.Sprintf("%t/%s", fatal, shape), func(t *testing.T) {
				in := inputFor("A", "B", "C")
				in.Options.HealthChecks = true
				in.Options.KeepGoing = true
				d := script(&ledger{})
				checks := []health.Check{commandCheck("A", "A"), commandCheck("B", "B"), commandCheck("C", "C")}
				d.Checks = func([]Machine) ([]health.Check, error) { return checks, nil }
				boom := errors.New("original fatal error")
				d.RunHealth = func(context.Context, []Machine, []health.Check) (health.Report, error) {
					r := healthResults(checks)
					r.RetainLog = true
					switch shape {
					case "count":
						r.Results = r.Results[:2]
					case "identity":
						r.Results[1].Check = checks[2]
					case "complete-unstarted":
						r.Results[1].Started = false
					case "complete-error":
						r.Results[1].Err = boom
					case "complete-no-attempt":
						r.Results[1].Attempts = 0
					case "unstarted-attempt":
						r.Results[1].Started = false
						r.Results[1].Complete = false
					case "unstarted-failure":
						r.Results[1] = health.Result{Check: checks[1], HadFailure: true}
					case "incomplete":
						r.Results[1].Complete = false
					}
					if fatal {
						return r, boom
					}
					return r, nil
				}
				report, err := runPlan(t, in, d)
				// A started but incomplete row is indeterminate only with a fatal round
				// error. Without that error, the same shape is a malformed normal result.
				if fatal && shape == "incomplete" {
					if !errors.Is(err, boom) || report.Outcomes[0].Health.State != Succeeded || report.Outcomes[1].Health.State != Unknown || report.Outcomes[2].Health.State != Succeeded {
						t.Fatal(report, err)
					}
					return
				}
				if err == nil || !report.RetainLog || len(report.HealthRounds) != 1 {
					t.Fatal(report, err)
				}
				if fatal && !errors.Is(err, boom) {
					t.Fatal(err)
				}
				if !fatal {
					var invalid *BindingError
					if !errors.As(err, &invalid) {
						t.Fatal(err)
					}
				}
				for _, o := range report.Outcomes {
					var invalid *BindingError
					if o.Health.State != Unknown || !errors.As(o.Health.Err, &invalid) {
						t.Fatal(o)
					}
				}
			})
		}
	}
}
