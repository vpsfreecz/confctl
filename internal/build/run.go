package build

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/generation"
)

// BuildRequest contains only the per-group arguments owned by this unit.
// Common Nix arguments, compatibility fallbacks, CWD and process errors belong
// to the later production adapter. Build returns raw stdout; DecodeBuild must
// decode it even though the subsequent batched eval is authoritative.
type BuildRequest struct {
	Installables []string
	LegacyArgs   []string
}

type Callbacks struct {
	BuildPlan  func() (generation.Object, error)
	Settings   func() (generation.Object, error)
	MachineKey func(host string) (string, error)
	// Optional synchronous phase hooks receive borrowed read-only views. They
	// must not mutate/reorder data, retain mutable aliases or reenter Run.
	// GroupStart uses a zero-based index and the invocation's fixed count.
	GroupsPrepared func(groups []Group) error
	GroupStart     func(index, count int, group Group) error
	// KeyLiteral receives each unique key; the owning Nix formatter supplies
	// its string literal. No formatter or input JSON parser is cloned here.
	KeyLiteral  func(string) (string, error)
	Eval        func(installable string, apply *string) (any, error)
	Build       func(BuildRequest, func(stdout, stderr []byte)) ([]byte, error)
	DecodeBuild func([]byte) (any, error)
	Load        func(host string) (*generation.BuildList, error)
	Create      func(host, toplevel string, rollback *string, inputs, info generation.Object, at time.Time) (*generation.Build, error)
	Save        func(*generation.Build) error
	SetCurrent  func(*generation.BuildList, *generation.Build) error
	Now         func() time.Time
	Progress    func(Event)
}

type HostGeneration struct {
	Host       string
	Generation *generation.Build
}

// Outcome exposes only the completed prefix on error. Complete is set after
// every group succeeds, never after a partial mutation or a failed callback.
type Outcome struct {
	Groups      []Group
	Generations []HostGeneration
	Time        time.Time
	Complete    bool
}

// Run has no retry, rollback or repair path. Callbacks are invocation-scoped;
// BuildPlan and Settings are each read once, in source access order.
func Run(hosts []string, c Callbacks) (out Outcome, err error) {
	rows, err := c.BuildPlan()
	if err != nil {
		return out, fmt.Errorf("build plan: %w", err)
	}
	// Fetch each host's inputs before settings/grouping, as eval_host_inputs does.
	ordinary, err := Plan(hosts, rows, nil)
	if err != nil {
		return out, err
	}
	settings, err := c.Settings()
	if err != nil {
		return out, fmt.Errorf("build settings: %w", err)
	}
	legacy, err := nixSetting(settings, "legacyNixPath")
	if err != nil {
		return out, err
	}
	out.Groups = ordinary
	if literalTrue(legacy) {
		out.Groups, err = Plan(hosts, rows, legacy)
		if err != nil {
			return out, err
		}
	}
	if c.GroupsPrepared != nil {
		if err := c.GroupsPrepared(out.Groups); err != nil {
			return out, fmt.Errorf("groups prepared: %w", err)
		}
	}
	out.Time = c.Now()
	for index, group := range out.Groups {
		if c.GroupStart != nil {
			if err := c.GroupStart(index, len(out.Groups), group); err != nil {
				return out, fmt.Errorf("group %d start: %w", index, err)
			}
		}
		completed, groupErr := runGroup(group, settings, out.Time, c)
		for _, row := range completed {
			out.Generations = putGeneration(out.Generations, row)
		}
		if groupErr != nil {
			return out, groupErr
		}
	}
	out.Complete = true
	return out, nil
}

func putGeneration(rows []HostGeneration, row HostGeneration) []HostGeneration {
	for i := range rows {
		if rows[i].Host == row.Host {
			rows[i] = row
			return rows
		}
	}
	return append(rows, row)
}

func keyFor(h HostPlan, c Callbacks) (string, error) {
	for _, name := range []string{"key", "machineKey", "flakeKey"} {
		v := value(h.Row, name)
		if !truth(v) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("build key %s for %q is not a string", name, h.Host)
		}
		return s, nil
	}
	return c.MachineKey(h.Host)
}

