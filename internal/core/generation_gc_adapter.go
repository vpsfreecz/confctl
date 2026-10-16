package core

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/ui"
)

type generationGCStreamRun func(context.Context, string, []string, []byte, func([]byte, []byte) error) (Result, error)

func (e *Engine) generationGCDependencies(stdout, stderr io.Writer) gcDependencies {
	return e.generationGCAdapter(stdout, stderr, func(ctx context.Context, root string, argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
		// Only the task context changes. Keep the checked log owner with Log;
		// processStreams owns the single child, capture, callback and Wait.
		task := &Engine{Root: root, Context: ctx, Log: e.Log, logOwner: e.logOwner}
		return task.processStreams(argv, input, nil, chunk)
	})
}

func (e *Engine) generationGCAdapter(stdout, stderr io.Writer, run generationGCStreamRun) gcDependencies {
	return gcDependencies{
		Prepare: func(context.Context) (gcExecute, error) {
			if err := e.required(); err != nil {
				return nil, err
			}
			if _, err := e.Settings(); err != nil {
				return nil, err
			}
			return func(ctx context.Context, machine Machine, command []string, feed func([]byte, []byte)) (Result, error) {
				argv, err := SSHArgs(machine, command)
				if err != nil {
					return Result{}, err
				}
				return run(ctx, e.Root, argv, nil, func(stdout, stderr []byte) error {
					feed(stdout, stderr)
					return nil // feed errors cancel the task through its GC owner.
				})
			}, nil
		},
		Open: func(ctx context.Context, spec gcViewSpec) (gcView, bool, error) {
			header := e.liveHeader("Collecting garbage", "")
			if spec.Count > 1 {
				header.Detail = fmt.Sprintf("%d machines", spec.Count)
			} else {
				header.Target = spec.FirstMachine
			}
			total := ui.LiveRow{ID: "gc-total", Label: "Deleted paths", Current: new(big.Int), State: ui.LivePending}
			// GC owns a semantic feed. A raw tail would duplicate these lines and
			// must not install a pool-wide cancellation callback.
			live, err := e.openLiveView(ctx, stdout, stderr, liveViewSpec{Header: header, Rows: []ui.LiveRow{total}}, nil)
			if err != nil {
				return nil, false, err
			}
			if live != nil {
				return &generationGCLiveView{live: live, total: total, hosts: make(map[int]ui.LiveRow, spec.Count)}, true, nil
			}
			if err := generationWrite(stdout, spec.Header+"\n"); err != nil {
				return nil, false, err
			}
			return &generationGCTextView{out: stdout}, false, nil
		},
	}
}

// The live adapter serializes only snapshots, never child execution or joins.
// All task callbacks have settled before Stop finalizes unstarted/interrupted
// rows and closes the borrowed common view. Counts have no invented total.
type generationGCLiveView struct {
	mu       sync.Mutex
	live     liveView
	total    ui.LiveRow
	hosts    map[int]ui.LiveRow
	order    []int
	stopOnce sync.Once
	stopErr  error
}

var _ gcTaskView = (*generationGCLiveView)(nil)

func (v *generationGCLiveView) Register(index int, host string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.hosts[index]; exists {
		return fmt.Errorf("GC host index already registered: %d", index)
	}
	row := ui.LiveRow{ID: fmt.Sprintf("gc-host-%d", index), Label: host, Current: new(big.Int), State: ui.LivePending, Target: true}
	if err := v.live.SetRows(row); err != nil {
		return err
	}
	v.hosts[index] = row
	v.order = append(v.order, index)
	return nil
}

func (v *generationGCLiveView) Begin(index int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	row, exists := v.hosts[index]
	if !exists {
		return fmt.Errorf("GC host index unavailable: %d", index)
	}
	row.State = ui.LiveRunning
	v.total.State = ui.LiveRunning
	if err := v.live.SetRows(v.total, row); err != nil {
		return err
	}
	v.hosts[index] = row
	return nil
}

func (v *generationGCLiveView) End(index int, result Result, bodyErr error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	row := v.hosts[index]
	row.State = ui.LiveIncomplete
	if bodyErr == nil {
		row.State = ui.LiveFailed
		if result.ExitCode == 0 && result.Signal == "" && result.signal == 0 {
			row.State = ui.LiveSucceeded
		}
	}
	v.hosts[index] = row
	v.total.State = v.aggregateState()
	return v.live.SetRows(v.total, row)
}

