package core

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

type timingPhase uint8

const (
	timingEvaluation timingPhase = iota
	timingBuild
	timingCopy
	timingDeployment
	timingStatusChecks
	timingRebootWait
	timingHealth
	timingSSH
	timingConnection
	timingGC
	timingPhaseCount
)

var timingPhaseLabels = [timingPhaseCount]string{"Evaluation", "Build", "Copy", "Deployment", "Status checks", "Reboot wait", "Health checks", "SSH commands", "Connection tests", "Garbage collection"}

type timingSnapshot struct {
	StartedAt, EndedAt                          time.Time
	Elapsed, Work, Prompt, PromptOverlap, Other time.Duration
	Phases                                      [timingPhaseCount]time.Duration
	Seen                                        [timingPhaseCount]bool
	ExitCode                                    int
	Canceled, Finished                          bool
}

// One invocation owns this ledger. Tokens distinguish nested/parallel calls;
// only positive active counts accrue time, so each field is a wall-time union.
type invocationTiming struct {
	mu      sync.Mutex
	now     func() time.Time
	last    time.Time
	s       timingSnapshot
	active  [timingPhaseCount]int
	work    int
	prompt  int
	next    uint64
	tokens  map[uint64]timingPhase
	prompts map[uint64]bool
}

func newInvocationTiming(start time.Time, now func() time.Time) *invocationTiming {
	if now == nil {
		now = time.Now
	}
	return &invocationTiming{now: now, last: start, s: timingSnapshot{StartedAt: start}, tokens: make(map[uint64]timingPhase), prompts: make(map[uint64]bool)}
}

// Called with mu held. Stale concurrent clock samples cannot move time back.
func (t *invocationTiming) accrue(now time.Time) {
	delta := now.Sub(t.last)
	if delta < 0 {
		return
	}
	for phase, active := range t.active {
		if active > 0 {
			t.s.Phases[phase] += delta
		}
	}
	if t.work > 0 {
		t.s.Work += delta
	}
	if t.prompt > 0 {
		t.s.Prompt += delta
		if t.work > 0 {
			t.s.PromptOverlap += delta
		}
	}
	t.last = now
	t.s.EndedAt = now
	t.s.Elapsed = now.Sub(t.s.StartedAt)
	t.s.Other = max(t.s.Elapsed-(t.s.Work+t.s.Prompt-t.s.PromptOverlap), 0)
}

func (t *invocationTiming) Begin(phase timingPhase) func() {
	if t == nil || phase >= timingPhaseCount {
		return func() {}
	}
	now := t.now() // The injected clock, like any callback, runs outside mu.
	t.mu.Lock()
	if t.s.Finished {
		t.mu.Unlock()
		return func() {}
	}
	t.accrue(now)
	t.next++
	id := t.next
	t.tokens[id] = phase
	t.active[phase]++
	t.work++
	t.s.Seen[phase] = true
	t.mu.Unlock()
	return func() {
		now := t.now()
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, ok := t.tokens[id]; !ok || t.s.Finished {
			return
		}
		t.accrue(now)
		delete(t.tokens, id)
		t.active[phase]--
		t.work--
	}
}

func (t *invocationTiming) BeginPrompt() func() {
	if t == nil {
		return func() {}
	}
	now := t.now()
	t.mu.Lock()
	if t.s.Finished {
		t.mu.Unlock()
		return func() {}
	}
	t.accrue(now)
	t.next++
	id := t.next
	t.prompts[id] = true
	t.prompt++
	t.mu.Unlock()
	return func() {
		now := t.now()
		t.mu.Lock()
		defer t.mu.Unlock()
		if !t.prompts[id] || t.s.Finished {
			return
		}
		t.accrue(now)
		delete(t.prompts, id)
		t.prompt--
	}
}

func (t *invocationTiming) Snapshot() timingSnapshot {
	if t == nil {
		return timingSnapshot{}
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.s.Finished {
		t.accrue(now)
	}
	return t.s
}

func (t *invocationTiming) Finish(exitCode int, canceled bool) timingSnapshot {
	if t == nil {
		return timingSnapshot{}
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.s.Finished {
		t.accrue(now)
		t.s.ExitCode, t.s.Canceled, t.s.Finished = exitCode, canceled, true
	}
	return t.s
}

func (e *Engine) beginTiming(phase timingPhase) func() {
	if e == nil {
		return func() {}
	}
	return e.timing.Begin(phase)
}

func (e *Engine) timedPromptLine(line commandLine) commandLine {
	if e == nil || e.timing == nil {
		return line
	}
	return func(ctx context.Context) (string, error) {
		done := e.timing.BeginPrompt()
		defer done()
		return line(ctx)
	}
}

func formatTimingDuration(d time.Duration) string {
	d = max(d, 0)
	seconds, millis := int64(d/time.Second), int64(d/time.Millisecond%1000)
	if d >= time.Hour {
		return fmt.Sprintf("%dh %dm %d.%03ds", seconds/3600, seconds/60%60, seconds%60, millis)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm %d.%03ds", seconds/60, seconds%60, millis)
	}
	return fmt.Sprintf("%d.%03ds", seconds, millis)
}

func writeTimingReport(out io.Writer, s timingSnapshot) error {
	state := "completed"
	if s.Canceled {
		state = "canceled"
	} else if s.ExitCode != 0 {
		state = "failed"
	}
	var b strings.Builder
	// A single separating LF also handles a prior diagnostic without a newline.
	fmt.Fprintf(&b, "\nTiming: %s (exit %d)\n  Started: %s\n  Ended: %s\n", state, s.ExitCode, s.StartedAt.Local().Format(time.RFC3339), s.EndedAt.Local().Format(time.RFC3339))
	sum := time.Duration(0)
	for phase, seen := range s.Seen {
		if seen {
			fmt.Fprintf(&b, "  %s: %s\n", timingPhaseLabels[phase], formatTimingDuration(s.Phases[phase]))
			sum += s.Phases[phase]
		}
	}
	fmt.Fprintf(&b, "  Prompt wait: %s\n", formatTimingDuration(s.Prompt))
	if s.PromptOverlap > 0 {
		fmt.Fprintf(&b, "  Prompt/work overlap: %s\n", formatTimingDuration(s.PromptOverlap))
	}
	fmt.Fprintf(&b, "  Other: %s\n  Total: %s\n", formatTimingDuration(s.Other), formatTimingDuration(s.Elapsed))
	if sum > s.Work {
		b.WriteString("  Note: phase times overlap; total is wall time.\n")
	}
	n, err := io.WriteString(out, b.String())
	if err == nil && n != b.Len() {
		err = io.ErrShortWrite
	}
	return err
}

func finishTimingReport(out io.Writer, timing *invocationTiming, result int, canceled bool) int {
	if err := writeTimingReport(out, timing.Finish(result, canceled)); err != nil && result == 0 {
		return 1
	}
	return result
}

func invocationHasTiming(inv cli.Invocation) bool {
	switch inv.Command.Handler {
	case cli.BuildHandler, cli.StatusHandler, cli.DeployHandler, cli.HealthCheckHandler, cli.TestConnectionHandler, cli.CollectGarbageHandler, cli.GenerationRemoveHandler, cli.GenerationRotateHandler:
		return true
	case cli.SSHHandler:
		return len(inv.Args) != 1
	default:
		return false
	}
}
