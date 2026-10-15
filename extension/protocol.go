// Package extension is the experimental public, language-neutral executable API.
// It does not import core/internal packages. IPC uses inherited FD3/FD4 only.
package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

const Major = 1
const Minor = 0

type Version struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type Peer struct {
	writer     *json.Encoder
	writeMu    sync.Mutex
	mu         sync.Mutex
	pending    map[string]chan Message
	used       map[string]bool
	done       chan struct{}
	readDone   chan struct{}
	err        error
	sealed     bool
	incoming   atomic.Int64
	handlers   sync.WaitGroup
	seq        atomic.Uint64
	prefix     string
	ready      chan struct{}
	AfterReply func(Message)
	Handle     func(context.Context, string, json.RawMessage) (any, error)
	ctx        context.Context
}

func NewPeer(ctx context.Context, in io.Reader, out io.Writer, prefix string) *Peer {
	p := &Peer{writer: json.NewEncoder(out), pending: map[string]chan Message{}, used: map[string]bool{}, done: make(chan struct{}), readDone: make(chan struct{}), prefix: prefix, ctx: ctx, ready: make(chan struct{})}
	go p.read(in)
	return p
}
func (p *Peer) send(m Message) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	m.JSONRPC = "2.0"
	return p.writer.Encode(m)
}
func (p *Peer) fail(e error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = e
		close(p.done)
	}
	p.mu.Unlock()
}
func (p *Peer) SetHandler(h func(context.Context, string, json.RawMessage) (any, error)) {
	p.Handle = h
	close(p.ready)
}
func (p *Peer) read(in io.Reader) {
	defer close(p.readDone)
	<-p.ready
	r := bufio.NewReader(in)
	for {
		line, e := r.ReadBytes('\n')
		if e != nil {
			p.fail(e)
			return
		}
		var m Message
		if e = json.Unmarshal(line, &m); e != nil {
			p.fail(fmt.Errorf("invalid protocol frame: %w", e))
			return
		}
		if m.JSONRPC != "2.0" {
			p.fail(errors.New("invalid JSON-RPC version"))
			return
		}
		if m.Method != "" {
			if m.ID != "" {
				p.mu.Lock()
				used := p.used[m.ID]
				p.used[m.ID] = true
				p.mu.Unlock()
				if used {
					p.fail(fmt.Errorf("duplicate request id %s", m.ID))
					return
				}
			}
			p.mu.Lock()
			if p.sealed {
				p.mu.Unlock()
				p.fail(errors.New("reverse service after invocation completion"))
				return
			}
			p.incoming.Add(1)
			p.handlers.Add(1)
			p.mu.Unlock()
			go p.dispatch(m)
			continue
		}
		if m.ID == "" || (m.Result == nil) == (m.Error == nil) {
			p.fail(errors.New("invalid response"))
			return
		}
		p.mu.Lock()
		ch := p.pending[m.ID]
		delete(p.pending, m.ID)
		p.mu.Unlock()
		if ch == nil {
			p.fail(fmt.Errorf("unexpected response id %s", m.ID))
			return
		}
		ch <- m
	}
}
func (p *Peer) dispatch(m Message) {
	defer p.handlers.Done()
	var value any
	var err error
	if p.Handle == nil {
		err = fmt.Errorf("unsupported method %s", m.Method)
	} else {
		value, err = p.Handle(p.ctx, m.Method, m.Params)
	}
	p.incoming.Add(-1)
	if m.ID == "" {
		return
	}
	response := Message{ID: m.ID}
	if err != nil {
		kind, code := "service", -32000
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind, code = "cancellation", -32800
		}
		response.Error = &RPCError{Code: code, Message: err.Error(), Data: map[string]any{"kind": kind, "message": err.Error(), "retryable": false}}
	} else {
		response.Result, err = json.Marshal(value)
		if err != nil {
			response.Error = &RPCError{Code: -32603, Message: err.Error()}
		}
	}
	if err = p.send(response); err != nil {
		p.fail(err)
	}
	if p.AfterReply != nil {
		response.Method = m.Method
		p.AfterReply(response)
	}
}
func (p *Peer) Call(ctx context.Context, method string, params, result any) error {
	b, e := json.Marshal(params)
	if e != nil {
		return e
	}
	id := fmt.Sprintf("%s:%d", p.prefix, p.seq.Add(1))
	ch := make(chan Message, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	if e = p.send(Message{ID: id, Method: method, Params: b}); e != nil {
		return e
	}

	decode := func(m Message) error {
		if m.Error != nil {
			return m.Error
		}
		d := json.NewDecoder(bytes.NewReader(m.Result))
		d.UseNumber()
		return d.Decode(result)
	}
	select {
	case m := <-ch:
		return decode(m)
	case <-p.done:
		// A final response can be followed immediately by EOF; retain that response.
		select {
		case m := <-ch:
			return decode(m)
		default:
			return p.Err()
		}
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return ctx.Err()
	}

}
func (p *Peer) Notify(method string, params any) error {
	b, e := json.Marshal(params)
	if e != nil {
		return e
	}
	return p.send(Message{Method: method, Params: b})
}
func (p *Peer) Err() error            { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *Peer) Done() <-chan struct{} { return p.done }
func (p *Peer) SealIncoming() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sealed = true
	return int(p.incoming.Load())
}
func (p *Peer) Incoming() int    { return int(p.incoming.Load()) }
func (p *Peer) WaitHandlers()    { <-p.readDone; p.handlers.Wait() }
func (p *Peer) Outstanding() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.pending) }