func (v *generationGCLiveView) aggregateState() ui.LiveState {
	state := ui.LiveSucceeded
	for _, row := range v.hosts {
		switch row.State {
		case ui.LivePending, ui.LiveRunning:
			return ui.LiveRunning
		case ui.LiveIncomplete:
			state = ui.LiveIncomplete
		case ui.LiveFailed:
			if state != ui.LiveIncomplete {
				state = ui.LiveFailed
			}
		}
	}
	return state
}

func (v *generationGCLiveView) Line(_ int, text string) error { return v.live.Line(text) }

func (v *generationGCLiveView) Advance(index int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	row, exists := v.hosts[index]
	if !exists {
		return fmt.Errorf("GC host index unavailable: %d", index)
	}
	if err := v.live.Advance(row.ID, v.total.ID); err != nil {
		return err
	}
	row.Current.Add(row.Current, big.NewInt(1))
	v.total.Current.Add(v.total.Current, big.NewInt(1))
	return nil
}

func (v *generationGCLiveView) Finish(index int, text string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	row := v.hosts[index]
	row.Detail = strings.TrimSuffix(text, "\n")
	v.hosts[index] = row
	if err := v.live.SetRows(row); err != nil {
		return err
	}
	return v.live.Line(text)
}

func (v *generationGCLiveView) Flush(ctx context.Context) error { return v.live.Flush(ctx) }
func (v *generationGCLiveView) Err() error                      { return v.live.Err() }
func (v *generationGCLiveView) Failed() <-chan struct{}         { return v.live.Failed() }
func (v *generationGCLiveView) Stop() error {
	v.stopOnce.Do(func() {
		v.mu.Lock()
		rows := make([]ui.LiveRow, 0, len(v.order)+1)
		succeeded, failed, incomplete := 0, 0, 0
		for _, index := range v.order {
			row := v.hosts[index]
			switch row.State {
			case ui.LiveSucceeded:
				succeeded++
			case ui.LiveFailed:
				failed++
			default:
				incomplete++
				row.State = ui.LiveIncomplete
				v.hosts[index] = row
			}
			rows = append(rows, row)
		}
		v.total.State = v.aggregateState()
		rows = append([]ui.LiveRow{v.total}, rows...)
		summary := fmt.Sprintf("Deleted paths: %s; hosts: %d succeeded, %d failed, %d incomplete", v.total.Current, succeeded, failed, incomplete)
		v.mu.Unlock()
		v.stopErr = v.live.SetRows(rows...)
		if v.stopErr == nil {
			v.stopErr = v.live.Line(summary)
		}
		if err := v.live.Close(); v.stopErr == nil {
			v.stopErr = err
		}
	})
	return v.stopErr
}

// Redirected output retains serialized raw payloads, including non-UTF8 bytes.
type generationGCTextView struct {
	mu  sync.Mutex
	out io.Writer
}

func (v *generationGCTextView) Register(int, string) error  { return nil }
func (v *generationGCTextView) Advance(int) error           { return nil }
func (v *generationGCTextView) Flush(context.Context) error { return nil }
func (v *generationGCTextView) Stop() error                 { return nil }
func (v *generationGCTextView) Line(_ int, text string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return generationWrite(v.out, text)
}
func (v *generationGCTextView) Finish(index int, text string) error { return v.Line(index, text) }

func (e *Engine) CollectGarbage(inv cli.Invocation, stdin, stdout, stderr *os.File) error {
	return e.collectGarbage(inv, stdin, stdout, e.generationGCDependencies(stdout, stderr))
}

func (e *Engine) collectGarbage(inv cli.Invocation, in io.Reader, out io.Writer, deps gcDependencies) error {
	ms, _, err := e.generationMutationSelection(inv)
	if err != nil {
		return err
	}
	tasks := []gcTask{}
	for _, machine := range ms {
		if machine.CarrierName == nil && machine.Target.Host != nil {
			tasks = append(tasks, gcTask{Host: machine.Name, Machine: machine})
		}
	}
	if len(tasks) == 0 {
		return fmt.Errorf("No machines to collect garbage on")
	}
	if !inv.Options["yes"].Value.Bool {
		runnable := make([]Machine, len(tasks))
		for i, task := range tasks {
			runnable[i] = task.Machine
		}
		table, err := e.List(runnable, nil, true)
		if err != nil {
			return err
		}
		if err := generationConfirm(e.Context, inv, in, out, "Collect garbage on the following machines:\n"+table); err != nil {
			return err
		}
	}
	return runGenerationGC(e.Context, tasks, generationGCRequest(inv), deps)
}
