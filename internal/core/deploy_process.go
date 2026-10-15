package core

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/health"
)

type deployRun func(context.Context, []string, []byte, []string, bool, func([]byte, []byte) error) (Result, error)

type deployProcessOps struct {
	Run       deployRun
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) error
	CheckFile func() (string, error)
}

func (e *Engine) deployProcess(ctx context.Context, argv []string, input []byte, env []string, logged bool, chunk func([]byte, []byte) error) (Result, error) {
	if !logged {
		return processWithStreamsEnv(ctx, e.Root, argv, input, env, chunk)
	}
	attempt := *e
	attempt.Context = ctx
	return attempt.processStreams(argv, input, env, chunk)
}

func deploySleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func deployCheckFile() (string, error) {
	var random [3]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "/run/confctl-confirm-" + hex.EncodeToString(random[:]), nil
}

func (e *Engine) deployOperations(ctx context.Context, all, selected []Machine, input *bufio.Reader, out io.Writer, inv cli.Invocation) deploy.Deps {
	ops := deployProcessOps{Run: e.deployProcess, Now: time.Now, Sleep: deploySleep, CheckFile: deployCheckFile}
	return e.deployOperationsWithOps(ctx, all, selected, input, out, inv, ops)
}

func (e *Engine) deployOperationsWithOps(ctx context.Context, all, selected []Machine, input *bufio.Reader, out io.Writer, inv cli.Invocation, ops deployProcessOps) deploy.Deps {
	byName := map[string]Machine{}
	for _, m := range selected {
		byName[m.Name] = m
	}
	machine := func(m deploy.Machine) Machine { return byName[m.Name] }
	return deploy.Deps{
		QueryTarget: func(ctx context.Context, m deploy.Machine) (deploy.Status, error) {
			attempt := *e
			attempt.Context = ctx
			st := Status{Machine: machine(m)}
			attempt.queryWithInputs(&st, all, queryOptions{Toplevel: true, Generations: true}, func(argv []string, input []byte) (Result, error) {
				return ops.Run(ctx, argv, input, nil, true, nil)
			}, false)
			return deploy.Status{CurrentToplevel: st.CurrentToplevel, Generations: st.Generations}, st.Err
		},
		CarrierName: func(m deploy.Machine) (string, error) {
			c, err := statusConnection(machine(m), all)
			return c.Name, err
		},
		AutoRollbackDefault: func(m deploy.Machine) (bool, error) {
			settings, present := machine(m).Attributes["autoRollback"]
			if !present {
				return true, nil
			}
			values, ok := settings.(map[string]any)
			if !ok {
				return false, errors.New("autoRollback must be an object")
			}
			v, present := values["enable"]
			if !present {
				return true, nil
			}
			return truth(v), nil
		},
		Copy: func(ctx context.Context, m deploy.Machine, paths []string, progress func(deploy.CopyProgress)) (bool, error) {
			return deployCopy(ctx, machine(m), all, paths, ops.Run, progress)
		},
		Activate: func(ctx context.Context, m deploy.Machine, g *generation.Build, action deploy.Action) (bool, error) {
			// Stored toplevels retain physical symlink/.. semantics. Append the
			// child without lexically cleaning or resolving the borrowed path.
			top := g.Toplevel
			if !strings.HasSuffix(top, "/") {
				top += "/"
			}
			return deployMachineCommand(ctx, machine(m), []string{top + "bin/switch-to-configuration", string(action)}, false, true, ops.Run)
		},
		ActivateWithRollback: func(ctx context.Context, m deploy.Machine, g *generation.Build, action deploy.Action) (bool, error) {
			return deployRollback(ctx, machine(m), g, action, ops)
		},
		SetProfile: func(ctx context.Context, m deploy.Machine, top string) (bool, error) {
			v := machine(m)
			return deployMachineCommand(ctx, v, []string{"nix-env", "-p", v.Profile, "--set", top}, false, true, ops.Run)
		},
		SetCarriedProfile: func(ctx context.Context, m deploy.Machine, top string) (bool, error) {
			v := machine(m)
			carrier, err := statusConnection(v, all)
			if err != nil {
				return false, err
			}
			return deployMachineCommand(ctx, carrier, []string{"carrier-env", "-p", v.Profile, "--set", top}, false, true, ops.Run)
		},
		RebootAndWait: func(ctx context.Context, m deploy.Machine, wait deploy.WaitMode) error {
			return deployReboot(ctx, machine(m), wait, ops)
		},
		Checks: func(ms []deploy.Machine) ([]health.Check, error) {
			var checks []health.Check
			for _, m := range ms {
				c, err := machine(m).healthSpecs()
				if err != nil {
					return nil, err
				}
				checks = append(checks, c...)
			}
			return checks, nil
		},
		RunHealth: func(ctx context.Context, ms []deploy.Machine, checks []health.Check) (health.Report, error) {
			workers, err := deployHealthWorkers(inv)
			if err != nil {
				// Admission reached these checks, but none started. Keep their
				// identities so the deployment report preserves that distinction.
				report := health.Report{Results: make([]health.Result, len(checks))}
				for i, check := range checks {
					report.Results[i].Check = check
				}
				return report, err
			}
			scope := make([]Machine, len(ms))
			for i, m := range ms {
				scope[i] = machine(m)
			}
			attempt := *e
			attempt.Context = ctx
			result, err := attempt.runHealth(HealthPlan{Machines: scope, Checks: checks, Workers: *workers}, health.Deps{}, func(ctx context.Context, _ string, argv []string, data []byte) (Result, error) {
				return ops.Run(ctx, argv, data, nil, true, nil)
			})
			return result.Report, err
		},
		Confirm: func(_ context.Context, _ deploy.Confirmation) (bool, error) { return deployConfirm(input, out) },
		ChooseActivation: func(_ context.Context, _ deploy.Target, retry bool) (deploy.ActivationChoice, error) {
			choices := "ysa"
			if retry {
				choices += "r"
			}
			choice, err := deployChoice(input, out, choices)
			return map[string]deploy.ActivationChoice{"y": deploy.ActivationContinue, "r": deploy.ActivationRetry, "s": deploy.ActivationSkip, "a": deploy.ActivationAbort}[choice], err
		},
		ChooseHealth: func(context.Context, []deploy.Machine, health.Report) (deploy.HealthChoice, error) {
			choice, err := deployChoice(input, out, "crfa")
			return map[string]deploy.HealthChoice{"c": deploy.HealthContinue, "r": deploy.HealthRetryAll, "f": deploy.HealthRetryFailed, "a": deploy.HealthAbort}[choice], err
		},
	}
}

