package core

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
)

// These private operations sequence an already selected GC task set. Callers
// retain selection, irreversible removals, process/log ownership and real UI.
type gcTask struct {
	Host    string
	Machine Machine
}

type gcExecute func(context.Context, Machine, []string, func(stdout, stderr []byte)) (Result, error)

type gcViewSpec struct {
	Header       string
	Count        int
	FirstMachine string
}

type gcView interface {
	Register(index int, host string) error
	Line(index int, prefixedRaw string) error
	Advance(index int) error
	Finish(index int, text string) error
	Flush(context.Context) error
	Stop() error
}

// Only the acquired live adapter needs task lifecycle and asynchronous failure
// observation. The existing gcView contract and worker pool stay unchanged:
// raw Result/error determines terminal state, and each task joins its own
// observer rather than cancelling the pool when a later worker fails.
type gcTaskView interface {
	gcView
	Begin(index int) error
	End(index int, result Result, bodyErr error) error
	Err() error
	Failed() <-chan struct{}
}

type gcDependencies struct {
	Prepare func(context.Context) (gcExecute, error)
	Open    func(context.Context, gcViewSpec) (gcView, bool, error)
}

func runGenerationGC(ctx context.Context, tasks []gcTask, request *big.Int, deps gcDependencies) (primary error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deps.Prepare == nil {
		return fmt.Errorf("GC Prepare binding unavailable")
	}
	execute, err := deps.Prepare(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	spec := gcViewSpec{Count: len(tasks)}
	if len(tasks) > 0 {
		spec.FirstMachine = tasks[0].Machine.Name
	}
	spec.Header = "Collecting garbage on " + spec.FirstMachine
	if len(tasks) > 1 {
		spec.Header = fmt.Sprintf("Collecting garbage on %d machines", len(tasks))
	}
	if deps.Open == nil {
		return fmt.Errorf("GC Open binding unavailable")
	}
	view, tty, err := deps.Open(ctx, spec)
	if err != nil {
		return err
	} // Open owns its failed-construction cleanup.
	if view == nil {
		return fmt.Errorf("GC view binding unavailable")
	}
	if tty {
		defer func() {
			err := view.Stop()
			if primary == nil {
				primary = err
			}
		}()
	}
	queue := make([]gcTask, 0, len(tasks))
	for i, task := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tty {
			if err := view.Register(i, task.Host); err != nil {
				return err
			}
		}
		queue = append(queue, task)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	workers, err := gcWorkerCount(request, len(queue))
	if err != nil {
		return err
	}
	failed, err := gcRunWorkers(ctx, queue, workers, func(ctx context.Context, index int, task gcTask) (bool, error) {
		return gcRunTask(ctx, index, task, execute, view, tty)
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tty {
		if err := view.Flush(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("Gargabe collection failed on: %s", strings.Join(failed, ", "))
	}
	return nil
}

func gcRunTask(parent context.Context, index int, task gcTask, execute gcExecute, view gcView, tty bool) (success bool, primary error) {
	if err := parent.Err(); err != nil {
		return false, err
	}
	if execute == nil {
		return false, fmt.Errorf("GC Execute binding unavailable")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var result Result
	var live gcTaskView
	if tty {
		live, _ = view.(gcTaskView)
	}
	if live != nil {
		if err := live.Err(); err != nil {
			return false, err
		}
		if err := live.Begin(index); err != nil {
			return false, err
		}
		defer func() {
			if err := live.End(index, result, primary); primary == nil {
				primary = err
			}
		}()
		settled, joined := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			select {
			case <-live.Failed():
				cancel()
			case <-settled:
			case <-ctx.Done():
			}
		}()
		defer func() { close(settled); <-joined }()
		// Failure already published before the child starts is a refusal. A
		// later failure cancels only this task and still waits for Execute.
		if err := live.Err(); err != nil {
			return false, err
		}
	}
	progress := gcTaskProgress{index: index, host: task.Host, view: view, tty: tty}
	var mu sync.Mutex
	var presentationErr error
	feed := func(stdout, stderr []byte) {
		mu.Lock()
		defer mu.Unlock()
		if presentationErr != nil {
			return
		}
		presentationErr = progress.feed(stdout, stderr)
		if presentationErr != nil {
			cancel()
		}
	}
	// Execute owns its one child/wait and returns only after every feed settles.
	// A feed failure cancels this task only; join-order policy belongs to the pool.
	result, err := execute(ctx, task.Machine, []string{"nix-collect-garbage"}, feed)
	mu.Lock()
	defer mu.Unlock()
	if presentationErr != nil {
		return false, presentationErr
	}
	// A renderer notification may cancel this child, but it must not replace
	// a distinct execution/capture failure already returned by its owner.
	if err != nil && (result.capturePrimary() != nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded))) {
		return false, err
	}
	if live != nil {
		if err := live.Err(); err != nil {
			return false, err
		}
	}
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := progress.finish(); err != nil {
		return false, err
	}
	success = result.ExitCode == 0 && result.Signal == "" && result.signal == 0
	summary := "error occurred"
	if success {
		summary = progress.summary
		if summary == "" {
			summary = "done"
		}
	}
	if err := view.Finish(index, task.Host+": "+summary); err != nil {
		return false, err
	}
	return success, nil
}
