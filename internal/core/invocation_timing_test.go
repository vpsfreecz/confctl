package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type invocationTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *invocationTestClock) read() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *invocationTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestInvocationTimingNestedPromptAndResidualUnions(t *testing.T) {
	c := &invocationTestClock{now: time.Now()}
	ledger := newInvocationTiming(c.read(), c.read)
	build := ledger.Begin(timingBuild)
	c.advance(2 * time.Second)
	nested := ledger.Begin(timingBuild)
	eval := ledger.Begin(timingEvaluation)
	c.advance(2 * time.Second)
	prompt := ledger.BeginPrompt()
	c.advance(3 * time.Second)
	prompt()
	c.advance(time.Second)
	nested()
	eval()
	nested() // Repeated ends cannot end a different active token.
	c.advance(2 * time.Second)
	build()
	s := ledger.Snapshot()
	if s.Elapsed != 10*time.Second || s.Phases[timingBuild] != 10*time.Second || s.Phases[timingEvaluation] != 6*time.Second || s.Work != 10*time.Second || s.Prompt != 3*time.Second || s.PromptOverlap != 3*time.Second || s.Other != 0 {
		t.Fatal(s)
	}
	c.advance(1234 * time.Microsecond)
	s = ledger.Finish(7, true)
	if s.Other != 1234*time.Microsecond || s.Elapsed != 10*time.Second+1234*time.Microsecond || !s.Canceled || !s.Finished || s.ExitCode != 7 {
		t.Fatal(s)
	}
	// Snapshot arrays are values; a caller cannot alter the frozen ledger.
	copy := s
	copy.Phases[timingBuild] = 0
	copy.Seen[timingBuild] = false
	c.advance(time.Hour)
	ledger.Begin(timingCopy)()
	ledger.BeginPrompt()()
	build()
	if ledger.Snapshot() != s || ledger.Finish(0, false) != s {
		t.Fatal("finish was not frozen", s, ledger.Snapshot())
	}
}

func TestInvocationTimingParallelPhaseUnionAndClockOutsideLock(t *testing.T) {
	c := &invocationTestClock{now: time.Now()}
	ledger := newInvocationTiming(c.read(), c.read)
	var admitted, joined sync.WaitGroup
	release := make(chan struct{})
	for range 32 {
		admitted.Add(1)
		joined.Add(1)
		go func() {
			defer joined.Done()
			done := ledger.Begin(timingCopy)
			admitted.Done()
			<-release
			done()
			done()
		}()
	}
	admitted.Wait()
	c.advance(5 * time.Second)
	close(release)
	joined.Wait()
	c.advance(time.Second)
	s := ledger.Finish(0, false)
	if s.Phases[timingCopy] != 5*time.Second || s.Work != 5*time.Second || s.Elapsed != 6*time.Second || s.Other != time.Second {
		t.Fatal("parallel time summed instead of unioned", s)
	}
	// A clock callback is never invoked with the ledger mutex held.
	ledger = newInvocationTiming(c.read(), nil)
	ledger.now = func() time.Time {
		if !ledger.mu.TryLock() {
			t.Fatal("clock callback ran under ledger lock")
		}
		ledger.mu.Unlock()
		return c.read()
	}
	ledger.Begin(timingEvaluation)()
	ledger.BeginPrompt()()
	ledger.Snapshot()
	ledger.Finish(0, false)
}

func TestInvocationTimingPromptUnionAndOffsetIndependentDurations(t *testing.T) {
	start := time.Now()
	now := start
	ledger := newInvocationTiming(start, func() time.Time { return now })
	a := ledger.BeginPrompt()
	now = start.Add(time.Second).In(time.FixedZone("offset changed", 5*3600))
	b := ledger.BeginPrompt()
	now = start.Add(2 * time.Second)
	a()
	now = start.Add(3 * time.Second)
	b()
	now = start.Add(4 * time.Second)
	s := ledger.Finish(0, false)
	if s.Prompt != 3*time.Second || s.PromptOverlap != 0 || s.Work != 0 || s.Other != time.Second || s.Elapsed != 4*time.Second {
		t.Fatal(s)
	}
}

