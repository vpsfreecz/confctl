package core

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/build"
	"github.com/vpsfreecz/confctl/internal/generation"
)

func TestBuildNixOptionsCacheAndOrderedValues(t *testing.T) {
	for _, max := range []*string{nil, buildText(""), buildText("auto")} {
		for _, cores := range []*string{nil, buildText("")} {
			t.Run("options", func(t *testing.T) {
				var calls [][]string
				n := &buildNix{e: &Engine{Context: context.Background()}, options: buildOptions{MaxJobs: max, Cores: cores, ShowTrace: true}}
				n.run = func(argv []string, _ []byte, _ func([]byte, []byte) error) (Result, error) {
					calls = append(calls, append([]string(nil), argv...))
					body := `{"nix":{"impureEval":true},"nested":{"z":1,"_module":false,"a":2}}`
					if argv[len(argv)-1] == ".#confctl.buildPlan" {
						body = `{}`
					}
					return Result{ExitCode: 0, Stdout: []byte(body)}, nil
				}
				for i := 0; i < 2; i++ {
					object, err := n.buildPlan()
					if err != nil || object == nil || len(object) != 0 {
						t.Fatal(object, err)
					}
				}
				if len(calls) != 2 || containsBuildArg(calls[0], "--impure") || !containsBuildArg(calls[1], "--impure") {
					t.Fatal("bootstrap/caching", calls)
				}
				for i, argv := range calls {
					want := []string{"nix", "eval", "--json"}
					if i == 1 {
						want = append(want, "--impure")
					}
					want = append(want, "--no-write-lock-file", "--no-update-lock-file", "--show-trace")
					if max != nil {
						want = append(want, "--option", "max-jobs", *max)
					}
					if cores != nil {
						want = append(want, "--option", "cores", *cores)
					}
					want = append(want, []string{".#confctl.settings", ".#confctl.buildPlan"}[i])
					if !reflect.DeepEqual(argv, want) {
						t.Fatal(argv, want)
					}
				}
				nested := buildObjectValue(n.settings, "nested").(generation.Object)
				if len(nested) != 2 || nested[0].Name != "z" || nested[1].Name != "a" {
					t.Fatal("settings demod lost ordered members", nested)
				}
			})
		}
	}
}

func containsBuildArg(argv []string, value string) bool {
	for _, arg := range argv {
		if arg == value {
			return true
		}
	}
	return false
}

func TestBuildNixCompatibilityAndFreshProgress(t *testing.T) {
	for _, messages := range [][]string{
		{"unknown --no-update-lock-file", "experimental flakes"},
		{"experimental nix-command", "unsupported --no-update-lock-file"},
	} {
		t.Run(messages[0], func(t *testing.T) {
			e := &Engine{Context: context.Background()}
			var events []build.Event
			var commands []buildCommandEvent
			var attempts [][]string
			n := &buildNix{e: e, settingsReady: true, progress: func(event build.Event) error { events = append(events, event); return nil }}
			n.commandEvent = func(event buildCommandEvent) error {
				commands = append(commands, event)
				if event.Stage == buildCommandSucceeded && (len(events) != 3 || events[2].Path != "/successful-tail.drv") {
					t.Fatal("completion preceded successful parser remainder", events)
				}
				return nil
			}
			n.run = func(argv []string, _ []byte, feed func([]byte, []byte) error) (Result, error) {
				attempts = append(attempts, append([]string(nil), argv...))
				i := len(attempts) - 1
				body := "building '/failed-complete.drv'\nbuilding '/stale-tail"
				status := 1
				stdout := []byte("not JSON on failed child")
				if i == 2 {
					body, status, stdout = "building '/successful-tail.drv'", 0, []byte("null")
				}
				if err := feed(nil, []byte(body)); err != nil {
					return Result{}, err
				}
				stderr := []byte(body)
				if i < 2 {
					stderr = append(stderr, []byte("\n"+messages[i])...)
				}
				return Result{ExitCode: status, Stdout: stdout, Stderr: stderr}, nil
			}
			body, err := n.build(build.BuildRequest{Installables: []string{"literal"}}, func([]byte, []byte) { t.Fatal("B1b group parser was used") })
			if err != nil || string(body) != "null" || len(attempts) != 3 || len(events) != 3 {
				t.Fatal(err, attempts, events)
			}
			if events[0].Path != "/failed-complete.drv" || events[2].Path != "/successful-tail.drv" || events[2].Progress.Int64() != 1 {
				t.Fatal("failed tail or counter leaked", events)
			}
			if containsBuildArg(attempts[2], "--no-update-lock-file") || !containsBuildArg(attempts[2], "--extra-experimental-features") {
				t.Fatal(attempts)
			}
			var stages []buildCommandStage
			for _, event := range commands {
				stages = append(stages, event.Stage)
				if event.Op != "build" || event.Err != nil || !reflect.DeepEqual(event.Installables, []string{"literal"}) {
					t.Fatal("incorrect attempt binding", event)
				}
			}
			if !reflect.DeepEqual(stages, []buildCommandStage{buildCommandStarted, buildCommandStarted, buildCommandStarted, buildCommandSucceeded}) {
				t.Fatal("retry emitted terminal event or missed start", stages)
			}
		})
	}
}

