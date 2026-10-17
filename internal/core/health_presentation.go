package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/health"
	"github.com/vpsfreecz/confctl/internal/ui"
)

type healthProcessFunc = func(context.Context, string, []string, []byte) (Result, error)

// runHealthPresented owns one admitted round, including deployment retry subsets.
// It observes the existing serialized events; the health runner owns attempts,
// cooldowns, cancellation and joins. Child output stays in the durable log.
func (e *Engine) runHealthPresented(plan HealthPlan, deps health.Deps, process healthProcessFunc, stdout, stderr io.Writer) (HealthResult, error) {
	ctx, cancel := context.WithCancel(e.Context)
	defer cancel()
	attempt := *e
	attempt.Context = ctx
	var errorMu sync.Mutex
	var progressErr error
	fail := func(err error) {
		if err == nil {
			return
		}
		errorMu.Lock()
		if progressErr == nil {
			progressErr = err
		}
		errorMu.Unlock()
		cancel()
	}
	failure := func() error {
		errorMu.Lock()
		defer errorMu.Unlock()
		return progressErr
	}
	header := e.healthHeader(plan)
	presentation := newHealthRoundPresentation(plan.Checks)
	view, err := e.openLiveView(ctx, stdout, stderr, liveViewSpec{Header: header, Rows: presentation.rows, RawLog: false}, fail)
	if err != nil {
		return HealthResult{}, err
	}
	closeView := func() error {
		if view == nil {
			return nil
		}
		return view.Close()
	}
	emit := func(text string) error {
		if view == nil {
			if err := healthWrite(stdout, text); err != nil {
				return err
			}
		} else {
			if err := view.Line(strings.TrimSuffix(text, "\n")); err != nil {
				return err
			}
		}
		return e.logText(text)
	}
	headerText := fmt.Sprintf("Running %d health checks on %d machines\n", len(plan.Checks), len(plan.Machines))
	if e.Log != nil {
		headerText += "Full log: " + e.Log.Name() + "\n"
	}
	if view == nil {
		err = emit(headerText)
	} else {
		err = e.logText(headerText)
	}
	if err != nil {
		fail(err)
	}
	if err := failure(); err != nil {
		return HealthResult{}, errors.Join(err, closeView())
	}

	previousProgress := deps.Progress
	deps.Progress = func(event health.Event) {
		if failure() != nil {
			return
		}
		if previousProgress != nil {
			previousProgress(event)
		}
		if view != nil {
			if err := presentation.event(event); err != nil {
				fail(err)
				return
			}
			if err := view.SetRows(presentation.rows...); err != nil {
				fail(err)
				return
			}
		}
		fail(emit(healthEventText(event, len(plan.Checks))))
	}
	roundDone := attempt.beginTiming(timingHealth)
	result, runErr := attempt.runHealth(plan, deps, process)
	roundDone()
	if failure() == nil && view != nil {
		presentation.finish(result.Report)
		fail(view.SetRows(presentation.rows...))
	}
	// The runner has joined children and progress callbacks. Close always drains
	// and joins the view, even after cancellation, before summary/choice output.
	if err := healthClosePresentedRound(runErr, failure, closeView); err != nil {
		return result, err
	}
	summary, _ := healthSummary(result.Report)
	styledSummary, _ := e.healthSummary(stdout, result.Report)
	if err := healthWrite(stdout, styledSummary); err != nil {
		return result, errors.Join(runErr, err)
	}
	if err := e.logText(summary); err != nil {
		return result, errors.Join(runErr, err)
	}
	return result, runErr
}

func (e *Engine) healthHeader(plan HealthPlan) ui.LiveHeader {
	detail := fmt.Sprintf("%d checks on %d machines", len(plan.Checks), len(plan.Machines))
	if len(plan.Machines) == 1 {
		header := e.liveHeader("Health checks", fmt.Sprintf("%d checks on", len(plan.Checks)))
		header.Target = plan.Machines[0].Name
		return header
	}
	return e.liveHeader("Health checks", detail)
}

func healthClosePresentedRound(runErr error, failure func() error, closeView func() error) error {
	eventErr := failure()
	closeErr := closeView()
	if eventErr != nil {
		return errors.Join(eventErr, closeErr)
	}
	if lateErr := failure(); lateErr != nil || closeErr != nil {
		return errors.Join(runErr, lateErr, closeErr)
	}
	return nil
}

func healthEventText(event health.Event, total int) string {
	prefix := fmt.Sprintf("[%d/%d] %s> ", event.Index+1, total, event.Result.Check.Machine)
	var text string
	switch event.Phase {
	case "start":
		text = event.Result.Check.Description
	case "failed":
		text = fmt.Sprintf("failed #%d: %s", event.Result.Attempts, HealthMessages(event.Result))
	case "done":
		if event.Result.Passed() {
			text = "succeeded"
		} else {
			text = "error: " + HealthMessages(event.Result)
		}
	}
	return prefix + text + "\n"
}

