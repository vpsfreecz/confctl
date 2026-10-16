package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/health"
	"github.com/vpsfreecz/confctl/internal/ui"
)

// One invocation owns this coordinator. Bulk copies borrow one view; all other
// scopes are sequential. Error notification never takes the presentation lock.
type deployPresentation struct {
	engine         *Engine
	stdout, stderr io.Writer
	cancel         context.CancelFunc
	action         deploy.Action
	mu             sync.Mutex
	view           liveView
	rows           map[string]ui.LiveRow
	order          []string
	raw            bool
	bulk           bool
	errMu          sync.Mutex
	err            error
}

func (p *deployPresentation) initialize(_ context.Context, stdout, stderr io.Writer, cancel context.CancelFunc) {
	p.stdout, p.stderr, p.cancel = stdout, stderr, cancel
}

func (p *deployPresentation) fail(err error) {
	if err == nil {
		return
	}
	p.errMu.Lock()
	first := p.err == nil
	if first {
		p.err = err
	}
	p.errMu.Unlock()
	if first && p.cancel != nil {
		p.cancel()
	}
}

func (p *deployPresentation) result(body error) error {
	// Preserve independent process/log causes; cancellation caused by a view
	// failure must expose the original I/O error after all work has joined.
	if body != nil && !errors.Is(body, context.Canceled) {
		return body
	}
	p.errMu.Lock()
	err := p.err
	p.errMu.Unlock()
	if err != nil {
		return err
	}
	return body
}

func (p *deployPresentation) header(operation, target, action, generation string) ui.LiveHeader {
	header := p.engine.liveHeader(operation, "")
	header.Target, header.Action, header.Generation = target, action, generation
	return header
}

func (p *deployPresentation) scope(ctx context.Context, spec liveViewSpec, body func() error) error {
	view, err := p.engine.openLiveView(ctx, p.stdout, p.stderr, spec, p.fail)
	if err != nil {
		return err
	}
	if view == nil {
		return body()
	}
	p.mu.Lock()
	p.view, p.raw = view, spec.RawLog
	p.rows, p.order = make(map[string]ui.LiveRow, len(spec.Rows)), nil
	for _, row := range spec.Rows {
		p.rows[row.ID] = row
		p.order = append(p.order, row.ID)
	}
	p.mu.Unlock()
	bodyErr := p.result(nil)
	if bodyErr == nil {
		bodyErr = body() // The existing wrapper joins all children/callbacks here.
	}
	p.mu.Lock()
	for _, id := range p.order {
		row := p.rows[id]
		if row.State == ui.LivePending || row.State == ui.LiveRunning {
			// Scope completion does not prove a pending operation ran. Actual
			// callbacks, never a finish event, establish terminal success.
			row.State = ui.LiveIncomplete
			if bodyErr == nil && id == "copy:all" && len(p.order) == 1 {
				row.State, row.Detail = ui.LiveSkipped, "No copies queued"
			}
			p.rows[id] = row
		}
	}
	p.publishLocked()
	p.mu.Unlock()
	if bodyErr == nil {
		p.fail(view.Flush(ctx))
	}
	closeErr := view.Close()
	p.mu.Lock()
	p.view, p.rows, p.order = nil, nil, nil
	p.mu.Unlock()
	if err := p.result(bodyErr); err != nil {
		return err
	}
	return closeErr
}

func (p *deployPresentation) publishLocked() {
	rows := make([]ui.LiveRow, 0, len(p.order))
	for _, id := range p.order {
		rows = append(rows, p.rows[id])
	}
	p.fail(p.view.SetRows(rows...))
}

func (p *deployPresentation) update(id string, change func(*ui.LiveRow)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.view == nil {
		return
	}
	row, exists := p.rows[id]
	if !exists {
		return
	}
	change(&row)
	p.rows[id] = row
	if strings.HasPrefix(id, "copy:") && id != "copy:all" {
		p.copyAggregateLocked()
	}
	p.publishLocked()
}

func (p *deployPresentation) copyAggregateLocked() {
	aggregate, ok := p.rows["copy:all"]
	if !ok {
		return
	}
	current, total := new(big.Int), new(big.Int)
	known, finished, failed := true, true, false
	for _, id := range p.order {
		if id == "copy:all" {
			continue
		}
		row := p.rows[id]
		if row.Current != nil {
			current.Add(current, row.Current)
		}
		if row.Total == nil {
			known = false
		} else {
			total.Add(total, row.Total)
		}
		finished = finished && row.State != ui.LivePending && row.State != ui.LiveRunning
		failed = failed || row.State == ui.LiveFailed || row.State == ui.LiveIncomplete
	}
	aggregate.Current, aggregate.Total, aggregate.State = current, nil, ui.LiveRunning
	if known {
		aggregate.Total = total
	}
	if finished {
		aggregate.State = ui.LiveSucceeded
		if failed {
			aggregate.State = ui.LiveFailed
		}
	}
	p.rows[aggregate.ID] = aggregate
}