func deployConfirm(input *bufio.Reader, out io.Writer) (bool, error) {
	choice, err := deployChoice(input, out, "yn")
	return choice == "y", err
}

func deployChoice(input *bufio.Reader, out io.Writer, choices string) (string, error) {
	for {
		if err := buildWrite(out, fmt.Sprintf("Choose [%s]: ", choices)); err != nil {
			return "", err
		}
		line, err := input.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if errors.Is(err, io.EOF) && line == "" {
			return "", errors.New("end of file reached")
		}
		value := strings.ToLower(strings.Trim(line, " \t\n\v\f\r\x00"))
		if len(value) == 1 && strings.Contains(choices, value) {
			err := buildWrite(out, "\n")
			return value, err
		}
	}
}

func deployMachineCommand(ctx context.Context, m Machine, command []string, checkConnection, logged bool, run deployRun) (bool, error) {
	argv, err := SSHArgs(m, command)
	if err != nil {
		return false, err
	}
	if checkConnection && m.Target.Host != nil && *m.Target.Host != "localhost" {
		at := len(argv) - 2
		argv = append(append(append([]string{}, argv[:at]...), "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1"), argv[at:]...)
	}
	r, err := run(ctx, argv, nil, nil, logged, nil)
	status, err := testConnectionStatus(ctx, argv, r, err)
	return status == 0, err
}

var deployCopyTotal = regexp.MustCompile(`^copying ([0-9]+) paths`)
var deployCopyPath = regexp.MustCompile(`^copying path '([^']+)`)

type deployCopyParser struct {
	tail           string
	current, total *big.Int
	progress       func(deploy.CopyProgress)
}

func (p *deployCopyParser) line(line string) {
	if p.total == nil {
		if m := deployCopyTotal.FindStringSubmatch(line); m != nil {
			p.total, _ = new(big.Int).SetString(m[1], 10)
			return
		}
	}
	if m := deployCopyPath.FindStringSubmatch(line); m != nil {
		if p.current == nil {
			p.current = new(big.Int)
		}
		p.current.Add(p.current, big.NewInt(1))
		if p.progress != nil {
			var total *big.Int
			if p.total != nil {
				total = new(big.Int).Set(p.total)
			}
			p.progress(deploy.CopyProgress{Path: m[1], Current: new(big.Int).Set(p.current), Total: total})
		}
	}
}
func (p *deployCopyParser) feed(data []byte) {
	p.tail += string(data)
	for {
		i := strings.IndexByte(p.tail, '\n')
		if i < 0 {
			return
		}
		p.line(p.tail[:i+1])
		p.tail = p.tail[i+1:]
	}
}
func (p *deployCopyParser) flush() {
	if p.tail != "" {
		p.line(p.tail)
		p.tail = ""
	}
}

func deployCopy(ctx context.Context, m Machine, all []Machine, paths []string, run deployRun, progress func(deploy.CopyProgress)) (bool, error) {
	if m.Target.Host != nil && *m.Target.Host == "localhost" {
		return true, nil
	}
	connection, err := statusConnection(m, all)
	if err != nil {
		return false, err
	}
	if connection.Target.Host == nil {
		return false, errors.New("copy target is unavailable")
	}
	argv := append([]string{"nix-copy-closure", "--to", "root@" + *connection.Target.Host}, paths...)
	var env []string
	if connection.Target.Port != 22 {
		env = os.Environ()
		value := strings.Trim(os.Getenv("NIX_SSHOPTS")+fmt.Sprintf(" -p %d", connection.Target.Port), " \t\r\n\v\f\x00")
		filtered := env[:0]
		for _, entry := range env {
			if !strings.HasPrefix(entry, "NIX_SSHOPTS=") {
				filtered = append(filtered, entry)
			}
		}
		env = append(filtered, "NIX_SSHOPTS="+value)
	}
	parser := &deployCopyParser{progress: progress}
	r, err := run(ctx, argv, nil, env, true, func(_, stderr []byte) error { parser.feed(stderr); return nil })
	status, err := testConnectionStatus(ctx, argv, r, err)
	if err != nil {
		return false, err
	}
	parser.flush() // Unlike build attempts, a normal nonzero copy flushes too.
	return status == 0, nil
}
