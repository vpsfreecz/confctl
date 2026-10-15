package core

import (
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
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/generation"
)

func TestDeployDeliveryCopyRoutingEnvironmentAndProgress(t *testing.T) {
	t.Setenv("NIX_SSHOPTS", "  -v ")
	t.Setenv("CONFCTL_SSH_CONFIG", "fixture config")
	carrier := deployTestMachine("carrier")
	carrier.Target.Port = 2202
	m := deployTestMachine("carried")
	name := "carrier"
	m.CarrierName = &name
	var trace []deploy.CopyProgress
	run := func(_ context.Context, argv []string, input []byte, env []string, logged bool, feed func([]byte, []byte) error) (Result, error) {
		if !reflect.DeepEqual(argv, []string{"nix-copy-closure", "--to", "root@carrier.invalid", "/fixture/top", ""}) || input != nil || !logged {
			t.Fatal("copy argv/route changed")
		}
		found := false
		for _, v := range env {
			if v == "NIX_SSHOPTS=-v  -p 2202" {
				found = true
			}
		}
		if !found {
			t.Fatal("port environment did not preserve original NIX_SSHOPTS")
		}
		if err := feed(nil, []byte("copying path '/first'\ncopying 999999999999999999999 paths\ncopying pa")); err != nil {
			t.Fatal(err)
		}
		if err := feed(nil, []byte("th '/second'")); err != nil {
			t.Fatal(err)
		}
		return Result{ExitCode: 1}, nil
	}
	ok, err := deployCopy(context.Background(), m, []Machine{carrier, m}, []string{"/fixture/top", ""}, run, func(p deploy.CopyProgress) { trace = append(trace, p) })
	if ok || err != nil || len(trace) != 2 || trace[0].Current.String() != "1" || trace[0].Total != nil || trace[1].Current.String() != "2" || trace[1].Total.String() != "999999999999999999999" {
		t.Fatal("copy normal failure/final tail/progress differs", err)
	}
	if os.Getenv("NIX_SSHOPTS") != "  -v " {
		t.Fatal("copy changed global environment")
	}
	local := "localhost"
	m.Target.Host = &local
	if ok, err := deployCopy(context.Background(), m, nil, nil, func(context.Context, []string, []byte, []string, bool, func([]byte, []byte) error) (Result, error) {
		t.Fatal("localhost copy ran child/carrier lookup")
		return Result{}, nil
	}, nil); !ok || err != nil {
		t.Fatal("localhost copy failed")
	}
	remote := deployTestMachine("remote")
	if _, err := deployCopy(context.Background(), remote, nil, nil, func(_ context.Context, _ []string, _ []byte, env []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
		if env != nil {
			t.Fatal("port22 added environment override")
		}
		return Result{ExitCode: 0}, nil
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDeployDeliveryQueryPartialStatusAndCarrier(t *testing.T) {
	for _, failedCommand := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(failedCommand), func(t *testing.T) {
			carrier, m := deployTestMachine("carrier"), deployTestMachine("carried")
			m.CarrierName = &carrier.Name
			m.Profile = "/nix/var/nix/profiles/confctl-alias"
			calls := 0
			top := "/fixture/old"
			ops := deployProcessOps{Run: func(_ context.Context, argv []string, data []byte, env []string, logged bool, _ func([]byte, []byte) error) (Result, error) {
				calls++
				if argv[len(argv)-2] != "carrier.invalid" || env != nil || !logged {
					t.Fatal("status query did not preserve carrier/log route")
				}
				commands := []string{"cat /proc/uptime", "realpath " + m.Profile, "bash --norc"}
				if calls > len(commands) || argv[len(argv)-1] != commands[calls-1] {
					t.Fatalf("unexpected query (including input-info): %v", argv)
				}
				if calls == 3 && string(data) != string(generation.ProfileScript(m.Profile)) {
					t.Fatal("query replaced the profile protocol")
				}
				if calls == failedCommand {
					return Result{ExitCode: 1}, nil
				}
				return Result{Stdout: []byte([]string{"100 0", top, top + "\n" + m.Profile + "-9-link;" + top + ";1;\n"}[calls-1])}, nil
			}}
			e := &Engine{Context: context.Background()}
			d := e.deployOperationsWithOps(e.Context, []Machine{carrier, m}, []Machine{m}, nil, io.Discard, cli.Invocation{}, ops)
			status, err := d.QueryTarget(e.Context, deployMachine(m))
			wantCalls := failedCommand
			if wantCalls == 0 {
				wantCalls = 3
			}
			if err != nil || calls != wantCalls || (status.CurrentToplevel != nil) != (failedCommand == 0 || failedCommand > 2) || (status.Generations != nil) != (failedCommand == 0) {
				t.Fatalf("partial query status=%#v calls=%d err=%v", status, calls, err)
			}
			if failedCommand == 0 && (status.Generations.Current() == nil || status.Generations.Current().ID.String() != "9") {
				t.Fatal("query lost the current profile record")
			}
		})
	}
}

func TestDeployDeliveryRollbackStrictExpiryAndSwitching(t *testing.T) {
	m := deployTestMachine("local")
	host := "localhost"
	m.Target.Host = &host
	m.Target.Localhost = true
	m.Attributes["autoRollback"] = map[string]any{"timeout": 10}
	g := deployTestGeneration("new")
	helper := "/fixture/rollback"
	g.AutoRollback = &helper
	started, confirmed, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	now := time.Unix(100, 0)
	polls, sleeps := 0, 0
	ops := deployProcessOps{Now: func() time.Time { return now }, CheckFile: func() (string, error) { return "/run/confctl-confirm-123abc", nil }, Sleep: func(context.Context, time.Duration) error { sleeps++; return nil }}
	ops.Run = func(ctx context.Context, argv []string, _ []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
		if argv[0] == helper {
			defer close(joined)
			close(started)
			select {
			case <-confirmed:
				return Result{}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
		<-started
		if strings.HasPrefix(argv[2], "cat ") {
			polls++
			switch polls {
			case 1:
				now = time.Unix(120, 0)
				return Result{}, nil // equality does not expire
			case 2:
				now = time.Unix(121, 0)
				return Result{Stdout: []byte("switching\n")}, nil
			case 3:
				return Result{}, nil // expired and no longer switching
			default:
				return Result{}, errors.New("unexpected extra rollback poll")
			}
		}
		close(confirmed)
		return Result{}, nil
	}
	ok, err := deployRollback(context.Background(), m, g, deploy.Switch, ops)
	if !ok || err != nil || polls != 3 || sleeps != 2 {
		t.Fatalf("polls=%d sleeps=%d err=%v", polls, sleeps, err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("helper was not joined")
	}
}

func TestDeployDeliveryActivationAndCarriedProfile(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	m := deployTestMachine("host")
	carrier := deployTestMachine("carrier")
	carried := deployTestMachine("carried")
	n := "carrier"
	carried.CarrierName = &n
	carried.Profile = "/nix/var/nix/profiles/confctl-alias"
	var calls [][]string
	ops := deployProcessOps{Run: func(_ context.Context, argv []string, _ []byte, _ []string, logged bool, _ func([]byte, []byte) error) (Result, error) {
		if !logged {
			t.Fatal("activation not logged")
		}
		calls = append(calls, append([]string(nil), argv...))
		return Result{ExitCode: 0}, nil
	}}
	e := &Engine{Context: context.Background()}
	d := e.deployOperationsWithOps(e.Context, []Machine{m, carrier, carried}, []Machine{m, carried}, nil, io.Discard, cli.Invocation{}, ops)
	if ok, err := d.Activate(e.Context, deployMachine(m), deployTestGeneration("with space"), deploy.Test); !ok || err != nil {
		t.Fatal(err)
	}
	if ok, err := d.SetProfile(e.Context, deployMachine(m), "/fixture/new"); !ok || err != nil {
		t.Fatal(err)
	}
	if ok, err := d.SetCarriedProfile(e.Context, deployMachine(carried), "/fixture/carried"); !ok || err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[0][len(calls[0])-1] != `/fixture/with\ space/bin/switch-to-configuration test` || calls[1][len(calls[1])-1] != "nix-env -p /nix/var/nix/profiles/system --set /fixture/new" || calls[2][len(calls[2])-2] != "carrier.invalid" || calls[2][len(calls[2])-1] != "carrier-env -p /nix/var/nix/profiles/confctl-alias --set /fixture/carried" {
		t.Fatalf("activation/profile routing differs: %v", calls)
	}
}

func TestDeployDeliveryActivationLiteralToplevel(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	for _, row := range []struct{ top, executable, quoted string }{
		{"/fixture/system", "/fixture/system/bin/switch-to-configuration", "/fixture/system/bin/switch-to-configuration"},
		{"/fixture/system/", "/fixture/system/bin/switch-to-configuration", "/fixture/system/bin/switch-to-configuration"},
		{"/fixture/system//", "/fixture/system//bin/switch-to-configuration", "/fixture/system//bin/switch-to-configuration"},
		{"config/link/../system", "config/link/../system/bin/switch-to-configuration", "config/link/../system/bin/switch-to-configuration"},
		{"config/a link/../system", "config/a link/../system/bin/switch-to-configuration", `config/a\ link/../system/bin/switch-to-configuration`},
	} {
		for _, local := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/local=%t", row.top, local), func(t *testing.T) {
				m := deployTestMachine("m")
				if local {
					host := "localhost"
					m.Target.Host = &host
					m.Target.Localhost = true
				}
				g := deployTestGeneration("g")
				g.Toplevel = row.top
				var got []string
				calls := 0
				ops := deployProcessOps{Run: func(_ context.Context, argv []string, input []byte, env []string, logged bool, _ func([]byte, []byte) error) (Result, error) {
					calls++
					got = append([]string(nil), argv...)
					if input != nil || env != nil || !logged {
						t.Fatal("activation process contract changed")
					}
					return Result{}, nil // Only argv inspection; no child or filesystem access.
				}}
				e := &Engine{Context: context.Background()}
				d := e.deployOperationsWithOps(e.Context, []Machine{m}, []Machine{m}, nil, io.Discard, cli.Invocation{}, ops)
				ok, err := d.Activate(e.Context, deployMachine(m), g, deploy.Test)
				want := []string{row.executable, "test"}
				if !local {
					want = []string{"ssh", "-l", "root", "m.invalid", row.quoted + " test"}
				}
				if !ok || err != nil || calls != 1 || !reflect.DeepEqual(got, want) {
					t.Fatalf("literal activation argv=%q want=%q calls=%d err=%v", got, want, calls, err)
				}
			})
		}
	}
}

func TestDeployDeliveryFakeCopyTool(t *testing.T) {
	// Only this disposable executable is launched; it cannot contact a host.
	dir := t.TempDir()
	tool := filepath.Join(dir, "nix-copy-closure")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf \"copying path '/fixture/path'\\n\" >&2\nprintf '%s\\n' \"$NIX_SSHOPTS\"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NIX_SSHOPTS", "-v")
	m := deployTestMachine("remote")
	m.Target.Port = 2222
	e := &Engine{Root: dir, Context: context.Background()}
	if err := e.OpenLog("deploy-test"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseLog(false)
	var paths []string
	ok, err := deployCopy(e.Context, m, []Machine{m}, []string{"/fixture/top"}, e.deployProcess, func(p deploy.CopyProgress) { paths = append(paths, p.Path) })
	if ok || err != nil || !reflect.DeepEqual(paths, []string{"/fixture/path"}) {
		t.Fatal("fake copy/logged process did not preserve ordinary status", err)
	}
	b, err := os.ReadFile(e.Log.Name())
	if err != nil || !strings.Contains(string(b), "-v -p 2222") {
		t.Fatal("command-local env not observed by fake child/log")
	}
}

func TestDeployDeliveryRollbackHandshakeAndJoin(t *testing.T) {
	for _, mode := range []string{"success", "confirm-fails", "poll-error", "helper-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			m := deployTestMachine("local")
			host := "localhost"
			m.Target.Host = &host
			m.Target.Localhost = true
			m.Attributes["autoRollback"] = map[string]any{"timeout": 10}
			g := deployTestGeneration("new")
			helper := "/fixture/auto-rollback"
			g.AutoRollback = &helper
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			cause := errors.New("synthetic rollback infrastructure")
			confirmations := 0
			ops := deployProcessOps{Now: func() time.Time { return time.Unix(100, 0) }, CheckFile: func() (string, error) { return "/run/confctl-confirm-abcdef", nil }, Sleep: func(context.Context, time.Duration) error { t.Fatal("script should not sleep"); return nil }}
			ops.Run = func(ctx context.Context, argv []string, _ []byte, _ []string, logged bool, _ func([]byte, []byte) error) (Result, error) {
				if argv[0] == helper {
					defer close(joined)
					close(started)
					if mode == "helper-error" {
						return Result{}, cause
					}
					select {
					case <-release:
						return Result{ExitCode: map[bool]int{true: 1, false: 0}[mode == "confirm-fails"]}, nil
					case <-ctx.Done():
						return Result{}, ctx.Err()
					}
				}
				<-started
				if logged {
					t.Fatal("rollback poll/confirm reached command log")
				}
				if strings.HasPrefix(argv[2], "cat ") {
					if mode == "poll-error" {
						return Result{}, cause
					}
					if mode == "cancel" {
						cancel()
						return Result{}, ctx.Err()
					}
					return Result{ExitCode: 0, Stdout: []byte("switched\n")}, nil
				}
				confirmations++
				if mode != "confirm-fails" || confirmations == 10 {
					close(release)
				}
				return Result{ExitCode: map[bool]int{true: 1, false: 0}[mode == "confirm-fails"]}, nil
			}
			ok, err := deployRollback(ctx, m, g, deploy.Switch, ops)
			select {
			case <-joined:
			default:
				t.Fatal("rollback returned without helper join")
			}
			switch mode {
			case "success":
				if !ok || err != nil || confirmations != 1 {
					t.Fatal("successful rollback handshake differs", err)
				}
			case "confirm-fails":
				if ok || err != nil || confirmations != 10 {
					t.Fatal("confirmation failure did not retain helper result", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost")
				}
			default:
				if !errors.Is(err, cause) {
					t.Fatal("infrastructure cause lost", err)
				}
			}
		})
	}
}

