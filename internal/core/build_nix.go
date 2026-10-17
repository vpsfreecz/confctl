package core

import (
	"fmt"
	"strings"

	"github.com/vpsfreecz/confctl/internal/build"
	"github.com/vpsfreecz/confctl/internal/generation"
)

// Each invocation owns its plan, settings and machine-key caches. Empty objects
// are successful values; failed evaluations never populate a cache.
type buildNix struct {
	e              *Engine
	options        buildOptions
	run            func([]string, []byte, func([]byte, []byte) error) (Result, error)
	progress       func(build.Event) error
	commandEvent   func(buildCommandEvent) error
	settings, plan generation.Object
	keys           generation.Object
	settingsReady  bool
	planReady      bool
	keysReady      bool
}

func (n *buildNix) settingsObject() (generation.Object, error) {
	if !n.settingsReady {
		// Bootstrap settings explicitly in pure mode; no recursive Settings call.
		v, err := n.evalWithSettings(".#confctl.settings", nil, nil)
		if err != nil {
			return nil, err
		}
		object, ok := v.(generation.Object)
		if !ok {
			return nil, fmt.Errorf("build settings must be an object")
		}
		n.settings = buildDemod(object).(generation.Object)
		n.settingsReady = true
	}
	return n.settings, nil
}

func buildDemod(value any) any {
	switch value := value.(type) {
	case generation.Object:
		out := make(generation.Object, 0, len(value))
		for _, member := range value {
			if member.Name != "_module" {
				out = append(out, generation.Member{Name: member.Name, Value: buildDemod(member.Value)})
			}
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = buildDemod(item)
		}
		return out
	default:
		return value
	}
}

func (n *buildNix) eval(installable string, apply *string) (any, error) {
	settings, err := n.settingsObject()
	if err != nil {
		return nil, err
	}
	return n.evalWithSettings(installable, apply, settings)
}

func (n *buildNix) evalWithSettings(installable string, apply *string, settings generation.Object) (any, error) {
	b, err := n.command("eval", []string{installable}, nil, apply, settings, false)
	if err != nil {
		return nil, err
	}
	return generation.DecodeJSONValue(b)
}

func (n *buildNix) buildPlan() (generation.Object, error) {
	if !n.planReady {
		value, err := n.eval(".#confctl.buildPlan", nil)
		if err != nil {
			return nil, err
		}
		object, ok := value.(generation.Object)
		if !ok {
			return nil, fmt.Errorf("build plan must be an object")
		}
		n.plan, n.planReady = object, true
	}
	return n.plan, nil
}

func (n *buildNix) machineKey(host string) (string, error) {
	if !n.keysReady {
		value, err := n.eval(".#confctl.machineKeys", nil)
		if err != nil {
			return "", err
		}
		keys, ok := value.(generation.Object)
		if !ok {
			return "", fmt.Errorf("machine keys must be an object")
		}
		n.keys, n.keysReady = keys, true
	}
	if value, present := n.keys.Lookup(host); present {
		key, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("machine key for %q is not a string", host)
		}
		return key, nil
	}
	for _, entry := range n.keys {
		if key, ok := entry.Value.(string); ok && key == host {
			return host, nil
		}
	}
	return "", fmt.Errorf("Unknown machine %q", host)
}

func (n *buildNix) build(request build.BuildRequest, _ func([]byte, []byte)) ([]byte, error) {
	settings, err := n.settingsObject()
	if err != nil {
		return nil, err
	}
	return n.command("build", request.Installables, request.LegacyArgs, nil, settings, true)
}

func buildImpure(settings generation.Object) bool {
	if nix, ok := buildObjectValue(settings, "nix").(generation.Object); ok {
		return buildObjectValue(nix, "impureEval") == true
	}
	return false
}

func buildObjectValue(object generation.Object, key string) any {
	value, _ := object.Lookup(key)
	return value
}

func (n *buildNix) command(op string, installables, legacy []string, apply *string, settings generation.Object, progress bool) ([]byte, error) {
	notify := func(stage buildCommandStage, err error) error {
		if n.commandEvent == nil {
			return nil
		}
		return n.commandEvent(buildCommandEvent{Op: op, Installables: append([]string(nil), installables...), Stage: stage, Err: err})
	}
	extra, noUpdate := false, true
	for {
		argv := []string{"nix", op, "--json"}
		if op == "build" {
			argv = append(argv, "--no-link")
		}
		if extra {
			argv = append(argv, "--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes")
		}
		if buildImpure(settings) {
			argv = append(argv, "--impure")
		}
		argv = append(argv, "--no-write-lock-file")
		if noUpdate {
			argv = append(argv, "--no-update-lock-file")
		}
		if n.options.ShowTrace {
			argv = append(argv, "--show-trace")
		}
		for _, option := range []struct {
			name  string
			value *string
		}{{"max-jobs", n.options.MaxJobs}, {"cores", n.options.Cores}} {
			if option.value != nil {
				argv = append(argv, "--option", option.name, *option.value)
			}
		}
		if apply != nil {
			argv = append(argv, "--apply", *apply)
		}
		argv = append(argv, legacy...)
		argv = append(argv, installables...)
		var parser *build.Progress
		var progressErr error
		if progress {
			// A compatibility retry owns fresh buffers/totals. B1b's group Feed
			// is deliberately unused; only this successful attempt flushes.
			parser = build.NewProgress(func(event build.Event) {
				if progressErr == nil && n.progress != nil {
					progressErr = n.progress(event)
				}
			})
		}
		var chunk func([]byte, []byte) error
		if parser != nil {
			chunk = func(stdout, stderr []byte) error {
				parser.Feed(stdout, stderr)
				return progressErr
			}
		}
		if err := notify(buildCommandStarted, nil); err != nil {
			return nil, err
		}
		result, runErr := func() (Result, error) {
			phase := timingEvaluation
			if op == "build" {
				phase = timingBuild
			}
			end := n.e.beginTiming(phase)
			defer end()
			return n.run(argv, nil, chunk)
		}()
		status, err := buildStatus(n.e.Context, result, runErr)
		if err != nil {
			_ = notify(buildCommandFailed, err) // The existing command cause wins.
			return nil, err
		}
		if status == 0 {
			if parser != nil {
				parser.Finish()
				if progressErr != nil {
					_ = notify(buildCommandFailed, progressErr)
					return nil, progressErr
				}
			}
			if err := notify(buildCommandSucceeded, nil); err != nil {
				return nil, err
			}
			return result.Stdout, nil
		}
		failure := fmt.Errorf("Running `%s` failed with\n  exit status: %d\n  stdout: %s\n  stderr: %s", testConnectionShellJoin(argv), status, testConnectionOutput(result.Stdout), testConnectionOutput(testConnectionStderr(result.Stderr)))
		message, lower := failure.Error(), strings.ToLower(failure.Error())
		if noUpdate && strings.Contains(message, "--no-update-lock-file") && (strings.Contains(lower, "unknown") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "invalid") || strings.Contains(lower, "unsupported")) {
			noUpdate = false
			continue
		}
		if !extra && strings.Contains(lower, "experimental") && (strings.Contains(lower, "nix-command") || strings.Contains(lower, "flakes")) {
			extra = true
			continue
		}
		_ = notify(buildCommandFailed, failure)
		return nil, failure
	}
}