func TestInvocationTimingNilZeroAndPromptAnswerIdentity(t *testing.T) {
	var ledger *invocationTiming
	ledger.Begin(timingBuild)()
	ledger.BeginPrompt()()
	if ledger.Snapshot() != (timingSnapshot{}) || ledger.Finish(1, true) != (timingSnapshot{}) {
		t.Fatal("nil ledger is not inert")
	}
	var engine *Engine
	engine.beginTiming(timingBuild)()
	line := commandLine(func(context.Context) (string, error) { return "exact answer", io.EOF })
	if reflect.ValueOf(engine.timedPromptLine(line)).Pointer() != reflect.ValueOf(line).Pointer() {
		t.Fatal("absent timing replaced line owner")
	}
	c := &invocationTestClock{now: time.Now()}
	engine = &Engine{timing: newInvocationTiming(c.read(), c.read)}
	ctx := context.Background()
	wantErr := errors.New("line result")
	called := 0
	wrapped := engine.timedPromptLine(func(got context.Context) (string, error) {
		called++
		if got != ctx {
			t.Fatal("context replaced")
		}
		c.advance(1200 * time.Microsecond)
		return " y\n", wantErr
	})
	answer, err := wrapped(ctx)
	if answer != " y\n" || err != wantErr || called != 1 || engine.timing.Snapshot().Prompt != 1200*time.Microsecond {
		t.Fatal(answer, err, called, engine.timing.Snapshot())
	}
	engine.beginTiming(timingPhaseCount)()
	clone := *engine
	clone.beginTiming(timingSSH)()
	if !engine.timing.Snapshot().Seen[timingSSH] || engine.timing.Snapshot().Seen[timingBuild] {
		t.Fatal("cloned engine did not share ledger")
	}
}

func TestInvocationTimingDurationAndExactReport(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-1, "0.000s"}, {0, "0.000s"}, {12999 * time.Microsecond, "0.012s"},
		{61 * time.Second, "1m 1.000s"}, {time.Hour + time.Second + time.Millisecond, "1h 0m 1.001s"},
	} {
		if got := formatTimingDuration(tc.d); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	start := time.Date(2026, 10, 18, 9, 8, 7, 0, time.Local)
	s := timingSnapshot{StartedAt: start, EndedAt: start.Add(10 * time.Second), Elapsed: 10 * time.Second, Work: 8 * time.Second, Prompt: 3 * time.Second, PromptOverlap: 2 * time.Second, Other: time.Second, Finished: true}
	s.Seen[timingEvaluation], s.Seen[timingBuild] = true, true
	s.Phases[timingEvaluation], s.Phases[timingBuild] = 3*time.Second, 6*time.Second
	var out bytes.Buffer
	if err := writeTimingReport(&out, s); err != nil {
		t.Fatal(err)
	}
	want := "\nTiming: completed (exit 0)\n  Started: " + start.Format(time.RFC3339) + "\n  Ended: " + s.EndedAt.Format(time.RFC3339) + "\n  Evaluation: 3.000s\n  Build: 6.000s\n  Prompt wait: 3.000s\n  Prompt/work overlap: 2.000s\n  Other: 1.000s\n  Total: 10.000s\n  Note: phase times overlap; total is wall time.\n"
	if out.String() != want {
		t.Fatal(out.String(), want)
	}
	s.PromptOverlap = time.Nanosecond
	out.Reset()
	_ = writeTimingReport(&out, s)
	if _, footer, err := splitTimingFooter(out.String()); err != nil || !strings.Contains(footer, "  Prompt/work overlap: 0.000s\n") {
		t.Fatal("positive overlap lost during display truncation", footer, err)
	}
	for _, tc := range []struct {
		code     int
		canceled bool
		state    string
	}{{0, false, "completed"}, {7, false, "failed"}, {1, true, "canceled"}} {
		s.ExitCode, s.Canceled = tc.code, tc.canceled
		out.Reset()
		_ = writeTimingReport(&out, s)
		if !strings.HasPrefix(out.String(), fmt.Sprintf("\nTiming: %s (exit %d)\n", tc.state, tc.code)) {
			t.Fatal(out.String())
		}
	}
	for _, short := range []bool{false, true} {
		w := &liveTestWriter{short: short}
		want := io.ErrShortWrite
		if !short {
			want = errors.New("report write")
			w.writeErr = want
		}
		if err := writeTimingReport(w, s); err != want || w.writes != 1 {
			t.Fatal(err, w.writes)
		}
	}
}

