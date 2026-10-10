package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
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
