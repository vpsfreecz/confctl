package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func TestConnectionTimingActualRunsEndBeforeDiagnosticsAndKeepFailedHostOutcome(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	first := connectionMachine("first", "first", "localhost", true)
	second := connectionMachine("second", "second", "second.invalid", true)
	e := connectionEngine(t, first, second)
	clock := invocationAdapterTiming(e)
	out := &invocationAdapterWriter{clock: clock}
	var calls [][]string
	err := e.runTestConnection(connectionInvocation(t, "--yes"), nil, out, testConnectionDeps{Run: func(ctx context.Context, root string, argv []string, input []byte) (Result, error) {
		if ctx != e.Context || root != e.Root || input != nil {
			t.Error("connection invocation changed")
		}
		calls = append(calls, append([]string(nil), argv...))
		clock.advance(2 * time.Second)
		if s := e.timing.Snapshot(); s.Phases[timingConnection] != time.Duration(len(calls))*2*time.Second {
			t.Error("connection interval is not active during runner", s)
		}
		if len(calls) == 1 {
			return Result{ExitCode: 7, Stdout: []byte("inside\nlast\n"), Stderr: []byte("diagnostic")}, nil
		}
		return Result{ExitCode: 0}, nil
	}})
	clock.advance(5 * time.Second)
	if err != nil || len(calls) != 2 || !reflect.DeepEqual(calls[0], []string{"cat", "/proc/uptime"}) || calls[1][0] != "ssh" {
		t.Fatal("failed-host invocation must still complete successfully", calls, err)
	}
	want := "Unable to connect to first: Running `cat /proc/uptime` failed with\n  exit status: 7\n  stdout: inside\nlast\n  stderr: diagnostic\n\n\nResult: 1 successful, 1 failed\n\nFailed machines:\n  first\n"
	if out.String() != want {
		t.Fatal("timing altered connection diagnostics", out.String())
	}
	if s := e.timing.Snapshot(); s.Phases[timingConnection] != 4*time.Second || s.Work != 4*time.Second || s.Prompt != 0 || s.Other == 0 {
		t.Fatal("runner interval includes diagnostics or post-return time", s)
	}
}

func TestConnectionTimingRunnerErrorAndCanceledIdentity(t *testing.T) {
	for _, failure := range []error{errors.New("connection capture error"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			e := connectionEngine(t, connectionMachine("one", "one", "localhost", true))
			clock := invocationAdapterTiming(e)
			var out bytes.Buffer
			err := e.runTestConnection(connectionInvocation(t, "--yes"), nil, &out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
				clock.advance(2 * time.Second)
				return Result{}, failure
			}})
			clock.advance(5 * time.Second)
			if err != failure || out.Len() != 0 || e.timing.Snapshot().Phases[timingConnection] != 2*time.Second {
				t.Fatal("runner error/result timing changed", err, out.String(), e.timing.Snapshot())
			}
		})
	}
}

func TestConnectionTimingConfirmationAndPreRunRefusal(t *testing.T) {
	for _, mode := range []string{"accept", "decline", "empty-selection"} {
		t.Run(mode, func(t *testing.T) {
			e := connectionEngine(t, connectionMachine("one", "one", "localhost", true))
			clock := invocationAdapterTiming(e)
			reader := &invocationAdapterReader{clock: clock, steps: []connectionRead{{data: "y\n"}}}
			if mode == "decline" {
				reader.steps[0].data = "n\n"
			}
			if mode == "empty-selection" {
				e.Inventory = []Machine{}
			}
			err := e.runTestConnection(connectionInvocation(t), reader, io.Discard, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
				if mode != "accept" {
					t.Error("refused connection ran child")
				}
				clock.advance(time.Second)
				return Result{ExitCode: 0}, nil
			}})
			if (err == nil) != (mode == "accept") {
				t.Fatal(err)
			}
			clock.advance(5 * time.Second)
			s := e.timing.Snapshot()
			wantPrompt := 2 * time.Second
			if mode == "empty-selection" {
				wantPrompt = 0
			}
			if s.Prompt != wantPrompt || s.Seen[timingConnection] != (mode == "accept") {
				t.Fatal("confirmation was not confined to input read", s)
			}
		})
	}
}
