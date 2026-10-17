package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/build"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/ui"
)

type buildTestView struct {
	rows     []ui.LiveRow
	updates  [][]ui.LiveRow
	err      error
	closeErr error
	closed   int
	failed   chan struct{}
	model    *ui.LiveModel
}

func (v *buildTestView) SetRows(rows ...ui.LiveRow) error {
	if v.model != nil {
		if err := v.model.SetRows(rows...); err != nil {
			return err
		}
		rows = v.model.Rows()
	}
	v.rows = make([]ui.LiveRow, len(rows))
	for i, row := range rows {
		row.Current, row.Total = buildCount(row.Current), buildCount(row.Total)
		v.rows[i] = row
	}
	v.updates = append(v.updates, v.rows)
	return v.err
}

func buildPresentationRow(t *testing.T, v *buildTestView, id string) ui.LiveRow {
	t.Helper()
	for _, row := range v.rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing row %s: %+v", id, v.rows)
	return ui.LiveRow{}
}

func TestBuildPresentationCommandAttemptsRealModel(t *testing.T) {
	for _, next := range []string{"one family", "no progress"} {
		t.Run(next, func(t *testing.T) {
			f := newBuildFixture(t)
			v := &buildTestView{}
			p := f.e.buildOutputWithView(io.Discard, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
				var err error
				v.model, err = ui.NewLiveModel(spec.Header, spec.Rows, false)
				return v, err
			})
			// Evaluations before admission do not acquire a view or rows.
			if err := p.Command(buildCommandEvent{Op: "eval", Stage: buildCommandStarted}); err != nil || v.model != nil {
				t.Fatal(err)
			}
			if err := p.GroupStart(0, 1, build.Group{}); err != nil {
				t.Fatal(err)
			}
			emit := func(op string, stage buildCommandStage, target string) {
				t.Helper()
				if err := p.Command(buildCommandEvent{Op: op, Stage: stage, Installables: []string{target}}); err != nil {
					t.Fatal(err)
				}
			}
			emit("eval", buildCommandStarted, ".#confctl.inputs")
			emit("eval", buildCommandSucceeded, ".#confctl.inputs")
			emit("build", buildCommandStarted, "build-target")
			for _, family := range []string{"build", "fetch"} {
				if err := p.Progress(build.Event{Type: family, Progress: big.NewInt(1), Total: big.NewInt(2), Path: "/stale/" + family}); err != nil {
					t.Fatal(err)
				}
			}
			if len(v.model.Rows()) != 4 || buildPresentationRow(t, v, "observed").Current.Int64() != 2 {
				t.Fatal("aggregate/eval not composed in real model", v.model.Rows())
			}
			// The next Started is the same shape used by a recognized retry.
			emit("build", buildCommandStarted, "build-target")
			for _, row := range v.model.Rows() {
				if row.ID == "eval" {
					if row.State != ui.LiveSucceeded || row.Detail != ".#confctl.inputs" {
						t.Fatal("pre-build eval left running or lost outcome", row)
					}
				} else {
					want := ui.LivePending
					if row.ID == "observed" {
						want = ui.LiveRunning
					}
					if row.Current != nil || row.Total != nil || row.Detail != "" || row.State != want {
						t.Fatal("retry retained old snapshot or lost aggregate attempt state", row)
					}
				}
			}
			if next == "one family" {
				if err := p.Progress(build.Event{Type: "build", Progress: big.NewInt(1), Total: big.NewInt(0), Path: "/fresh"}); err != nil {
					t.Fatal(err)
				}
			}
			emit("build", buildCommandSucceeded, "build-target")
			for _, row := range v.model.Rows() {
				if row.ID == "eval" {
					continue
				}
				if row.ID == "build" && next == "one family" {
					if row.State != ui.LiveSucceeded || row.Current.Int64() != 1 || row.Total != nil || row.Detail != "/fresh" {
						t.Fatal("fresh observed counts changed", row)
					}
				} else if row.ID == "observed" {
					detail := ""
					if next == "one family" {
						detail = "Total unknown"
					}
					if row.State != ui.LiveSucceeded || row.Current != nil || row.Total != nil || row.Detail != detail {
						t.Fatal("admitted aggregate did not retain actual retry result", row)
					}
				} else if row.State != ui.LiveSkipped || row.Current != nil || row.Total != nil || row.Detail != "No steps observed" {
					t.Fatal("unobserved retry family retained stale data", row)
				}
			}
			for _, target := range []string{".#confctl.build", ".#confctl.inputsInfo"} {
				emit("eval", buildCommandStarted, target)
				row := buildPresentationRow(t, v, "eval")
				if len(v.model.Rows()) != 4 || row.State != ui.LiveRunning || row.Detail != target || row.Current != nil || row.Total != nil {
					t.Fatal("eval row duplicated or acquired counters", v.model.Rows())
				}
				emit("eval", buildCommandSucceeded, target)
			}
			before := v.model.Rows()
			if err := p.Close(errors.New("later generation failure")); err != nil || !reflect.DeepEqual(before, v.model.Rows()) {
				t.Fatal("close recolored completed children", err, before, v.model.Rows())
			}
		})
	}
}

