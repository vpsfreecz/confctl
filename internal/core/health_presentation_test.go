package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/vpsfreecz/confctl/internal/health"
	"github.com/vpsfreecz/confctl/internal/ui"
)

func TestHealthRoundPresentationAttemptsCompletionAndIncomplete(t *testing.T) {
	checks := []health.Check{{Machine: "z", Description: "first"}, {Machine: "a", Description: "second"}, {Machine: "z", Description: "third"}}
	p := newHealthRoundPresentation(checks)
	if p.rows[0].Current.Sign() != 0 || p.rows[0].Total.Int64() != 3 || p.rows[1].ID == p.rows[3].ID || p.rows[1].Label != "z" || !p.rows[1].Target || p.rows[1].Detail != "first: pending" {
		t.Fatal(p.rows)
	}
	oldCount := p.rows[0].Current
	result := health.Result{Check: checks[1], Started: true}
	if err := p.event(health.Event{Index: 1, Phase: "start", Result: result}); err != nil {
		t.Fatal(err)
	}
	result.Attempts = 1
	result.HadFailure = true
	if err := p.event(health.Event{Index: 1, Phase: "failed", Result: result}); err != nil {
		t.Fatal(err)
	}
	if p.rows[2].State != ui.LiveRunning || !strings.Contains(p.rows[2].Detail, "failed #1") || p.rows[0].Current.Sign() != 0 {
		t.Fatal("a failed attempt completed the round", p.rows)
	}
	result.Attempts, result.Complete = 2, true
	for i := 0; i < 2; i++ {
		if err := p.event(health.Event{Index: 1, Phase: "done", Result: result}); err != nil {
			t.Fatal(err)
		}
	}
	if p.rows[2].State != ui.LiveSucceeded || p.rows[2].Detail != "second: 2 attempts" || p.rows[0].Current.Int64() != 1 || oldCount.Sign() != 0 {
		t.Fatal("completion was duplicated or mutated an earlier count snapshot", p.rows)
	}
	report := health.Report{Results: []health.Result{{Check: checks[0], Started: true, Attempts: 1, Err: context.Canceled}, result, {Check: checks[2]}}}
	p.finish(report)
	if p.rows[0].State != ui.LiveIncomplete || p.rows[0].Current.Int64() != 1 || p.rows[1].State != ui.LiveIncomplete || p.rows[3].State != ui.LiveIncomplete {
		t.Fatal("interrupted/unstarted work became complete", p.rows)
	}
	if p.rows[1].Label != "z" || p.rows[2].Label != "a" || p.rows[3].Label != "z" || !strings.HasPrefix(p.rows[1].Detail, "first:") || !strings.HasPrefix(p.rows[2].Detail, "second:") || !strings.HasPrefix(p.rows[3].Detail, "third:") {
		t.Fatal("completion order replaced input order", p.rows)
	}
	if err := p.event(health.Event{Index: -1}); err == nil {
		t.Fatal("invalid index admitted")
	}
}

func TestHealthPresentationCompactHeaderAndFrame(t *testing.T) {
	e := &Engine{Root: t.TempDir(), Context: context.Background(), liveCommand: "confctl health-check --yes"}
	if err := e.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.CloseLog(false) })
	checks := make([]health.Check, 10)
	for i := range checks {
		checks[i] = health.Check{Machine: "solo", Description: "check description"}
	}
	plan := HealthPlan{Machines: []Machine{{Name: "solo"}}, Checks: checks}
	header := e.healthHeader(plan)
	if header.Target != "solo" || header.Command != e.liveCommand || header.Detail != "10 checks on" || header.LogPath != e.Log.Name() {
		t.Fatal(header)
	}
	multi := e.healthHeader(HealthPlan{Machines: []Machine{{Name: "one"}, {Name: "two"}}, Checks: checks})
	if multi.Target != "" || multi.Detail != "10 checks on 2 machines" {
		t.Fatal("multi-machine header lost the admitted round count", multi)
	}
	p := newHealthRoundPresentation(checks)
	p.result(0, health.Result{Check: checks[0], Started: true, Complete: true, Attempts: 1})
	model, err := ui.NewLiveModel(header, p.rows, true)
	if err != nil {
		t.Fatal(err)
	}
	lines := model.Header()
	if len(lines) != 3 || ansi.Strip(lines[1]) != "Health checks: 10 checks on solo" || !strings.Contains(lines[1], "\x1b[33msolo\x1b[0m") {
		t.Fatal("health header is not compact with a styled target", lines)
	}
	for i := 0; i < 20; i++ {
		model.Line("\x1b[35mCHECK_OUTPUT\x1b[0m")
	}
	frame := model.Frame(24, 80, 0)
	title, bottom, progress := -1, -1, -1
	progressRow := regexp.MustCompile("^" + regexp.QuoteMeta("Health checks") + ` +\[running[|/\\-]\]`)
	for i, line := range frame {
		plain := ansi.Strip(line)
		if ansi.StringWidth(line) > 80 {
			t.Fatal("health frame exceeds terminal width", line)
		}
		if strings.Contains(plain, "Live view") {
			title = i
		}
		if strings.HasPrefix(plain, "<---") {
			bottom = i
		}
		if progressRow.MatchString(plain) {
			progress = i
		}
		if strings.Contains(plain, "CHECK_OUTPUT") && strings.Contains(line, "\x1b[") {
			t.Fatal("child semantic output acquired styling", line)
		}
	}
	if len(frame) > 23 || title < 0 || bottom-title-1 < 12 || progress <= bottom || len(frame)-bottom-1 > 6 || !strings.Contains(ansi.Strip(strings.Join(frame, "\n")), "6 more tasks") {
		t.Fatal("health frame lost log-before-progress layout or overflow", frame)
	}
	if !strings.Contains(strings.Join(model.Summary(0), "\n"), "\x1b[33msolo\x1b[0m") || !strings.Contains(strings.Join(model.Summary(0), "\n"), "check description") {
		t.Fatal("target styling swallowed the plain check detail", model.Summary(0))
	}
}