func deployLiveState(ok bool, err error) ui.LiveState {
	if err != nil {
		return ui.LiveIncomplete
	}
	if !ok {
		return ui.LiveFailed
	}
	return ui.LiveSucceeded
}

func deployCopyCount(value *big.Int) *big.Int {
	if value == nil {
		return nil
	}
	return new(big.Int).Set(value)
}

func copyLiveRows(targets []deploy.Target) []ui.LiveRow {
	rows := []ui.LiveRow{{ID: "copy:all", Label: "All hosts", Current: new(big.Int), State: ui.LivePending}}
	for _, target := range targets {
		rows = append(rows, ui.LiveRow{ID: "copy:" + target.Host, Label: target.Host, Target: true, Current: new(big.Int), State: ui.LivePending})
	}
	return rows
}

func (p *deployPresentation) copy(ctx context.Context, m deploy.Machine, run func() (bool, error)) (bool, error) {
	p.mu.Lock()
	borrowed := p.bulk || p.view != nil
	p.mu.Unlock()
	var ok bool
	var copyErr error
	body := func() error {
		p.update("copy:"+m.Name, func(row *ui.LiveRow) { row.State = ui.LiveRunning })
		if err := p.result(ctx.Err()); err != nil {
			return err
		}
		ok, copyErr = run()
		p.update("copy:"+m.Name, func(row *ui.LiveRow) {
			row.State = deployLiveState(ok, copyErr)
			if m.Localhost && ok && copyErr == nil {
				row.State, row.Detail = ui.LiveSkipped, "Already local"
			}
		})
		return copyErr
	}
	if borrowed {
		copyErr = body()
	} else {
		copyErr = p.scope(ctx, liveViewSpec{Header: p.header("Copying closures", m.Name, "", ""), Rows: copyLiveRows([]deploy.Target{{Host: m.Name}})}, body)
	}
	return ok, copyErr
}

func (p *deployPresentation) event(event deploy.Event) {
	if p.result(nil) != nil {
		return
	}
	p.mu.Lock()
	view, raw := p.view, p.raw
	p.mu.Unlock()
	if view == nil {
		line := fmt.Sprintf("%s: %s", event.Host, event.Kind)
		if event.Progress != nil {
			if event.Progress.Current != nil {
				line += " " + event.Progress.Current.String()
				if event.Progress.Total != nil {
					line += "/" + event.Progress.Total.String()
				}
			}
			line += " " + event.Progress.Path
		}
		p.fail(buildWrite(p.stdout, line+"\n"))
		return
	}
	if event.Kind == deploy.CopyAdvanced && event.Progress != nil {
		progress := *event.Progress
		p.update("copy:"+event.Host, func(row *ui.LiveRow) {
			row.Current, row.Total, row.Detail = deployCopyCount(progress.Current), deployCopyCount(progress.Total), progress.Path
		})
	}
	detail := map[deploy.EventKind]string{
		deploy.DryStarted: "Dry activation", deploy.ActivationAdmitted: "Awaiting activation",
		deploy.ActivationStarted: "Activating", deploy.ProfileStarted: "Updating profile",
		deploy.ActivationSkipped: "Skipped", deploy.ActivationNoop: "Carried host",
	}[event.Kind]
	if detail != "" {
		p.update("activation:"+event.Host, func(row *ui.LiveRow) {
			row.Detail = detail
			if event.Kind == deploy.ActivationSkipped || event.Kind == deploy.ActivationNoop {
				row.State = ui.LiveSkipped
			} else {
				row.State = ui.LiveRunning
			}
		})
	}
	if !raw {
		line := fmt.Sprintf("%s: %s", event.Host, event.Kind)
		if event.Progress != nil {
			line += " " + event.Progress.Path
		}
		p.fail(view.Line(line))
	}
}

func (p *deployPresentation) prompt(ctx context.Context, body func() error) error {
	if err := p.result(ctx.Err()); err != nil {
		return err
	}
	p.mu.Lock()
	view := p.view
	p.mu.Unlock()
	if view == nil {
		return body()
	}
	return view.WithPrompt(ctx, body)
}