func TestBuildTimingCanonicalRows(t *testing.T) {
	f := newBuildFixture(t)
	v := &buildTestView{}
	p := f.e.buildOutputWithView(io.Discard, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
		var err error
		v.model, err = ui.NewLiveModel(spec.Header, spec.Rows, false)
		return v, err
	})
	if err := p.GroupStart(0, 1, build.Group{}); err != nil {
		t.Fatal(err)
	}
	assertOrder := func(want []string) {
		t.Helper()
		var ids []string
		for _, row := range v.model.Rows() {
			ids = append(ids, row.ID)
			if row.DisplayOrder != map[string]int{"build": 10, "fetch": 20, "observed": 30, "eval": 40}[row.ID] {
				t.Fatal("build snapshot lost canonical rank", row)
			}
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatal("late aggregate/eval admission reordered rows", ids, want)
		}
		for _, height := range []int{8, 24} {
			last := 0
			for _, line := range v.model.Frame(height, 80, 0) {
				for i, label := range []string{"Building", "Fetching", "nix build", "nix eval"} {
					if strings.HasPrefix(line, label+" ") {
						if i+1 <= last {
							t.Fatal("priority selection rendered out of canonical order", height, v.model.Frame(height, 80, 0))
						}
						last = i + 1
					}
				}
			}
		}
	}
	command := func(op string, stage buildCommandStage) {
		t.Helper()
		if err := p.Command(buildCommandEvent{Op: op, Stage: stage, Installables: []string{".#confctl.inputs"}}); err != nil {
			t.Fatal(err)
		}
	}
	command("eval", buildCommandStarted)
	assertOrder([]string{"build", "fetch", "eval"})
	command("eval", buildCommandSucceeded)
	command("build", buildCommandStarted)
	for _, family := range []string{"fetch", "build"} {
		if err := p.Progress(build.Event{Type: family, Progress: big.NewInt(1), Total: big.NewInt(2)}); err != nil {
			t.Fatal(err)
		}
	}
	assertOrder([]string{"build", "fetch", "observed", "eval"})
	command("build", buildCommandSucceeded)
	command("eval", buildCommandStarted)
	assertOrder([]string{"build", "fetch", "observed", "eval"})
	command("eval", buildCommandSucceeded)
	if err := p.Close(nil); err != nil {
		t.Fatal(err)
	}
	assertOrder([]string{"build", "fetch", "observed", "eval"})
}

