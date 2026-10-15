package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/health"
)

// HealthCheck supplies the standalone command adapter. The shared CLI owner
// must retain the invocation log when RetainLog is true, including on success.
// Human progress uses the same serialized lines on terminals and redirected
// output; it does not own a terminal or alter machine-consumed protocols.
func (e *Engine) HealthCheck(inv cli.Invocation, in io.Reader, out io.Writer) (HealthResult, error) {
	return e.healthCheck(inv, in, out, health.Deps{}, e.healthProcess)
}

// LogHealthCLI records the existing parsed options without adding another
// option catalogue. Shared dispatch calls it after acquiring the command log.
func (e *Engine) LogHealthCLI(inv cli.Invocation) error {
	data, err := json.Marshal(struct {
		Command []string       `json:"command"`
		Options map[string]any `json:"options"`
		Args    []string       `json:"arguments"`
	}{inv.Command.Path, invocationOptions(inv), inv.Args})
	if err != nil {
		return err
	}
	return e.logMetadata(string(data) + "\n")
}

func (e *Engine) healthCheck(inv cli.Invocation, in io.Reader, out io.Writer, deps health.Deps, process func(context.Context, string, []string, []byte) (Result, error)) (HealthResult, error) {
	request := HealthRequest{
		Attrs: inv.Options["attr"].Value.Strings,
		Tags:  inv.Options["tag"].Value.Strings,
	}
	if len(inv.Args) > 0 {
		request.Pattern = &inv.Args[0]
	}
	if workers, present := inv.Options["max-jobs"]; present && workers.Value.Kind == cli.Number {
		request.Workers = new(big.Int).Set(&workers.Value.Integer)
	}
	plan, err := e.PlanHealth(request)
	if err != nil {
		return HealthResult{}, err
	}
	if !inv.Options["yes"].Value.Bool {
		table, err := e.healthConfirmationTable(plan)
		if err != nil {
			return HealthResult{}, err
		}
		if err := healthWrite(out, "Health checks will be run on the following machines:\n"+table+fmt.Sprintf("\n%d checks in total\n", len(plan.Checks))); err != nil {
			return HealthResult{}, err
		}
		if err := testConnectionConfirm(in, out); err != nil {
			return HealthResult{}, err
		}
	}
	emit := func(text string) error {
		if err := healthWrite(out, text); err != nil {
			return err
		}
		return e.logText(text)
	}
	header := fmt.Sprintf("Running %d health checks on %d machines\n", len(plan.Checks), len(plan.Machines))
	if e.Log != nil {
		header += "Full log: " + e.Log.Name() + "\n"
	}
	if err := emit(header); err != nil {
		return HealthResult{}, err
	}
	ctx, cancel := context.WithCancel(e.Context)
	defer cancel()
	runEngine := *e
	runEngine.Context = ctx
	var progressErr error
	previousProgress := deps.Progress
	deps.Progress = func(event health.Event) {
		if progressErr != nil {
			return
		}
		if previousProgress != nil {
			previousProgress(event)
		}
		prefix := fmt.Sprintf("[%d/%d] %s> ", event.Index+1, len(plan.Checks), event.Result.Check.Machine)
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
		if progressErr = emit(prefix + text + "\n"); progressErr != nil {
			cancel()
		}
	}
	result, runErr := runEngine.runHealth(plan, deps, process)
	if progressErr != nil {
		return result, progressErr
	}
	summary, failed := healthSummary(result.Report)
	if err := emit(summary); err != nil {
		return result, errors.Join(runErr, err)
	}
	if runErr != nil {
		return result, runErr
	}
	if failed {
		return result, fmt.Errorf("Health checks failed")
	}
	return result, nil
}

func healthWrite(out io.Writer, text string) error {
	n, err := io.WriteString(out, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

func (e *Engine) healthConfirmationTable(plan HealthPlan) (string, error) {
	settings, err := e.Settings()
	if err != nil {
		return "", err
	}
	columns := []string{"checks"}
	configured, _ := nested(settings, "list", "columns").([]any)
	for _, column := range configured {
		columns = append(columns, str(column))
	}
	counts := make(map[string]int, len(plan.Machines))
	for _, check := range plan.Checks {
		counts[check.Machine]++
	}
	rows := make([]map[string]any, 0, len(plan.Machines))
	for _, machine := range plan.Machines {
		row := make(map[string]any, len(columns))
		for _, column := range columns {
			row[column] = machine.Attr(column)
		}
		row["checks"] = counts[machine.Name]
		rows = append(rows, row)
	}
	return Table(rows, columns, true), nil
}

// Summaries follow input machine/check order even when workers finish in a
// different order. Interrupted or unstarted work is never reported as passed.
func healthSummary(report health.Report) (string, bool) {
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
	fmt.Fprintf(&summary, "%d checks passed, %d failed", passed, failed)
	if incomplete > 0 {
		fmt.Fprintf(&summary, ", %d incomplete", incomplete)
	}
	summary.WriteString("\n\n")
	for _, machine := range machineOrder {
		fmt.Fprintf(&summary, "%s: %d failures\n", machine, len(byMachine[machine]))
		for _, message := range byMachine[machine] {
			fmt.Fprintf(&summary, "  %s\n", message)
		}
		summary.WriteByte('\n')
	}
	return summary.String(), failed > 0
}
