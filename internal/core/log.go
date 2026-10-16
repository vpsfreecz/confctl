package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func logID() string { var b [4]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

type engineLogOps struct {
	Write  func([]byte) (int, error)
	Flush  func() error
	Close  func() error
	Remove func() error
}
type engineLog struct {
	file      *os.File
	ops       engineLogOps
	rawMu     sync.Mutex
	live      *liveLog
	closeOnce sync.Once
	closeErr  error
}

func newEngineLog(file *os.File, ops engineLogOps) *engineLog {
	l := &engineLog{file: file, ops: ops}
	l.live = newLiveLog(l.writeAndFlush)
	return l
}
func nativeEngineLog(file *os.File) *engineLog {
	// os.File has no buffered user-space output here. Flush is deliberately a
	// no-op; Sync/fsync would introduce a different durability/effect contract.
	return newEngineLog(file, engineLogOps{Write: file.Write, Flush: func() error { return nil }, Close: file.Close, Remove: func() error { return os.Remove(file.Name()) }})
}

var errLogOwnerUnavailable = errors.New("log owner is unavailable")

func (e *Engine) checkedLogOwner() (*engineLog, error) {
	if e.Log == nil && e.logOwner == nil {
		return nil, nil
	}
	if e.Log == nil || e.logOwner == nil || e.logOwner.file != e.Log {
		return nil, errLogOwnerUnavailable
	}
	return e.logOwner, nil
}
func (l *engineLog) rawWrite(b []byte) (int, error) {
	if l.ops.Write == nil {
		return 0, errLogOwnerUnavailable
	}
	n, err := l.ops.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return n, err
}
func (l *engineLog) writeAndFlush(b []byte) (int, error) {
	l.rawMu.Lock()
	defer l.rawMu.Unlock()
	n, err := l.rawWrite(b)
	if err != nil {
		return n, err
	}
	if l.ops.Flush == nil {
		return n, errLogOwnerUnavailable
	}
	return n, l.ops.Flush()
}
func (e *Engine) logText(s string) error {
	l, err := e.checkedLogOwner()
	if err != nil || l == nil {
		return err
	}
	_, err = l.live.Write([]byte(s))
	return err
}
func (e *Engine) logMetadata(s string) error {
	l, err := e.checkedLogOwner()
	if err != nil || l == nil {
		return err
	}
	l.rawMu.Lock()
	defer l.rawMu.Unlock()
	_, err = l.rawWrite([]byte(s))
	return err
}
func (e *Engine) attachLog(read func([]byte) error) (func() error, error) {
	l, err := e.checkedLogOwner()
	if err != nil {
		return nil, err
	}
	if l == nil {
		return func() error { return nil }, nil
	}
	return l.live.Attach(read)
}

// Root finalization only, after its children/copies/readers have quiesced.
// The first success choice and exact cause are cached; no retry or reopening.
func (e *Engine) CloseLog(success bool) error {
	l, err := e.checkedLogOwner()
	if err != nil || l == nil {
		return err
	}
	l.closeOnce.Do(func() {
		if l.ops.Close == nil {
			l.closeErr = errLogOwnerUnavailable
			return
		}
		l.closeErr = l.ops.Close()
		if l.closeErr == nil && success {
			if l.ops.Remove == nil {
				l.closeErr = errLogOwnerUnavailable
			} else {
				l.closeErr = l.ops.Remove()
			}
		}
	})
	return l.closeErr
}

// Main and hook-driver preserve a body/nonzero status and try one diagnostic
// on original stderr. Reporting failure is separate from the cached close cause.
func LogCleanupStatus(status int, cleanupErr error, stderr io.Writer) (int, error) {
	if cleanupErr == nil {
		return status, nil
	}
	if status == 0 {
		status = 1
	}
	b := []byte("log cleanup: " + cleanupErr.Error() + "\n")
	n, err := stderr.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return status, err
}
func (e *Engine) processLogStart(argv []string) (string, time.Time, error) {
	id := logID()
	command := ShellJoin(argv)
	if len(argv) > 0 && argv[0] == "nix" {
		command = "cd " + ShellJoin([]string{e.Root}) + " && " + command
	}
	err := e.logText("[" + id + "] Running " + command + "\n")
	return id, time.Now(), err
}
func (e *Engine) processLogFinish(id string, start time.Time, r Result) error {
	duration := time.Since(start).Seconds()
	unit := "seconds"
	if int(duration) == 1 {
		unit = "second"
	}
	state := "successful"
	if r.ExitCode != 0 {
		state = "failed"
	}
	return e.logText(fmt.Sprintf("[%s] Finished in %5.3f %s with exit status %d (%s)\n", id, duration, unit, r.ExitCode, state))
}
func (e *Engine) LogCLI(cmd string, o Options, raw []string) error {
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
		return e.logMetadata("{command: " + rubyInspect(cmds) + ",\n global_options: {\"c\" => " + rubyInspect(e.Color) + ", \"color\" => " + rubyInspect(e.Color) + ", \"help\" => false},\n command_options: {},\n arguments: " + rubyInspect(args) + "}\n")
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
	return e.logMetadata(b.String())
}

// The two option-free inputs leaves retain the original PP parent-key layout.
// GLI uses one PARENT constant at both depths; its address is log-scoped only.
func (e *Engine) LogInputsCLI(path []string, args []string) error {
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
	return e.logMetadata(fmt.Sprintf("{command: %s,\n global_options: {\"c\" => %s, \"color\" => %s, \"help\" => false},\n command_options:%s,\n arguments: %s}\n", rubyInspect(command), rubyInspect(e.Color), rubyInspect(e.Color), options, rubyInspect(arguments)))
}

// GLI callbacks insert explicit options in first encounter order, then append
// defaults in declaration order. Each spelling stays together; the one parent
// identity is reused at both depths, exactly as in the read-only adapter.
func (e *Engine) LogInputUpdateCLI(inv cli.Invocation) error {
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
	return e.logMetadata(b.String())
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
func (e *Engine) LogInputSetCLI(inv cli.Invocation) error { return e.LogInputUpdateCLI(inv) }

// This leaf has no GLI parent key. Aliases share one value and keep callback
// encounter order before declaration defaults, including nil managed and lists.
func (e *Engine) LogTestConnectionCLI(inv cli.Invocation) error {
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
	return e.logMetadata(b.String())
}
