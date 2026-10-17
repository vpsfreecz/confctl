package core

import (
	"bufio"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shared by these owning adapter tests; only explicit fixture actions advance
// time. In particular no child scheduling, output count or sleep is a clock.
type invocationAdapterClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *invocationAdapterClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *invocationAdapterClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func invocationAdapterTiming(e *Engine) *invocationAdapterClock {
	c := &invocationAdapterClock{at: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	e.timing = newInvocationTiming(c.now(), c.now)
	return c
}

type invocationAdapterReader struct {
	clock  *invocationAdapterClock
	steps  []connectionRead
	cancel context.CancelFunc
}

func (r *invocationAdapterReader) Read(b []byte) (int, error) {
	r.clock.advance(2 * time.Second)
	if r.cancel != nil {
		r.cancel()
	}
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	s := r.steps[0]
	r.steps = r.steps[1:]
	return copy(b, s.data), s.err
}

type invocationAdapterWriter struct {
	strings.Builder
	clock *invocationAdapterClock
}

func (w *invocationAdapterWriter) Write(b []byte) (int, error) {
	w.clock.advance(time.Second)
	return w.Builder.Write(b)
}

func (w *invocationAdapterWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestGenerationTimingPreparedRunWithoutViewAndExactOutcome(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"failure", errors.New("owned GC capture failure")}, {"canceled", context.Canceled}} {
		t.Run(tc.name, func(t *testing.T) {
			e := gcAdapterEngine(t)
			clock := invocationAdapterTiming(e)
			want := Result{ExitCode: 7, Stdout: []byte("out\x00"), Stderr: []byte("err\n")}
			taskCtx := context.WithValue(e.Context, struct{}{}, "owned task")
			d := e.generationGCAdapter(io.Discard, io.Discard, func(ctx context.Context, root string, argv []string, input []byte, feed func([]byte, []byte) error) (Result, error) {
				if ctx != taskCtx || root != e.Root || !reflect.DeepEqual(argv, []string{"ssh", "-l", "root", "node.invalid", "nix-collect-garbage"}) || input != nil {
					t.Errorf("GC invocation changed: %v %q %q", ctx, root, argv)
				}
				clock.advance(2 * time.Second)
				if s := e.timing.Snapshot(); !s.Seen[timingGC] || s.Phases[timingGC] != 2*time.Second {
					t.Errorf("run is outside GC interval: %+v", s)
				}
				if err := feed(want.Stdout, want.Stderr); err != nil {
					t.Error(err)
				}
				return want, tc.err
			})
			execute, err := d.Prepare(e.Context)
			if err != nil || e.timing.Snapshot().Seen[timingGC] {
				t.Fatal("prepare invented work", err)
			}
			got, err := execute(taskCtx, mutationMachine("node"), []string{"nix-collect-garbage"}, func(stdout, stderr []byte) {
				if !reflect.DeepEqual(stdout, want.Stdout) || !reflect.DeepEqual(stderr, want.Stderr) {
					t.Error("feed changed captured bytes")
				}
				clock.advance(time.Second)
			})
			clock.advance(5 * time.Second)
			if !reflect.DeepEqual(got, want) || err != tc.err {
				t.Fatal("run result/error identity changed", got, err)
			}
			if s := e.timing.Snapshot(); s.Phases[timingGC] != 3*time.Second || s.Work != 3*time.Second || s.Other != 5*time.Second {
				t.Fatal("GC interval did not end on run return", s)
			}
		})
	}
}

func TestGenerationTimingAdmissionFailureHasNoGCInterval(t *testing.T) {
	e := gcAdapterEngine(t)
	invocationAdapterTiming(e)
	d := e.generationGCAdapter(io.Discard, io.Discard, func(context.Context, string, []string, []byte, func([]byte, []byte) error) (Result, error) {
		t.Error("targetless task ran a child")
		return Result{}, nil
	})
	execute, err := d.Prepare(e.Context)
	if err != nil {
		t.Fatal(err)
	}
	m := mutationMachine("node")
	m.Target.Host = nil
	if _, err := execute(e.Context, m, []string{"nix-collect-garbage"}, func([]byte, []byte) {}); err == nil || e.timing.Snapshot().Seen[timingGC] {
		t.Fatal("routing refusal was timed as execution", err)
	}
}

func TestGenerationTimingConfirmationReadIntervalsAndQueuedInput(t *testing.T) {
	for _, mode := range []string{"yes", "retry", "decline", "eof", "error", "cancel", "assume-yes"} {
		t.Run(mode, func(t *testing.T) {
			e := gcAdapterEngine(t)
			clock := invocationAdapterTiming(e)
			ctx, cancel := context.WithCancel(e.Context)
			defer cancel()
			e.Context = ctx
			readErr := errors.New("prompt read failed")
			reader := &invocationAdapterReader{clock: clock, steps: []connectionRead{{data: "y\n"}, {data: "TAIL\n"}}}
			var wantErr error
			wantPrompt := 2 * time.Second
			args := []string{}
			switch mode {
			case "retry":
				reader.steps = append([]connectionRead{{data: "invalid\n"}}, reader.steps...)
				wantPrompt = 4 * time.Second
			case "decline":
				reader.steps[0].data = "n\n"
			case "eof":
				reader.steps = nil
			case "error":
				reader.steps = []connectionRead{{err: readErr}}
				wantErr = readErr
			case "cancel":
				reader.cancel = cancel
				wantErr = context.Canceled
			case "assume-yes":
				args = []string{"--yes"}
				wantPrompt = 0
			}
			input := bufio.NewReader(reader)
			out := &invocationAdapterWriter{clock: clock}
			err := e.generationConfirm(gcAdapterInvocation(t, args...), input, out, "selected hosts\n")
			if mode == "decline" || mode == "eof" {
				want := map[string]string{"decline": "Aborted", "eof": "end of file reached"}[mode]
				if err == nil || err.Error() != want {
					t.Fatal(err)
				}
			} else if err != wantErr {
				t.Fatal("confirmation error identity changed", err, wantErr)
			}
			clock.advance(5 * time.Second)
			if s := e.timing.Snapshot(); s.Prompt != wantPrompt || s.Work != 0 {
				t.Fatal("prompt includes rendering/returned time", s)
			}
			if mode == "yes" || mode == "retry" {
				if tail, err := input.ReadString('\n'); tail != "TAIL\n" || err != nil {
					t.Fatal("queued reader replaced", tail, err)
				}
			}
			if mode == "assume-yes" && out.Len() != 0 {
				t.Fatal("--yes wrote or read a prompt", out.String())
			}
		})
	}
}