func TestBuildPresentationChildOutcomeSurvivesLaterErrors(t *testing.T) {
	for _, failure := range []string{"decode", "outputs", "metadata", "save", "current"} {
		t.Run(failure, func(t *testing.T) {
			f := newBuildFixture(t)
			v := &buildTestView{}
			p := f.e.buildOutputWithView(io.Discard, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
				return v, v.SetRows(spec.Rows...)
			})
			deps := f.deps()
			deps.Run = func(argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
				target := argv[len(argv)-1]
				if failure == "outputs" && target == ".#confctl.build" || failure == "metadata" && target == ".#confctl.inputsInfo" {
					return Result{ExitCode: 7}, nil
				}
				result, err := f.run(argv, input, chunk)
				if failure == "decode" && argv[1] == "build" {
					result.Stdout = []byte("invalid JSON")
				}
				return result, err
			}
			if failure == "save" {
				deps.RootDir = func() (string, error) { return "", errors.New("save root failure") }
			}
			if failure == "current" {
				if err := os.MkdirAll(filepath.Join(f.e.Root, ".confctl/generations/one/current"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.e.buildSelected([]string{"one"}, buildOptions{}, p, deps)
			if err == nil || out.Complete || v.closed != 1 {
				t.Fatal("late failure or cleanup lost", out, err, v.closed)
			}
			built, fetched := buildPresentationRow(t, v, "build"), buildPresentationRow(t, v, "fetch")
			if built.State != ui.LiveSucceeded || built.Current.Int64() != 1 || fetched.State != ui.LiveSkipped || fetched.Current != nil {
				t.Fatal("late failure recolored completed build or forced counts", built, fetched)
			}
			if failure != "decode" {
				want := ui.LiveSucceeded
				if failure == "outputs" || failure == "metadata" {
					want = ui.LiveFailed
				}
				if row := buildPresentationRow(t, v, "eval"); row.State != want {
					t.Fatal("eval child outcome lost", row)
				}
			}
		})
	}
}
func (v *buildTestView) Advance(...string) error { panic("build must use snapshots") }
func (v *buildTestView) Line(string) error       { panic("build must use raw tail") }
func (v *buildTestView) WithPrompt(context.Context, func() error) error {
	panic("build must not own prompts")
}
func (v *buildTestView) Flush(context.Context) error { return v.err }
func (v *buildTestView) Err() error                  { return v.err }
func (v *buildTestView) Failed() <-chan struct{}     { return v.failed }
func (v *buildTestView) Close() error                { v.closed++; return v.closeErr }

func TestBuildPresentationSnapshotsAndFinalOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failed", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			e := newBuildFixture(t).e
			v := &buildTestView{}
			var out, stderr bytes.Buffer
			prelude := "Building machines\n  host\n    with inputs\n      nixpkgs=/input path\n"
			p := e.buildOutputWithView(&out, &stderr, nil, func(ctx context.Context, stdout, errout io.Writer, spec liveViewSpec, onError func(error)) (liveView, error) {
				if stdout != &out || errout != &stderr || ctx != e.Context || !spec.RawLog || len(spec.Rows) != 2 {
					t.Fatal("lost original streams/context/raw binding", spec)
				}
				if spec.Header != e.liveHeader("Build group", "2/3 (1 machines)") || out.String() != prelude {
					t.Fatal("compact header or pre-acquisition context lost", spec.Header, out.String())
				}
				return v, v.SetRows(spec.Rows...)
			})
			group := build.Group{Hosts: []build.HostPlan{{Host: "host", Inputs: generation.Object{{Name: "nixpkgs", Value: "/input path"}}}}}
			if err := p.GroupStart(1, 3, group); err != nil {
				t.Fatal(err)
			}
			count := new(big.Int).Lsh(big.NewInt(1), 100)
			wantCount := count.String()
			if err := p.Progress(build.Event{Type: "build", Progress: count, Total: big.NewInt(0), Path: "/drv"}); err != nil {
				t.Fatal(err)
			}
			count.SetInt64(1)
			if v.rows[0].Current.String() != wantCount || v.rows[0].Total != nil || v.rows[0].State != ui.LiveRunning || len(v.rows) != 2 {
				t.Fatal("unknown/huge count or observed state lost", v.rows)
			}
			for _, event := range []build.Event{
				{Type: "build", Progress: big.NewInt(1), Total: big.NewInt(2), Path: "/drv"},
				{Type: "fetch", Progress: big.NewInt(1), Total: big.NewInt(3), Path: "/path"},
			} {
				if err := p.Progress(event); err != nil {
					t.Fatal(err)
				}
			}
			if len(v.rows) != 3 || v.rows[2].Current.String() != "2" || v.rows[2].Total.String() != "5" || v.rows[0].State != ui.LiveRunning || v.rows[0].Label != "Building" || v.rows[1].Label != "Fetching" || v.rows[2].Label != "nix build" || v.rows[0].Detail != "/drv" {
				t.Fatal("aggregate or premature success", v.rows)
			}
			var bodyErr error
			state := ui.LiveSucceeded
			if outcome == "failed" {
				bodyErr, state = errors.New("child failed"), ui.LiveFailed
			} else if outcome == "canceled" {
				bodyErr, state = context.Canceled, ui.LiveIncomplete
			}
			if err := p.Close(bodyErr); err != nil || v.closed != 1 {
				t.Fatal(err, v.closed)
			}
			for _, row := range v.rows {
				if row.State != state {
					t.Fatal("body outcome lost", row)
				}
			}
			if err := p.Close(bodyErr); err != nil || v.closed != 1 || out.String() != prelude || stderr.Len() != 0 {
				t.Fatal("duplicate close or competing textual writer", err, v.closed, out.String(), stderr.String())
			}
		})
	}
}

