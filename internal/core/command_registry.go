package core

import (
	"encoding/json"

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

// One projection serves command and actual origin-hook options. Parser-internal
// zero values do not imply a declaration default or wire presence.
func invocationOptions(inv cli.Invocation) map[string]any {
	out := map[string]any{}
	for _, spec := range inv.Command.Options {
		parsed := inv.Options[spec.Key]
		if !parsed.Present && !spec.DefaultPresent {
			continue
		}
		v := parsed.Value
		switch v.Kind {
		case cli.Null:
			out[spec.Key] = nil
		case cli.Boolean:
			out[spec.Key] = v.Bool
		case cli.Text:
			out[spec.Key] = v.String
		case cli.Number:
			out[spec.Key] = json.Number(v.Integer.String())
		case cli.TextList:
			out[spec.Key] = append([]string{}, v.Strings...)
		}
	}
	return out
}

func commandInvocation(root string, bound registeredCommand, inv cli.Invocation, raw []string) ext.Invocation {
	if supportedRuntimeCommand(bound.Command) {
		return extensionInvocation(root, bound.Registration, bound.Command, handlerOptions(inv), raw)
	}
	return ext.Invocation{Root: root, ExtensionID: bound.Registration.ID, CommandPath: append([]string(nil), inv.Command.Path...), OriginCommand: append([]string(nil), inv.Command.Path...), Options: invocationOptions(inv), Arguments: append([]string(nil), inv.Args...), RawArgv: append([]string(nil), inv.RawArgv...)}
}
