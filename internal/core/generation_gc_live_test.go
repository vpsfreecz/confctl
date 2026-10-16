package core

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/vpsfreecz/confctl/internal/ui"
)

type gcRecordingLive struct {
	mu       sync.Mutex
	rows     map[string]ui.LiveRow
	lines    []string
	closed   int
	closeErr error
	setErr   error
	lineErr  error
}

func gcCopyRow(row ui.LiveRow) ui.LiveRow {
	if row.Current != nil {
		row.Current = new(big.Int).Set(row.Current)
	}
	if row.Total != nil {
		row.Total = new(big.Int).Set(row.Total)
	}
	return row
}
func (v *gcRecordingLive) SetRows(rows ...ui.LiveRow) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.setErr != nil {
		return v.setErr
	}
	for _, row := range rows {
		v.rows[row.ID] = gcCopyRow(row)
	}
	return nil
}
func (v *gcRecordingLive) Advance(ids ...string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, id := range ids {
		row := v.rows[id]
		row.Current.Add(row.Current, big.NewInt(1))
	}
	return nil
}
func (v *gcRecordingLive) Line(line string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lines = append(v.lines, line)
	return v.lineErr
}
func (v *gcRecordingLive) WithPrompt(_ context.Context, body func() error) error { return body() }
func (v *gcRecordingLive) Flush(context.Context) error                           { return nil }
func (v *gcRecordingLive) Err() error                                            { return nil }
func (v *gcRecordingLive) Failed() <-chan struct{}                               { return nil }
func (v *gcRecordingLive) Close() error {
	v.closed++
	return v.closeErr
}

func gcLiveFixture() (*generationGCLiveView, *gcRecordingLive) {
	total := ui.LiveRow{ID: "gc-total", Label: "Deleted paths", Current: new(big.Int), State: ui.LivePending}
	live := &gcRecordingLive{rows: map[string]ui.LiveRow{total.ID: gcCopyRow(total)}}
	return &generationGCLiveView{live: live, total: total, hosts: make(map[int]ui.LiveRow)}, live
}