func TestBuildPresentationManyHostPrelude(t *testing.T) {
	for _, count := range []int{2, 20, 100} {
		for _, common := range []bool{false, true} {
			t.Run(fmt.Sprintf("hosts=%d/common=%t", count, common), func(t *testing.T) {
				f := newBuildFixture(t)
				f.e.liveCommand = "confctl status -y"
				if err := f.e.OpenLog("status"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.e.CloseLog(false) })
				group := build.Group{}
				var want strings.Builder
				want.WriteString("Building machines\n")
				for i := range count {
					host := build.HostPlan{Host: fmt.Sprintf("host-%03d", i)}
					inputs := generation.Object{}
					fmt.Fprintf(&want, "  %s\n", host.Host)
					if !common {
						want.WriteString("    with inputs\n")
					}
					for _, name := range []string{"nixpkgs", "vpsadminos", "site"} {
						path := "/inputs/" + host.Host + "/" + name
						inputs = append(inputs, generation.Member{Name: name, Value: path})
						if !common {
							fmt.Fprintf(&want, "      %s=%s\n", name, path)
						}
					}
					host.Inputs = inputs
					group.Hosts = append(group.Hosts, host)
				}
				if common {
					group.Inputs = generation.Object{{Name: "nixpkgs", Value: "/shared/input"}}
					want.WriteString("with inputs\n  nixpkgs=/shared/input\n")
				}
				v := &buildTestView{}
				var out bytes.Buffer
				p := f.e.buildOutputWithView(&out, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
					if out.String() != want.String() {
						t.Fatal("complete once-only prelude must precede view", out.String())
					}
					if spec.Header != f.e.liveHeader("Build group", fmt.Sprintf("2/3 (%d machines)", count)) {
						t.Fatal("details leaked into persistent header", spec.Header)
					}
					model, err := ui.NewLiveModel(spec.Header, spec.Rows, false)
					if err != nil {
						t.Fatal(err)
					}
					header := model.Header()
					if len(header) != 3 || header[0] != "Command: confctl status -y" || header[1] != fmt.Sprintf("Build group: 2/3 (%d machines)", count) || header[2] != "Full log: "+f.e.Log.Name() {
						t.Fatal("compact header height/context changed", header)
					}
					for _, text := range []string{strings.Join(header, "\n"), strings.Join(model.Frame(24, 80, 0), "\n"), strings.Join(model.Summary(0), "\n")} {
						if strings.Contains(text, "host-") || strings.Contains(text, "/inputs/") || strings.Contains(text, "/shared/input") || strings.Contains(text, "Building machines") {
							t.Fatal("prelude retained for repaint or teardown", text)
						}
					}
					return v, v.SetRows(spec.Rows...)
				})
				if err := p.GroupStart(1, 3, group); err != nil {
					t.Fatal(err)
				}
				if len(v.updates) != 1 || len(v.rows) != 2 {
					t.Fatal("group context changed progress events", v.updates)
				}
				for _, row := range v.rows {
					if row.State != ui.LivePending || row.Current != nil || row.Total != nil {
						t.Fatal("group population became observed build progress", row)
					}
				}
				if err := p.Close(nil); err != nil || v.closed != 1 || out.String() != want.String() || len(f.calls) != 0 || f.rootCalls != 0 {
					t.Fatal("presentation changed operation lifecycle or replayed prelude", err, v.closed, f.calls, f.rootCalls)
				}
			})
		}
	}
}

