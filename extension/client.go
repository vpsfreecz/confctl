package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

type Target struct {
	Host      *string `json:"host"`
	Port      int     `json:"port"`
	Localhost bool    `json:"localhost"`
}
type Machine struct {
	Name         string         `json:"name"`
	Key          string         `json:"key"`
	Spin         string         `json:"spin"`
	Managed      bool           `json:"managed"`
	ClusterName  string         `json:"cluster_name"`
	CarrierName  *string        `json:"carrier_name"`
	CarriedAlias string         `json:"carried_alias"`
	Target       Target         `json:"target"`
	Profile      string         `json:"profile"`
	Attributes   map[string]any `json:"attributes"`
}

func (m Machine) Attr(path string) any {
	if path == "name" {
		return m.Name
	}
	if path == "key" {
		return m.Key
	}
	var v any = m.Attributes
	for _, k := range strings.Split(path, ".") {
		h, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = h[k]
	}
	return v
}

type Invocation struct {
	Root          string         `json:"config_root"`
	ExtensionID   string         `json:"extension_id"`
	CommandPath   []string       `json:"command_path"`
	Event         string         `json:"event"`
	OriginCommand []string       `json:"origin_command"`
	Options       map[string]any `json:"options"`
	Arguments     []string       `json:"arguments"`
	RawArgv       []string       `json:"raw_argv"`
	SelectedNames []string       `json:"selected_names"`
	Action        string         `json:"action"`
	Generation    *string        `json:"generation"`
	Terminal      map[string]any `json:"terminal"`
}
type Selection struct {
	Pattern *string  `json:"pattern"`
	Attrs   []string `json:"attrs"`
	Tags    []string `json:"tags"`
	Managed string   `json:"managed"`
	Refresh bool     `json:"refresh"`
}
type Snapshot struct {
	ID       string    `json:"snapshot"`
	Machines []Machine `json:"machines"`
}

func (s Snapshot) Filter(f func(Machine) bool) Snapshot {
	out := s
	out.Machines = []Machine{}
	for _, m := range s.Machines {
		if f(m) {
			out.Machines = append(out.Machines, m)
		}
	}
	return out
}
func (s Snapshot) Names() []string {
	out := make([]string, len(s.Machines))
	for i, m := range s.Machines {
		out[i] = m.Name
	}
	return out
}
func SelectionFrom(in Invocation) Selection {
	s := Selection{Managed: "all"}
	if len(in.Arguments) > 0 {
		s.Pattern = &in.Arguments[0]
	}
	for _, k := range []string{"attr", "tag"} {
		b, _ := json.Marshal(in.Options[k])
		var a []string
		_ = json.Unmarshal(b, &a)
		if k == "attr" {
			s.Attrs = a
		} else {
			s.Tags = a
		}
	}
	return s
}

type ExecResult struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal"`
	Stdout   []byte `json:"stdout_b64"`
	Stderr   []byte `json:"stderr_b64"`
}
type Client struct{ peer *Peer }

func (c *Client) Select(ctx context.Context, s Selection) (Snapshot, error) {
	var out Snapshot
	e := c.peer.Call(ctx, "machines.select", s, &out)
	return out, e
}
func (c *Client) Settings(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	e := c.peer.Call(ctx, "settings.get", map[string]any{}, &out)
	return out, e
}
func (c *Client) Exec(ctx context.Context, s Snapshot, m Machine, argv []string) (ExecResult, error) {
	var out ExecResult
	e := c.peer.Call(ctx, "exec.run", map[string]any{"snapshot": s.ID, "machine": m.Name, "argv": argv, "stdin_b64": nil, "output": "capture", "timeout_ms": 0}, &out)
	return out, e
}
func (c *Client) Write(ctx context.Context, stream, text, style string) error {
	var out map[string]any
	return c.peer.Call(ctx, "ui.write", map[string]any{"stream": stream, "text": text, "style": style}, &out)
}
func (c *Client) Machines(ctx context.Context, s Snapshot) error {
	var out map[string]any
	return c.peer.Call(ctx, "ui.machines", map[string]any{"snapshot": s.ID, "names": s.Names(), "columns": nil, "header": true}, &out)
}
func (c *Client) Confirm(ctx context.Context) (bool, error) {
	var out struct {
		Accepted bool `json:"accepted"`
	}
	e := c.peer.Call(ctx, "ui.confirm", map[string]any{"message": "", "always": false}, &out)
	return out.Accepted, e
}
func (c *Client) Nix(ctx context.Context, value any) (string, error) {
	var out struct {
		Text string `json:"text"`
	}
	e := c.peer.Call(ctx, "format.nix", map[string]any{"value": value}, &out)
	return out.Text, e
}
func (c *Client) Table(ctx context.Context, columns []map[string]any, rows []map[string]any) error {
	var out map[string]any
	return c.peer.Call(ctx, "ui.table", map[string]any{"columns": columns, "rows": rows, "separator": " ", "header": true}, &out)
}

type Item struct {
	Result ExecResult
	Error  error
	// Completion is the local worker insertion order, not an RPC wire field.
	Completion uint64
}

// RunMany preserves input order with the baseline machines.length concurrency.
func (c *Client) RunMany(ctx context.Context, s Snapshot, argv []string) ([]Item, error) {
	out := make([]Item, len(s.Machines))
	var wg sync.WaitGroup
	var completed atomic.Uint64
	for i, m := range s.Machines {
		wg.Add(1)
		go func(i int, m Machine) {
			defer wg.Done()
			out[i].Result, out[i].Error = c.Exec(ctx, s, m, argv)
			out[i].Completion = completed.Add(1)
		}(i, m)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for _, item := range out {
		var rpc *RPCError
		if errors.As(item.Error, &rpc) && rpc.Code == -32800 {
			return nil, fmt.Errorf("invocation canceled: %w", rpc)
		}
	}
	if e := c.peer.Err(); e != nil {
		return nil, fmt.Errorf("extension channel: %w", e)
	}
	return out, nil
}