func TestHealthPresentationSummaryPaletteAndPlainLog(t *testing.T) {
	failedCheck := health.Check{Machine: "target", Description: "check"}
	report := health.Report{Results: []health.Result{
		{Check: health.Check{Machine: "passed"}, Complete: true},
		{Check: failedCheck, Complete: true, Failures: []health.Failure{{Kind: "missing-property", Property: "Ready"}}},
		{Check: health.Check{Machine: "interrupted"}},
	}}
	plain, failed := healthSummary(report)
	if !failed || strings.Contains(plain, "\x1b") {
		t.Fatal(plain, failed)
	}
	t.Setenv("NO_COLOR", "1")
	for _, policy := range []string{"always", "never", "auto", ""} {
		t.Run(policy, func(t *testing.T) {
			e := &Engine{Color: policy}
			var out bytes.Buffer
			styled, gotFailed := e.healthSummary(&out, report)
			if gotFailed != failed || ansi.Strip(styled) != plain {
				t.Fatal("summary styling changed values or order", styled, plain)
			}
			if policy == "always" {
				for _, want := range []string{"\x1b[32m1 checks passed\x1b[0m", "\x1b[31m1 failed\x1b[0m", "\x1b[33m1 incomplete\x1b[0m", "\x1b[33mtarget\x1b[0m"} {
					if !strings.Contains(styled, want) {
						t.Fatal("missing semantic summary style", want, styled)
					}
				}
			} else if styled != plain {
				t.Fatal("plain/NO_COLOR/redirected summary acquired styling", policy, styled)
			}
		})
	}
	t.Run("sanitize-before-style", func(t *testing.T) {
		e := &Engine{Color: "always"}
		unsafe := health.Report{Results: []health.Result{{Check: health.Check{Machine: "tar\x1b[35mget\x1b[0m"}, Complete: true, Failures: []health.Failure{{Kind: "missing-property", Property: "Ready"}}}}}
		styled, _ := e.healthSummary(io.Discard, unsafe)
		if strings.Contains(styled, "\x1b[35m") || !strings.Contains(styled, "\x1b[33mtarget\x1b[0m") {
			t.Fatal("source control text became presentation styling", styled)
		}
	})
	t.Run("production-round-log-stays-plain", func(t *testing.T) {
		e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
		e.Color = "always"
		if err := e.OpenLog("health-check"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.CloseLog(false) })
		plan, err := e.PlanHealth(HealthRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		_, err = e.runHealthPresented(plan, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) { return Result{}, nil }, &out, io.Discard)
		if err != nil || !strings.Contains(out.String(), "\x1b[32m1 checks passed\x1b[0m") {
			t.Fatal("styled round summary missing", err, out.String())
		}
		log, err := os.ReadFile(e.Log.Name())
		if err != nil || bytes.Contains(log, []byte("\x1b")) || !bytes.Contains(log, []byte("1 checks passed, 0 failed")) {
			t.Fatal("presentation styles changed the durable semantic log", err, string(log))
		}
	})
}

func TestHealthRoundPresentationRetrySubsetAndFailure(t *testing.T) {
	check := health.Check{Machine: "only-failed", Description: "retry"}
	p := newHealthRoundPresentation([]health.Check{check})
	failed := health.Result{Check: check, Complete: true, Started: true, Attempts: 3, Failures: []health.Failure{{Kind: "missing-property", Property: "Ready"}}}
	p.finish(health.Report{Results: []health.Result{failed}})
	if p.rows[0].Total.Int64() != 1 || p.rows[0].Current.Int64() != 1 || p.rows[0].State != ui.LiveFailed || p.rows[1].State != ui.LiveFailed || !strings.Contains(p.rows[1].Detail, "3 attempts") {
		t.Fatal("subset total or failed completion changed", p.rows)
	}
	if p.rows[1].Current != nil || p.rows[1].Total != nil {
		t.Fatal("an attempt was presented as a completion counter")
	}
}