func TestBuildPresentationGroupLifecycleAndFailures(t *testing.T) {
	for _, failure := range []string{"", "acquire", "body", "view", "close"} {
		t.Run(failure, func(t *testing.T) {
			f := newBuildFixture(t)
			f.plan = `{"one":{"key":"k1","inputs":{"os":"/one"}},"two":{"key":"k2","inputs":{"os":"/two"}}}`
			ctx, cancel := context.WithCancel(f.e.Context)
			defer cancel()
			f.e.Context = ctx
			sentinel := errors.New("fixture " + failure)
			var views []*buildTestView
			var failView func(error)
			p := f.e.buildOutputWithView(io.Discard, io.Discard, cancel, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, onError func(error)) (liveView, error) {
				if len(views) > 0 && views[len(views)-1].closed != 1 {
					t.Fatal("next group overlaps previous view")
				}
				if failure == "acquire" {
					return nil, sentinel
				}
				v := &buildTestView{}
				_ = v.SetRows(spec.Rows...)
				views = append(views, v)
				failView = onError
				if failure == "close" {
					v.closeErr = sentinel
				}
				return v, nil
			})
			deps := f.deps()
			deps.Run = func(argv []string, in []byte, chunk func([]byte, []byte) error) (Result, error) {
				if argv[len(argv)-1] == ".#confctl.settings" {
					return Result{Stdout: []byte(`{"nix":{"legacyNixPath":true,"impureEval":true}}`)}, nil
				}
				if argv[len(argv)-1] == ".#confctl.inputs" {
					return Result{Stdout: []byte(`{"k1":{"os":"/one"},"k2":{"os":"/two"}}`)}, nil
				}
				if argv[1] == "build" {
					if failure == "body" {
						return Result{}, sentinel
					}
					if failure == "view" {
						views[len(views)-1].err = sentinel
						failView(sentinel)
						if ctx.Err() != context.Canceled {
							t.Fatal("view failure did not cancel child context")
						}
						return Result{}, ctx.Err()
					}
				}
				return f.run(argv, in, chunk)
			}
			outcome, err := f.e.buildSelected([]string{"one", "two"}, buildOptions{}, p, deps)
			if failure == "" {
				if err != nil || !outcome.Complete || len(views) != 2 || views[0].rows[0].State != ui.LiveSucceeded {
					t.Fatal(outcome, err, views)
				}
			} else if !errors.Is(err, sentinel) || outcome.Complete || len(views) > 1 {
				t.Fatal("failure lost or advanced next group", outcome, err, len(views))
			}
			for _, v := range views {
				if v.closed != 1 {
					t.Fatal("view cleanup not exactly once", v.closed)
				}
			}
		})
	}
}

