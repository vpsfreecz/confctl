package core

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

type gcTestEvent struct {
	kind  string
	index int
	text  string
}
type gcTestView struct {
	mu         sync.Mutex
	events     []gcTestEvent
	transcript strings.Builder
	register   func(int, string) error
	line       func(int, string) error
	advance    func(int) error
	finish     func(int, string) error
	flush      func(context.Context) error
	stop       func() error
}

func (v *gcTestView) record(kind string, index int, text string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.events = append(v.events, gcTestEvent{kind, index, text})
}
func (v *gcTestView) puts(text string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.transcript.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		v.transcript.WriteByte('\n')
	}
}
func (v *gcTestView) Register(i int, s string) error {
	v.record("register", i, s)
	if v.register != nil {
		return v.register(i, s)
	}
	return nil
}
func (v *gcTestView) Line(i int, s string) error {
	v.record("line", i, s)
	v.puts(s)
	if v.line != nil {
		return v.line(i, s)
	}
	return nil
}
func (v *gcTestView) Advance(i int) error {
	v.record("advance", i, "")
	if v.advance != nil {
		return v.advance(i)
	}
	return nil
}
func (v *gcTestView) Finish(i int, s string) error {
	v.record("finish", i, s)
	v.puts(s)
	if v.finish != nil {
		return v.finish(i, s)
	}
	return nil
}
func (v *gcTestView) Flush(ctx context.Context) error {
	v.record("flush", -1, "")
	if v.flush != nil {
		return v.flush(ctx)
	}
	return nil
}
func (v *gcTestView) Stop() error {
	v.record("stop", -1, "")
	if v.stop != nil {
		return v.stop()
	}
	return nil
}
func (v *gcTestView) snapshot() []gcTestEvent {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]gcTestEvent(nil), v.events...)
}
func (v *gcTestView) output() string { v.mu.Lock(); defer v.mu.Unlock(); return v.transcript.String() }
func gcInt(n int64) *big.Int         { return big.NewInt(n) }
func gcTasks(names ...string) []gcTask {
	tasks := make([]gcTask, len(names))
	for i, name := range names {
		tasks[i] = gcTask{Host: name, Machine: Machine{Name: "effective-" + name, Attributes: map[string]any{"borrowed": name}}}
	}
	return tasks
}
func gcFixture(v *gcTestView, tty bool, execute gcExecute) gcDependencies {
	return gcDependencies{Prepare: func(context.Context) (gcExecute, error) { v.record("prepare", -1, ""); return execute, nil }, Open: func(_ context.Context, s gcViewSpec) (gcView, bool, error) {
		v.record("open", s.Count, s.Header)
		v.puts(s.Header)
		return v, tty, nil
	}}
}
func gcExpect(t *testing.T, v *gcTestView, want ...gcTestEvent) {
	t.Helper()
	if got := v.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%#v\nwant=%#v", got, want)
	}
}
func gcAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case x := <-ch:
		return x
	case <-time.After(5 * time.Second):
		t.Fatal("GC private gate watchdog")
		var x T
		return x
	}
}

