package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/ui"
)

type deployViewFixture struct {
	mu      sync.Mutex
	rows    map[string]ui.LiveRow
	lines   []string
	prompts int
	err     error
	failed  chan struct{}
}

func (v *deployViewFixture) SetRows(rows ...ui.LiveRow) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, row := range rows {
		row.Current, row.Total = deployCopyCount(row.Current), deployCopyCount(row.Total)
		v.rows[row.ID] = row
	}
	return v.err
}
func (v *deployViewFixture) Advance(...string) error { panic("copy uses exact snapshots") }
func (v *deployViewFixture) Line(line string) error  { v.lines = append(v.lines, line); return v.err }
func (v *deployViewFixture) WithPrompt(_ context.Context, body func() error) error {
	v.prompts++
	return body()
}
func (v *deployViewFixture) Flush(context.Context) error { return v.err }
func (v *deployViewFixture) Err() error                  { return v.err }
func (v *deployViewFixture) Failed() <-chan struct{}     { return v.failed }
func (v *deployViewFixture) Close() error                { return v.err }

func deployPresentedFixture(t *testing.T) (*deployPresentation, *deployViewFixture) {
	t.Helper()
	v := &deployViewFixture{rows: map[string]ui.LiveRow{}, failed: make(chan struct{})}
	p := &deployPresentation{view: v, stdout: io.Discard, stderr: io.Discard, rows: map[string]ui.LiveRow{}}
	for _, row := range copyLiveRows([]deploy.Target{{Host: "a"}, {Host: "b"}}) {
		p.rows[row.ID] = row
		p.order = append(p.order, row.ID)
	}
	return p, v
}

func TestDeployPresentationCopyResultsAndExactSnapshots(t *testing.T) {
	p, v := deployPresentedFixture(t)
	if p.rows["copy:all"].Target || !p.rows["copy:a"].Target || !p.rows["copy:b"].Target {
		t.Fatal("machine styling roles changed aggregate membership", p.rows)
	}
	current, total := big.NewInt(2), new(big.Int)
	total.SetString("999999999999999999999999", 10)
	p.event(deploy.Event{Kind: deploy.CopyAdvanced, Host: "a", Progress: &deploy.CopyProgress{Path: "/first", Current: current, Total: total}})
	current.SetInt64(0)
	total.SetInt64(0)
	if row := v.rows["copy:a"]; row.Current.String() != "2" || row.Total.String() != "999999999999999999999999" || row.Detail != "/first" || v.rows["copy:all"].Total != nil {
		t.Fatal("snapshot alias or unknown aggregate total", row)
	}
	d := p.decorate(deploy.Deps{Copy: func(_ context.Context, m deploy.Machine, _ []string, _ func(deploy.CopyProgress)) (bool, error) {
		return m.Name == "b", nil
	}})
	if ok, err := d.Copy(context.Background(), deploy.Machine{Name: "a"}, nil, nil); ok || err != nil {
		t.Fatal(ok, err)
	}
	d.Event(deploy.Event{Kind: deploy.CopyFinished, Host: "a"})
	if v.rows["copy:a"].State != ui.LiveFailed || !v.rows["copy:a"].Target {
		t.Fatal("CopyFinished invented success")
	}
	// A later exact observation replaces a previously known total.
	p.event(deploy.Event{Kind: deploy.CopyAdvanced, Host: "b", Progress: &deploy.CopyProgress{Current: big.NewInt(3), Total: big.NewInt(7)}})
	p.event(deploy.Event{Kind: deploy.CopyAdvanced, Host: "a", Progress: &deploy.CopyProgress{Current: big.NewInt(2), Total: nil}})
	if v.rows["copy:all"].Current.String() != "5" || v.rows["copy:all"].Total != nil {
		t.Fatal("aggregate guessed a total")
	}
	if ok, err := d.Copy(context.Background(), deploy.Machine{Name: "b"}, nil, nil); !ok || err != nil || v.rows["copy:all"].State != ui.LiveFailed {
		t.Fatal("actual failed host was lost", ok, err)
	}
	if len(v.lines) == 0 {
		t.Fatal("semantic copy progress was lost")
	}
}

