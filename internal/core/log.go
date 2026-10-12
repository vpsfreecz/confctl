package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func logID() string { var b [4]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (e *Engine) logText(s string) {
	if e.Log == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.Log.WriteString(s)
}
func (e *Engine) processLogStart(argv []string) (string, time.Time) {
	id := logID()
	command := ShellJoin(argv)
	if len(argv) > 0 && argv[0] == "nix" {
		command = "cd " + ShellJoin([]string{e.Root}) + " && " + command
	}
	e.logText("[" + id + "] Running " + command + "\n")
	return id, time.Now()
}
func (e *Engine) processLogFinish(id string, start time.Time, r Result) {
	for _, b := range [][]byte{r.Stdout, r.Stderr} {
		if len(b) > 0 {
			e.logText("[" + id + "] \t" + strings.TrimSuffix(string(b), "\n") + "\n")
		}
	}
	duration := time.Since(start).Seconds()
	unit := "seconds"
	if int(duration) == 1 {
		unit = "second"
	}
	state := "successful"
	if r.ExitCode != 0 {
		state = "failed"
	}
	e.logText(fmt.Sprintf("[%s] Finished in %5.3f %s with exit status %d (%s)\n", id, duration, unit, r.ExitCode, state))
}
func (e *Engine) LogCLI(cmd string, o Options, raw []string) {
	command := []string{cmd}
	if cmd == "runtime-kernels" {
		command = []string{"runtime-kernels", "update"}
	}
	opts := []struct {
		k string
		v any
	}{}
	add := func(k string, v any) {
		opts = append(opts, struct {
			k string
			v any
		}{k, v})
	}
	attrs := []any{}
	for _, v := range o.Attrs {
		attrs = append(attrs, v)
	}
	tags := []any{}
	for _, v := range o.Tags {
		tags = append(tags, v)
	}
	switch cmd {
	case "ls":
		add("L", o.List)
		add("list", o.List)
		add("H", o.HideHeader)
		add("hide-header", o.HideHeader)
		add("a", attrs)
		add("attr", attrs)
		add("t", tags)
		add("tag", tags)
		var out any
		if o.HasOutput {
			out = o.Output
		}
		add("o", out)
		add("output", out)
		var managed any
		if o.Managed != "" {
			managed = o.Managed
		}
		add("managed", managed)
		add("show-trace", o.Trace)
	case "status":
		add("y", o.Yes)
		add("yes", o.Yes)
		add("a", attrs)
		add("attr", attrs)
		add("t", tags)
		add("tag", tags)
		var g any
		if o.Generation != "" {
			g = o.Generation
		}
		add("g", g)
		add("generation", g)
	case "runtime-kernels":
		add("y", o.Yes)
		add("yes", o.Yes)
		add("a", attrs)
		add("attr", attrs)
		add("t", tags)
		add("tag", tags)
		add("show-trace", o.Trace)
	}
	cmds := make([]any, len(command))
	for i, s := range command {
		cmds[i] = s
	}
	args := make([]any, len(o.Args))
	for i, s := range o.Args {
		args[i] = s
	}
	var b strings.Builder
	b.WriteString("{command: " + rubyInspect(cmds) + ",\n global_options: {\"c\" => " + rubyInspect(e.Color) + ", \"color\" => " + rubyInspect(e.Color) + ", \"help\" => false},\n command_options:\n  {")
	if cmd == "init" || cmd == "add" || cmd == "rename" || cmd == "rediscover" {
		e.logText("{command: " + rubyInspect(cmds) + ",\n global_options: {\"c\" => " + rubyInspect(e.Color) + ", \"color\" => " + rubyInspect(e.Color) + ", \"help\" => false},\n command_options: {},\n arguments: " + rubyInspect(args) + "}\n")
		return
	}
	for i, v := range opts {
		if i > 0 {
			b.WriteString(",\n   ")
		}
		b.WriteString(rubyInspect(v.k) + " => " + rubyInspect(v.v))
	}
	if cmd == "runtime-kernels" {
		b.WriteString(",\n   #<GLI::Command::ParentKey:0x0000000000000000> => {}")
	}
	b.WriteString("},\n arguments: " + rubyInspect(args) + "}\n")
	e.logText(b.String())
}

// The two option-free inputs leaves retain the original PP parent-key layout.
// GLI uses one PARENT constant at both depths; its address is log-scoped only.
func (e *Engine) LogInputsCLI(path []string, args []string) {
	command, arguments := make([]any, len(path)), make([]any, len(args))
	for i, p := range path {
		command[i] = p
	}
	for i, a := range args {
		arguments[i] = a
	}
	parent := "#<GLI::Command::ParentKey:0x0000000000000000>"
	options := " {" + parent + " => {}}"
	if len(path) == 3 {
		options = "\n  {" + parent + " =>\n    {" + parent + " => {}}}"
	}
	e.logText(fmt.Sprintf("{command: %s,\n global_options: {\"c\" => %s, \"color\" => %s, \"help\" => false},\n command_options:%s,\n arguments: %s}\n", rubyInspect(command), rubyInspect(e.Color), rubyInspect(e.Color), options, rubyInspect(arguments)))
}

// GLI callbacks insert explicit options in first encounter order, then append
// defaults in declaration order. Each spelling stays together; the one parent
// identity is reused at both depths, exactly as in the read-only adapter.
func (e *Engine) LogInputUpdateCLI(inv cli.Invocation) {
	command, arguments := make([]any, len(inv.Command.Path)), make([]any, len(inv.Args))
	for i, p := range inv.Command.Path {
		command[i] = p
	}
	for i, a := range inv.Args {
		arguments[i] = a
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{command: %s,\n global_options: {\"c\" => %s, \"color\" => %s, \"help\" => false},\n command_options:\n  {", rubyInspect(command), rubyInspect(e.Color), rubyInspect(e.Color))
	separator := ""
	specs := append([]cli.OptionSpec(nil), inv.Command.Options...)
	sort.SliceStable(specs, func(i, j int) bool {
		a, b := inv.Options[specs[i].Key].FirstEncounter, inv.Options[specs[j].Key].FirstEncounter
		return a != 0 && (b == 0 || a < b)
	})
	for _, spec := range specs {
		for _, name := range spec.Names {
			fmt.Fprintf(&b, "%s%s => %s", separator, rubyInspect(name), rubyInspect(inv.Options[spec.Key].Value.Bool))
			separator = ",\n   "
		}
	}
	parent := "#<GLI::Command::ParentKey:0x0000000000000000>"
	b.WriteString(separator + parent + " =>")
	if len(inv.Command.Path) == 3 {
		b.WriteString("\n    {" + parent + " => {}}}")
	} else {
		b.WriteString(" {}}")
	}
	b.WriteString(inputLogArgumentsField(arguments))
	e.logText(b.String())
}

// Logger's final arguments hash field has a 79-character PP context. The closing
// hash brace participates in fit; the array's comma group breaks together.
// Count the existing inspected tokens as characters, without splitting them.
func inputLogArgumentsField(arguments []any) string {
	tokens := make([]string, len(arguments))
	for i, argument := range arguments {
		tokens[i] = rubyInspect(argument)
	}
	array := "[" + strings.Join(tokens, ", ") + "]"
	width := utf8.RuneCountInString(array)
	if width <= 66 {
		return ",\n arguments: " + array + "}\n"
	}
	if width >= 77 {
		array = "[" + strings.Join(tokens, ",\n   ") + "]"
	}
	return ",\n arguments:\n  " + array + "}\n"
}

// Set uses the same ordered boolean declarations and PP parent layout.
func (e *Engine) LogInputSetCLI(inv cli.Invocation) { e.LogInputUpdateCLI(inv) }

// This leaf has no GLI parent key. Aliases share one value and keep callback
// encounter order before declaration defaults, including nil managed and lists.
func (e *Engine) LogTestConnectionCLI(inv cli.Invocation) {
	command, arguments := make([]any, len(inv.Command.Path)), make([]any, len(inv.Args))
	for i, p := range inv.Command.Path {
		command[i] = p
	}
	for i, a := range inv.Args {
		arguments[i] = a
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{command: %s,\n global_options: {\"c\" => %s, \"color\" => %s, \"help\" => false},\n command_options:\n  {", rubyInspect(command), rubyInspect(e.Color), rubyInspect(e.Color))
	specs := append([]cli.OptionSpec(nil), inv.Command.Options...)
	sort.SliceStable(specs, func(i, j int) bool {
		a, b := inv.Options[specs[i].Key].FirstEncounter, inv.Options[specs[j].Key].FirstEncounter
		return a != 0 && (b == 0 || a < b)
	})
	separator := ""
	for _, spec := range specs {
		v := inv.Options[spec.Key].Value
		var value any
		switch v.Kind {
		case cli.Boolean:
			value = v.Bool
		case cli.Text:
			value = v.String
		case cli.TextList:
			a := make([]any, len(v.Strings))
			for i, s := range v.Strings {
				a[i] = s
			}
			value = a
		}
		for _, name := range spec.Names {
			fmt.Fprintf(&b, "%s%s => %s", separator, rubyInspect(name), rubyInspect(value))
			separator = ",\n   "
		}
	}
	b.WriteString("}" + inputLogArgumentsField(arguments))
	e.logText(b.String())
}