func TestBuildPresentationNoEventsAndPlainFallback(t *testing.T) {
	for _, bodyErr := range []error{nil, errors.New("before first step")} {
		e := newBuildFixture(t).e
		v := &buildTestView{}
		p := e.buildOutputWithView(io.Discard, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
			return v, v.SetRows(spec.Rows...)
		})
		_ = p.GroupStart(0, 1, build.Group{})
		if err := p.Close(bodyErr); err != nil {
			t.Fatal(err)
		}
		want := ui.LiveSkipped
		if bodyErr != nil {
			want = ui.LiveIncomplete
		}
		for _, row := range v.rows {
			if row.State != want || row.Current != nil || row.Total != nil {
				t.Fatal("unobserved work became counted success", row)
			}
		}
	}
	f := newBuildFixture(t)
	var out, stderr bytes.Buffer
	result, err := f.e.buildWithPresentation([]string{"one", "two"}, buildOptions{}, &out, &stderr, f.deps())
	if err != nil || !result.Complete || !reflect.DeepEqual(f.calls[:2], []string{".#confctl.settings", ".#confctl.buildPlan"}) {
		t.Fatal(result, err, f.calls)
	}
	for _, want := range []string{"Evaluating inputs for 2", "1 groups", "Build group: 1/1", "one", "two", "nixpkgs=/synthetic/input"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing plain context", want, out.String())
		}
	}
	if !strings.Contains(stderr.String(), "build 1/0") || strings.Contains(out.String()+stderr.String(), "\x1b") {
		t.Fatal("plain progress stream changed", out.String(), stderr.String())
	}
	want := "Evaluating inputs for 2 machines...\n\nMachines will be built in 1 groups\n\nBuilding machines\n  one\n    with inputs\n      nixpkgs=/synthetic/input\n  two\n    with inputs\n      nixpkgs=/synthetic/input\nBuild group: 1/1 (2 machines)\n"
	if out.String() != want || stderr.String() != "build 1/0 /synthetic/build.drv\n" {
		t.Fatal("command observations changed nonTTY bytes", out.String(), stderr.String())
	}
}

func TestBuildPresentationCommandFailureStates(t *testing.T) {
	for _, cause := range []error{errors.New("child failed"), context.Canceled, context.DeadlineExceeded} {
		f := newBuildFixture(t)
		v := &buildTestView{}
		p := f.e.buildOutputWithView(io.Discard, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
			return v, v.SetRows(spec.Rows...)
		})
		if err := p.GroupStart(0, 1, build.Group{}); err != nil {
			t.Fatal(err)
		}
		if err := p.Command(buildCommandEvent{Op: "build", Stage: buildCommandStarted}); err != nil {
			t.Fatal(err)
		}
		for _, family := range []string{"build", "fetch"} {
			if err := p.Progress(build.Event{Type: family, Progress: big.NewInt(1), Total: big.NewInt(2), Path: "/previous/" + family}); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Command(buildCommandEvent{Op: "build", Stage: buildCommandStarted}); err != nil {
			t.Fatal(err)
		}
		if err := p.Progress(build.Event{Type: "build", Progress: big.NewInt(2), Total: big.NewInt(9), Path: "/partial"}); err != nil {
			t.Fatal(err)
		}
		if err := p.Command(buildCommandEvent{Op: "build", Stage: buildCommandFailed, Err: cause}); err != nil {
			t.Fatal(err)
		}
		want := ui.LiveFailed
		if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			want = ui.LiveIncomplete
		}
		row := buildPresentationRow(t, v, "build")
		if row.State != want || row.Current.Int64() != 2 || row.Total.Int64() != 9 || buildPresentationRow(t, v, "fetch").State != ui.LiveIncomplete {
			t.Fatal("failed child count/state lost", v.rows)
		}
		aggregate := buildPresentationRow(t, v, "observed")
		if aggregate.State != want || aggregate.Current != nil || aggregate.Total != nil || aggregate.Detail != "Total unknown" {
			t.Fatal("admitted aggregate lost failed/canceled retry outcome", aggregate)
		}
		if err := p.Close(nil); err != nil || buildPresentationRow(t, v, "build").State != want {
			t.Fatal("close overrode terminal command outcome", err, v.rows)
		}
	}
}

func TestBuildPresentationEvalFailureKeepsBuildSuccess(t *testing.T) {
	for _, cause := range []error{errors.New("eval failed"), context.Canceled} {
		f := newBuildFixture(t)
		v := &buildTestView{}
		p := f.e.buildOutputWithView(io.Discard, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
			return v, v.SetRows(spec.Rows...)
		})
		if err := p.GroupStart(0, 1, build.Group{}); err != nil {
			t.Fatal(err)
		}
		if err := p.Progress(build.Event{Type: "build", Progress: big.NewInt(1), Total: big.NewInt(3)}); err != nil {
			t.Fatal(err)
		}
		for _, event := range []buildCommandEvent{
			{Op: "build", Stage: buildCommandSucceeded},
			{Op: "eval", Stage: buildCommandStarted, Installables: []string{".#confctl.build"}},
			{Op: "eval", Stage: buildCommandFailed, Installables: []string{".#confctl.build"}, Err: cause},
		} {
			if err := p.Command(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Close(cause); err != nil {
			t.Fatal(err)
		}
		built, eval := buildPresentationRow(t, v, "build"), buildPresentationRow(t, v, "eval")
		want := ui.LiveFailed
		if cause == context.Canceled {
			want = ui.LiveIncomplete
		}
		if built.State != ui.LiveSucceeded || built.Current.Int64() != 1 || built.Total.Int64() != 3 || eval.State != want || eval.Detail != ".#confctl.build" || eval.Current != nil || eval.Total != nil {
			t.Fatal("later eval failed/canceled state corrupted completion", v.rows)
		}
	}
}

type buildPreludeFailure struct{ err error }

func (w buildPreludeFailure) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	return len(b) - 1, nil
}

