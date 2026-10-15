package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os/exec"
	"regexp"
	"strings"

	"github.com/vpsfreecz/confctl/internal/health"
)

type HealthRequest struct {
	Pattern     *string
	Attrs, Tags []string
	Workers     *big.Int // nil means the declared default, five.
}

// Parsed worker/filter errors retain the shared CLI's usage exit policy.
type healthArgumentError struct{ error }

type HealthPlan struct {
	Machines []Machine
	Checks   []health.Check
	Workers  big.Int
}

type HealthResult struct {
	Report    health.Report
	RetainLog bool // Later CLI integration must preserve logs even after success.
}

// PlanHealth is private operation preparation, not a registered CLI handler.
// Invalid worker requests precede inventory, selection and schema decoding.
func (e *Engine) PlanHealth(request HealthRequest) (HealthPlan, error) {
	var plan HealthPlan
	if request.Workers == nil {
		plan.Workers.SetInt64(5)
	} else {
		plan.Workers.Set(request.Workers)
	}
	if _, err := health.WorkerCount(&plan.Workers, 0); err != nil {
		return plan, &healthArgumentError{err}
	}
	all, err := e.Machines(false)
	if err != nil {
		return plan, err
	}
	selected, err := Select(all, request.Pattern, request.Attrs, request.Tags, "yes")
	if err != nil {
		return plan, &healthArgumentError{err}
	}
	for _, machine := range selected {
		if machine.CarrierName != nil || machine.Target.Host == nil {
			continue
		}
		checks, err := machine.healthSpecs()
		if err != nil {
			return plan, err
		}
		if len(checks) > 0 {
			plan.Machines = append(plan.Machines, machine)
			plan.Checks = append(plan.Checks, checks...)
		}
	}
	if len(plan.Checks) == 0 {
		return plan, health.ErrNoChecks
	}
	return plan, nil
}

func (e *Engine) RunHealth(plan HealthPlan, progress func(health.Event)) (HealthResult, error) {
	return e.runHealth(plan, health.Deps{Progress: progress}, e.healthProcess)
}

func (e *Engine) runHealth(plan HealthPlan, deps health.Deps, process func(context.Context, string, []string, []byte) (Result, error)) (HealthResult, error) {
	byName := make(map[string]Machine, len(plan.Machines))
	for _, machine := range plan.Machines {
		byName[machine.Name] = machine
	}
	deps.Execute = func(ctx context.Context, check health.Check) (health.CommandResult, error) {
		argv := append([]string(nil), check.Argv...)
		if check.Remote {
			machine, present := byName[check.Machine]
			if !present {
				return health.CommandResult{}, fmt.Errorf("health machine %s is absent from the plan", check.Machine)
			}
			var err error
			argv, err = SSHArgs(machine, argv)
			if err != nil {
				return health.CommandResult{}, err
			}
		}
		result, err := process(ctx, e.Root, argv, nil)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && result.waitErr != nil {
			var exit *exec.ExitError
			if !errors.As(result.waitErr, &exit) {
				err = result.waitErr
			}
		}
		if err == nil && result.ExitCode < 0 {
			err = fmt.Errorf("health command did not return an exit status")
		}
		return health.CommandResult{Status: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, err
	}
	report, err := health.Run(e.Context, plan.Checks, &plan.Workers, deps)
	return HealthResult{Report: report, RetainLog: report.RetainLog}, err
}

func (e *Engine) healthProcess(ctx context.Context, root string, argv []string, input []byte) (Result, error) {
	// Each attempt uses the existing Engine.Process route with its owned context.
	// os.File supports concurrent writes; the caller's log stays open until join.
	attempt := &Engine{Root: root, Context: ctx, Log: e.Log, logOwner: e.logOwner}
	return attempt.Process(argv, input)
}

// HealthMessages preserves configured failure order and the source diagnostics.
func HealthMessages(result health.Result) string {
	check := result.Check
	messages := make([]string, 0, len(result.Failures))
	for _, failure := range result.Failures {
		switch failure.Kind {
		case "status":
			messages = append(messages, fmt.Sprintf("%s failed with %d (%s)", ShellJoin(check.Argv), failure.Status, check.Command.Description))
		case "match":
			messages = append(messages, fmt.Sprintf("%s: %s does not match %s", ShellJoin(check.Argv), failure.Stream, rubyInspect(failure.Fragment)))
		case "include":
			messages = append(messages, fmt.Sprintf("%s: %s does not include %s", ShellJoin(check.Argv), failure.Stream, rubyInspect(failure.Fragment)))
		case "exclude":
			messages = append(messages, fmt.Sprintf("%s: %s includes %s", ShellJoin(check.Argv), failure.Stream, rubyInspect(failure.Fragment)))
		case "systemd-status":
			messages = append(messages, fmt.Sprintf("%s failed with %d", strings.Join(check.Argv, " "), failure.Status))
		case "missing-property":
			messages = append(messages, "property "+rubyInspect(failure.Property)+" not found")
		case "property":
			messages = append(messages, failure.Property+"="+failure.Actual+", expected "+failure.Want)
		}
	}
	message := strings.Join(messages, "; ")
	if check.Command == nil && check.Pattern != nil {
		message = *check.Pattern + ": " + message
	}
	return message
}

type healthField struct {
	name string
	raw  json.RawMessage
}

func healthObject(raw json.RawMessage, path string) ([]healthField, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, &health.SchemaError{Path: path, Problem: "must be an object"}
	}
	var fields []healthField
	positions := map[string]int{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, &health.SchemaError{Path: path, Problem: err.Error()}
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, &health.SchemaError{Path: path, Problem: err.Error()}
		}
		name := key.(string)
		if name == "_module" { // The existing machine decoder demod policy.
			continue
		}
		if i, duplicate := positions[name]; duplicate {
			fields[i].raw = value // Ruby Hash replacement retains first key order.
		} else {
			positions[name] = len(fields)
			fields = append(fields, healthField{name, value})
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, &health.SchemaError{Path: path, Problem: err.Error()}
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, &health.SchemaError{Path: path, Problem: "trailing JSON content"}
	}
	return fields, nil
}

