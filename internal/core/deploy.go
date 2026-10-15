package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/generation"
)

type deployArgumentError struct{ error }

// DeployHookError carries a reached subscriber status separately from an
// ordinary operational error. Main owns its final status and log presentation.
type DeployHookError struct{ Status int }

func (e *DeployHookError) Error() string {
	return fmt.Sprintf("deploy.prepare hook failed with status %d", e.Status)
}

type deployCommandDeps struct {
	Machines   func() ([]Machine, error)
	Prepare    func([]string) (int, error)
	Build      func([]string) ([]deploy.Target, error)
	Load       func(string) (*generation.BuildList, error)
	Operations func(context.Context, []Machine, []Machine, commandLine, io.Writer, cli.Invocation) deploy.Deps
}

// Deploy prepares exactly once, rebinds selected names after hooks, then reuses
// the existing build, local-generation and private deployment owners. It returns
// partial outcomes and RetainLog even when a later phase fails.
func (e *Engine) Deploy(inv cli.Invocation, registry Registry, stdin, stdout, stderr *os.File) (deploy.Report, error) {
	return e.deployCommand(inv, stdin, stdout, deployCommandDeps{
		Machines: func() ([]Machine, error) { return e.Machines(false) },
		Prepare:  func(names []string) (int, error) { return e.HooksFrom(registry, "deploy.prepare", names, inv) },
		Load: func(host string) (*generation.BuildList, error) {
			return generation.LoadBuildList(e.Root, host, generation.Options{Warn: e.generationWarning(stderr)})
		},
		Build: func(hosts []string) ([]deploy.Target, error) { return e.deployBuild(hosts, inv, stdout) },
		Operations: func(ctx context.Context, all, selected []Machine, line commandLine, out io.Writer, inv cli.Invocation) deploy.Deps {
			return e.deployOperations(ctx, all, selected, line, out, inv)
		},
	})
}

func (e *Engine) LogDeployCLI(inv cli.Invocation) error { return e.LogTestConnectionCLI(inv) }

func deployOptions(inv cli.Invocation) (deploy.Options, error) {
	argument := func(s string) (deploy.Options, error) { return deploy.Options{}, &deployArgumentError{errors.New(s)} }
	action := deploy.Switch
	if len(inv.Args) > 1 {
		action = deploy.Action(inv.Args[1])
	}
	switch action {
	case deploy.Boot, deploy.Switch, deploy.Test, deploy.DryActivate:
	default:
		return argument(fmt.Sprintf("invalid action '%s'", action))
	}
	boolean := func(key string) bool { return inv.Options[key].Value.Bool }
	o := deploy.Options{Action: action, Interactive: boolean("interactive"), OneByOne: boolean("one-by-one"), CopyOnly: boolean("copy-only"), DryActivateFirst: boolean("dry-activate-first"), EnableAutoRollback: boolean("enable-auto-rollback"), DisableAutoRollback: boolean("disable-auto-rollback"), Reboot: boolean("reboot"), HealthChecks: boolean("health-checks"), KeepGoing: boolean("keep-going")}
	workers := inv.Options["max-concurrent-copy"].Value
	o.CopyWorkers = new(big.Int).Set(&workers.Integer)
	if o.Reboot {
		if action != deploy.Boot {
			return argument("--reboot can be used only with switch-action boot")
		}
		var err error
		o.Wait, err = deployWaitMode(inv.Options["wait-online"].Value.String)
		if err != nil {
			return argument(err.Error())
		}
	}
	return o, nil
}

// Ruby /^\d+$/ matches a complete line, then String#to_i converts the original
// prefix. Preserve that existing odd admission without narrowing a huge integer.
var deployWaitDigits = regexp.MustCompile(`(?m)^[0-9]+$`)
var deployWaitPrefix = regexp.MustCompile(`^[\t\n\v\f\r ]*([+-]?[0-9]+(?:_[0-9]+)*)`)

func deployWaitMode(text string) (deploy.WaitMode, error) {
	switch text {
	case "nowait":
		return deploy.WaitMode{Kind: deploy.NoWait}, nil
	case "wait":
		return deploy.WaitMode{Kind: deploy.WaitForever}, nil
	}
	if !deployWaitDigits.MatchString(text) {
		return deploy.WaitMode{}, errors.New("invalid value of --wait-online")
	}
	n := new(big.Int)
	if match := deployWaitPrefix.FindStringSubmatch(text); match != nil {
		n.SetString(strings.ReplaceAll(match[1], "_", ""), 10)
	}
	return deploy.WaitMode{Kind: deploy.WaitSeconds, Seconds: n}, nil
}