func TestGenerationGCReachedNoWorkAndSetup(t *testing.T) {
	for _, tty := range []bool{false, true} {
		for _, n := range []int{0, 3} {
			for _, request := range []*big.Int{gcInt(0), gcInt(-1), new(big.Int).Neg(new(big.Int).Lsh(gcInt(1), 100)), gcInt(5), new(big.Int).Lsh(gcInt(1), 100)} {
				if n > 0 && request.Sign() > 0 {
					continue
				}
				t.Run(fmt.Sprintf("%t/%d/%s", tty, n, request), func(t *testing.T) {
					tasks := gcTasks("A", "B", "C")[:n]
					v := &gcTestView{}
					before := new(big.Int).Set(request)
					d := gcFixture(v, tty, nil)
					d.Open = func(_ context.Context, s gcViewSpec) (gcView, bool, error) {
						wantFirst := ""
						if n > 0 {
							wantFirst = "effective-A"
						}
						if s.FirstMachine != wantFirst {
							t.Fatal(s)
						}
						v.record("open", s.Count, s.Header)
						return v, tty, nil
					}
					if err := runGenerationGC(context.Background(), tasks, request, d); err != nil {
						t.Fatal(err)
					}
					header := "Collecting garbage on "
					if n > 1 {
						header = "Collecting garbage on 3 machines"
					}
					want := []gcTestEvent{{"prepare", -1, ""}, {"open", n, header}}
					if tty {
						for i, task := range tasks {
							want = append(want, gcTestEvent{"register", i, task.Host})
						}
						want = append(want, gcTestEvent{"flush", -1, ""}, gcTestEvent{"stop", -1, ""})
					}
					gcExpect(t, v, want...)
					if request.Cmp(before) != 0 {
						t.Fatal("mutated borrowed count")
					}
				})
			}
		}
	}
	boom, secondary := errors.New("setup"), errors.New("stop")
	for _, stage := range []string{"cancel-entry", "prepare", "open", "nil-open", "nil-prepare", "nil-view", "register", "count", "cancel-after-open"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			v := &gcTestView{}
			d := gcFixture(v, true, nil)
			request := gcInt(0)
			switch stage {
			case "cancel-entry":
				cancel()
			case "prepare":
				d.Prepare = func(context.Context) (gcExecute, error) { v.record("prepare", -1, ""); return nil, boom }
			case "open":
				d.Open = func(context.Context, gcViewSpec) (gcView, bool, error) {
					v.record("open", -1, "")
					return nil, true, boom
				}
			case "nil-open":
				d.Open = nil
			case "nil-prepare":
				d.Prepare = nil
			case "nil-view":
				d.Open = func(context.Context, gcViewSpec) (gcView, bool, error) { return nil, true, nil }
			case "register":
				v.register = func(i int, _ string) error {
					if i == 1 {
						return boom
					}
					return nil
				}
			case "count":
				request = nil
			case "cancel-after-open":
				d.Open = func(_ context.Context, s gcViewSpec) (gcView, bool, error) {
					v.record("open", s.Count, s.Header)
					cancel()
					return v, true, nil
				}
			}
			v.stop = func() error { return secondary }
			err := runGenerationGC(ctx, gcTasks("A", "B", "C"), request, d)
			if err == nil || errors.Is(err, secondary) {
				t.Fatal(err)
			}
			events := v.snapshot()
			stops, flushes := 0, 0
			for _, e := range events {
				if e.kind == "stop" {
					stops++
				}
				if e.kind == "flush" {
					flushes++
				}
				if e.kind == "line" || e.kind == "finish" {
					t.Fatal(events)
				}
			}
			wantStop := stage == "register" || stage == "count" || stage == "cancel-after-open"
			if (stops == 1) != wantStop || flushes != 0 {
				t.Fatal(events)
			}
			if stage == "register" {
				gcExpect(t, v, gcTestEvent{"prepare", -1, ""}, gcTestEvent{"open", 3, "Collecting garbage on 3 machines"}, gcTestEvent{"register", 0, "A"}, gcTestEvent{"register", 1, "B"}, gcTestEvent{"stop", -1, ""})
			}
			if stage == "prepare" || stage == "open" || stage == "register" {
				if !errors.Is(err, boom) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestGenerationGCNormalStatusTailsAndBorrowedInputs(t *testing.T) {
	for _, result := range []Result{{ExitCode: 0}, {ExitCode: 7}, {ExitCode: -1, Signal: "synthetic signal"}} {
		t.Run(fmt.Sprint(result.ExitCode), func(t *testing.T) {
			v := &gcTestView{}
			tasks := gcTasks("map-key")
			request := gcInt(5)
			execute := func(_ context.Context, m Machine, argv []string, feed func([]byte, []byte)) (Result, error) {
				v.record("execute", 0, m.Name)
				if !reflect.DeepEqual(argv, []string{"nix-collect-garbage"}) || !reflect.DeepEqual(m, tasks[0].Machine) {
					t.Fatal(m, argv)
				}
				argv[0] = "mutated private argv"
				return result, nil
			}
			err := runGenerationGC(context.Background(), tasks, request, gcFixture(v, false, execute))
			summary := "error occurred"
			if result.ExitCode == 0 {
				summary = "done"
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != "Gargabe collection failed on: map-key" {
				t.Fatal(err)
			}
			if want := "Collecting garbage on effective-map-key\nmap-key> \nmap-key> \nmap-key: " + summary + "\n"; v.output() != want {
				t.Fatalf("%q != %q", v.output(), want)
			}
			if tasks[0].Host != "map-key" || tasks[0].Machine.Name != "effective-map-key" || tasks[0].Machine.Attributes["borrowed"] != "map-key" || request.Int64() != 5 {
				t.Fatal(tasks, request)
			}
		})
	}
	// Each entered task receives fresh literal argv; no output-capture replay.
	v := &gcTestView{}
	n := 0
	execute := func(_ context.Context, _ Machine, argv []string, feed func([]byte, []byte)) (Result, error) {
		n++
		if argv[0] != "nix-collect-garbage" {
			t.Fatal(argv)
		}
		argv[0] = "changed"
		feed([]byte("once\n"), nil)
		return Result{Stdout: []byte("MUST NOT REPLAY"), Stderr: []byte("MUST NOT REPLAY")}, nil
	}
	if err := runGenerationGC(context.Background(), gcTasks("A", "B"), gcInt(1), gcFixture(v, false, execute)); err != nil || n != 2 || strings.Contains(v.output(), "REPLAY") {
		t.Fatal(err, n, v.output())
	}
}

func TestGenerationGCFailuresFlushStopAndPrimary(t *testing.T) {
	boom, stopErr := errors.New("primary"), errors.New("stop")
	for _, stage := range []string{"execute", "nil-execute", "tail", "finish", "flush", "aggregate", "stop", "cancel-before-flush", "cancel-in-flush"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			v := &gcTestView{}
			var joined atomic.Bool
			execute := gcExecute(func(_ context.Context, _ Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
				defer joined.Store(true)
				feed([]byte("complete\npartial"), nil)
				if stage == "execute" {
					return Result{}, boom
				}
				if stage == "aggregate" || stage == "flush" {
					return Result{ExitCode: 2}, nil
				}
				return Result{}, nil
			})
			if stage == "nil-execute" {
				execute = nil
			}
			if stage == "tail" {
				v.line = func(_ int, s string) error {
					if s == "A> partial" {
						return boom
					}
					return nil
				}
			}
			if stage == "finish" {
				v.finish = func(int, string) error { return boom }
			}
			if stage == "flush" {
				v.flush = func(context.Context) error { return boom }
			}
			if stage == "cancel-before-flush" {
				v.finish = func(int, string) error { cancel(); return nil }
			}
			if stage == "cancel-in-flush" {
				v.flush = func(context.Context) error { cancel(); return nil }
			}
			v.stop = func() error {
				if execute != nil && !joined.Load() {
					t.Error("Stop before callback joined")
				}
				return stopErr
			}
			err := runGenerationGC(ctx, gcTasks("A"), gcInt(1), gcFixture(v, true, execute))
			if err == nil {
				t.Fatal("lost error")
			}
			if stage == "stop" {
				if !errors.Is(err, stopErr) {
					t.Fatal(err)
				}
			} else if errors.Is(err, stopErr) {
				t.Fatal("secondary replaced primary", err)
			}
			if stage == "execute" || stage == "tail" || stage == "finish" || stage == "flush" {
				if !errors.Is(err, boom) {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(stage, "cancel-") && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var kinds []string
			for _, e := range v.snapshot() {
				kinds = append(kinds, e.kind)
			}
			if kinds[len(kinds)-1] != "stop" || strings.Count(strings.Join(kinds, ","), "stop") != 1 {
				t.Fatal(kinds)
			}
			wantFlush := stage == "flush" || stage == "aggregate" || stage == "stop" || stage == "cancel-in-flush"
			if strings.Contains(strings.Join(kinds, ","), "flush") != wantFlush {
				t.Fatal(kinds)
			}
			if stage == "execute" && (strings.Contains(v.output(), "partial") || strings.Contains(v.output(), "A: ")) {
				t.Fatal(v.output())
			}
			if stage == "aggregate" && err.Error() != "Gargabe collection failed on: A" {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerationGCTaskPresentationErrorCancelsAndJoins(t *testing.T) {
	for _, op := range []string{"line", "advance"} {
		t.Run(op, func(t *testing.T) {
			boom := errors.New(op)
			v := &gcTestView{}
			if op == "line" {
				v.line = func(int, string) error { return boom }
			} else {
				v.advance = func(int) error { return boom }
			}
			returned := make(chan struct{})
			cancelled := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			execute := func(ctx context.Context, _ Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
				defer close(returned)
				feed([]byte("deleting '/nix/store/a'\n"), nil)
				<-ctx.Done()
				close(cancelled)
				feed([]byte("suppressed\n"), []byte("also suppressed\n"))
				<-release
				return Result{}, ctx.Err()
			}
			v.stop = func() error {
				select {
				case <-returned:
				default:
					t.Error("Stop before execution returned")
				}
				return nil
			}
			done := make(chan error, 1)
			go func() {
				done <- runGenerationGC(context.Background(), gcTasks("A"), gcInt(1), gcFixture(v, true, execute))
			}()
			gcAwait(t, cancelled)
			select {
			case e := <-done:
				t.Fatal("detached callback", e)
			default:
			}
			close(release)
			if err := gcAwait(t, done); !errors.Is(err, boom) {
				t.Fatal(err)
			}
			for _, e := range v.snapshot() {
				if strings.Contains(e.text, "suppressed") || e.kind == "finish" || e.kind == "flush" {
					t.Fatal(v.snapshot())
				}
			}
		})
	}
}

// Existing registry only; these are source-backed admission assertions, with no
// command handler or new GC availability. HC/copy positivity is not reused.
func TestGenerationGCRegistryIntegerAdmission(t *testing.T) {
	registry := cli.BuiltinRegistry()
	for _, path := range [][]string{{"collect-garbage"}, {"generation", "rm"}, {"generation", "rotate"}} {
		inv, err := registry.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		value := inv.Options["max-concurrent-gc"]
		if value.Present || value.Value.Integer.String() != "5" {
			t.Fatal(inv)
		}
		for _, tc := range []struct{ raw, want string }{{"0", "0"}, {"-0", "0"}, {"-1", "-1"}, {"+5", "5"}, {"010", "8"}, {"+0X1_A", "26"}, {"-0b100", "-4"}, {"0_10", "8"}, {"1_000", "1000"}, {"9223372036854775808", "9223372036854775808"}, {"-9223372036854775809", "-9223372036854775809"}} {
			for _, args := range [][]string{{"--max-concurrent-gc=" + tc.raw}, {"--max-concurrent-gc", tc.raw}} {
				argv := append(append([]string{}, path...), args...)
				inv, err := registry.Parse(argv)
				v := inv.Options["max-concurrent-gc"]
				if err != nil || !v.Present || v.Value.Kind != cli.Number || v.Value.Integer.String() != tc.want || !reflect.DeepEqual(inv.RawArgv, argv) {
					t.Fatal(argv, inv, err)
				}
			}
		}
		argv := append(append([]string{}, path...), "--max-concurrent-gc=010", "--max-concurrent-gc", "-0b100")
		inv, err = registry.Parse(argv)
		last := inv.Options["max-concurrent-gc"]
		if err != nil || last.Value.Integer.String() != "-4" || last.FirstEncounter != 1 {
			t.Fatal(inv, err)
		}
		for _, bad := range []string{"08", "0o10", "0d10", "1__0", "0x_10", "10_", "_10", "1.0", "1e2", "", " 1", "1 ", "+"} {
			argv := append(append([]string{}, path...), "--max-concurrent-gc="+bad)
			if _, err := registry.Parse(argv); err == nil {
				t.Fatal(argv)
			}
		}
		if _, err := registry.Parse(append(append([]string{}, path...), "--max-concurrent-gc")); err == nil {
			t.Fatal("missing value")
		}
	}
}

func TestGenerationGCRegistrationPrecedesAllExecution(t *testing.T) {
	v := &gcTestView{}
	tasks := gcTasks("A", "B", "C")
	wantPrefix := []gcTestEvent{{"prepare", -1, ""}, {"open", 3, "Collecting garbage on 3 machines"}, {"register", 0, "A"}, {"register", 1, "B"}, {"register", 2, "C"}}
	execute := func(_ context.Context, m Machine, _ []string, _ func([]byte, []byte)) (Result, error) {
		events := v.snapshot()
		if len(events) < len(wantPrefix) || !reflect.DeepEqual(events[:len(wantPrefix)], wantPrefix) {
			t.Error(events)
		}
		v.record("execute", -1, m.Name)
		return Result{}, nil
	}
	if err := runGenerationGC(context.Background(), tasks, gcInt(3), gcFixture(v, true, execute)); err != nil {
		t.Fatal(err)
	}
	finishes := map[string]int{}
	for _, e := range v.snapshot() {
		if e.kind == "finish" {
			finishes[e.text]++
		}
	}
	if !reflect.DeepEqual(finishes, map[string]int{"A: done": 1, "B: done": 1, "C: done": 1}) {
		t.Fatal(finishes)
	}
}