func TestDeployPresentationTypedContextAndStyles(t *testing.T) {
	log, err := os.CreateTemp(t.TempDir(), "deploy.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	engine := &Engine{Log: log, liveCommand: "confctl deploy -gselected"}
	clone := *engine
	p := &deployPresentation{engine: &clone}
	for _, test := range []struct {
		operation, target, action, generation, want string
	}{
		{"Copying closures", "", "", "", "Copying closures:"},
		{"Copying closures", "node", "", "", "Copying closures: node"},
		{"Activating", "node", "switch", "selected", "Activating: node (switch; selected)"},
		{"Rebooting", "node", "boot", "", "Rebooting: node (boot)"},
	} {
		header := p.header(test.operation, test.target, test.action, test.generation)
		if header.Command != engine.liveCommand || header.LogPath != log.Name() || header.Operation != test.operation || header.Detail != "" || header.Target != test.target || header.Action != test.action || header.Generation != test.generation {
			t.Fatal("deployment context lost its typed role", header)
		}
		model, err := ui.NewLiveModel(header, nil, false)
		if err != nil || !reflect.DeepEqual(model.Header(), []string{"Command: " + engine.liveCommand, test.want, "Full log: " + log.Name()}) {
			t.Fatal("deployment header is not compact", header, err)
		}
	}
	// A direct Engine has no invented command or empty log locator.
	plain := &deployPresentation{engine: &Engine{}}
	model, err := ui.NewLiveModel(plain.header("Copying closures", "node", "", ""), nil, false)
	if err != nil || !reflect.DeepEqual(model.Header(), []string{"Copying closures: node"}) {
		t.Fatal("injected deployment invented invocation context", err)
	}

	model, err = ui.NewLiveModel(p.header("Activating", "node", "switch", "selected"), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	header := model.Header()[1]
	for _, styled := range []string{"\x1b[1mActivating:\x1b[0m", "\x1b[33mnode\x1b[0m", "\x1b[33mswitch\x1b[0m", "\x1b[36mselected\x1b[0m"} {
		if !strings.Contains(header, styled) {
			t.Fatal("deployment field style missing", styled, header)
		}
	}
	rows := copyLiveRows([]deploy.Target{{Host: "first"}, {Host: "second"}})
	rows[0].State, rows[1].State, rows[2].State = ui.LiveFailed, ui.LiveSucceeded, ui.LiveFailed
	model, err = ui.NewLiveModel(p.header("Copying closures", "", "", ""), rows, true)
	if err != nil {
		t.Fatal(err)
	}
	summary := strings.Join(model.Summary(0), "\n")
	for _, styled := range []string{"\x1b[33mfirst\x1b[0m", "\x1b[33msecond\x1b[0m", "\x1b[32msucceeded\x1b[0m", "\x1b[31mfailed\x1b[0m"} {
		if !strings.Contains(summary, styled) {
			t.Fatal("deployment row role/state style missing", styled, summary)
		}
	}
	model.Line("\x1b[31mchild-plain\x1b[0m")
	frame := model.Frame(24, 80, 0)
	text := ansi.Strip(strings.Join(frame, "\n"))
	top, child := strings.Index(text, "Live view"), strings.Index(text, "child-plain")
	bottom, progress := strings.Index(text, "<---"), strings.Index(text, "All hosts [failed]")
	if top < 0 || child <= top || bottom <= child || progress <= bottom {
		t.Fatal("deployment log was not above progress", text)
	}
	for _, line := range frame {
		if ansi.StringWidth(line) > 80 || strings.Contains(line, "child-plain") && strings.Contains(line, "\x1b") {
			t.Fatal("deployment frame width or child styling changed", line)
		}
	}
}

func TestDeployPresentationFailureRefusesLaterWorkAndPrompts(t *testing.T) {
	p, v := deployPresentedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.cancel = cancel
	cause := errors.New("presentation output failed")
	v.err = cause
	called := false
	d := p.decorate(deploy.Deps{
		Copy: func(context.Context, deploy.Machine, []string, func(deploy.CopyProgress)) (bool, error) {
			called = true
			return true, nil
		},
		Confirm: func(context.Context, deploy.Confirmation) (bool, error) { called = true; return true, nil },
	})
	if _, err := d.Copy(ctx, deploy.Machine{Name: "a"}, nil, nil); err != cause || called || ctx.Err() != context.Canceled {
		t.Fatal("failed view admitted child work", err, called)
	}
	if _, err := d.Confirm(ctx, deploy.Confirmation{}); err != cause || called || v.prompts != 0 {
		t.Fatal("failed view admitted input", err)
	}
	body := errors.New("independent process log error")
	if p.result(body) != body || p.result(context.Canceled) != cause {
		t.Fatal("body or presentation error precedence changed")
	}
}

func TestDeployPresentationNoopZeroAndIncomplete(t *testing.T) {
	p, v := deployPresentedFixture(t)
	cause := errors.New("copy transport stopped")
	d := p.decorate(deploy.Deps{Copy: func(_ context.Context, m deploy.Machine, _ []string, _ func(deploy.CopyProgress)) (bool, error) {
		if m.Localhost {
			return true, nil
		}
		return true, cause
	}})
	d.Event(deploy.Event{Kind: deploy.CopyAdvanced, Host: "a", Progress: &deploy.CopyProgress{Current: new(big.Int), Total: new(big.Int)}})
	if ok, err := d.Copy(context.Background(), deploy.Machine{Name: "a", Localhost: true}, nil, nil); !ok || err != nil || v.rows["copy:a"].State != ui.LiveSkipped || v.rows["copy:a"].Current.Sign() != 0 || v.rows["copy:a"].Total.Sign() != 0 {
		t.Fatal("localhost no-op or known zero changed", ok, err)
	}
	if ok, err := d.Copy(context.Background(), deploy.Machine{Name: "b"}, nil, nil); !ok || err != cause || v.rows["copy:b"].State != ui.LiveIncomplete || v.rows["copy:all"].State != ui.LiveFailed {
		t.Fatal("error accompanied by true result invented success", ok, err)
	}
}

func TestDeployPresentationTextKeepsPathAndUnboundedCount(t *testing.T) {
	var out bytes.Buffer
	p := &deployPresentation{stdout: &out}
	current := new(big.Int)
	current.SetString("999999999999999999999999", 10)
	p.event(deploy.Event{Kind: deploy.CopyAdvanced, Host: "node", Progress: &deploy.CopyProgress{Current: current, Path: "/fixture/path"}})
	if !strings.Contains(out.String(), "node: copy-progress 999999999999999999999999 /fixture/path") || strings.Contains(out.String(), "\x1b") || p.result(nil) != nil {
		t.Fatal("plain copy progress lost context/count/path", out.String())
	}
}

func TestDeployPresentationActivationPromptUsesExistingChoice(t *testing.T) {
	p, v := deployPresentedFixture(t)
	p.raw = true
	p.rows = map[string]ui.LiveRow{"activation:a": {ID: "activation:a", Label: "a", State: ui.LiveRunning}}
	p.order = []string{"activation:a"}
	_, line := commandInput(strings.NewReader("r\ns\n"))
	deps := (&Engine{Context: context.Background()}).deployOperationsWithOps(context.Background(), nil, nil, line, io.Discard, io.Discard, deployInvocation(t), deployProcessOps{})
	d := p.decorate(deps)
	for _, want := range []deploy.ActivationChoice{deploy.ActivationRetry, deploy.ActivationSkip} {
		choice, err := d.ChooseActivation(context.Background(), deploy.Target{Host: "a"}, true)
		if err != nil || choice != want {
			t.Fatal("existing choice order changed", choice, err)
		}
	}
	d.Event(deploy.Event{Kind: deploy.ActivationSkipped, Host: "a"})
	if v.prompts != 2 || len(v.lines) != 0 || v.rows["activation:a"].State != ui.LiveSkipped {
		t.Fatal("raw view mixed semantic lines or missed prompt/skip")
	}
}

func TestDeployRebootObservationsPreservePollsAndSleeps(t *testing.T) {
	for _, mode := range []string{"up", "down-sequence", "timeout", "cancel", "observer-error", "nowait"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Unix(100, 0)
			var effects, states []string
			var snapshots []rebootProgress
			reads := 0
			cause := errors.New("observer write failed")
			ops := deployProcessOps{Now: func() time.Time { return now }, Sleep: func(ctx context.Context, d time.Duration) error {
				effects = append(effects, "sleep")
				now = now.Add(d)
				if mode == "cancel" {
					cancel()
				}
				return ctx.Err()
			}, Run: func(_ context.Context, argv []string, _ []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
				if argv[len(argv)-1] == "reboot" {
					effects = append(effects, "reboot")
					return Result{ExitCode: 1}, nil
				}
				reads++
				effects = append(effects, "probe")
				if reads == 1 {
					return Result{Stdout: []byte("100 0")}, nil
				}
				if mode == "down-sequence" && reads == 2 {
					return Result{Stdout: []byte("101 0")}, nil
				}
				if reads == 2 || mode == "timeout" || mode == "down-sequence" && reads == 3 {
					return Result{ExitCode: 1}, nil
				}
				return Result{Stdout: []byte("1 0")}, nil
			}}
			wait := deploy.WaitMode{Kind: deploy.WaitSeconds, Seconds: big.NewInt(1)}
			if mode == "nowait" {
				wait.Kind = deploy.NoWait
			}
			err := deployReboot(ctx, deployTestMachine("m"), wait, ops, func(progress rebootProgress) error {
				states = append(states, progress.State)
				snapshots = append(snapshots, progress)
				if mode == "observer-error" {
					return cause
				}
				return nil
			})
			switch mode {
			case "down-sequence":
				if err != nil || !reflect.DeepEqual(effects, []string{"probe", "reboot", "probe", "sleep", "probe", "sleep", "probe"}) || !reflect.DeepEqual(states, []string{"reboot", "waiting-down", "waiting-up", "up"}) {
					t.Fatal(effects, states, err)
				}
			case "up":
				if err != nil || !reflect.DeepEqual(effects, []string{"probe", "reboot", "probe", "sleep", "probe"}) || !reflect.DeepEqual(states, []string{"reboot", "waiting-up", "up"}) {
					t.Fatal(effects, states, err)
				}
				if snapshots[0].Elapsed.Sign() != 0 || snapshots[0].Remaining.Cmp(big.NewRat(1, 1)) != 0 {
					t.Fatal("earlier snapshot mutated")
				}
			case "observer-error":
				if err != cause || !reflect.DeepEqual(effects, []string{"probe"}) {
					t.Fatal(effects, err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || len(effects) != 4 {
					t.Fatal(effects, err)
				}
			case "timeout":
				if err == nil || err.Error() != "timeout" || states[len(states)-1] != "waiting-up" || snapshots[len(snapshots)-1].Remaining.Sign() != 0 {
					t.Fatal(states, err)
				}
			case "nowait":
				if err != nil || len(states) != 0 || !reflect.DeepEqual(effects, []string{"reboot"}) {
					t.Fatal(effects, states, err)
				}
			}
		})
	}
}
