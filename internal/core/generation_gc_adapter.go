package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/cli"
)

type generationGCStreamRun func(context.Context, string, []string, []byte, func([]byte, []byte) error) (Result, error)

func (e *Engine) generationGCDependencies(out io.Writer) gcDependencies {
	return e.generationGCAdapter(out, func(ctx context.Context, root string, argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
		// Only the task context changes. Keep the checked log owner with Log;
		// processStreams owns the single child, capture, callback and Wait.
		task := &Engine{Root: root, Context: ctx, Log: e.Log, logOwner: e.logOwner}
		return task.processStreams(argv, input, nil, chunk)
	})
}

func (e *Engine) generationGCAdapter(out io.Writer, run generationGCStreamRun) gcDependencies {
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
		Open: func(_ context.Context, spec gcViewSpec) (gcView, bool, error) {
			if err := generationWrite(out, spec.Header+"\n"); err != nil {
				return nil, false, err
			}
			return &generationGCTextView{out: out}, false, nil
		},
	}
}

// Human progress is intentionally textual. Serialize individual writes, never
// child execution, and preserve the raw payload including non-UTF8 bytes.
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
	return e.collectGarbage(inv, stdin, stdout, e.generationGCDependencies(stdout))
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