func (e *Engine) deployCommand(inv cli.Invocation, in io.Reader, out io.Writer, d deployCommandDeps) (deploy.Report, error) {
	all, err := d.Machines()
	if err != nil {
		return deploy.Report{}, err
	}
	var pattern *string
	if len(inv.Args) > 0 {
		pattern = &inv.Args[0]
	}
	opts := handlerOptions(inv)
	selected, err := Select(all, pattern, opts.Attrs, opts.Tags, "yes")
	if err != nil {
		return deploy.Report{}, &deployArgumentError{err}
	}
	o, err := deployOptions(inv)
	if err != nil {
		return deploy.Report{}, err
	}
	if len(selected) == 0 {
		return deploy.Report{}, errors.New("No machines to deploy")
	}
	_, line := commandInput(in)
	names := make([]string, len(selected))
	for i, m := range selected {
		names[i] = m.Name
	}
	if err := buildWrite(out, fmt.Sprintf("Deploying %s (%s)\n", strings.Join(names, ", "), o.Action)); err != nil {
		return deploy.Report{}, err
	}
	if !inv.Options["yes"].Value.Bool {
		if err := testConnectionConfirm(e.Context, line, out); err != nil {
			return deploy.Report{}, err
		}
	}
	code, err := d.Prepare(append([]string(nil), names...))
	if err != nil {
		return deploy.Report{}, err
	}
	if code != 0 {
		return deploy.Report{}, &DeployHookError{code}
	}
	all, err = d.Machines()
	if err != nil {
		return deploy.Report{}, err
	}
	byName := make(map[string]Machine, len(all))
	for _, m := range all {
		byName[m.Name] = m
	}
	for i, name := range names {
		m, ok := byName[name]
		if !ok {
			return deploy.Report{}, fmt.Errorf("Selected machine %s disappeared after deploy.prepare", name)
		}
		selected[i] = m
	}
	var targets []deploy.Target
	selector := inv.Options["generation"]
	if selector.Value.Kind == cli.Text {
		var missing []string
		for _, m := range selected {
			list, err := d.Load(m.Name)
			if err != nil {
				return deploy.Report{}, err
			}
			g, err := generation.ResolveBuild(list, selector.Value.String)
			if err != nil {
				return deploy.Report{}, err
			}
			if g == nil {
				missing = append(missing, m.Name)
			} else {
				targets = append(targets, deploy.Target{Host: m.Name, Generation: g})
			}
		}
		if len(targets) == 0 {
			return deploy.Report{}, errors.New("No generation found")
		}
		if err := buildWrite(out, "Resolved host generations:\n"); err != nil {
			return deploy.Report{}, err
		}
		for _, target := range targets {
			if err := buildWrite(out, fmt.Sprintf("  %s %s\n", target.Host, target.Generation.Name)); err != nil {
				return deploy.Report{}, err
			}
		}
		if o.Interactive && !inv.Options["yes"].Value.Bool {
			if err := testConnectionConfirm(e.Context, line, out); err != nil {
				return deploy.Report{}, err
			}
		}
		if len(missing) > 0 {
			if err := buildWrite(out, fmt.Sprintf("Generation %q is missing on %s; these hosts will be ignored.\n", selector.Value.String, strings.Join(missing, ", "))); err != nil {
				return deploy.Report{}, err
			}
			if o.Interactive || !inv.Options["yes"].Value.Bool {
				if err := testConnectionConfirm(e.Context, line, out); err != nil {
					return deploy.Report{}, err
				}
			}
		}
	} else {
		targets, err = d.Build(names)
		if err != nil {
			return deploy.Report{}, err
		}
	}
	machines := make([]deploy.Machine, len(selected))
	for i, m := range selected {
		machines[i] = deployMachine(m)
	}
	plan, err := deploy.Prepare(deploy.Input{Machines: machines, Targets: targets, Options: o})
	if err != nil {
		return deploy.Report{}, err
	}
	ctx, cancel := context.WithCancel(e.Context)
	defer cancel()
	operations := d.Operations(ctx, all, selected, line, out, inv)
	var mu sync.Mutex
	var outputErr error
	previous := operations.Event
	operations.Event = func(event deploy.Event) {
		mu.Lock()
		defer mu.Unlock()
		if previous != nil {
			previous(event)
		}
		if outputErr != nil {
			return
		}
		if err := buildWrite(out, fmt.Sprintf("%s: %s\n", event.Host, event.Kind)); err != nil {
			outputErr = err
			cancel()
		}
	}
	report, err := deploy.Run(ctx, plan, operations)
	// Run has joined all owned work before exposing the synchronous I/O cause.
	if outputErr != nil {
		return report, outputErr
	}
	return report, err
}

func deployMachine(m Machine) deploy.Machine {
	return deploy.Machine{Name: m.Name, Carried: m.CarrierName != nil, Localhost: m.Target.Host != nil && *m.Target.Host == "localhost", Runnable: m.CarrierName == nil && m.Target.Host != nil}
}

// BuildSelected is called once with already selected/rebound names. Its owning
// defaults and fresh dependency caches are retained; no public Build prompt runs.
func (e *Engine) deployBuild(hosts []string, inv cli.Invocation, out io.Writer) ([]deploy.Target, error) {
	options := buildOptions{ShowTrace: inv.Options["show-trace"].Value.Bool}
	for _, option := range []struct {
		name string
		to   **string
	}{{"max-jobs", &options.MaxJobs}, {"cores", &options.Cores}} {
		if value := inv.Options[option.name].Value; value.Kind == cli.Text {
			text := value.String
			*option.to = &text
		}
	}
	outcome, err := e.buildSelected(hosts, options, buildPresentation{Evaluating: func(count int) error {
		return buildWrite(out, fmt.Sprintf("Building %d deployment targets\n", count))
	}}, buildDeps{})
	targets := make([]deploy.Target, len(outcome.Generations))
	for i, row := range outcome.Generations {
		targets[i] = deploy.Target{Host: row.Host, Generation: row.Generation}
	}
	return targets, err
}
