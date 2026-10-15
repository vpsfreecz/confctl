package deploy

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/vpsfreecz/confctl/internal/generation"
)

func integer(v int64) *big.Int { return big.NewInt(v) }
func text(v string) *string    { return &v }

func inputFor(names ...string) Input {
	in := Input{Options: Options{Action: Switch, DisableAutoRollback: true, CopyWorkers: integer(1)}}
	for _, name := range names {
		in.Machines = append(in.Machines, Machine{Name: name, Runnable: true})
		in.Targets = append(in.Targets, Target{Host: name, Generation: &generation.Build{
			Host: name, Name: "generation-" + name, Toplevel: "/literal/" + name,
		}})
	}
	return in
}

func prepared(t *testing.T, in Input) Plan {
	t.Helper()
	p, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrepareValidationAndZeroTargets(t *testing.T) {
	for _, action := range []Action{Boot, Switch, Test, DryActivate} {
		in := inputFor()
		in.Options.Action = action
		p := prepared(t, in)
		report, err := Run(context.Background(), p, Deps{})
		if err != nil || len(report.Outcomes) != 0 {
			t.Fatal(report, err)
		}
	}
	for _, name := range []string{"action", "reboot", "nil-workers", "zero", "negative", "duplicate-machine", "duplicate-target", "foreign-target", "nil-generation", "wait-binding"} {
		t.Run(name, func(t *testing.T) {
			in := inputFor("a")
			switch name {
			case "action":
				in.Options.Action = "unsupported"
			case "reboot":
				in.Options.Reboot = true
			case "nil-workers":
				in.Options.CopyWorkers = nil
			case "zero":
				in.Options.CopyWorkers = integer(0)
			case "negative":
				in.Options.CopyWorkers = integer(-1)
			case "duplicate-machine":
				in.Machines = append(in.Machines, in.Machines[0])
			case "duplicate-target":
				in.Targets = append(in.Targets, in.Targets[0])
			case "foreign-target":
				in.Targets[0].Host = "foreign"
			case "nil-generation":
				in.Targets[0].Generation = nil
			case "wait-binding":
				in.Options.Action, in.Options.Reboot, in.Options.Wait.Kind = Boot, true, WaitSeconds
			}
			_, err := Prepare(in)
			var invalid *InputError
			if !errors.As(err, &invalid) {
				t.Fatal(err)
			}
		})
	}
	_, err := Run(context.Background(), Plan{}, Deps{})
	var invalid *InputError
	if !errors.As(err, &invalid) {
		t.Fatal(err)
	}
}

func TestPlanOwnsOrdersAndOptionsBorrowsBuild(t *testing.T) {
	in := inputFor("a", "b", "c")
	in.Targets = []Target{in.Targets[2], in.Targets[0], in.Targets[1]}
	in.Options.Action, in.Options.Reboot = Boot, true
	large := new(big.Int).Lsh(integer(1), 1000)
	in.Options.CopyWorkers = new(big.Int).Set(large)
	in.Options.Wait = WaitMode{WaitSeconds, new(big.Int).Set(large)}
	build := in.Targets[0].Generation
	p := prepared(t, in)
	in.Machines[0].Name, in.Targets[0].Host = "changed", "changed"
	in.Options.CopyWorkers.SetInt64(-1)
	in.Options.Wait.Seconds.SetInt64(-1)
	if p.machines[0].Name != "a" || p.targets[0].Host != "c" || p.targets[0].Generation != build ||
		p.options.CopyWorkers.Cmp(large) != 0 || p.options.Wait.Seconds.Cmp(large) != 0 {
		t.Fatal(p)
	}
	for _, count := range []int{0, 1, 3} {
		if got := copyWorkerCount(p.options.CopyWorkers, count); got != count {
			t.Fatal(got, count)
		}
	}
	if got := copyWorkerCount(integer(2), 3); got != 2 {
		t.Fatal(got)
	}
	in = inputFor("a", "missing")
	in.Targets = in.Targets[:1]
	in.Options.Wait = WaitMode{Kind: 250, Seconds: integer(-42)} // Unused when reboot is off.
	prepared(t, in)
	before := *build
	if _, err := Run(context.Background(), p, script(&ledger{})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, *p.targets[0].Generation) {
		t.Fatal("borrowed build changed")
	}
}

// Source: cluster_skip_current_deploy_spec.rb and cluster.rb's predicate.
// These are source-family assertions, not an observed original invocation.
func TestSkipCurrentSourceFamilies(t *testing.T) {
	for _, tc := range []struct {
		action           Action
		carried          bool
		runtime, profile *string
		want             bool
	}{
		{Boot, false, text("target"), text("target"), true},
		{Switch, false, text("target"), text("target"), true},
		{Boot, false, text("target"), text("old"), false},
		{Switch, false, text("target"), nil, false},
		{Test, false, text("target"), text("old"), true},
		{DryActivate, false, text("target"), nil, true},
		{Switch, true, text("target"), nil, true},
		{Switch, true, text("old"), text("target"), false},
		{Switch, false, nil, text("target"), false},
		{Switch, false, text(""), text("target"), false},
	} {
		status := Status{CurrentToplevel: tc.runtime}
		if tc.profile != nil {
			status.Generations = &generation.HostList{Generations: []*generation.Host{{Current: true, Toplevel: tc.profile}}}
		}
		if got := isCurrent(Machine{Carried: tc.carried}, &generation.Build{Toplevel: "target"}, status, tc.action); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	if isCurrent(Machine{}, &generation.Build{Toplevel: ""}, Status{}, Switch) {
		t.Fatal("nil became empty")
	}
	status := Status{CurrentToplevel: text(""), Generations: &generation.HostList{Generations: []*generation.Host{{Current: true, Toplevel: text("")}}}}
	if !isCurrent(Machine{}, &generation.Build{Toplevel: ""}, status, Switch) {
		t.Fatal("literal empty pointers lost")
	}
}
