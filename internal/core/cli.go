package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/inputs"
)

func Main(ctx context.Context, argv []string) (result int) {
	builtins := cli.BuiltinRegistry()
	globals, commandArgv, globalErr := builtins.ParseGlobals(argv)
	// Keep the accepted version failure before any extension registry read.
	if globalErr == nil && len(commandArgv) == 1 && commandArgv[0] == "--version" {
		fmt.Fprint(os.Stderr, "confctl: version unknown\nerror: confctl: version unknown\n")
		return 1
	}
	registrations, err := LoadRegistry()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if globalErr != nil {
		fmt.Fprint(os.Stderr, "error: "+globalErr.Error()+"\n\n")
		text, _ := builtins.Help(nil, 80)
		fmt.Print(text)
		return 64
	}
	registry, registered, err := commandRegistry(registrations)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	inv, err := registry.Parse(argv)
	if err != nil {
		diagnostic := err.Error()
		var parseErr *cli.ParseError
		var path []string
		if errors.As(err, &parseErr) {
			path = parseErr.HelpPath
			// This accepted leaf presentation depends on the existing ParseError
			// prefix; the shared parser has no unknown-option kind/token field.
			if len(path) == 1 && len(inv.Command.Path) == 1 && path[0] == inv.Command.Path[0] && (inv.Command.Handler == cli.TestConnectionHandler || inv.Command.Handler == cli.SSHHandler || inv.Command.Handler == cli.CSSHHandler) {
				if option, ok := strings.CutPrefix(parseErr.Message, "invalid option: "); ok && option != "" {
					diagnostic = "Unknown option " + option
				}
			}
		}
		fmt.Fprint(os.Stderr, "error: "+diagnostic+"\n\n")
		text, _ := registry.Help(path, 80)
		fmt.Print(text)
		return 64
	}
	if inv.Help {
		text, err := registry.Help(inv.HelpPath, 80)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 64
		}
		fmt.Print(text)
		return 0
	}
	if err = registry.CheckAvailability(inv); err != nil {
		fmt.Fprintln(os.Stderr, "confctl-go-prototype: "+err.Error())
		return 2
	}
	cmd := inv.Command.Path[0]
	extensionCommand := inv.Command.Handler == cli.KnownExtension
	var registration Registration
	var command Command
	if extensionCommand {
		bound := registered[strings.Join(inv.Command.Path, " ")]
		registration, command = bound.Registration, bound.Command
		if err = checkExtensionArity(inv); err != nil {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
			return 64
		}
	}
	var opts Options
	if !extensionCommand || supportedRuntimeCommand(command) {
		opts = handlerOptions(inv)
	}
	color := globals["color"].Value.String
	e, err := New(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	e.Color = color
	e.Yes = inv.Options["yes"].Value.Bool
	e.ShowTrace = inv.Options["show-trace"].Value.Bool
	logName := cmd
	if extensionCommand {
		logName = strings.Join(command.Path, "-")
	} else if inv.Command.Handler == cli.InputsReadHandler || inv.Command.Handler == cli.InputsUpdateHandler || inv.Command.Handler == cli.InputsSetHandler || inv.Command.Handler == cli.GenerationListHandler || inv.Command.Handler == cli.GenerationRemoveHandler || inv.Command.Handler == cli.GenerationRotateHandler || inv.Command.Handler == cli.CollectGarbageHandler || inv.Command.Handler == cli.GenDataHandler {
		logName = strings.Join(inv.Command.Path, "-")
	}
	if err = e.OpenLog(logName); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	success := false
	originalStderr := os.Stderr
	defer func() { result, _ = LogCleanupStatus(result, e.CloseLog(success), originalStderr) }()

	if inv.Command.Handler == cli.InputsReadHandler {
		err = e.LogInputsCLI(inv.Command.Path, inv.Args)
	} else if inv.Command.Handler == cli.InputsSetHandler {
		err = e.LogInputSetCLI(inv)
	} else if inv.Command.Handler == cli.InputsUpdateHandler {
		err = e.LogInputUpdateCLI(inv)
	} else if inv.Command.Handler == cli.TestConnectionHandler {
		err = e.LogTestConnectionCLI(inv)
	} else if inv.Command.Handler == cli.SSHHandler || inv.Command.Handler == cli.CSSHHandler {
		err = e.LogSSHCLI(inv)
	} else if inv.Command.Handler == cli.GenerationListHandler || inv.Command.Handler == cli.GenerationRemoveHandler || inv.Command.Handler == cli.GenerationRotateHandler || inv.Command.Handler == cli.CollectGarbageHandler {
		err = e.LogGenerationCLI(inv)
	} else if inv.Command.Handler == cli.ComparisonHandler {
		err = e.LogComparisonCLI(inv)
	} else if inv.Command.Handler == cli.HealthCheckHandler {
		err = e.LogHealthCLI(inv)
	} else if inv.Command.Handler == cli.GenDataHandler {
		err = e.LogGenDataCLI(inv)
	} else if inv.Command.Handler == cli.BuildHandler {
		err = e.LogBuildCLI(inv)
	} else if inv.Command.Handler == cli.StatusHandler {
		err = e.LogStatusCLI(inv)
	} else if !extensionCommand || supportedRuntimeCommand(command) {
		err = e.LogCLI(cmd, opts, commandArgv)
	} else {
		b, _ := json.Marshal(commandInvocation(e.Root, registered[strings.Join(inv.Command.Path, " ")], inv, commandArgv))
		err = e.logMetadata(string(b) + "\n")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fail := func(err error, code int) int {
		fmt.Fprintf(os.Stderr, "\nLog file: %s\n", e.Log.Name())
		diagnostic := err.Error()
		if inv.Command.Handler == cli.InputsSetHandler {
			diagnostic = inputSetDiagnostic(err)
		}
		var updateExit *inputs.ExitError
		if (inv.Command.Handler == cli.InputsUpdateHandler || inv.Command.Handler == cli.InputsSetHandler) && errors.As(err, &updateExit) {
			// Completed input mutation failures carry tty-command's final newline.
			fmt.Fprint(os.Stderr, "error: "+diagnostic)
		} else {
			fmt.Fprintln(os.Stderr, "error: "+diagnostic)
		}
		return code
	}
	if extensionCommand {
		in := commandInvocation(e.Root, registered[strings.Join(inv.Command.Path, " ")], inv, commandArgv)
		code, err := e.Invoke(registration, command.Handler, in)
		if err != nil {
			return fail(err, 1)
		}
		success = code == 0
		return code
	}
	if inv.Command.Handler == cli.InputsReadHandler || inv.Command.Handler == cli.InputsUpdateHandler || inv.Command.Handler == cli.InputsSetHandler {
		var text string
		if inv.Command.Handler == cli.InputsSetHandler {
			err = e.SetInputs(inv, func(s string) { fmt.Print(s) })
		} else if inv.Command.Handler == cli.InputsUpdateHandler {
			text, err = e.UpdateInputs(inv)
		} else {
			text, err = e.ListInputs(len(inv.Command.Path) == 3, inv.Args)
		}
		if err != nil {
			var argumentErr *inputsArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				help, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(help)
				return code
			}
			return fail(err, 1)
		}
		fmt.Print(text)
		success = true
		return 0
	}
	if inv.Command.Handler == cli.ConfigurationHandler {
		operation := configurationOperation{Registry: registrations, Origin: inv}
		code := 0
		switch cmd {
		case "init":
			err = e.Init()
		case "add":
			code, err = e.Add(operation)
		case "rename":
			code, err = e.Rename(operation)
		case "rediscover":
			code, err = e.Rediscover(operation)
		}
		if err != nil {
			if code == 0 {
				code = 1
			}
			var argumentErr *configurationArgumentError
			if errors.As(err, &argumentErr) {
				code = fail(err, 64)
				fmt.Fprintln(os.Stderr)
				text, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(text)
				return code
			}
			return fail(err, code)
		}
		success = code == 0
		return code
	}
	if inv.Command.Handler == cli.TestConnectionHandler {
		if err = e.TestConnection(inv, os.Stdin, os.Stdout); err != nil {
			var argumentErr *testConnectionArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				text, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(text)
				return code
			}
			return fail(err, 1)
		}
		success = true
		return 0
	}
	if inv.Command.Handler == cli.SSHHandler || inv.Command.Handler == cli.CSSHHandler {
		if inv.Command.Handler == cli.SSHHandler {
			err = e.SSH(inv, os.Stdin, os.Stdout)
		} else {
			err = e.CSSH(inv, os.Stdin, os.Stdout)
		}
		if err != nil {
			var argumentErr *sshArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				text, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(text)
				return code
			}
			return fail(err, 1)
		}
		success = true
		return 0
	}
	if inv.Command.Handler == cli.GenerationListHandler {
		text, err := e.ListGenerations(inv)
		if err != nil {
			var argumentErr *generationArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				help, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(help)
				return code
			}
			return fail(err, 1)
		}
		fmt.Print(text)
		success = true
		return 0
	}
	if inv.Command.Handler == cli.GenerationRemoveHandler || inv.Command.Handler == cli.GenerationRotateHandler || inv.Command.Handler == cli.CollectGarbageHandler {
		switch inv.Command.Handler {
		case cli.GenerationRemoveHandler:
			err = e.RemoveGenerations(inv, os.Stdin, os.Stdout, os.Stderr)
		case cli.GenerationRotateHandler:
			err = e.RotateGenerations(inv, os.Stdin, os.Stdout, os.Stderr)
		case cli.CollectGarbageHandler:
			err = e.CollectGarbage(inv, os.Stdin, os.Stdout, os.Stderr)
		}
		if err != nil {
			var argumentErr *generationArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				help, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(help)
				return code
			}
			return fail(err, 1)
		}
		success = true
		return 0
	}
	if inv.Command.Handler == cli.ComparisonHandler {
		if err = e.Compare(inv, os.Stdin, os.Stdout, os.Stderr); err != nil {
			var argumentErr *comparisonArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				help, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(help)
				return code
			}
			return fail(err, 1)
		}
		success = true
		return 0
	}
	if inv.Command.Handler == cli.HealthCheckHandler {
		checked, err := e.HealthCheck(inv, os.Stdin, os.Stdout)
		if err != nil {
			var argumentErr *healthArgumentError
			if errors.As(err, &argumentErr) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				help, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(help)
				return code
			}
			return fail(err, 1)
		}
		// A later successful attempt must preserve the earlier failure's log.
		success = !checked.RetainLog
		return 0
	}
	if inv.Command.Handler == cli.GenDataHandler {
		if err = e.GenData(inv, os.Stdin, os.Stdout, os.Stderr); err != nil {
			return fail(err, 1)
		}
		success = true
		return 0
	}
	if inv.Command.Handler == cli.BuildHandler || inv.Command.Handler == cli.StatusHandler {
		if inv.Command.Handler == cli.BuildHandler {
			err = e.Build(inv, os.Stdin, os.Stdout, os.Stderr)
		} else {
			err = e.StatusCLI(inv, os.Stdin, os.Stdout, os.Stderr)
		}
		if err != nil {
			var buildArgs *buildArgumentError
			var statusArgs *statusArgumentError
			if errors.As(err, &buildArgs) || errors.As(err, &statusArgs) {
				code := fail(err, 64)
				fmt.Fprintln(os.Stderr)
				help, _ := registry.Help(inv.Command.Path, 80)
				fmt.Print(help)
				return code
			}
			return fail(err, 1)
		}
		success = true
		return 0
	}
	if cmd == "ls" && opts.List {
		e.ShowTrace = false
		n, er := e.evaluator()
		if er == nil {
			var v any
			v, er = n.eval(".#confctl.moduleOptions")
			if er == nil {
				fmt.Println("name")
				a, _ := v.([]any)
				for _, x := range a {
					h, _ := x.(map[string]any)
					name := str(h["name"])
					if strings.HasPrefix(name, "cluster.<name>.") {
						fmt.Println(strings.TrimPrefix(name, "cluster.<name>."))
					}
				}
			}
		}
		if er != nil {
			return fail(er, 1)
		}
		success = true
		return 0
	}
	all, err := e.Machines(false)
	if err != nil {
		return fail(err, 1)
	}
	var pattern *string
	if len(opts.Args) > 0 {
		pattern = &opts.Args[0]
	}
	managed := opts.Managed
	if managed == "" {
		managed = "yes"
	}
	ms, err := Select(all, pattern, opts.Attrs, opts.Tags, managed)
	if err != nil {
		return fail(err, 64)
	}
	if cmd == "ls" {
		var cols []string
		if opts.HasOutput {
			cols = strings.Split(opts.Output, ",")
			for len(cols) > 0 && cols[len(cols)-1] == "" {
				cols = cols[:len(cols)-1]
			}
		}
		text, err := e.List(ms, cols, !opts.HideHeader)
		if err != nil {
			return fail(err, 1)
		}
		fmt.Print(text)
		success = true
		return 0
	}
	return fail(fmt.Errorf("unsupported command %s", cmd), 1)
}