func evalKeys(installable string, keys []string, c Callbacks) (generation.Object, error) {
	keys = unique(keys)
	if len(keys) == 0 {
		return generation.Object{}, nil
	}
	quoted := make([]string, 0, len(keys))
	for _, key := range keys {
		v, err := c.KeyLiteral(key)
		if err != nil {
			return nil, err
		}
		quoted = append(quoted, v)
	}
	apply := "x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ " + strings.Join(quoted, " ") + " ])"
	v, err := c.Eval(installable, &apply)
	if err != nil {
		return nil, err
	}
	o, ok := v.(generation.Object)
	if !ok {
		return nil, fmt.Errorf("%s did not return an object", installable)
	}
	return o, nil
}

func legacyArgs(hosts []HostPlan, settings generation.Object, c Callbacks) ([]string, error) {
	if len(hosts) == 0 {
		return []string{}, nil
	}
	legacy, err := nixSetting(settings, "legacyNixPath")
	if err != nil || !literalTrue(legacy) {
		return []string{}, err
	}
	impure, err := nixSetting(settings, "impureEval")
	if err != nil {
		return nil, err
	}
	if !literalTrue(impure) {
		return nil, fmt.Errorf("legacyNixPath requires impureEval")
	}
	keys := make([]string, 0, len(hosts))
	for _, h := range hosts {
		key, err := c.MachineKey(h.Host)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	byKey, err := evalKeys(".#confctl.inputs", keys, c)
	if err != nil {
		return nil, err
	}
	merged := generation.Object{}
	for i, h := range hosts {
		inputs := value(byKey, keys[i])
		if !truth(inputs) {
			// The source inputs_installable performs its own mapped-key lookup.
			key, err := c.MachineKey(h.Host)
			if err != nil {
				return nil, err
			}
			inputs, err = c.Eval(".#confctl.inputs."+key, nil)
			if err != nil {
				return nil, err
			}
		}
		paths, ok := inputs.(generation.Object)
		if !ok {
			return nil, fmt.Errorf("inputs for %q are not an object", h.Host)
		}
		for _, m := range paths {
			path, ok := m.Value.(string)
			if !ok || path == "" {
				continue
			}
			if old, found := merged.Lookup(m.Name); found {
				if old != path {
					return nil, fmt.Errorf("legacyNixPath requires consistent input %s across hosts; build hosts separately", m.Name)
				}
				continue
			}
			merged = append(merged, m)
		}
	}
	names, err := nixSetting(settings, "legacyNixPathMap")
	if err != nil {
		return nil, err
	}
	if names == nil {
		names = []any{"nixpkgs", "vpsadminos", "vpsadmin"}
	}
	list, ok := names.([]any)
	if !ok {
		return nil, fmt.Errorf("legacyNixPathMap is not a list")
	}
	args := []string{}
	for _, name := range list {
		s, ok := name.(string)
		if !ok {
			return nil, fmt.Errorf("legacyNixPathMap name is not a string")
		}
		if path, ok := value(merged, s).(string); ok && path != "" {
			args = append(args, "-I", s+"="+path)
		}
	}
	return args, nil
}

type pending struct {
	host, key, toplevel string
	rollback            any
	inputs              generation.Object
	list                *generation.BuildList
}

func runGroup(group Group, settings generation.Object, at time.Time, c Callbacks) ([]HostGeneration, error) {
	ret := []HostGeneration{}
	keys := make([]string, 0, len(group.Hosts))
	installables := []string{}
	for _, h := range group.Hosts {
		key, err := keyFor(h, c)
		if err != nil {
			return ret, err
		}
		keys = append(keys, key)
		installables = append(installables, ".#confctl.build."+key+".toplevel", ".#confctl.build."+key+".autoRollback")
	}
	args, err := legacyArgs(group.Hosts, settings, c)
	if err != nil {
		return ret, err
	}
	progress := NewProgress(c.Progress)
	bytes, err := c.Build(BuildRequest{installables, args}, progress.Feed)
	if err != nil {
		return ret, fmt.Errorf("nix build: %w", err)
	}
	progress.Finish()
	if _, err := c.DecodeBuild(bytes); err != nil {
		return ret, fmt.Errorf("nix build JSON: %w", err)
	}
	outputs, err := evalKeys(".#confctl.build", keys, c)
	if err != nil {
		return ret, fmt.Errorf("build outputs: %w", err)
	}
	waiting := []pending{}
	for _, h := range group.Hosts {
		key, err := keyFor(h, c)
		if err != nil {
			return ret, err
		}
		v := value(outputs, key)
		if v == nil {
			return ret, fmt.Errorf("missing build outputs for %q", key)
		}
		row, ok := v.(generation.Object)
		if !ok {
			return ret, fmt.Errorf("invalid build outputs for %q", key)
		}
		topValue, rollbackValue := value(row, "toplevel"), value(row, "autoRollback")
		if topValue == nil || rollbackValue == nil {
			return ret, fmt.Errorf("invalid build outputs for %q", key)
		}
		list, err := c.Load(h.Host)
		if err != nil {
			return ret, fmt.Errorf("%s load generations: %w", h.Host, err)
		}
		top, ok := topValue.(string)
		if !ok {
			return ret, fmt.Errorf("toplevel for %q is not a string", key)
		}
		inputs, ok := h.Inputs.(generation.Object)
		if !ok {
			return ret, fmt.Errorf("inputs for %q are not an object", h.Host)
		}
		if build := generation.FindReusable(list, top, inputs); build != nil {
			if err := c.SetCurrent(list, build); err != nil {
				return ret, fmt.Errorf("%s reuse current: %w", h.Host, err)
			}
			ret = putGeneration(ret, HostGeneration{h.Host, build})
			continue
		}
		waiting = append(waiting, pending{h.Host, key, top, rollbackValue, inputs, list})
	}
	if len(waiting) == 0 {
		return ret, nil
	}
	pendingKeys := make([]string, 0, len(waiting))
	for _, p := range waiting {
		pendingKeys = append(pendingKeys, p.key)
	}
	infos, err := evalKeys(".#confctl.inputsInfo", pendingKeys, c)
	if err != nil {
		return ret, fmt.Errorf("inputs info: %w", err)
	}
	for _, p := range waiting {
		v := value(infos, p.key)
		if !truth(v) {
			key, err := c.MachineKey(p.host)
			if err != nil {
				return ret, err
			}
			v, err = c.Eval(".#confctl.inputsInfo."+key, nil)
			if err != nil {
				return ret, fmt.Errorf("%s inputs info: %w", p.host, err)
			}
		}
		info, ok := v.(generation.Object)
		if !ok {
			return ret, fmt.Errorf("inputs info for %q is not an object", p.host)
		}
		rollback, supported := p.rollback.(string)
		var target *string
		if supported {
			target = &rollback
		}
		// Construction does not read rollback. Nil is used solely for this
		// unused constructor argument; the raw unsupported value stays private
		// and cannot reach Save or an outcome/list through the temporary Build.
		build, err := c.Create(p.host, p.toplevel, target, p.inputs, info, at)
		if err != nil {
			return ret, fmt.Errorf("%s create generation: %w", p.host, err)
		}
		// Accepted malformed-output exception: metadata and constructor errors
		// win, but a new non-string rollback is rejected before Save. Ruby may
		// instead mkdir/create the toplevel link or fail at an earlier Save
		// operation. Admission outranks those unattempted filesystem errors;
		// it does not undo earlier hosts' writes or reuse/current effects.
		if !supported {
			return ret, fmt.Errorf("%s (%s): autoRollback build output must be a string (got %s)", p.host, p.key, targetType(p.rollback))
		}
		if err := c.Save(build); err != nil {
			return ret, fmt.Errorf("%s save generation: %w", p.host, err)
		}
		if err := c.SetCurrent(p.list, build); err != nil {
			return ret, fmt.Errorf("%s new current: %w", p.host, err)
		}
		ret = putGeneration(ret, HostGeneration{p.host, build})
	}
	return ret, nil
}

func targetType(v any) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case []any:
		return "array"
	case generation.Object:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}
