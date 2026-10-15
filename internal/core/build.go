package core

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/build"
	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

type buildArgumentError struct{ error }

type buildOptions struct {
	MaxJobs, Cores *string
	ShowTrace      bool
}

type buildDeps struct {
	Run     func([]string, []byte, func([]byte, []byte) error) (Result, error)
	Now     func() time.Time
	RootDir func() (string, error)
}

type buildPresentation struct {
	Evaluating     func(int) error
	GroupsPrepared func([]build.Group) error
	GroupStart     func(int, int, build.Group) error
	Progress       func(build.Event) error
	Close          func() error
}

func (e *Engine) Build(inv cli.Invocation, stdin, stdout, stderr *os.File) error {
	return e.buildCLI(inv, stdin, stdout, stderr, buildDeps{})
}

// buildCLI's explicit dependencies keep fixture roots independent of login.
// Shared CLI dispatch calls Build; status/deploy call buildSelected directly.
func (e *Engine) buildCLI(inv cli.Invocation, stdin io.Reader, stdout, stderr io.Writer, deps buildDeps) error {
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
		return &buildArgumentError{err}
	}
	if len(machines) == 0 {
		return fmt.Errorf("No machines to build")
	}
	if !inv.Options["yes"].Value.Bool {
		table, err := e.List(machines, nil, true)
		if err != nil {
			return err
		}
		if err := buildWrite(stdout, "The following machines will be built:\n"+table); err != nil {
			return err
		}
		if err := testConnectionConfirm(stdin, stdout); err != nil {
			return err
		}
	}
	options := buildOptions{ShowTrace: inv.Options["show-trace"].Value.Bool}
	for _, option := range []struct {
		name string
		to   **string
	}{{"max-jobs", &options.MaxJobs}, {"cores", &options.Cores}} {
		parsed := inv.Options[option.name]
		if parsed.Value.Kind == cli.Text {
			value := parsed.Value.String
			*option.to = &value
		}
	}
	hosts := make([]string, len(machines))
	for i, machine := range machines {
		hosts[i] = machine.Name
	}
	outcome, err := e.buildSelected(hosts, options, e.buildOutput(stdout, stderr), deps)
	if err != nil {
		return err
	}
	return writeBuildSummary(stdout, outcome)
}

// Standalone build and build-backed status share the same presentation owner.
func (e *Engine) buildOutput(stdout, stderr io.Writer) buildPresentation {
	return buildPresentation{
		Evaluating: func(count int) error {
			return buildWrite(stdout, fmt.Sprintf("Evaluating inputs for %d machines...\n", count))
		},
		GroupsPrepared: func(groups []build.Group) error {
			return buildWrite(stdout, fmt.Sprintf("\nMachines will be built in %d groups\n\n", len(groups)))
		},
		GroupStart: func(index, count int, group build.Group) error {
			var text strings.Builder
			fmt.Fprintf(&text, "Building group %d/%d\n", index+1, count)
			for _, host := range group.Hosts {
				fmt.Fprintf(&text, "  %s\n", host.Host)
			}
			if inputs, ok := group.Inputs.(generation.Object); ok {
				text.WriteString("with inputs\n")
				for _, input := range inputs {
					fmt.Fprintf(&text, "  %s=%s\n", input.Name, str(input.Value))
				}
			}
			if e.Log != nil {
				fmt.Fprintf(&text, "Full log: %s\n", e.Log.Name())
			}
			return buildWrite(stdout, text.String())
		},
		Progress: func(event build.Event) error {
			return buildWrite(stderr, fmt.Sprintf("%s %s/%s %s\n", event.Type, event.Progress, event.Total, event.Path))
		},
	}
}

func writeBuildSummary(stdout io.Writer, outcome build.Outcome) error {
	var summary strings.Builder
	summary.WriteString("\nBuilt generations:\n")
	for _, row := range outcome.Generations {
		fmt.Fprintf(&summary, "  %s: %s\n", row.Host, row.Generation.Name)
	}
	return buildWrite(stdout, summary.String())
}

func buildWrite(out io.Writer, text string) error {
	n, err := io.WriteString(out, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

func (e *Engine) buildSelected(hosts []string, options buildOptions, ui buildPresentation, deps buildDeps) (out build.Outcome, err error) {
	if options.MaxJobs == nil {
		settings, err := e.Settings()
		if err != nil {
			return out, err
		}
		if value := nested(settings, "nix", "maxJobs"); value != nil {
			text := str(value)
			options.MaxJobs = &text
		}
	}
	// Keep the physical configuration and context fixed for this invocation.
	// Global settings above retain Engine's cache; Nix instance caches below
	// remain separate, even when inventory already evaluated settings.
	invocation := *e
	e = &invocation
	if deps.Run == nil {
		deps.Run = e.buildProcess
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.RootDir == nil {
		deps.RootDir = e.generationRootDir
	}
	if ui.Evaluating != nil {
		if err := ui.Evaluating(len(hosts)); err != nil {
			return out, err
		}
	}
	nix := &buildNix{e: e, options: options, run: deps.Run, progress: ui.Progress}
	var writer *generation.Writer
	getWriter := func() (*generation.Writer, error) {
		if writer == nil {
			var err error
			writer, err = generation.NewWriter(e.Root, deps.RootDir)
			if err != nil {
				return nil, err
			}
		}
		return writer, nil
	}
	activeGroup := false
	closeCurrent := func() error {
		if !activeGroup {
			return nil
		}
		activeGroup = false
		if ui.Close != nil {
			return ui.Close()
		}
		return nil
	}
	defer func() {
		if closeErr := closeCurrent(); closeErr != nil {
			err = closeErr
		}
	}()
	out, err = build.Run(hosts, build.Callbacks{
		BuildPlan: nix.buildPlan, Settings: nix.settingsObject, MachineKey: nix.machineKey,
		GroupsPrepared: ui.GroupsPrepared,
		GroupStart: func(index, count int, group build.Group) error {
			if err := closeCurrent(); err != nil {
				return err
			}
			if ui.GroupStart != nil {
				activeGroup = true // Close partial acquisition even if entry fails.
				return ui.GroupStart(index, count, group)
			}
			return nil
		},
		KeyLiteral: func(key string) (string, error) { return NixLiteral(key) },
		Eval:       nix.eval, Build: nix.build, DecodeBuild: generation.DecodeJSONValue,
		Load: func(host string) (*generation.BuildList, error) {
			return generation.LoadBuildList(e.Root, host, e.generationOptions())
		},
		Create: func(host, top string, rollback *string, inputs, info generation.Object, at time.Time) (*generation.Build, error) {
			return generation.CreateFlake(e.Root, host, top, rollback, inputs, info, at)
		},
		Save: func(record *generation.Build) error {
			w, err := getWriter()
			if err != nil {
				return err
			}
			return w.Save(record)
		},
		SetCurrent: func(list *generation.BuildList, record *generation.Build) error {
			w, err := getWriter()
			if err != nil {
				return err
			}
			return w.SetCurrent(list, record)
		},
		Now: deps.Now,
	})
	return out, err
}
