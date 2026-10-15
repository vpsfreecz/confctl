// Package deploy owns private deployment sequencing with injected operations.
// It neither installs production adapters nor exposes a public command.
package deploy

import (
	"fmt"
	"math/big"

	"github.com/vpsfreecz/confctl/internal/generation"
)

type Action string

const (
	Boot        Action = "boot"
	Switch      Action = "switch"
	Test        Action = "test"
	DryActivate Action = "dry-activate"
)

type Machine struct {
	Name                         string
	Carried, Localhost, Runnable bool
}

type Target struct {
	Host       string
	Generation *generation.Build
}

type Status struct {
	CurrentToplevel *string
	Generations     *generation.HostList
}

type WaitKind uint8

const (
	NoWait WaitKind = iota
	WaitForever
	WaitSeconds
)

type WaitMode struct {
	Kind    WaitKind
	Seconds *big.Int
}

type Options struct {
	Action                                                    Action
	Interactive, OneByOne, CopyOnly                           bool
	DryActivateFirst, EnableAutoRollback, DisableAutoRollback bool
	Reboot, HealthChecks, KeepGoing                           bool
	CopyWorkers                                               *big.Int
	Wait                                                      WaitMode
}

type Input struct {
	Machines []Machine
	Targets  []Target
	Options  Options
}

// Plan owns the input orders and options. Build records remain immutable,
// borrowed generation-owner values; preparing a deployment never reads a path.
type Plan struct {
	machines []Machine
	targets  []Target
	options  Options
	prepared bool
}

type InputError struct{ Problem string }

func (e *InputError) Error() string { return "deploy input: " + e.Problem }

func copyInteger(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}

func copyWait(v WaitMode) WaitMode {
	v.Seconds = copyInteger(v.Seconds)
	return v
}

func Prepare(input Input) (Plan, error) {
	o := input.Options
	switch o.Action {
	case Boot, Switch, Test, DryActivate:
	default:
		return Plan{}, &InputError{"unsupported action"}
	}
	if o.Reboot && o.Action != Boot {
		return Plan{}, &InputError{"reboot requires boot action"}
	}
	if o.CopyWorkers == nil || o.CopyWorkers.Sign() <= 0 {
		return Plan{}, &InputError{"copy workers must be greater than zero"}
	}
	if o.Reboot && (o.Wait.Kind > WaitSeconds || o.Wait.Kind == WaitSeconds && o.Wait.Seconds == nil) {
		return Plan{}, &InputError{"invalid reboot wait binding"}
	}
	names := make(map[string]bool, len(input.Machines))
	for _, m := range input.Machines {
		if names[m.Name] {
			return Plan{}, &InputError{fmt.Sprintf("duplicate machine %q", m.Name)}
		}
		names[m.Name] = true
	}
	targets := make(map[string]bool, len(input.Targets))
	for _, t := range input.Targets {
		if targets[t.Host] || !names[t.Host] || t.Generation == nil {
			return Plan{}, &InputError{fmt.Sprintf("invalid target binding %q", t.Host)}
		}
		targets[t.Host] = true
	}
	o.CopyWorkers = copyInteger(o.CopyWorkers)
	o.Wait = copyWait(o.Wait)
	return Plan{append([]Machine(nil), input.Machines...), append([]Target(nil), input.Targets...), o, true}, nil
}

func copyWorkerCount(request *big.Int, count int) int {
	if request.Cmp(big.NewInt(int64(count))) >= 0 {
		return count
	}
	return int(request.Int64())
}
