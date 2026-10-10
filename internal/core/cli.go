package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func Main(ctx context.Context, argv []string) int {
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
		fmt.Fprint(os.Stderr, "error: "+err.Error()+"\n\n")
		var parseErr *cli.ParseError
		var path []string
		if errors.As(err, &parseErr) {
			path = parseErr.HelpPath
		}
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
	} else if inv.Command.Handler == cli.InputsReadHandler {
		logName = strings.Join(inv.Command.Path, "-")
	}
	if err = e.OpenLog(logName); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if inv.Command.Handler == cli.InputsReadHandler {
		e.LogInputsCLI(inv.Command.Path, inv.Args)
	} else if !extensionCommand || supportedRuntimeCommand(command) {
		e.LogCLI(cmd, opts, commandArgv)
	} else {
		b, _ := json.Marshal(commandInvocation(e.Root, registered[strings.Join(inv.Command.Path, " ")], inv, commandArgv))
		e.logText(string(b) + "\n")
	}
	fail := func(err error, code int) int {
		fmt.Fprintf(os.Stderr, "\nLog file: %s\n", e.Log.Name())
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return code
	}
	success := false
	defer func() { e.CloseLog(success) }()
	if extensionCommand {
		in := commandInvocation(e.Root, registered[strings.Join(inv.Command.Path, " ")], inv, commandArgv)
		code, err := e.Invoke(registration, command.Handler, in)
		if err != nil {
			return fail(err, 1)
		}
		success = code == 0
		return code
	}
	if inv.Command.Handler == cli.InputsReadHandler {
		text, err := e.ListInputs(len(inv.Command.Path) == 3, inv.Args)
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
	if cmd == "status" || managed == "" {
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
	selected := []Machine{}
	for _, m := range ms {
		if m.Target.Host != nil || m.CarrierName != nil {
			selected = append(selected, m)
		}
	}
	if len(selected) == 0 {
		return fail(fmt.Errorf("No machines to check"), 1)
	}
	if !opts.Yes {
		text, err := e.List(selected, nil, true)
		if err != nil {
			return fail(err, 1)
		}
		fmt.Print("The following machines will be checked:\n" + text + "\nGeneration: none\n")
		ok, err := e.Confirm(os.Stdin, "", false)
		if err != nil || !ok {
			if err != nil {
				return fail(err, 1)
			}
			return fail(fmt.Errorf("Aborted"), 1)
		}
	}
	text, err := e.Status(selected, all)
	if err != nil {
		return fail(err, 1)
	}
	fmt.Print(text)
	success = true
	return 0
}