func healthLookup(fields []healthField, key, path string) (json.RawMessage, error) {
	for _, field := range fields {
		if field.name == key {
			return field.raw, nil
		}
	}
	return nil, &health.SchemaError{Path: path + "." + key, Problem: "missing evaluated field"}
}

func healthString(raw json.RawMessage, path string) (string, error) {
	var value string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", &health.SchemaError{Path: path, Problem: "must be a string"}
	}
	return value, nil
}

func healthArray(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var value []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &value) != nil {
		return nil, &health.SchemaError{Path: path, Problem: "must be an array"}
	}
	return value, nil
}

func healthSeconds(raw json.RawMessage, path string) (*big.Int, error) {
	value, ok := new(big.Int).SetString(string(raw), 10)
	if !ok || value.Sign() < 0 {
		return nil, &health.SchemaError{Path: path, Problem: "must be a nonnegative integer"}
	}
	return value, nil
}

func healthTiming(fields []healthField, path string) (health.Timing, error) {
	var timing health.Timing
	for _, field := range []struct {
		name  string
		value **big.Int
	}{{"timeout", &timing.Timeout}, {"cooldown", &timing.Cooldown}} {
		raw, err := healthLookup(fields, field.name, path)
		if err != nil {
			return timing, err
		}
		*field.value, err = healthSeconds(raw, path+"."+field.name)
		if err != nil {
			return timing, err
		}
	}
	return timing, nil
}

func healthOutput(raw json.RawMessage, path string) (health.Output, error) {
	var output health.Output
	fields, err := healthObject(raw, path)
	if err != nil {
		return output, err
	}
	match, err := healthLookup(fields, "match", path)
	if err != nil {
		return output, err
	}
	if string(match) != "null" {
		text, err := healthString(match, path+".match")
		if err != nil {
			return output, err
		}
		output.Match = &text
	}
	for _, field := range []struct {
		name  string
		value *[]string
	}{{"include", &output.Include}, {"exclude", &output.Exclude}} {
		raw, err := healthLookup(fields, field.name, path)
		if err != nil {
			return output, err
		}
		items, err := healthArray(raw, path+"."+field.name)
		if err != nil {
			return output, err
		}
		for i, item := range items {
			text, err := healthString(item, fmt.Sprintf("%s.%s[%d]", path, field.name, i))
			if err != nil {
				return output, err
			}
			*field.value = append(*field.value, text)
		}
	}
	return output, nil
}

var healthAttribute = regexp.MustCompile(`\{([^}]+)\}`)