func TestInvocationTimingReportErrorKeepsPriorStatusAndFreezesBeforeWrite(t *testing.T) {
	for _, prior := range []int{0, 1, 64, 130, 143} {
		c := &invocationTestClock{now: time.Now()}
		ledger := newInvocationTiming(c.read(), c.read)
		c.advance(time.Second)
		out := &liveTestWriter{writeErr: errors.New("report failure")}
		want := prior
		if prior == 0 {
			want = 1
		}
		if got := finishTimingReport(out, ledger, prior, false); got != want || out.writes != 1 || ledger.Snapshot().ExitCode != prior || ledger.Snapshot().Elapsed != time.Second {
			t.Fatal("report failure masked status or retried", got, out.writes, ledger.Snapshot())
		}
	}
}

func TestInvocationTimingSSHUsesRealRunBoundaryAndResultIdentity(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	c := &invocationTestClock{now: time.Now()}
	e := &Engine{Root: "/fixture", Context: context.Background(), timing: newInvocationTiming(c.read(), c.read)}
	m := connectionMachine("node", "key", "node.invalid", true)
	payload := "literal input"
	wantErr := errors.New("run result")
	result, err := e.sshRunMachine(e.Context, m, []string{"cat", "literal token"}, sshInput{String: &payload}, sshDeps{Run: func(ctx context.Context, root string, argv []string, input sshInput) (Result, error) {
		if ctx != e.Context || root != "/fixture" || !reflect.DeepEqual(argv, []string{"ssh", "-l", "root", "node.invalid", `cat literal\ token`}) || input.String != &payload {
			t.Fatal("run binding changed", root, argv, input)
		}
		c.advance(3 * time.Second)
		if e.timing.Snapshot().Phases[timingSSH] != 3*time.Second {
			t.Fatal("timer not active inside actual call")
		}
		return Result{ExitCode: 7, Stdout: []byte("kept")}, wantErr
	}})
	if err != wantErr || result.Name != "node" || string(result.Result.Stdout) != "kept" {
		t.Fatal(result, err)
	}
	c.advance(2 * time.Second)
	s := e.timing.Snapshot()
	if s.Phases[timingSSH] != 3*time.Second || s.Other != 2*time.Second {
		t.Fatal("timer extended beyond run return", s)
	}
}

func TestInvocationTimingPromptKeepsBufferedAnswersAndEOF(t *testing.T) {
	c := &invocationTestClock{now: time.Now()}
	e := &Engine{Context: context.Background(), timing: newInvocationTiming(c.read(), c.read)}
	retained, line := commandInput(strings.NewReader("y\ny\nQ"))
	line = e.timedPromptLine(line)
	var out bytes.Buffer
	for range 2 {
		if answer, err := e.confirm(e.Context, line, &out, "permission", false); err != nil || !answer {
			t.Fatal("queued permission changed", answer, err)
		}
	}
	if b, err := retained.ReadByte(); err != nil || b != 'Q' {
		t.Fatal("prompt acquired a second reader", b, err)
	}
	if answer, err := line(e.Context); answer != "" || err != io.EOF {
		t.Fatal("timing changed EOF", answer, err)
	}
}