func TestDeployDeliveryRebootWait(t *testing.T) {
	for _, mode := range []string{"nowait", "lower-uptime", "timeout", "forever"} {
		t.Run(mode, func(t *testing.T) {
			m := deployTestMachine("remote")
			now := time.Unix(100, 0)
			reads, reboots := 0, 0
			ops := deployProcessOps{Now: func() time.Time { return now }, Sleep: func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }, Run: func(_ context.Context, argv []string, _ []byte, _ []string, _ bool, _ func([]byte, []byte) error) (Result, error) {
				if strings.HasSuffix(argv[len(argv)-1], "reboot") {
					reboots++
					return Result{ExitCode: 1}, nil
				}
				reads++
				if reads == 1 {
					return Result{ExitCode: 0, Stdout: []byte("100.0 0")}, nil
				}
				if mode == "timeout" {
					return Result{ExitCode: 1}, nil
				}
				if reads == 2 {
					return Result{ExitCode: 1}, nil
				}
				return Result{ExitCode: 0, Stdout: []byte("2.5 0")}, nil
			}}
			wait := deploy.WaitMode{Kind: deploy.WaitSeconds, Seconds: big.NewInt(0)}
			if mode == "nowait" {
				wait.Kind = deploy.NoWait
			}
			if mode == "forever" {
				wait.Kind = deploy.WaitForever
			}
			if mode == "lower-uptime" {
				wait.Seconds = big.NewInt(100)
			}
			err := deployReboot(context.Background(), m, wait, ops)
			if reboots != 1 {
				t.Fatal("reboot count differs")
			}
			if mode == "timeout" {
				if err == nil || err.Error() != "timeout" {
					t.Fatal("wait timeout changed", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "nowait" && reads != 0 {
				t.Fatal("nowait queried uptime")
			}
		})
	}
}