type Handler func(context.Context, *Client, Invocation) error
type Init struct {
	Protocol    Version  `json:"protocol"`
	ExtensionID string   `json:"extension_id"`
	Supported   []string `json:"supported_services"`
}
type InitResult struct {
	Protocol Version  `json:"protocol"`
	Required []string `json:"required_services"`
}
type Run struct {
	Handler string     `json:"handler"`
	Context Invocation `json:"context"`
}
type RunResult struct {
	ExitCode int `json:"exit_code"`
}

func Serve(handlers map[string]Handler, required []string) { os.Exit(serve(handlers, required)) }
func serve(handlers map[string]Handler, required []string) int {
	in := os.NewFile(3, "confctl-in")
	out := os.NewFile(4, "confctl-out")
	if in == nil || out == nil {
		fmt.Fprintln(os.Stderr, "extension requires FD3/FD4")
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPeer(ctx, in, out, "e")
	complete := make(chan int, 1)
	initialized := false
	running := false
	var mu sync.Mutex
	p.AfterReply = func(m Message) {
		if m.Method == "run" {
			if m.Error != nil {
				complete <- 1
			} else {
				complete <- 0
			}
		}
	}
	p.SetHandler(func(ctx context.Context, method string, b json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			var init Init
			if e := json.Unmarshal(b, &init); e != nil {
				return nil, e
			}
			if init.Protocol.Major != Major {
				return nil, fmt.Errorf("protocol major mismatch")
			}
			for _, needed := range required {
				found := false
				for _, available := range init.Supported {
					if needed == available {
						found = true
					}
				}
				if !found {
					return nil, fmt.Errorf("required service unavailable: %s", needed)
				}
			}
			mu.Lock()
			if initialized {
				mu.Unlock()
				return nil, fmt.Errorf("duplicate initialize")
			}
			initialized = true
			mu.Unlock()
			return InitResult{Protocol: Version{Major, Minor}, Required: required}, nil
		case "cancel":
			cancel()
			return map[string]any{}, nil
		case "run":
			mu.Lock()
			ready := initialized && !running
			running = true
			mu.Unlock()
			if !ready {
				return nil, fmt.Errorf("run before initialize")
			}
			var run Run
			if e := decodeRun(b, &run); e != nil {
				return nil, e
			}
			handler := handlers[run.Handler]
			if handler == nil {
				return nil, fmt.Errorf("unknown handler %s", run.Handler)
			}
			e := handler(ctx, &Client{peer: p}, run.Context)
			if e != nil {
				return nil, e
			}
			return RunResult{ExitCode: 0}, nil
		default:
			return nil, fmt.Errorf("unknown core method %s", method)
		}
	})
	// Wait until the run response writer has completed, not merely the handler.
	code := 1
	select {
	case code = <-complete:
	case <-p.done:
		fmt.Fprintln(os.Stderr, p.Err())
		return 1
	}
	p.writeMu.Lock()
	p.writeMu.Unlock()
	_ = out.Close()
	_ = in.Close()
	return code
}

// Run is the arbitrary JSON option boundary. Preserve numeric tokens and accept
// exactly one JSON value; Peer.Call already applies this policy to responses.
func decodeRun(b []byte, run *Run) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(run); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := d.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing run JSON")
	}
	return nil
}