func TestHealthPresentedTextRetainsEventsRetryAndSummary(t *testing.T) {
	e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
	plan, err := e.PlanHealth(HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.CloseLog(false) })
	attempts, cooldowns := 0, 0
	var events []string
	deps := health.Deps{Now: func() time.Time { return time.Unix(0, 0) }, Cooldown: func(context.Context, *big.Int) error { cooldowns++; return nil }, Progress: func(event health.Event) { events = append(events, event.Phase) }}
	var out bytes.Buffer
	result, err := e.runHealthPresented(plan, deps, func(ctx context.Context, _ string, _ []string, _ []byte) (Result, error) {
		if ctx == e.Context {
			t.Fatal("round did not own a derived cancellation context")
		}
		attempts++
		if attempts == 1 {
			return Result{ExitCode: 7}, nil
		}
		return Result{}, nil
	}, &out, io.Discard)
	if err != nil || attempts != 2 || cooldowns != 1 || !result.RetainLog || !result.Report.Results[0].Passed() || !reflect.DeepEqual(events, []string{"start", "failed", "done"}) {
		t.Fatal(result, err, attempts, cooldowns, events)
	}
	text := out.String()
	for _, want := range []string{"Running 1 health checks on 1 machines", "Full log:", "[1/1] m> fixture", "failed #1", "succeeded", "1 checks passed, 0 failed"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing semantic output", want, text)
		}
	}
	if strings.Contains(text, "\x1b") || strings.Index(text, "succeeded") >= strings.Index(text, "1 checks passed") {
		t.Fatal("text fallback or final ordering changed", text)
	}
}

type healthPresentationFailWriter struct {
	bytes.Buffer
	needle string
	err    error
}

func (w *healthPresentationFailWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(w.needle)) {
		return 0, w.err
	}
	return w.Buffer.Write(p)
}

func (w *healthPresentationFailWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestHealthPresentedFailureCancelsRetryAndKeepsPrimaryError(t *testing.T) {
	e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
	plan, err := e.PlanHealth(HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	outputErr := errors.New("semantic output failed")
	out := &healthPresentationFailWriter{needle: "failed #1", err: outputErr}
	calls := 0
	result, err := e.runHealthPresented(plan, health.Deps{Cooldown: func(ctx context.Context, _ *big.Int) error { return ctx.Err() }}, func(context.Context, string, []string, []byte) (Result, error) {
		calls++
		return Result{ExitCode: 7}, nil
	}, out, io.Discard)
	if !errors.Is(err, outputErr) || calls != 1 || !result.RetainLog || result.Report.Results[0].Complete || strings.Contains(out.String(), "checks passed") {
		t.Fatal("progress failure retried, changed retention or became cancellation", result, err, calls, out.String())
	}
}

func TestHealthPresentedBodyAndSummaryErrorsRemainObservable(t *testing.T) {
	e := healthCLIEngine(t, healthMachineJSON("m", "nixos", `{"builderCommands":[`+healthImmediateCommand("fixture")+`]}`))
	plan, err := e.PlanHealth(HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	bodyErr, outputErr := errors.New("check executor failed"), errors.New("summary output failed")
	out := &healthPresentationFailWriter{needle: "incomplete", err: outputErr}
	result, err := e.runHealthPresented(plan, health.Deps{}, func(context.Context, string, []string, []byte) (Result, error) { return Result{}, bodyErr }, out, io.Discard)
	if !errors.Is(err, bodyErr) || !errors.Is(err, outputErr) || result.Report.Results[0].Complete {
		t.Fatal(result, err)
	}
}

func TestHealthPresentedCloseFailureKeepsBodyAndProgressPrecedence(t *testing.T) {
	bodyErr := errors.New("health executor failed")
	progressErr := errors.New("health progress output failed")
	closeErr := errors.New("health final frame failed")
	for _, tc := range []struct {
		name         string
		body, before error
		late, close  error
		want         []error
		absent       error
	}{
		{name: "body-only-still-emits-summary", body: bodyErr},
		{name: "close-callback-retains-body", body: bodyErr, late: closeErr, close: closeErr, want: []error{bodyErr, closeErr}},
		{name: "close-error-without-callback", body: bodyErr, close: closeErr, want: []error{bodyErr, closeErr}},
		{name: "prior-progress-caused-cancellation", body: context.Canceled, before: progressErr, late: progressErr, close: closeErr, want: []error{progressErr, closeErr}, absent: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := tc.before
			closed := 0
			err := healthClosePresentedRound(tc.body, func() error { return failure }, func() error {
				closed++
				failure = tc.late
				return tc.close
			})
			if closed != 1 || len(tc.want) == 0 && err != nil {
				t.Fatal("close skipped/repeated or body prevented summary", closed, err)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatal("body or teardown cause lost", want, err)
				}
			}
			if tc.absent != nil && errors.Is(err, tc.absent) {
				t.Fatal("cancellation replaced the initiating progress error", err)
			}
		})
	}
}