func (m Machine) healthCommand(raw json.RawMessage, path string, remote bool, accumulated int) (health.Check, error) {
	check := health.Check{Machine: m.Name, Remote: remote, Command: &health.Command{}}
	fields, err := healthObject(raw, path)
	if err != nil {
		return check, err
	}
	getString := func(key string) (string, error) {
		raw, err := healthLookup(fields, key, path)
		if err != nil {
			return "", err
		}
		return healthString(raw, path+"."+key)
	}
	check.Command.Description, err = getString("description")
	if err != nil {
		return check, err
	}
	command, err := healthLookup(fields, "command", path)
	if err != nil {
		return check, err
	}
	args, err := healthArray(command, path+".command")
	if err != nil {
		return check, err
	}
	if len(args) == 0 {
		return check, &health.SchemaError{Path: path + ".command", Problem: "must not be empty"}
	}
	for i, arg := range args {
		text, err := healthString(arg, fmt.Sprintf("%s.command[%d]", path, i))
		if err != nil {
			return check, err
		}
		text = healthAttribute.ReplaceAllStringFunc(text, func(match string) string {
			key := match[1 : len(match)-1]
			if key == "checks" {
				return fmt.Sprint(accumulated)
			}
			return str(m.Attr(key))
		})
		check.Argv = append(check.Argv, text)
	}
	check.Description = check.Command.Description
	if check.Description == "" {
		check.Description = ShellJoin(check.Argv)
	}
	status, err := healthLookup(fields, "exitStatus", path)
	if err != nil {
		return check, err
	}
	check.Command.ExpectedStatus, err = healthSeconds(status, path+".exitStatus")
	if err != nil {
		return check, err
	}
	check.Timing, err = healthTiming(fields, path)
	if err != nil {
		return check, err
	}
	for _, field := range []struct {
		name  string
		value *health.Output
	}{{"standardOutput", &check.Command.Stdout}, {"standardError", &check.Command.Stderr}} {
		raw, err := healthLookup(fields, field.name, path)
		if err != nil {
			return check, err
		}
		*field.value, err = healthOutput(raw, path+"."+field.name)
		if err != nil {
			return check, err
		}
	}
	return check, nil
}

func healthProperties(raw json.RawMessage, path string) ([]health.Property, error) {
	items, err := healthArray(raw, path)
	if err != nil {
		return nil, err
	}
	var properties []health.Property
	for i, item := range items {
		path := fmt.Sprintf("%s[%d]", path, i)
		fields, err := healthObject(item, path)
		if err != nil {
			return nil, err
		}
		var property health.Property
		for _, field := range []struct {
			name  string
			value *string
		}{{"property", &property.Name}, {"value", &property.Value}} {
			raw, err := healthLookup(fields, field.name, path)
			if err != nil {
				return nil, err
			}
			*field.value, err = healthString(raw, path+"."+field.name)
			if err != nil {
				return nil, err
			}
		}
		property.Timing, err = healthTiming(fields, path)
		if err != nil {
			return nil, err
		}
		properties = append(properties, property)
	}
	return properties, nil
}

func (m Machine) healthSpecs() ([]health.Check, error) {
	path := m.Name + ".healthChecks"
	meta, err := healthObject(m.rawMeta, m.Name+".metaConfig")
	if err != nil {
		return nil, err
	}
	raw, err := healthLookup(meta, "healthChecks", m.Name)
	if err != nil {
		return nil, err
	}
	types, err := healthObject(raw, path)
	if err != nil {
		return nil, err
	}
	var checks []health.Check
	for _, typ := range types {
		typePath := path + "." + typ.name
		switch typ.name {
		case "builderCommands", "machineCommands":
			remote := typ.name == "machineCommands"
			if remote && (m.CarrierName != nil || m.Target.Host == nil) {
				continue
			}
			items, err := healthArray(typ.raw, typePath)
			if err != nil {
				return nil, err
			}
			for i, item := range items {
				check, err := m.healthCommand(item, fmt.Sprintf("%s[%d]", typePath, i), remote, len(checks))
				if err != nil {
					return nil, err
				}
				checks = append(checks, check)
			}
		case "systemd":
			fields, err := healthObject(typ.raw, typePath)
			if err != nil {
				return nil, err
			}
			enabled, err := healthLookup(fields, "enable", typePath)
			if err != nil {
				return nil, err
			}
			if string(enabled) != "true" && string(enabled) != "false" {
				return nil, &health.SchemaError{Path: typePath + ".enable", Problem: "must be a boolean"}
			}
			if string(enabled) == "false" || m.Spin != "nixos" {
				continue
			}
			manager, err := healthLookup(fields, "systemProperties", typePath)
			if err != nil {
				return nil, err
			}
			properties, err := healthProperties(manager, typePath+".systemProperties")
			if err != nil {
				return nil, err
			}
			if len(properties) > 0 {
				check, err := health.SystemdCheck(m.Name, properties, nil)
				if err != nil {
					return nil, err
				}
				checks = append(checks, check)
			}
			units, err := healthLookup(fields, "unitProperties", typePath)
			if err != nil {
				return nil, err
			}
			unitFields, err := healthObject(units, typePath+".unitProperties")
			if err != nil {
				return nil, err
			}
			for _, unit := range unitFields {
				properties, err := healthProperties(unit.raw, typePath+".unitProperties."+unit.name)
				if err != nil {
					return nil, err
				}
				pattern := unit.name
				check, err := health.SystemdCheck(m.Name, properties, &pattern)
				if err != nil {
					return nil, err
				}
				checks = append(checks, check)
			}
		default:
			return nil, &health.SchemaError{Path: typePath, Problem: "unsupported health-check type " + rubyInspect(typ.name)}
		}
	}
	return checks, nil
}
