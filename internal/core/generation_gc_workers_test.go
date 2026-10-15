package core

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGenerationGCWorkerCountExactSignedAndBorrowed(t *testing.T) {
	for _, text := range []string{"-999999999999999999999999999999999", "-1", "-0", "0", "1", "2", "3", "5", "999999999999999999999999999999999"} {
		request, ok := new(big.Int).SetString(text, 10)
		if !ok {
			t.Fatal(text)
		}
		before := new(big.Int).Set(request)
		for _, n := range []int{0, 1, 3} {
			want := n
			if request.Sign() <= 0 {
				want = 0
			} else if request.Cmp(big.NewInt(int64(n))) < 0 {
				want = int(request.Int64())
			}
			got, err := gcWorkerCount(request, n)
			if err != nil || got != want || request.Cmp(before) != 0 {
				t.Fatal(text, n, got, want, err)
			}
		}
	}
	for _, tc := range []struct {
		request *big.Int
		n       int
	}{{nil, 0}, {gcInt(1), -1}} {
		if _, err := gcWorkerCount(tc.request, tc.n); err == nil {
			t.Fatal(tc)
		}
	}
	max := int(^uint(0) >> 1)
	for _, request := range []*big.Int{new(big.Int).SetUint64(uint64(max)), new(big.Int).Lsh(gcInt(1), 100)} {
		if got, err := gcWorkerCount(request, max); err != nil || got != max {
			t.Fatal(got, err)
		}
	}
}

func TestGenerationGCWorkerCapacityAndFIFO(t *testing.T) {
	for _, request := range []*big.Int{gcInt(1), gcInt(2), gcInt(5), new(big.Int).Lsh(gcInt(1), 100)} {
		t.Run(request.String(), func(t *testing.T) {
			count, _ := gcWorkerCount(request, 3)
			started := make(chan string, 3)
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var active, max atomic.Int32
			var mu sync.Mutex
			seen := map[string]int{}
			run := func(_ context.Context, _ int, task gcTask) (bool, error) {
				a := active.Add(1)
				defer active.Add(-1)
				for old := max.Load(); a > old && !max.CompareAndSwap(old, a); old = max.Load() {
				}
				mu.Lock()
				seen[task.Host]++
				mu.Unlock()
				started <- task.Host
				<-release
				return true, nil
			}
			type result struct {
				failed []string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				f, e := gcRunWorkers(context.Background(), gcTasks("A", "B", "C"), count, run)
				done <- result{f, e}
			}()
			first := map[string]bool{}
			for i := 0; i < count; i++ {
				first[gcAwait(t, started)] = true
			}
			for i, name := range []string{"A", "B", "C"} {
				if first[name] != (i < count) {
					t.Fatal(first, count)
				}
			}
			if int(active.Load()) != count {
				t.Fatal(active.Load(), count)
			}
			select {
			case extra := <-started:
				t.Fatal("exceeded W", extra)
			default:
			}
			close(release)
			resultValue := gcAwait(t, done)
			if resultValue.err != nil || len(resultValue.failed) != 0 || max.Load() > int32(count) || active.Load() != 0 || !reflect.DeepEqual(seen, map[string]int{"A": 1, "B": 1, "C": 1}) {
				t.Fatal(resultValue, seen, max.Load(), active.Load())
			}
		})
	}
}

