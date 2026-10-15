package health

import (
	"context"
	"math"
	"math/big"
	"sync"
	"time"
)

type Result struct {
	Check             Check
	Started, Complete bool
	Attempts          int
	Failures          []Failure
	HadFailure        bool
	Err               error
}

func (r Result) Passed() bool { return r.Complete && r.Err == nil && len(r.Failures) == 0 }

type Report struct {
	Results   []Result // Input order, including unstarted and interrupted work.
	RetainLog bool     // A failed attempt remains relevant after a later success.
}

type Event struct {
	Index  int
	Phase  string // start, failed, done
	Result Result
}

type Deps struct {
	Execute  func(context.Context, Check) (CommandResult, error)
	Now      func() time.Time
	Cooldown func(context.Context, *big.Int) error
	Progress func(Event) // Serialized; each check's events retain their order.
}

func Run(ctx context.Context, checks []Check, requested *big.Int, deps Deps) (Report, error) {
	report := Report{Results: make([]Result, len(checks))}
	for i, check := range checks {
		report.Results[i].Check = check
	}
	workers, err := WorkerCount(requested, len(checks))
	if err != nil {
		return report, err
	}
	if len(checks) == 0 {
		return report, ErrNoChecks
	}
	for _, check := range checks {
		if err := check.validate(); err != nil {
			return report, err
		}
	}
	if deps.Execute == nil {
		return report, &SchemaError{"health runner", "command executor is required"}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Cooldown == nil {
		deps.Cooldown = sleepSeconds
	}
	var progressMu sync.Mutex
	progress := func(event Event) {
		if deps.Progress != nil {
			progressMu.Lock()
			defer progressMu.Unlock()
			event.Result.Failures = append([]Failure(nil), event.Result.Failures...)
			deps.Progress(event)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	next := 0
	var firstError error
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				mu.Lock()
				if firstError != nil || ctx.Err() != nil || next == len(checks) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				result := runCheck(ctx, checks[i], deps, func(phase string, result Result) { progress(Event{i, phase, result}) })
				mu.Lock()
				report.Results[i] = result
				report.RetainLog = report.RetainLog || result.HadFailure
				if result.Err != nil && firstError == nil {
					firstError = result.Err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if firstError != nil {
		return report, firstError
	}
	return report, ctx.Err()
}

func runCheck(ctx context.Context, check Check, deps Deps, progress func(string, Result)) Result {
	result := Result{Check: check}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	started := deps.Now()
	result.Started = true
	progress("start", result)
	for {
		if err := ctx.Err(); err != nil {
			result.Err = err
			return result
		}
		result.Attempts++
		result.Failures = nil
		command, err := deps.Execute(ctx, check)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			result.Err = err
			return result
		}
		if check.Command != nil {
			result.Failures = commandFailures(check.Command, command)
		} else {
			result.Failures = systemdFailures(check, command)
		}
		if len(result.Failures) == 0 {
			break
		}
		result.HadFailure = true
		progress("failed", result)
		if err := deps.Cooldown(ctx, new(big.Int).Set(check.Cooldown)); err != nil {
			result.Err = err
			return result
		}
		if expired(started, check.Timeout, deps.Now()) {
			break
		}
	}
	result.Complete = true
	progress("done", result)
	return result
}

func expired(start time.Time, seconds *big.Int, now time.Time) bool {
	deadline := new(big.Int).Add(big.NewInt(start.Unix()), seconds)
	comparison := deadline.Cmp(big.NewInt(now.Unix()))
	return comparison < 0 || comparison == 0 && start.Nanosecond() < now.Nanosecond()
}

func sleepSeconds(ctx context.Context, seconds *big.Int) error {
	remaining := new(big.Int).Set(seconds)
	maxChunk := big.NewInt(math.MaxInt64 / int64(time.Second))
	for remaining.Sign() > 0 {
		chunk := new(big.Int).Set(remaining)
		if chunk.Cmp(maxChunk) > 0 {
			chunk.Set(maxChunk)
		}
		timer := time.NewTimer(time.Duration(chunk.Int64()) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		remaining.Sub(remaining, chunk)
	}
	return ctx.Err()
}
