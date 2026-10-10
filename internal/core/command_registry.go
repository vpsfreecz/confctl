package core

import (
	"fmt"
	"strings"

	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/internal/cli"
)

// Options is the existing handler/log adapter. New declaration-only options do
// not change the supported handlers' fields or extension invocation payload.
type Options struct {
	Attrs, Tags, Args                       []string
	Managed, Output, Generation             string
	HasOutput, HideHeader, List, Trace, Yes bool
}

type registeredCommand struct {
	Registration Registration
	Command      Command
}

func commandRegistry(extensions Registry) (*cli.Registry, map[string]registeredCommand, error) {
	builtins := cli.BuiltinRegistry()
	commands := builtins.Commands()
	registered := map[string]registeredCommand{}
	for _, reg := range extensions.Extensions {
		for _, c := range reg.Commands {
			// ReadRegistry has already validated the only supported schema1 shape.
			group := Group{Path: []string{"runtime-kernels"}, Description: "Manage node runtime kernel versions"}
			if len(reg.Groups) > 0 {
				group = reg.Groups[0]
			}
			commands = append(commands, cli.CommandSpec{Path: group.Path, Summary: group.Description, ArgumentPolicy: cli.GroupArguments})
			opts := []cli.OptionSpec{}
			for _, set := range c.OptionSets {
				switch set {
				case "machine-filter":
					opts = append(opts, cli.MachineFilterOptions()...)
				case "confirmation":
					opts = append(opts, cli.ConfirmationOptions()...)
				default:
					return nil, nil, fmt.Errorf("unsupported option set %q", set)
				}
			}
			arguments := []cli.ArgumentSpec{}
			for _, a := range c.Arguments {
				name, _ := a["name"].(string)
				required, _ := a["required"].(bool)
				arguments = append(arguments, cli.ArgumentSpec{Name: name, Required: required})
			}
			commands = append(commands, cli.CommandSpec{Path: c.Path, Summary: c.Description, Usage: "[machine-pattern]", Arguments: arguments, ArgumentPolicy: cli.FreeForm, Options: opts, Handler: cli.KnownExtension, Availability: cli.Availability{Mode: cli.Available}})
			registered[strings.Join(c.Path, " ")] = registeredCommand{reg, c}
		}
	}
	r, err := cli.NewRegistry(builtins.Globals(), commands)
	return r, registered, err
}

func handlerOptions(inv cli.Invocation) Options {
	text := func(key string) string { return inv.Options[key].Value.String }
	boolean := func(key string) bool { return inv.Options[key].Value.Bool }
	return Options{
		Attrs:   append([]string(nil), inv.Options["attr"].Value.Strings...),
		Tags:    append([]string(nil), inv.Options["tag"].Value.Strings...),
		Args:    append([]string(nil), inv.Args...),
		Managed: text("managed"), Output: text("output"), Generation: text("generation"),
		HasOutput: inv.Options["output"].Present, HideHeader: boolean("hide-header"),
		List: boolean("list"), Trace: boolean("show-trace"), Yes: boolean("yes"),
	}
}

// Keep the existing schema1 invocation payload at this adapter boundary.
func extensionInvocation(root string, registration Registration, command Command, opts Options, raw []string) ext.Invocation {
	return ext.Invocation{Root: root, ExtensionID: registration.ID, CommandPath: command.Path, OriginCommand: command.Path, Options: map[string]any{"yes": opts.Yes, "attr": opts.Attrs, "tag": opts.Tags, "show-trace": opts.Trace}, Arguments: opts.Args, RawArgv: raw}
}