type healthRoundPresentation struct {
	rows      []ui.LiveRow
	completed []bool
}

func newHealthRoundPresentation(checks []health.Check) *healthRoundPresentation {
	p := &healthRoundPresentation{completed: make([]bool, len(checks))}
	p.rows = append(p.rows, ui.LiveRow{ID: "health:round", Label: "Health checks", Current: new(big.Int), Total: big.NewInt(int64(len(checks))), State: ui.LiveRunning})
	for i, check := range checks {
		p.rows = append(p.rows, ui.LiveRow{ID: fmt.Sprintf("health:check:%d", i), Label: check.Machine, Target: true, Detail: check.Description + ": pending", State: ui.LivePending})
	}
	return p
}

func (p *healthRoundPresentation) event(event health.Event) error {
	if event.Index < 0 || event.Index >= len(p.completed) {
		return fmt.Errorf("health progress index %d is outside this round", event.Index)
	}
	row := &p.rows[event.Index+1]
	switch event.Phase {
	case "start":
		row.State, row.Detail = ui.LiveRunning, event.Result.Check.Description+": attempt 1"
	case "failed":
		row.State, row.Detail = ui.LiveRunning, fmt.Sprintf("%s: failed #%d: %s", event.Result.Check.Description, event.Result.Attempts, HealthMessages(event.Result))
	case "done":
		p.result(event.Index, event.Result)
	}
	return nil
}

func (p *healthRoundPresentation) result(index int, result health.Result) {
	row := &p.rows[index+1]
	row.Detail = fmt.Sprintf("%s: %d attempts", result.Check.Description, result.Attempts)
	switch {
	case !result.Complete:
		row.State = ui.LiveIncomplete
		if result.Err != nil {
			row.Detail += ": " + result.Err.Error()
		}
	case result.Passed():
		row.State = ui.LiveSucceeded
	default:
		row.State = ui.LiveFailed
		row.Detail += ": " + HealthMessages(result)
	}
	if result.Complete && !p.completed[index] {
		p.completed[index] = true
		p.rows[0].Current = new(big.Int).Add(p.rows[0].Current, big.NewInt(1))
	}
}

func (p *healthRoundPresentation) finish(report health.Report) {
	state := ui.LiveSucceeded
	for i, result := range report.Results {
		if i >= len(p.completed) {
			break
		}
		p.result(i, result)
		if !result.Complete {
			state = ui.LiveIncomplete
		} else if !result.Passed() && state != ui.LiveIncomplete {
			state = ui.LiveFailed
		}
	}
	p.rows[0].State = state
}

func healthWrite(out io.Writer, text string) error {
	n, err := io.WriteString(out, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

// Summaries follow input machine/check order even when workers finish in a
// different order. Interrupted or unstarted work is never reported as passed.
func healthSummary(report health.Report) (string, bool) {
	return healthSummaryText(report, func(text, _ string) string { return text })
}

func (e *Engine) healthSummary(out io.Writer, report health.Report) (string, bool) {
	return healthSummaryText(report, func(text, style string) string { return e.liveText(out, text, style) })
}

func healthSummaryText(report health.Report, style func(string, string) string) (string, bool) {
	passed, failed, incomplete := 0, 0, 0
	byMachine := map[string][]string{}
	var machineOrder []string
	for _, result := range report.Results {
		if result.Passed() {
			passed++
			continue
		}
		if !result.Complete {
			incomplete++
			continue
		}
		failed++
		name := result.Check.Machine
		if _, present := byMachine[name]; !present {
			machineOrder = append(machineOrder, name)
		}
		byMachine[name] = append(byMachine[name], HealthMessages(result))
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "%s, %s", style(fmt.Sprintf("%d checks passed", passed), "green"), style(fmt.Sprintf("%d failed", failed), "red"))
	if incomplete > 0 {
		fmt.Fprintf(&summary, ", %s", style(fmt.Sprintf("%d incomplete", incomplete), "yellow"))
	}
	summary.WriteString("\n\n")
	for _, machine := range machineOrder {
		fmt.Fprintf(&summary, "%s: %s\n", style(machine, "yellow"), style(fmt.Sprintf("%d failures", len(byMachine[machine])), "red"))
		for _, message := range byMachine[machine] {
			fmt.Fprintf(&summary, "  %s\n", style(message, "red"))
		}
		summary.WriteByte('\n')
	}
	return summary.String(), failed > 0
}