func TestGenerationGCLiveRowsCountUnknownTotalsAndActualResults(t *testing.T) {
	v, live := gcLiveFixture()
	for i, name := range []string{"alpha", "beta", "interrupted", "unstarted"} {
		if err := v.Register(i, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Begin(0); err != nil {
		t.Fatal(err)
	}
	// A count greater than MaxUint64 survives Advance and later snapshots.
	huge := new(big.Int).Lsh(big.NewInt(1), 100)
	v.hosts[0].Current.Set(huge)
	v.total.Current.Set(huge)
	if err := live.SetRows(v.hosts[0], v.total); err != nil {
		t.Fatal(err)
	}
	if err := v.Advance(0); err != nil {
		t.Fatal(err)
	}
	if err := v.Finish(0, "alpha: error occurred"); err != nil {
		t.Fatal(err)
	}
	if err := v.End(0, Result{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Begin(1); err != nil {
		t.Fatal(err)
	}
	if err := v.Finish(1, "beta: done"); err != nil {
		t.Fatal(err)
	}
	if err := v.End(1, Result{ExitCode: 23}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Begin(2); err != nil {
		t.Fatal(err)
	}
	if err := v.End(2, Result{}, context.Canceled); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := v.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	want := new(big.Int).Add(huge, big.NewInt(1))
	if live.closed != 1 || live.rows["gc-host-0"].Current.Cmp(want) != 0 || live.rows["gc-total"].Current.Cmp(want) != 0 {
		t.Fatal(live.closed, live.rows)
	}
	for id, state := range map[string]ui.LiveState{"gc-host-0": ui.LiveSucceeded, "gc-host-1": ui.LiveFailed, "gc-host-2": ui.LiveIncomplete, "gc-host-3": ui.LiveIncomplete, "gc-total": ui.LiveIncomplete} {
		row := live.rows[id]
		if row.State != state || row.Total != nil || row.Target != (id != "gc-total") {
			t.Fatal(id, row)
		}
	}
	if got := strings.Join(live.lines, "\n"); !strings.Contains(got, "1 succeeded, 1 failed, 2 incomplete") || !strings.Contains(got, want.String()) {
		t.Fatal(got)
	}
}

func TestGenerationGCLiveTypedPresentation(t *testing.T) {
	v, live := gcLiveFixture()
	for index, host := range []string{"node", "failed"} {
		if err := v.Register(index, host); err != nil {
			t.Fatal(err)
		}
		if err := v.Begin(index); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Advance(0); err != nil {
		t.Fatal(err)
	}
	if err := v.End(0, Result{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.End(1, Result{ExitCode: 23}, nil); err != nil {
		t.Fatal(err)
	}
	rows := []ui.LiveRow{live.rows["gc-total"], live.rows["gc-host-0"], live.rows["gc-host-1"]}
	header := ui.LiveHeader{Command: "confctl collect-garbage -y", Operation: "Collecting garbage", Target: "node", LogPath: "/tmp/gc-presentation.log"}
	for _, color := range []bool{false, true} {
		model, err := ui.NewLiveModel(header, rows, color)
		if err != nil {
			t.Fatal(err)
		}
		model.Line("node> \x1b[35mchild output\x1b[0m")
		frame := model.Frame(24, 80, 0)
		title, child, bottom, progress := -1, -1, -1, -1
		for index, line := range frame {
			plain := ansi.Strip(line)
			if ansi.StringWidth(line) > 80 {
				t.Fatal("GC frame exceeds terminal width", line)
			}
			switch {
			case strings.HasPrefix(plain, "Live view"):
				title = index
			case plain == "node> child output":
				child = index
				if strings.Contains(line, "\x1b[") {
					t.Fatal("child output acquired styling", line)
				}
			case strings.HasPrefix(plain, "<"):
				bottom = index
			case strings.HasPrefix(plain, "Deleted paths"):
				progress = index
			}
		}
		if len(frame) > 23 || title < 0 || title > 3 || child <= title || bottom <= child || progress <= bottom || bottom-title-1 < 12 {
			t.Fatal("GC log region must precede progress with twelve slots", frame)
		}
		text := strings.Join(model.Summary(0), "\n")
		if !strings.Contains(ansi.Strip(text), "1/?") || rows[0].Total != nil {
			t.Fatal("GC invented a known total", text)
		}
		if color {
			for _, styled := range []string{"\x1b[33mnode\x1b[0m", "\x1b[32msucceeded\x1b[0m", "\x1b[31mfailed\x1b[0m"} {
				if !strings.Contains(text, styled) {
					t.Fatal("missing target/state style", styled, text)
				}
			}
		} else if strings.Contains(text+strings.Join(frame, ""), "\x1b[") {
			t.Fatal("plain GC presentation contains styling", text, frame)
		}
	}
}

func TestGenerationGCLiveResultSignalsAndCleanupErrors(t *testing.T) {
	for _, result := range []Result{{ExitCode: 1}, {Signal: "terminated"}, {signal: 15}} {
		v, live := gcLiveFixture()
		if err := v.Register(0, "host"); err != nil {
			t.Fatal(err)
		}
		if err := v.End(0, result, nil); err != nil || live.rows["gc-host-0"].State != ui.LiveFailed {
			t.Fatal(result, live.rows, err)
		}
	}
	for _, where := range []string{"snapshot", "line", "close"} {
		v, live := gcLiveFixture()
		first, secondary := errors.New(where), errors.New("secondary close")
		live.closeErr = secondary
		switch where {
		case "snapshot":
			live.setErr = first
		case "line":
			live.lineErr = first
		case "close":
			live.closeErr = first
		}
		if err := v.Stop(); err != first || live.closed != 1 {
			t.Fatal(where, live.closed, err)
		}
		if err := v.Stop(); err != first || live.closed != 1 {
			t.Fatal("repeated stop", where, live.closed, err)
		}
	}
}

func TestGenerationGCLiveConcurrentCountsSurviveHostFinishes(t *testing.T) {
	v, live := gcLiveFixture()
	for index := 0; index < 4; index++ {
		if err := v.Register(index, "host"); err != nil {
			t.Fatal(err)
		}
	}
	var joined sync.WaitGroup
	for index := 0; index < 4; index++ {
		joined.Go(func() {
			if err := v.Begin(index); err != nil {
				t.Error(err)
				return
			}
			for range 10 {
				if err := v.Advance(index); err != nil {
					t.Error(err)
					return
				}
			}
			if err := v.Finish(index, "completed"); err != nil {
				t.Error(err)
			}
			if err := v.End(index, Result{}, nil); err != nil {
				t.Error(err)
			}
		})
	}
	joined.Wait()
	if err := v.Stop(); err != nil || live.rows["gc-total"].Current.Int64() != 40 || live.rows["gc-total"].State != ui.LiveSucceeded {
		t.Fatal(live.rows, err)
	}
	for _, row := range v.hosts {
		if live.rows[row.ID].Current.Int64() != 10 {
			t.Fatal(row)
		}
	}
}

type gcTaskEnd struct {
	index  int
	result Result
	err    error
}

type gcTaskLifecycleFixture struct {
	*gcTestView
	mu        sync.Mutex
	failure   chan struct{}
	err       error
	beginErr  error
	endErr    error
	begins    []int
	ends      []gcTaskEnd
	observing chan struct{}
	allow     <-chan struct{}
}

func (v *gcTaskLifecycleFixture) Begin(index int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.begins = append(v.begins, index)
	return v.beginErr
}
func (v *gcTaskLifecycleFixture) End(index int, result Result, err error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ends = append(v.ends, gcTaskEnd{index, result, err})
	return v.endErr
}
func (v *gcTaskLifecycleFixture) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}
func (v *gcTaskLifecycleFixture) Failed() <-chan struct{} {
	if v.observing != nil {
		v.observing <- struct{}{}
	}
	if v.allow != nil {
		<-v.allow
	}
	return v.failure
}
func (v *gcTaskLifecycleFixture) fail(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err == nil {
		v.err = err
		close(v.failure)
	}
}

func TestGenerationGCLiveTaskAdmissionAndEndOwnActualOutcome(t *testing.T) {
	boom, cleanup := errors.New("task failure"), errors.New("end failure")
	for _, mode := range []string{"prefailed", "begin", "success", "nonzero", "signal", "execute", "feed", "finish", "end"} {
		t.Run(mode, func(t *testing.T) {
			v := &gcTaskLifecycleFixture{gcTestView: &gcTestView{}, failure: make(chan struct{}), endErr: cleanup}
			result, bodyErr := Result{}, error(nil)
			switch mode {
			case "prefailed":
				v.fail(boom)
			case "begin":
				v.beginErr = boom
			case "nonzero":
				result.ExitCode = 23
			case "signal":
				result.Signal = "terminated"
			case "execute":
				bodyErr = boom
			case "feed":
				v.line = func(int, string) error { return boom }
			case "finish":
				v.finish = func(int, string) error { return boom }
			}
			if mode != "end" && mode != "execute" && mode != "feed" && mode != "finish" {
				v.endErr = nil
			}
			calls := 0
			ok, err := gcRunTask(context.Background(), 0, gcTask{Host: "host"}, func(ctx context.Context, _ Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
				calls++
				feed([]byte("live\n"), nil)
				if mode == "feed" && ctx.Err() != context.Canceled {
					t.Fatal("feed failure did not cancel owning child")
				}
				return result, bodyErr
			}, v, true)
			if mode == "prefailed" || mode == "begin" {
				if calls != 0 || len(v.ends) != 0 || err != boom {
					t.Fatal(calls, v.ends, err)
				}
				return
			}
			if calls != 1 || !reflect.DeepEqual(v.begins, []int{0}) || len(v.ends) != 1 || !reflect.DeepEqual(v.ends[0].result, result) {
				t.Fatal(calls, v.begins, v.ends)
			}
			wantErr := bodyErr
			if mode == "feed" || mode == "finish" {
				wantErr = boom
			}
			if v.ends[0].err != wantErr {
				t.Fatal("lost owning task error", v.ends)
			}
			if mode == "end" {
				wantErr = cleanup
			}
			if err != wantErr || ok != (result.ExitCode == 0 && result.Signal == "" && bodyErr == nil && mode != "feed" && mode != "finish") {
				t.Fatal(ok, err, wantErr)
			}
		})
	}
}

func TestGenerationGCLiveFailureCancelsOnlyActiveTaskContextsAndJoins(t *testing.T) {
	v := &gcTaskLifecycleFixture{gcTestView: &gcTestView{}, failure: make(chan struct{}), observing: make(chan struct{}, 2)}
	started, cancelled := make(chan int, 2), make(chan int, 2)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := gcRunWorkers(ctx, gcTasks("A", "B", "C"), 2, func(ctx context.Context, index int, task gcTask) (bool, error) {
			return gcRunTask(ctx, index, task, func(ctx context.Context, _ Machine, _ []string, _ func([]byte, []byte)) (Result, error) {
				started <- index
				<-ctx.Done()
				cancelled <- index
				<-release
				return Result{}, ctx.Err()
			}, v, true)
		})
		done <- err
	}()
	gcAwait(t, started)
	gcAwait(t, started)
	gcAwait(t, v.observing)
	gcAwait(t, v.observing)
	boom := errors.New("renderer failed")
	v.fail(boom)
	gcAwait(t, cancelled)
	gcAwait(t, cancelled)
	if ctx.Err() != nil {
		t.Fatal("presentation cancelled the parent")
	}
	select {
	case err := <-done:
		t.Fatal("claimed a join before children returned", err)
	default:
	}
	close(release)
	if err := gcAwait(t, done); err != boom {
		t.Fatal(err)
	}
	if len(v.ends) != 2 {
		t.Fatal("missing task teardown", v.ends)
	}
	for _, end := range v.ends {
		if end.err != boom {
			t.Fatal(end)
		}
	}
	select {
	case index := <-started:
		t.Fatal("admitted after failed view", index)
	default:
	}
}

func TestGenerationGCLiveFailurePreservesExecutionAndCapturePrimary(t *testing.T) {
	body, renderer := errors.New("execution failed"), errors.New("renderer failed")
	for _, mode := range []string{"operation", "capture-cancellation", "view-cancellation"} {
		t.Run(mode, func(t *testing.T) {
			v := &gcTaskLifecycleFixture{gcTestView: &gcTestView{}, failure: make(chan struct{})}
			want := body
			_, err := gcRunTask(context.Background(), 0, gcTask{Host: "host"}, func(context.Context, Machine, []string, func([]byte, []byte)) (Result, error) {
				v.fail(renderer)
				switch mode {
				case "capture-cancellation":
					want = context.Canceled
					return Result{capture: &captureCompletion{primary: context.Canceled}}, context.Canceled
				case "view-cancellation":
					want = renderer
					return Result{}, context.Canceled
				default:
					return Result{}, body
				}
			}, v, true)
			if err != want || len(v.ends) != 1 || v.ends[0].err != want {
				t.Fatal(v.ends, err, want)
			}
		})
	}
}

func TestGenerationGCLiveTaskJoinsObserverBeforeEnd(t *testing.T) {
	allow := make(chan struct{})
	defer func() {
		select {
		case <-allow:
		default:
			close(allow)
		}
	}()
	v := &gcTaskLifecycleFixture{gcTestView: &gcTestView{}, failure: make(chan struct{}), observing: make(chan struct{}, 1), allow: allow}
	done := make(chan error, 1)
	go func() {
		_, err := gcRunTask(context.Background(), 0, gcTask{Host: "host"}, func(context.Context, Machine, []string, func([]byte, []byte)) (Result, error) {
			return Result{}, nil
		}, v, true)
		done <- err
	}()
	gcAwait(t, v.observing)
	v.mu.Lock()
	ends := len(v.ends)
	v.mu.Unlock()
	if ends != 0 {
		t.Fatal("End ran before observer joined")
	}
	select {
	case err := <-done:
		t.Fatal("observer still owns a callback", err)
	default:
	}
	close(allow)
	if err := gcAwait(t, done); err != nil || len(v.ends) != 1 {
		t.Fatal(v.ends, err)
	}
}