func TestBuildNixCommandObservationPriority(t *testing.T) {
	primary, notification := errors.New("command cause"), errors.New("notification cause")
	for _, kind := range []string{"start", "capture", "process", "context", "status", "progress", "success"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			n := &buildNix{e: &Engine{Context: ctx}, settingsReady: true}
			var events []buildCommandEvent
			var order []string
			n.commandEvent = func(event buildCommandEvent) error {
				events = append(events, event)
				order = append(order, "event")
				if kind == "start" || event.Stage != buildCommandStarted {
					return notification
				}
				return nil
			}
			n.progress = func(build.Event) error { return primary }
			n.run = func(_ []string, _ []byte, feed func([]byte, []byte) error) (Result, error) {
				order = append(order, "run")
				switch kind {
				case "capture":
					cancel()
					return Result{capture: &captureCompletion{primary: primary}}, primary
				case "process":
					return Result{}, primary
				case "context":
					cancel()
					return Result{}, nil
				case "status":
					return Result{ExitCode: 7}, nil
				case "progress":
					// Child succeeded but its final partial progress flush fails.
					if err := feed(nil, []byte("building '/last.drv'")); err != nil {
						t.Fatal(err)
					}
				}
				return Result{Stdout: []byte("null")}, nil
			}
			_, err := n.build(build.BuildRequest{Installables: []string{"target"}}, nil)
			if kind == "start" {
				if err != notification || len(events) != 1 || !reflect.DeepEqual(order, []string{"event"}) {
					t.Fatal("start refusal launched child", err, events, order)
				}
				return
			}
			if len(events) != 2 || !reflect.DeepEqual(order, []string{"event", "run", "event"}) {
				t.Fatal("missing joined terminal boundary", err, events, order)
			}
			last := events[1]
			switch kind {
			case "success":
				if err != notification || last.Stage != buildCommandSucceeded || last.Err != nil {
					t.Fatal("accepted success notification error lost", err, last)
				}
			case "context":
				if err != context.Canceled || last.Err != err {
					t.Fatal("context priority lost", err, last)
				}
			case "status":
				if err == nil || err == notification || last.Err != err || !strings.Contains(err.Error(), "exit status: 7") {
					t.Fatal("exit failure replaced by notification", err, last)
				}
			default:
				if err != primary || last.Err != primary {
					t.Fatal("original command/progress cause replaced", err, last)
				}
			}
			if kind != "success" && last.Stage != buildCommandFailed {
				t.Fatal("failure terminal stage lost", last)
			}
		})
	}
}