func (p *deployPresentation) decorate(d deploy.Deps) deploy.Deps {
	previousEvent := d.Event
	d.Event = func(event deploy.Event) {
		if previousEvent != nil {
			previousEvent(event)
		}
		p.event(event)
	}
	copyCall := d.Copy
	if copyCall != nil {
		d.Copy = func(ctx context.Context, m deploy.Machine, paths []string, progress func(deploy.CopyProgress)) (bool, error) {
			return p.copy(ctx, m, func() (bool, error) { return copyCall(ctx, m, paths, progress) })
		}
	}
	bulk := d.WithBulkCopy
	d.WithBulkCopy = func(ctx context.Context, targets []deploy.Target, body func() error) error {
		p.mu.Lock()
		p.bulk = true
		p.mu.Unlock()
		defer func() { p.mu.Lock(); p.bulk = false; p.mu.Unlock() }()
		return p.scope(ctx, liveViewSpec{Header: p.header("Copying closures", "", "", ""), Rows: copyLiveRows(targets)}, func() error {
			if bulk != nil {
				return bulk(ctx, targets, body)
			}
			return body()
		})
	}
	activation := d.WithActivation
	d.WithActivation = func(ctx context.Context, target deploy.Target, body func() error) error {
		return p.scope(ctx, liveViewSpec{Header: p.header("Activating", target.Host, string(p.action), target.Generation.Name), RawLog: true, Rows: []ui.LiveRow{{ID: "activation:" + target.Host, Label: target.Host, Target: true, State: ui.LivePending}}}, func() error {
			if activation != nil {
				return activation(ctx, target, body)
			}
			return body()
		})
	}
	wrapAction := func(call func(context.Context, deploy.Machine, *generation.Build, deploy.Action) (bool, error)) func(context.Context, deploy.Machine, *generation.Build, deploy.Action) (bool, error) {
		if call == nil {
			return nil
		}
		return func(ctx context.Context, m deploy.Machine, g *generation.Build, action deploy.Action) (bool, error) {
			if err := p.result(ctx.Err()); err != nil {
				return false, err
			}
			ok, err := call(ctx, m, g, action)
			p.update("activation:"+m.Name, func(row *ui.LiveRow) { row.State = deployLiveState(ok, err); row.Detail = string(action) })
			return ok, err
		}
	}
	d.Activate, d.ActivateWithRollback = wrapAction(d.Activate), wrapAction(d.ActivateWithRollback)
	wrapProfile := func(call func(context.Context, deploy.Machine, string) (bool, error)) func(context.Context, deploy.Machine, string) (bool, error) {
		if call == nil {
			return nil
		}
		return func(ctx context.Context, m deploy.Machine, top string) (bool, error) {
			if err := p.result(ctx.Err()); err != nil {
				return false, err
			}
			ok, err := call(ctx, m, top)
			p.update("activation:"+m.Name, func(row *ui.LiveRow) { row.State = deployLiveState(ok, err); row.Detail = "Profile" })
			return ok, err
		}
	}
	d.SetProfile, d.SetCarriedProfile = wrapProfile(d.SetProfile), wrapProfile(d.SetCarriedProfile)
	confirm := d.Confirm
	if confirm != nil {
		d.Confirm = func(ctx context.Context, c deploy.Confirmation) (ok bool, err error) {
			err = p.prompt(ctx, func() error { var err error; ok, err = confirm(ctx, c); return err })
			return
		}
	}
	chooseActivation := d.ChooseActivation
	if chooseActivation != nil {
		d.ChooseActivation = func(ctx context.Context, t deploy.Target, retry bool) (choice deploy.ActivationChoice, err error) {
			err = p.prompt(ctx, func() error { var err error; choice, err = chooseActivation(ctx, t, retry); return err })
			return
		}
	}
	chooseHealth := d.ChooseHealth
	if chooseHealth != nil {
		d.ChooseHealth = func(ctx context.Context, ms []deploy.Machine, report health.Report) (choice deploy.HealthChoice, err error) {
			err = p.prompt(ctx, func() error { var err error; choice, err = chooseHealth(ctx, ms, report); return err })
			return
		}
	}
	return d
}

func (p *deployPresentation) reboot(ctx context.Context, m Machine, wait deploy.WaitMode, ops deployProcessOps) error {
	if wait.Kind == deploy.NoWait {
		return deployReboot(ctx, m, wait, ops, nil)
	}
	return p.scope(ctx, liveViewSpec{Header: p.header("Rebooting", m.Name, string(p.action), ""), Rows: []ui.LiveRow{{ID: "reboot:" + m.Name, Label: m.Name, Target: true, State: ui.LiveRunning}}}, func() error {
		err := deployReboot(ctx, m, wait, ops, func(progress rebootProgress) error {
			detail := progress.State + " (elapsed " + progress.Elapsed.FloatString(1) + "s"
			if progress.Remaining != nil {
				detail += ", remaining " + progress.Remaining.FloatString(1) + "s"
			}
			detail += ")"
			p.update("reboot:"+m.Name, func(row *ui.LiveRow) { row.Detail = detail })
			p.mu.Lock()
			textual := p.view == nil
			p.mu.Unlock()
			if textual {
				p.fail(buildWrite(p.stdout, m.Name+": "+detail+"\n"))
			}
			return p.result(nil)
		})
		p.update("reboot:"+m.Name, func(row *ui.LiveRow) { row.State = deployLiveState(err == nil, err) })
		return err
	})
}
