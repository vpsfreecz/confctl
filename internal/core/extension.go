package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/internal/cli"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var Services = []string{"settings.get", "machines.select", "exec.run", "ui.write", "ui.machines", "ui.table", "ui.confirm", "format.nix"}

type supervisor struct {
	e         *Engine
	in        ext.Invocation
	mu        sync.Mutex
	snapshots map[string][]Machine
	seq       int
}

func (s *supervisor) handle(ctx context.Context, method string, b json.RawMessage) (any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	switch method {
	case "settings.get":
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.e.Settings()
	case "machines.select":
		var p ext.Selection
		if e := json.Unmarshal(b, &p); e != nil {
			return nil, e
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		all, e := s.e.Machines(p.Refresh)
		if e != nil {
			return nil, e
		}
		selected, e := Select(all, p.Pattern, p.Attrs, p.Tags, p.Managed)
		if e != nil {
			return nil, e
		}
		if s.in.Event == "deploy.prepare" {
			scope := map[string]bool{}
			for _, n := range s.in.SelectedNames {
				scope[n] = true
			}
			out := []Machine{}
			for _, m := range selected {
				if scope[m.Name] {
					out = append(out, m)
				}
			}
			selected = out
		}
		s.seq++
		id := fmt.Sprintf("s:%d", s.seq)
		s.snapshots[id] = selected
		return map[string]any{"snapshot": id, "machines": selected}, nil
	case "exec.run":
		var p struct {
			Snapshot string   `json:"snapshot"`
			Machine  string   `json:"machine"`
			Argv     []string `json:"argv"`
			Input    *string  `json:"stdin_b64"`
			Output   string   `json:"output"`
			Timeout  int      `json:"timeout_ms"`
		}
		if e := json.Unmarshal(b, &p); e != nil {
			return nil, e
		}
		m, e := s.machine(p.Snapshot, p.Machine)
		if e != nil {
			return nil, e
		}
		if p.Timeout != 0 {
			return nil, fmt.Errorf("timeout policy is outside parity experiment")
		}
		if p.Output != "capture" && p.Output != "stream" {
			return nil, fmt.Errorf("unknown output mode")
		}
		var input []byte
		if p.Input != nil {
			input, e = base64.StdEncoding.DecodeString(*p.Input)
			if *p.Input == "" {
				input = make([]byte, 0)
			}
		}
		if e != nil {
			return nil, e
		}
		argv, e := SSHArgs(m, p.Argv)
		if e != nil {
			return nil, e
		}
		r, e := s.e.Process(argv, input)
		if e != nil {
			return nil, e
		}
		if p.Output == "stream" {
			_, _ = os.Stdout.Write(r.Stdout)
			_, _ = os.Stderr.Write(r.Stderr)
			r.Stdout = nil
			r.Stderr = nil
		}
		return r, nil
	case "ui.write":
		var p struct {
			Stream string `json:"stream"`
			Text   string `json:"text"`
			Style  string `json:"style"`
		}
		if e := json.Unmarshal(b, &p); e != nil {
			return nil, e
		}
		text := p.Text
		if p.Style == "bright" {
			text = s.e.Colorize(strings.TrimSuffix(text, "\n"), "bright") + "\n"
		}
		if p.Stream == "stdout" {
			fmt.Print(text)
		} else if p.Stream == "stderr" {
			fmt.Fprint(os.Stderr, text)
		} else {
			return nil, fmt.Errorf("unknown UI stream")
		}
		return map[string]any{}, nil
	case "ui.machines":
		var p struct {
			Snapshot string   `json:"snapshot"`
			Names    []string `json:"names"`
			Columns  []string `json:"columns"`
			Header   bool     `json:"header"`
		}
		if e := json.Unmarshal(b, &p); e != nil {
			return nil, e
		}
		ms := []Machine{}
		for _, n := range p.Names {
			m, e := s.machine(p.Snapshot, n)
			if e != nil {
				return nil, e
			}
			ms = append(ms, m)
		}
		s.mu.Lock()
		text, e := s.e.List(ms, p.Columns, p.Header)
		s.mu.Unlock()
		if e != nil {
			return nil, e
		}
		fmt.Print(text)
		return map[string]any{}, nil
	case "ui.confirm":
		if s.in.Event != "" {
			return nil, fmt.Errorf("hooks cannot prompt")
		}
		var p struct {
			Message string `json:"message"`
			Always  bool   `json:"always"`
		}
		if e := json.Unmarshal(b, &p); e != nil {
			return nil, e
		}
		ok, e := s.e.Confirm(os.Stdin, p.Message, p.Always)
		return map[string]any{"accepted": ok}, e
	case "ui.table":
		var p struct {
			Columns []struct {
				Key   string `json:"key"`
				Label string `json:"label"`
				Min   int    `json:"min_width"`
			} `json:"columns"`
			Rows      []map[string]any `json:"rows"`
			Separator string           `json:"separator"`
			Header    bool             `json:"header"`
		}
		if e := decodeJSONNumbers(b, &p); e != nil {
			return nil, e
		}
		for rowIndex := -1; rowIndex < len(p.Rows); rowIndex++ {
			if rowIndex < 0 && !p.Header {
				continue
			}
			for i, c := range p.Columns {
				value := c.Label
				if rowIndex >= 0 {
					value = str(p.Rows[rowIndex][c.Key])
				}
				fmt.Print(value)
				if i < len(p.Columns)-1 {
					w := c.Min
					if width(value) > w {
						w = width(value)
					}
					fmt.Print(strings.Repeat(" ", w-width(value)) + p.Separator)
				}
			}
			fmt.Println()
		}
		return map[string]any{}, nil
	case "format.nix":
		var p struct {
			Value any `json:"value"`
		}
		if e := decodeJSONNumbers(b, &p); e != nil {
			return nil, e
		}
		text, e := NixLiteral(p.Value)
		return map[string]any{"text": text}, e
	}
	return nil, fmt.Errorf("unsupported service %s", method)
}
func (s *supervisor) machine(snapshot, name string) (Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.snapshots[snapshot] {
		if m.Name == name {
			return m, nil
		}
	}
	return Machine{}, fmt.Errorf("unknown snapshot/name %s/%s", snapshot, name)
}
func (e *Engine) Invoke(reg Registration, handler string, in ext.Invocation) (int, error) {
	if len(reg.Argv) == 0 {
		return 1, fmt.Errorf("missing executable")
	}
	toChildRead, toChildWrite, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer toChildWrite.Close()
	fromChildRead, fromChildWrite, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer fromChildRead.Close()
	ctx, cancel := context.WithCancel(e.Context)
	defer cancel()
	cmd := exec.CommandContext(ctx, reg.Argv[0], reg.Argv[1:]...)
	cmd.Dir = e.Root
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{toChildRead, fromChildWrite}
	group := prepareChild(cmd)
	if err = cmd.Start(); err != nil {
		toChildRead.Close()
		fromChildWrite.Close()
		return 1, err
	}
	group.bind(cmd.Process.Pid)
	_ = toChildRead.Close()
	_ = fromChildWrite.Close()
	scoped := &Engine{Root: e.Root, Context: ctx, SettingsCache: e.SettingsCache, Inventory: e.Inventory, ShowTrace: e.ShowTrace, Color: e.Color, Yes: e.Yes, Log: e.Log}
	s := &supervisor{e: scoped, in: in, snapshots: map[string][]Machine{}}
	p := ext.NewPeer(ctx, fromChildRead, toChildWrite, "c")
	p.SetHandler(s.handle)
	var initialized ext.InitResult
	err = p.Call(ctx, "initialize", ext.Init{Protocol: ext.Version{Major: ext.Major, Minor: ext.Minor}, ExtensionID: reg.ID, Supported: Services}, &initialized)
	if err == nil && initialized.Protocol.Major != ext.Major {
		err = fmt.Errorf("protocol major mismatch")
	}
	if err == nil {
		for _, r := range initialized.Required {
			found := false
			for _, x := range Services {
				if x == r {
					found = true
				}
			}
			if !found {
				err = fmt.Errorf("required service unavailable: %s", r)
				break
			}
		}
	}
	var result ext.RunResult
	if err == nil {
		err = p.Call(ctx, "run", ext.Run{Handler: handler, Context: in}, &result)
	}
	if err == nil && p.SealIncoming() != 0 {
		err = fmt.Errorf("extension completed with active reverse services")
	}
	if err != nil {
		go func() { _ = p.Notify("cancel", map[string]any{}) }()
		cancel()
		_ = group.cancel()
	}
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-p.Done():
			if !errors.Is(p.Err(), io.EOF) {
				cancel()
				_ = group.cancel()
			}
		case <-watchDone:
		}
	}()
	waitErr := cmd.Wait()
	close(watchDone)
	if protocolErr := p.Err(); protocolErr != nil && !errors.Is(protocolErr, io.EOF) && err == nil {
		err = protocolErr
	}
	group.finish()
	cancel()
	_ = fromChildRead.Close()
	_ = toChildWrite.Close()
	p.WaitHandlers()
	if err == nil {
		e.Inventory = scoped.Inventory
		e.SettingsCache = scoped.SettingsCache
	}
	if e.Context.Err() != nil {
		return 1, e.Context.Err()
	}
	if err != nil {
		return 1, err
	}
	if result.ExitCode < 0 || result.ExitCode > 125 {
		return 1, fmt.Errorf("invalid extension status")
	}
	if p.Outstanding() != 0 {
		return 1, fmt.Errorf("extension has outstanding requests")
	}
	if cmd.ProcessState.ExitCode() != result.ExitCode {
		return 1, fmt.Errorf("extension result/exit disagree")
	}
	if waitErr != nil && result.ExitCode == 0 {
		return 1, waitErr
	}
	return result.ExitCode, nil
}
func (e *Engine) Hooks(r Registry, event string, names []string) (int, error) {
	return e.hooks(r, event, names, nil)
}

