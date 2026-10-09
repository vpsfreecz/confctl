package core

import (
	"context"
	"encoding/json"
	"fmt"
	ext "github.com/vpsfreecz/confctl/experimental/extension"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorHelper(t *testing.T) {
	if os.Getenv("CORE_SUPERVISOR_HELPER") == "" {
		return
	}
	if os.Args[len(os.Args)-1] == "worker" {
		signal.Ignore(syscall.SIGTERM)
		_ = os.WriteFile(filepath.Join(os.Getenv("CORE_SUPERVISOR_ROOT"), "worker.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			time.Sleep(time.Hour)
		}
	}
	in, out := os.NewFile(3, "in"), os.NewFile(4, "out")
	d, enc := json.NewDecoder(in), json.NewEncoder(out)
	var m ext.Message
	_ = d.Decode(&m)
	b, _ := json.Marshal(ext.InitResult{Protocol: ext.Version{Major: 1}})
	_ = enc.Encode(ext.Message{JSONRPC: "2.0", ID: m.ID, Result: b})
	_ = d.Decode(&m)
	runID := m.ID
	var run ext.Run
	_ = json.Unmarshal(m.Params, &run)
	if run.Handler == "next" {
		_ = os.WriteFile(filepath.Join(os.Getenv("CORE_SUPERVISOR_ROOT"), "next-stage"), []byte("unexpected"), 0600)
		b, _ = json.Marshal(ext.RunResult{})
		_ = enc.Encode(ext.Message{JSONRPC: "2.0", ID: runID, Result: b})
		os.Exit(0)
	}
	params, _ := json.Marshal(ext.Selection{Managed: "all"})
	_ = enc.Encode(ext.Message{JSONRPC: "2.0", ID: "e:1", Method: "machines.select", Params: params})
	_ = d.Decode(&m)
	var selected ext.Snapshot
	_ = json.Unmarshal(m.Result, &selected)
	params, _ = json.Marshal(map[string]any{"snapshot": selected.ID, "machine": "m", "argv": []string{os.Args[0], "-test.run=TestSupervisorHelper", "worker"}, "stdin_b64": nil, "output": "capture"})
	_ = enc.Encode(ext.Message{JSONRPC: "2.0", ID: "e:2", Method: "exec.run", Params: params})
	until := time.Now().Add(3 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(os.Getenv("CORE_SUPERVISOR_ROOT"), "worker.pid")); e == nil {
			break
		}
		if time.Now().After(until) {
			os.Exit(9)
		}
		time.Sleep(time.Millisecond)
	}
	if os.Getenv("CORE_SUPERVISOR_HELPER") == "return" {
		b, _ = json.Marshal(ext.RunResult{})
		_ = enc.Encode(ext.Message{JSONRPC: "2.0", ID: runID, Result: b})
	}
	os.Exit(0)
}
func TestSupervisorRejectsAndReapsOutstandingExec(t *testing.T) {
	for _, mode := range []string{"return", "exit"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CORE_SUPERVISOR_HELPER", mode)
			t.Setenv("CORE_SUPERVISOR_ROOT", root)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			host := "localhost"
			engine := &Engine{Root: root, Context: ctx, Inventory: []Machine{{Name: "m", Managed: true, Target: Target{Host: &host, Port: 22, Localhost: true}}}}
			_, e := engine.Hooks(Registry{Schema: 1, Extensions: []Registration{{ID: "early", Argv: []string{os.Args[0], "-test.run=TestSupervisorHelper"}, Hooks: []Hook{{Event: "deploy.prepare", Handler: "run", Order: 0}, {Event: "deploy.prepare", Handler: "next", Order: 1}}}}}, "deploy.prepare", []string{"m"})
			if e == nil || ctx.Err() != nil {
				t.Fatal("incomplete invocation accepted or cleanup hung", e, ctx.Err())
			}
			b, e := os.ReadFile(filepath.Join(root, "worker.pid"))
			if e != nil {
				t.Fatal(e)
			}
			pid, e := strconv.Atoi(strings.TrimSpace(string(b)))
			if e != nil {
				t.Fatal(e)
			}
			if e = syscall.Kill(pid, 0); e != syscall.ESRCH {
				t.Fatal("reverse service child remains", pid, e)
			}
			if _, e = os.Stat(filepath.Join(root, "next-stage")); !os.IsNotExist(e) {
				t.Fatal("next stage mutation")
			}
		})
	}
}
func TestExplicitProcessCancellationEscalatesAndReaps(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CORE_SUPERVISOR_HELPER", "worker")
	t.Setenv("CORE_SUPERVISOR_ROOT", root)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := Process(ctx, root, []string{os.Args[0], "-test.run=TestSupervisorHelper", "worker"}, nil)
		done <- e
	}()
	until := time.Now().Add(3 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(root, "worker.pid")); e == nil {
			break
		}
		if time.Now().After(until) {
			cancel()
			t.Fatal("worker not ready")
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case e := <-done:
		if e != context.Canceled {
			t.Fatal(e)
		}
		if time.Since(start) < 1900*time.Millisecond {
			t.Fatal("grace skipped")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation hung")
	}
	b, _ := os.ReadFile(filepath.Join(root, "worker.pid"))
	pid, _ := strconv.Atoi(string(b))
	if e := syscall.Kill(pid, 0); e != syscall.ESRCH {
		t.Fatal(fmt.Sprint(pid), e)
	}
}