func TestBuildNixCommandSnapshotsAndDecodeBoundary(t *testing.T) {
	n := &buildNix{e: &Engine{Context: context.Background()}, settingsReady: true}
	var events []buildCommandEvent
	n.commandEvent = func(event buildCommandEvent) error {
		events = append(events, event)
		if event.Stage == buildCommandStarted {
			event.Installables[0] = "changed by observer"
		}
		return nil
	}
	n.run = func(argv []string, _ []byte, _ func([]byte, []byte) error) (Result, error) {
		if argv[len(argv)-1] != ".#confctl.build" {
			t.Fatal("observer changed executed target", argv)
		}
		return Result{Stdout: []byte("malformed JSON")}, nil
	}
	if _, err := n.evalWithSettings(".#confctl.build", nil, nil); err == nil {
		t.Fatal("eval decode failure disappeared")
	}
	if len(events) != 2 || events[1].Stage != buildCommandSucceeded || events[1].Installables[0] != ".#confctl.build" || events[1].Err != nil {
		t.Fatal("decode error reclassified child or snapshots alias", events)
	}
}

func TestBuildNixFallbackClassificationAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		err                  error
		attempts             int
	}{
		{name: "stdout-keyword", stdout: "invalid option", attempts: 2},
		{name: "omitted-middle", stderr: strings.Repeat("x", 32768) + "experimental flakes" + strings.Repeat("x", 32768), attempts: 1},
		{name: "spawn", err: errors.New("synthetic start"), attempts: 1},
		{name: "cancel", err: context.Canceled, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			n := &buildNix{e: &Engine{Context: context.Background()}}
			n.run = func(argv []string, _ []byte, _ func([]byte, []byte) error) (Result, error) {
				calls++
				if calls == 2 {
					return Result{ExitCode: 0, Stdout: []byte("{}")}, nil
				}
				return Result{ExitCode: 1, Stdout: []byte(tc.stdout), Stderr: []byte(tc.stderr)}, tc.err
			}
			_, err := n.evalWithSettings("literal", nil, nil)
			if calls != tc.attempts || tc.attempts == 1 && err == nil || tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatal(calls, err)
			}
		})
	}
	for _, invalid := range []string{"invalid JSON", "{} {}", "[]"} {
		n := &buildNix{e: &Engine{Context: context.Background()}, settingsReady: true}
		calls := 0
		n.run = func([]string, []byte, func([]byte, []byte) error) (Result, error) {
			calls++
			return Result{ExitCode: 0, Stdout: []byte(invalid)}, nil
		}
		if _, err := n.buildPlan(); err == nil || n.planReady || calls != 1 {
			t.Fatal("failed plan cached/retried", invalid, calls, err)
		}
	}
}

func TestBuildNixProgressErrorAndMachineKeyLaziness(t *testing.T) {
	sentinel := errors.New("progress write")
	n := &buildNix{e: &Engine{Context: context.Background()}, settingsReady: true, progress: func(build.Event) error { return sentinel }}
	n.run = func(_ []string, _ []byte, feed func([]byte, []byte) error) (Result, error) {
		err := feed(nil, []byte("building '/synthetic.drv'\n"))
		return Result{}, err
	}
	if _, err := n.build(build.BuildRequest{}, nil); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	calls := 0
	n.run = func(argv []string, _ []byte, _ func([]byte, []byte) error) (Result, error) {
		calls++
		return Result{ExitCode: 0, Stdout: []byte(`{"one":"key","key":"different"}`)}, nil
	}
	for host, want := range map[string]string{"one": "key", "key": "different", "different": "different"} {
		got, err := n.machineKey(host)
		if err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	if calls != 1 {
		t.Fatal("key cache not invocation-local", calls)
	}
	if _, err := n.machineKey("missing"); err == nil {
		t.Fatal("unknown machine accepted")
	}
}