func TestGenerationGCNormalFalseDrainsAndCompletionOrder(t *testing.T) {
	tasks := gcTasks("A", "B", "C")
	v := &gcTestView{}
	started := make(chan string, 3)
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	finishedC := make(chan struct{})
	defer func() {
		for _, ch := range []chan struct{}{releaseA, releaseB} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	execute := func(_ context.Context, m Machine, _ []string, _ func([]byte, []byte)) (Result, error) {
		name := strings.TrimPrefix(m.Name, "effective-")
		started <- name
		switch name {
		case "A":
			<-releaseA
			return Result{}, nil
		case "B":
			<-releaseB
		}
		return Result{ExitCode: 1}, nil
	}
	v.finish = func(_ int, text string) error {
		if text == "C: error occurred" {
			close(finishedC)
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runGenerationGC(context.Background(), tasks, gcInt(2), gcFixture(v, true, execute)) }()
	first := map[string]bool{gcAwait(t, started): true, gcAwait(t, started): true}
	if !first["A"] || !first["B"] {
		t.Fatal(first)
	}
	close(releaseB)
	if got := gcAwait(t, started); got != "C" {
		t.Fatal(got)
	}
	gcAwait(t, finishedC)
	close(releaseA)
	err := gcAwait(t, done)
	if err == nil || err.Error() != "Gargabe collection failed on: B, C" {
		t.Fatal(err)
	}
	events := v.snapshot()
	if events[len(events)-2].kind != "flush" || events[len(events)-1].kind != "stop" {
		t.Fatal(events)
	}
	var finishes []string
	for _, e := range events {
		if e.kind == "finish" {
			finishes = append(finishes, e.text)
		}
	}
	if !reflect.DeepEqual(finishes, []string{"B: error occurred", "C: error occurred", "A: done"}) {
		t.Fatal(finishes)
	}
	// Serial ordinary false returns after all three presentations, without retry.
	var calls []string
	failed, err := gcRunWorkers(context.Background(), tasks, 1, func(_ context.Context, _ int, task gcTask) (bool, error) {
		calls = append(calls, task.Host)
		return false, nil
	})
	if err != nil || !reflect.DeepEqual(calls, []string{"A", "B", "C"}) || !reflect.DeepEqual(failed, calls) {
		t.Fatal(calls, failed, err)
	}
}

func TestGenerationGCOrderedJoinObservationAndPrimary(t *testing.T) {
	early, late, secondary := errors.New("early"), errors.New("late"), errors.New("secondary")
	// Worker1 has already published an error; worker0's join is still pending.
	first, second, third := make(chan error), make(chan error, 1), make(chan error)
	second <- late
	stop := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- gcJoinWorkers(ctx, []<-chan error{first, second, third}, func() { cancel(); close(stop) })
	}()
	select {
	case <-stop:
		t.Fatal("stopped before observing the earlier worker")
	default:
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("cancelled before observing the earlier worker", err)
	}
	// The earlier worker's distinct error must win over the published late one.
	// The unbuffered send can complete only when its ordered slot is observed.
	observed := make(chan bool, 1)
	go func() {
		select {
		case first <- early:
			observed <- true
		case <-stop:
			observed <- false
		}
	}()
	if !gcAwait(t, observed) {
		t.Fatal("stopped before observing the earlier worker error")
	}
	gcAwait(t, stop)
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatal("observed worker error did not cancel", err)
	}
	select {
	case err := <-done:
		t.Fatal("did not join remaining worker", err)
	default:
	}
	third <- secondary
	if err := gcAwait(t, done); !errors.Is(err, early) {
		t.Fatal(err)
	}
	// Parent already visible at an observation boundary beats a ready error.
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	slot := make(chan error, 1)
	slot <- early
	stops := 0
	if err := gcJoinWorkers(ctx, []<-chan error{slot}, func() { stops++ }); !errors.Is(err, context.Canceled) || stops != 1 {
		t.Fatal(err, stops)
	}
	// A first adopted worker error cannot be replaced by later cancellation.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	a, b := make(chan error, 1), make(chan error, 1)
	a <- early
	b <- late
	if err := gcJoinWorkers(ctx, []<-chan error{a, b}, cancel); !errors.Is(err, early) {
		t.Fatal(err)
	}
}

func TestGenerationGCParentCancellationJoinsActualCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 2)
	cancelled := make(chan string, 2)
	release := make(chan struct{})
	var active atomic.Int32
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	v := &gcTestView{}
	v.stop = func() error {
		if active.Load() != 0 {
			t.Error("live callback at Stop")
		}
		return errors.New("secondary cleanup")
	}
	execute := func(ctx context.Context, m Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- m.Name
		<-ctx.Done()
		cancelled <- m.Name
		<-release
		feed([]byte("late complete\npartial"), nil)
		return Result{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- runGenerationGC(ctx, gcTasks("A", "B", "C"), gcInt(2), gcFixture(v, true, execute)) }()
	gcAwait(t, started)
	gcAwait(t, started)
	cancel()
	gcAwait(t, cancelled)
	gcAwait(t, cancelled)
	select {
	case err := <-done:
		t.Fatal("returned before owned joins", err)
	default:
	}
	select {
	case name := <-started:
		t.Fatal("admitted after stop", name)
	default:
	}
	close(release)
	if err := gcAwait(t, done); !errors.Is(err, context.Canceled) || active.Load() != 0 {
		t.Fatal(err, active.Load())
	}
	for _, e := range v.snapshot() {
		if e.kind == "flush" || e.kind == "finish" || strings.Contains(e.text, "partial") {
			t.Fatal(v.snapshot())
		}
	}
	// Actual single-worker infrastructure error leaves the following task unrun.
	boom := errors.New("fake transport")
	var hosts []string
	failed, err := gcRunWorkers(context.Background(), gcTasks("A", "B", "C"), 1, func(_ context.Context, _ int, task gcTask) (bool, error) {
		hosts = append(hosts, task.Host)
		return false, boom
	})
	if !errors.Is(err, boom) || len(failed) != 0 || !reflect.DeepEqual(hosts, []string{"A"}) {
		t.Fatal(err, failed, hosts)
	}
}

func TestGenerationGCWorkerTaskPresentationBeforeAppend(t *testing.T) {
	boom := errors.New("Finish failed")
	v := &gcTestView{}
	v.finish = func(int, string) error { return boom }
	failed, err := gcRunWorkers(context.Background(), gcTasks("A", "B"), 1, func(ctx context.Context, index int, task gcTask) (bool, error) {
		return gcRunTask(ctx, index, task, func(context.Context, Machine, []string, func([]byte, []byte)) (Result, error) {
			return Result{ExitCode: 1}, nil
		}, v, false)
	})
	if !errors.Is(err, boom) || len(failed) != 0 {
		t.Fatal(err, failed)
	}
	gcExpect(t, v, gcTestEvent{"line", 0, "A> "}, gcTestEvent{"line", 0, "A> "}, gcTestEvent{"finish", 0, "A: error occurred"})
}