func TestBuildPresentationPreludeWriteFailure(t *testing.T) {
	sentinel := errors.New("prelude writer failed")
	for _, failure := range []error{sentinel, nil} {
		f := newBuildFixture(t)
		opened := false
		p := f.e.buildOutputWithView(buildPreludeFailure{failure}, io.Discard, nil,
			func(context.Context, io.Writer, io.Writer, liveViewSpec, func(error)) (liveView, error) {
				opened = true
				t.Fatal("view acquired after failed prelude")
				return nil, nil
			})
		// Isolate the group prelude from the earlier textual phase messages.
		p.Evaluating, p.GroupsPrepared = nil, nil
		outcome, err := f.e.buildSelected([]string{"one"}, buildOptions{}, p, f.deps())
		want := failure
		if want == nil {
			want = io.ErrShortWrite
		}
		if !errors.Is(err, want) || opened || outcome.Complete || containsBuildArg(f.calls, "build") || f.rootCalls != 0 {
			t.Fatal("prelude failure advanced acquisition or build", outcome, err, opened, f.calls, f.rootCalls)
		}
	}
}

func TestBuildPresentationPreludeAndSummaryStyles(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, color := range []string{"", "auto", "never", "always"} {
		t.Run(color, func(t *testing.T) {
			f := newBuildFixture(t)
			f.e.Color = color
			var out bytes.Buffer
			p := f.e.buildOutputWithView(&out, io.Discard, nil, func(_ context.Context, _, _ io.Writer, spec liveViewSpec, _ func(error)) (liveView, error) {
				return &buildTestView{}, nil
			})
			group := build.Group{Hosts: []build.HostPlan{{Host: "host", Inputs: generation.Object{{Name: "os", Value: "/plain/input"}}}}}
			if err := p.GroupStart(0, 1, group); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(nil); err != nil {
				t.Fatal(err)
			}
			prelude := "Building machines\n  host\n    with inputs\n      os=/plain/input\n"
			if color == "always" {
				prelude = "\x1b[1mBuilding machines\x1b[0m\n  \x1b[33mhost\x1b[0m\n    with inputs\n      os=/plain/input\n"
			}
			if out.String() != prelude {
				t.Fatal("prelude style policy or plain input changed", out.String())
			}
			out.Reset()
			outcome := build.Outcome{Generations: []build.HostGeneration{
				{Host: "two", Generation: &generation.Build{Name: "second"}},
				{Host: "one", Generation: &generation.Build{Name: "first"}},
			}}
			if err := f.e.writeBuildSummary(&out, outcome); err != nil {
				t.Fatal(err)
			}
			want := "\nBuilt generations:\n  two: second\n  one: first\n"
			if color == "always" {
				want = "\n\x1b[1mBuilt generations:\x1b[0m\n  \x1b[33mtwo\x1b[0m: \x1b[36msecond\x1b[0m\n  \x1b[33mone\x1b[0m: \x1b[36mfirst\x1b[0m\n"
			}
			if out.String() != want {
				t.Fatal("summary order/content/style changed", out.String())
			}
			if err := f.e.writeBuildSummary(buildPreludeFailure{}, outcome); err != io.ErrShortWrite {
				t.Fatal("summary lost checked write", err)
			}
		})
	}
}
