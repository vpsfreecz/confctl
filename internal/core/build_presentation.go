package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/vpsfreecz/confctl/internal/build"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/ui"
)

// All three build entry points use the same invocation and presentation owner.
// A view failure cancels only this build, then Close returns its exact cause
// after the process callbacks have joined.
func (e *Engine) buildWithPresentation(hosts []string, options buildOptions, stdout, stderr io.Writer, deps buildDeps) (build.Outcome, error) {
	ctx, cancel := context.WithCancel(e.Context)
	defer cancel()
	invocation := *e
	invocation.Context = ctx
	return invocation.buildSelected(hosts, options, invocation.buildOutput(stdout, stderr, cancel), deps)
}

func (e *Engine) buildOutput(stdout, stderr io.Writer, cancel context.CancelFunc) buildPresentation {
	return e.buildOutputWithView(stdout, stderr, cancel, e.openLiveView)
}

// The opener seam exercises the production lifecycle without acquiring a TTY
// in callback tests. Production always binds the shared live-view owner.
func (e *Engine) buildOutputWithView(stdout, stderr io.Writer, cancel context.CancelFunc,
	open func(context.Context, io.Writer, io.Writer, liveViewSpec, func(error)) (liveView, error),
) buildPresentation {
	var view liveView
	var rows []ui.LiveRow
	rowIndex := func(id string) int {
		for i := range rows {
			if rows[i].ID == id {
				return i
			}
		}
		return -1
	}
	return buildPresentation{
		Evaluating: func(count int) error {
			return buildWrite(stdout, fmt.Sprintf("Evaluating inputs for %d machines...\n", count))
		},
		GroupsPrepared: func(groups []build.Group) error {
			return buildWrite(stdout, fmt.Sprintf("\nMachines will be built in %d groups\n\n", len(groups)))
		},
		GroupStart: func(index, count int, group build.Group) error {
			rows = []ui.LiveRow{
				{ID: "build", Label: "Building", State: ui.LivePending},
				{ID: "fetch", Label: "Fetching", State: ui.LivePending},
			}
			// Detailed context belongs to scrollback, never to a repaint or the
			// final view summary. Check this write before acquiring the view.
			var prelude strings.Builder
			prelude.WriteString(e.liveText(stdout, "Building machines", "bright") + "\n")
			inputs, commonInputs := group.Inputs.(generation.Object)
			commonInputs = commonInputs && len(inputs) > 0
			for _, host := range group.Hosts {
				prelude.WriteString("  " + e.liveText(stdout, host.Host, "yellow") + "\n")
				if !commonInputs {
					if inputs, ok := host.Inputs.(generation.Object); ok {
						prelude.WriteString("    with inputs\n")
						for _, input := range inputs {
							prelude.WriteString("      " + e.liveText(stdout, input.Name+"="+str(input.Value), "") + "\n")
						}
					}
				}
			}
			if commonInputs {
				prelude.WriteString("with inputs\n")
				for _, input := range inputs {
					prelude.WriteString("  " + e.liveText(stdout, input.Name+"="+str(input.Value), "") + "\n")
				}
			}
			if err := buildWrite(stdout, prelude.String()); err != nil {
				return err
			}
			header := e.liveHeader("Build group", fmt.Sprintf("%d/%d (%d machines)", index+1, count, len(group.Hosts)))
			var err error
			view, err = open(e.Context, stdout, stderr, liveViewSpec{Header: header, Rows: rows, RawLog: true}, func(error) {
				if cancel != nil {
					cancel()
				}
			})
			if err != nil {
				return err
			}
			if view == nil {
				model, err := ui.NewLiveModel(header, nil, e.liveColor(stdout))
				if err != nil {
					return err
				}
				return buildWrite(stdout, strings.Join(model.Header(), "\n")+"\n")
			}
			return nil
		},
		Progress: func(event build.Event) error {
			if view == nil {
				return buildWrite(stderr, fmt.Sprintf("%s %s/%s %s\n", event.Type, event.Progress, event.Total, event.Path))
			}
			index := 0
			switch event.Type {
			case "build":
			case "fetch":
				index = 1
			default:
				return fmt.Errorf("unknown build progress type %q", event.Type)
			}
			rows[index].Current = buildCount(event.Progress)
			// The existing parser's zero means no total was announced. Do not
			// present its observed starts as completed derivations or fake 0/0.
			rows[index].Total = nil
			if event.Total != nil && event.Total.Sign() > 0 {
				rows[index].Total = buildCount(event.Total)
			}
			rows[index].Detail = event.Path
			rows[index].State = ui.LiveRunning
			// An aggregate is justified only once both families have announced
			// totals. A missing family may still appear later in this group.
			aggregateIndex := rowIndex("observed")
			if rows[0].Current != nil && rows[1].Current != nil && rows[0].Total != nil && rows[1].Total != nil {
				aggregate := ui.LiveRow{ID: "observed", Label: "nix build", State: ui.LiveRunning,
					Current: new(big.Int).Add(rows[0].Current, rows[1].Current),
					Total:   new(big.Int).Add(rows[0].Total, rows[1].Total)}
				if aggregateIndex < 0 {
					rows = append(rows, aggregate)
				} else {
					rows[aggregateIndex] = aggregate
				}
			} else if aggregateIndex >= 0 {
				rows[aggregateIndex].Current = nil
				rows[aggregateIndex].Total = nil
				rows[aggregateIndex].Detail = "Total unknown"
			}
			return view.SetRows(rows...)
		},
		Command: func(event buildCommandEvent) error {
			// Pre-group evaluation and plain output keep their existing bytes.
			if view == nil {
				return nil
			}
			switch event.Op {
			case "build":
				for i := range rows {
					if rows[i].ID == "eval" {
						continue
					}
					if event.Stage == buildCommandStarted {
						// Every actual compatibility attempt owns fresh display
						// snapshots, just as it owns a fresh progress parser.
						rows[i].Current, rows[i].Total = nil, nil
						rows[i].Detail, rows[i].State = "", ui.LivePending
						if rows[i].ID == "observed" {
							// An admitted aggregate follows the child, even if
							// this attempt observes neither family again.
							rows[i].State = ui.LiveRunning
						}
					} else {
						buildFinishRow(&rows[i], event.Err)
					}
				}
			case "eval":
				index := rowIndex("eval")
				if index < 0 {
					rows = append(rows, ui.LiveRow{ID: "eval", Label: "nix eval"})
					index = len(rows) - 1
				}
				rows[index].Detail = strings.Join(event.Installables, " ")
				rows[index].State = ui.LiveRunning
				if event.Stage != buildCommandStarted {
					buildFinishRow(&rows[index], event.Err)
				}
			default:
				return fmt.Errorf("unknown build command %q", event.Op)
			}
			return view.SetRows(rows...)
		},
		Close: func(bodyErr error) error {
			if view == nil {
				return nil
			}
			current := view
			view = nil
			err := current.Err()
			if err == nil {
				for i := range rows {
					buildFinishRow(&rows[i], bodyErr)
				}
				err = current.SetRows(rows...)
			}
			closeErr := current.Close() // Always drain and join, even after failure.
			if err != nil {
				return err
			}
			return closeErr
		},
	}
}

// Only unresolved work follows the enclosing group outcome. Child completion
// remains authoritative when a later decode/evaluation/generation step fails.
func buildFinishRow(row *ui.LiveRow, err error) {
	if row.State != ui.LivePending && row.State != ui.LiveRunning {
		return
	}
	switch {
	case err == nil && row.State == ui.LivePending:
		row.State, row.Detail = ui.LiveSkipped, "No steps observed"
	case err == nil:
		row.State = ui.LiveSucceeded
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), row.State == ui.LivePending:
		row.State = ui.LiveIncomplete
	default:
		row.State = ui.LiveFailed
	}
}

func buildCount(value *big.Int) *big.Int {
	if value == nil {
		return nil
	}
	return new(big.Int).Set(value)
}
