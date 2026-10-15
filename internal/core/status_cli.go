package core

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

type statusArgumentError struct{ error }

func (e *Engine) StatusCLI(inv cli.Invocation, stdin, stdout, stderr *os.File) error {
	return e.statusCLI(inv, stdin, stdout, stderr, buildDeps{})
}

func (e *Engine) LogStatusCLI(inv cli.Invocation) error { return e.LogTestConnectionCLI(inv) }
func (e *Engine) LogBuildCLI(inv cli.Invocation) error  { return e.LogTestConnectionCLI(inv) }

func (e *Engine) statusCLI(inv cli.Invocation, in io.Reader, out, stderr io.Writer, deps buildDeps) error {
	_, line := commandInput(in)
	all, err := e.Machines(false)
	if err != nil {
		return err
	}
	var pattern *string
	if len(inv.Args) > 0 {
		pattern = &inv.Args[0]
	}
	machines, err := Select(all, pattern, inv.Options["attr"].Value.Strings, inv.Options["tag"].Value.Strings, "yes")
	if err != nil {
		return &statusArgumentError{err}
	}
	selected := make([]Machine, 0, len(machines))
	for _, machine := range machines {
		if machine.Target.Host != nil || machine.CarrierName != nil {
			selected = append(selected, machine)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("No machines to check")
	}
	selector := inv.Options["generation"]
	yes := inv.Options["yes"].Value.Bool
	if !yes {
		heading, label := "The following machines will be built and then checked:", "new build"
		if selector.Present {
			heading, label = "The following machines will be checked:", selector.Value.String
		}
		table, err := e.List(selected, nil, true)
		if err != nil {
			return err
		}
		if err := buildWrite(out, heading+"\n"+table+"\nGeneration: "+label+"\n"); err != nil {
			return err
		}
		if err := testConnectionConfirm(e.Context, line, out); err != nil {
			return err
		}
	}
	statuses := make([]Status, len(selected))
	for i, machine := range selected {
		if _, err := statusConnection(machine, all); err != nil {
			return err
		}
		statuses[i].Machine = machine
	}
	explicit := selector.Present && selector.Value.String != "none"
	if explicit {
		resolved, missing := []resolvedBuild{}, []string{}
		for _, machine := range selected {
			if err := e.Context.Err(); err != nil {
				return err
			}
			list, err := generation.LoadBuildList(e.Root, machine.Name, generation.Options{Warn: e.generationWarning(stderr)})
			if err != nil {
				return err
			}
			record, err := generation.ResolveBuild(list, selector.Value.String)
			if err != nil {
				return err
			}
			if record == nil {
				missing = append(missing, machine.Name)
			} else {
				resolved = append(resolved, resolvedBuild{machine.Name, record})
			}
		}
		if len(resolved) == 0 {
			return fmt.Errorf("No generation found")
		}
		table, err := resolvedBuildTable(resolved, missing)
		if err != nil {
			return err
		}
		if err := buildWrite(out, "Resolved host generations:\n"+table+"\n"); err != nil {
			return err
		}
		if len(missing) > 0 && !yes {
			text := "Generation '" + selector.Value.String + "' was not found on the following hosts:\n  " + strings.Join(missing, "\n  ") + "\n\nThese hosts will be ignored.\n"
			if err := buildWrite(out, text); err != nil {
				return err
			}
			if err := testConnectionConfirm(e.Context, line, out); err != nil {
				return err
			}
		}
		kept := make([]Status, 0, len(resolved))
		for _, found := range resolved {
			for _, status := range statuses {
				if status.Machine.Name == found.Host {
					top := found.Build.Toplevel
					status.TargetToplevel = &top
					status.Target = make(map[string]map[string]any, len(found.Build.InputsInfo))
					for _, role := range found.Build.InputsInfo {
						status.Target[role.Name] = generationInputInfo(role.Value)
					}
					kept = append(kept, status)
					break
				}
			}
		}
		statuses = kept
	} else {
		if !selector.Present {
			hosts := make([]string, len(selected))
			for i, machine := range selected {
				hosts[i] = machine.Name
			}
			built, err := e.buildSelected(hosts, buildOptions{}, e.buildOutput(out, stderr), deps)
			if err != nil {
				return err
			}
			if err := writeBuildSummary(out, built); err != nil {
				return err
			}
			for _, record := range built.Generations {
				for i := range statuses {
					if statuses[i].Machine.Name == record.Host {
						top := record.Generation.Toplevel
						statuses[i].TargetToplevel = &top
					}
				}
			}
		}
		if err := e.statusTargetInputs(statuses); err != nil {
			return err
		}
	}
	if err := e.Context.Err(); err != nil {
		return err
	}
	e.statusQuery(statuses, all, queryOptions{Toplevel: !selector.Present || selector.Value.String != "none", Generations: true, Inputs: true}, len(selected))
	text, err := e.statusTable(statuses)
	if err != nil {
		return err
	}
	return buildWrite(out, text)
}
