package core

import (
	"context"
	"fmt"
	"math/big"
	"sync"
)

func gcWorkerCount(request *big.Int, taskCount int) (int, error) {
	if request == nil || taskCount < 0 {
		return 0, fmt.Errorf("invalid GC worker count binding")
	}
	if request.Sign() <= 0 || taskCount == 0 {
		return 0, nil
	}
	if request.Cmp(new(big.Int).SetUint64(uint64(taskCount))) >= 0 {
		return taskCount, nil
	}
	return int(request.Int64()), nil // Here 0 < request < taskCount <= MaxInt.
}

// Slots are buffered so a later worker can publish an error while an earlier
// worker still owns the next ordered join. Publication alone never cancels GC.
func gcJoinWorkers(parent context.Context, slots []<-chan error, stop func()) error {
	var primary error
	for _, slot := range slots {
		if primary != nil {
			<-slot
			continue
		}
		if err := parent.Err(); err != nil {
			primary = err
			stop()
			<-slot
			continue
		}
		var workerErr error
		select {
		case workerErr = <-slot:
		case <-parent.Done():
			primary = parent.Err()
			stop()
			<-slot
			continue
		}
		// Parent has priority at each observation boundary, even with a ready slot.
		if err := parent.Err(); err != nil {
			primary = err
		} else if workerErr != nil {
			primary = workerErr
		}
		if primary != nil {
			stop()
		}
	}
	return primary
}

func gcRunWorkers(parent context.Context, tasks []gcTask, count int, run func(context.Context, int, gcTask) (bool, error)) ([]string, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var admission sync.Mutex
	next := 0
	stopped := false
	stop := func() { admission.Lock(); stopped = true; cancel(); admission.Unlock() }
	var results sync.Mutex
	var failed []string
	slots := make([]<-chan error, count)
	for worker := 0; worker < count; worker++ {
		slot := make(chan error, 1)
		slots[worker] = slot
		go func() {
			var workerErr error
			defer func() { slot <- workerErr }()
			for {
				admission.Lock()
				if stopped || ctx.Err() != nil || next == len(tasks) {
					admission.Unlock()
					return
				}
				index := next
				next++
				admission.Unlock()
				if err := ctx.Err(); err != nil {
					workerErr = err
					return
				}
				success, err := run(ctx, index, tasks[index])
				if err != nil {
					workerErr = err
					return
				}
				// Presentation completed before this append. Ordinary false keeps draining.
				results.Lock()
				if !success {
					failed = append(failed, tasks[index].Host)
				}
				results.Unlock()
			}
		}()
	}
	err := gcJoinWorkers(parent, slots, stop)
	return failed, err // Every worker/callback has joined before reading results.
}
