package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/deploy"
	"github.com/vpsfreecz/confctl/internal/generation"
)

// elapsed is compared to arbitrary-precision source seconds without narrowing
// an operator's wait value into time.Duration. Sleep intervals remain fixed.
func deployElapsed(start, now time.Time) *big.Rat {
	seconds := new(big.Int).Sub(big.NewInt(now.Unix()), big.NewInt(start.Unix()))
	elapsed := new(big.Rat).SetInt(seconds)
	return elapsed.Add(elapsed, new(big.Rat).SetFrac(big.NewInt(int64(now.Nanosecond()-start.Nanosecond())), big.NewInt(1e9)))
}

func deployRollbackTimeout(m Machine) (*big.Rat, string, error) {
	value := nested(m.Attributes, "autoRollback", "timeout")
	var text string
	switch v := value.(type) {
	case json.Number:
		text = string(v)
	case int:
		text = strconv.Itoa(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case float64:
		text = strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return nil, "", errors.New("autoRollback.timeout must be numeric")
	}
	n, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, "", errors.New("autoRollback.timeout must be finite")
	}
	return n, text, nil
}

func deployRollback(ctx context.Context, m Machine, g *generation.Build, action deploy.Action, ops deployProcessOps) (bool, error) {
	check, err := ops.CheckFile()
	if err != nil {
		return false, err
	}
	timeout, timeoutText, err := deployRollbackTimeout(m)
	if err != nil {
		return false, err
	}
	if g.AutoRollback == nil {
		return false, errors.New("rollback helper is unavailable")
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type activationResult struct {
		success bool
		err     error
	}
	done := make(chan activationResult, 1)
	go func() {
		ok, err := deployMachineCommand(childCtx, m, []string{*g.AutoRollback, "-t", timeoutText, g.Toplevel, string(action), check}, false, true, ops.Run)
		done <- activationResult{ok, err}
	}()
	var finished *activationResult
	join := func() activationResult {
		if finished == nil {
			result := <-done
			finished = &result
		}
		return *finished
	}
	fail := func(err error) (bool, error) { cancel(); join(); return false, err }
	start := ops.Now()
	limit := new(big.Rat).Add(timeout, big.NewRat(10, 1))
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		select {
		case result := <-done:
			finished = &result
			if result.err != nil {
				return fail(result.err)
			}
		default:
		}
		argv, err := SSHArgs(m, []string{"sh", "-c", "cat " + ShellJoin([]string{check}) + " 2>/dev/null"})
		if err != nil {
			return fail(err)
		}
		result, err := ops.Run(childCtx, argv, nil, nil, false, nil)
		if _, err = testConnectionStatus(childCtx, argv, result, err); err != nil {
			return fail(err)
		}
		state := strings.Trim(string(result.Stdout), " \t\r\n\v\f\x00")
		if state == "switched" || state != "switching" && deployElapsed(start, ops.Now()).Cmp(limit) > 0 {
			break
		}
		if err := ops.Sleep(childCtx, time.Second); err != nil {
			return fail(err)
		}
	}
	for i := 0; i < 10; i++ {
		ok, err := deployMachineCommand(childCtx, m, []string{"sh", "-c", "echo confirmed > " + ShellJoin([]string{check})}, false, false, ops.Run)
		if err != nil {
			return fail(err)
		}
		if ok {
			break
		}
	}
	result := join() // helper's ordinary result is authoritative, not health
	return result.success, result.err
}

func deployUptime(ctx context.Context, m Machine, checkConnection bool, run deployRun) (float64, bool, error) {
	argv, err := SSHArgs(m, []string{"cat", "/proc/uptime"})
	if err != nil {
		return 0, false, err
	}
	if checkConnection && m.Target.Host != nil && *m.Target.Host != "localhost" {
		at := len(argv) - 2
		argv = append(append(append([]string{}, argv[:at]...), "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1"), argv[at:]...)
	}
	result, err := run(ctx, argv, nil, nil, true, nil)
	status, err := testConnectionStatus(ctx, argv, result, err)
	if err != nil {
		return 0, false, err
	}
	if status != 0 {
		return 0, false, nil
	}
	fields := strings.Fields(string(result.Stdout))
	if len(fields) == 0 {
		return 0, false, errors.New("invalid uptime response")
	}
	// String#to_f accepts the numeric prefix and returns0 for a nonnumeric token.
	match := deployFloatPrefix.FindString(fields[0])
	up := 0.0
	if match != "" {
		up, _ = strconv.ParseFloat(match, 64)
	}
	return up, true, nil
}

func deployReboot(ctx context.Context, m Machine, wait deploy.WaitMode, ops deployProcessOps) error {
	if wait.Kind == deploy.NoWait {
		_, err := deployMachineCommand(ctx, m, []string{"reboot"}, false, true, ops.Run)
		return err // source ignores ordinary reboot status
	}
	initial, ok, err := deployUptime(ctx, m, false, ops.Run)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("unable to read initial uptime")
	}
	start := ops.Now()
	if _, err := deployMachineCommand(ctx, m, []string{"reboot"}, false, true, ops.Run); err != nil {
		return err
	}
	for {
		current, online, err := deployUptime(ctx, m, true, ops.Run)
		if err != nil {
			return err
		}
		if online && current < initial {
			return nil
		}
		if wait.Kind == deploy.WaitSeconds && deployElapsed(start, ops.Now()).Cmp(new(big.Rat).SetInt(wait.Seconds)) >= 0 {
			return fmt.Errorf("timeout")
		}
		if err := ops.Sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

var deployFloatPrefix = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)
