package core

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/health"
)

// HealthCheck supplies the standalone command adapter. The shared CLI owner
// must retain the invocation log when RetainLog is true, including on success.
// Live and textual output share one serialized round presentation.
func (e *Engine) HealthCheck(inv cli.Invocation, in io.Reader, out, stderr io.Writer) (HealthResult, error) {
	return e.healthCheck(inv, in, out, stderr, health.Deps{}, e.healthProcess)
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

func (e *Engine) healthCheck(inv cli.Invocation, in io.Reader, out, stderr io.Writer, deps health.Deps, process healthProcessFunc) (HealthResult, error) {
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
		_, line := commandInput(in)
		if err := testConnectionConfirm(e.Context, line, out); err != nil {
			return HealthResult{}, err
		}
	}
	result, runErr := e.runHealthPresented(plan, deps, process, out, stderr)
	if runErr != nil {
		return result, runErr
	}
	_, failed := healthSummary(result.Report)
	if failed {
		return result, fmt.Errorf("Health checks failed")
	}
	return result, nil
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