// HooksFrom carries the actual compiled origin; the fixture-only Hooks adapter
// retains its established context when no native origin command exists.
func (e *Engine) HooksFrom(r Registry, event string, names []string, origin cli.Invocation) (int, error) {
	return e.hooks(r, event, names, &origin)
}

func (e *Engine) hooks(r Registry, event string, names []string, origin *cli.Invocation) (int, error) {
	type item struct {
		reg  Registration
		hook Hook
	}
	items := []item{}
	for _, reg := range r.Extensions {
		for _, h := range reg.Hooks {
			if h.Event == event {
				items = append(items, item{reg, h})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].hook.Order != items[j].hook.Order {
			return items[i].hook.Order < items[j].hook.Order
		}
		if items[i].reg.ID != items[j].reg.ID {
			return items[i].reg.ID < items[j].reg.ID
		}
		return items[i].hook.Handler < items[j].hook.Handler
	})
	for _, x := range items {
		in := ext.Invocation{Root: e.Root, ExtensionID: x.reg.ID, Event: event, SelectedNames: names, Action: "switch", Options: map[string]any{}}
		if origin != nil {
			in.OriginCommand = append([]string(nil), origin.Command.Path...)
			in.Options = invocationOptions(*origin)
			in.Arguments = append([]string(nil), origin.Args...)
			in.RawArgv = append([]string(nil), origin.RawArgv...)
		}
		code, err := e.Invoke(x.reg, x.hook.Handler, in)
		e.Inventory = nil
		e.SettingsCache = nil
		if err != nil || code != 0 {
			return code, err
		}
	}
	return 0, nil
}
func NixLiteral(v any) (string, error) { return nixFormat(v, 0) }
func nixFormat(v any, level int) (string, error) {
	pad := strings.Repeat(" ", level)
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case json.Number:
		return string(x), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case string:
		s := strconv.Quote(x)
		s = strings.ReplaceAll(s, "${", "\\${")
		return s, nil
	case []any:
		var b strings.Builder
		b.WriteString("[\n")
		for _, v := range x {
			s, e := nixFormat(v, level+2)
			if e != nil {
				return "", e
			}
			b.WriteString(pad + "  " + s + "\n")
		}
		b.WriteString(pad + "]")
		return b.String(), nil
	case []string:
		a := make([]any, len(x))
		for i, v := range x {
			a[i] = v
		}
		return nixFormat(a, level)
	case map[string]any:
		keys := []string{}
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{\n")
		for _, k := range keys {
			s, e := nixFormat(x[k], level+2)
			if e != nil {
				return "", e
			}
			q, _ := nixFormat(k, 0)
			b.WriteString(pad + "  " + q + " = " + s + ";\n")
		}
		b.WriteString(pad + "}")
		return b.String(), nil
	}
	return "", fmt.Errorf("unsupported Nix literal %T", v)
}
