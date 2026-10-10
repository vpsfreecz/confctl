package inputs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type Role struct {
	Name  string
	Input any
}
type Channel struct {
	Name  string
	Roles []Role
}

// DecodeChannels preserves each object's insertion order. A non-object channel
// result is empty, as in the original eval_channels; nil mappings have no roles.
func DecodeChannels(b []byte) ([]Channel, error) {
	var value json.RawMessage
	if err := decode(b, &value); err != nil {
		return nil, err
	}
	if v := bytes.TrimSpace(value); len(v) == 0 || v[0] != '{' {
		return []Channel{}, nil
	}
	names, raw, err := members(value)
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, 0, len(names))
	for _, name := range names {
		channel := Channel{Name: name}
		if v := string(bytes.TrimSpace(raw[name])); v != "null" && v != "false" {
			roles, values, err := members(raw[name])
			if err != nil {
				return nil, err
			}
			for _, role := range roles {
				var input any
				if err = decode(values[role], &input); err != nil {
					return nil, err
				}
				channel.Roles = append(channel.Roles, Role{Name: role, Input: input})
			}
		}
		channels = append(channels, channel)
	}
	return channels, nil
}

func members(b []byte) ([]string, map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	token, err := d.Token()
	if err != nil {
		return nil, nil, err
	}
	if token != json.Delim('{') {
		return nil, nil, fmt.Errorf("expected channel mapping object")
	}
	names := []string{}
	values := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, nil, err
		}
		name := token.(string)
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, exists := values[name]; !exists {
			names = append(names, name)
		}
		values[name] = v
	}
	_, err = d.Token()
	return names, values, err
}

// Matcher is the existing Ruby-compatible core pattern adapter, not filepath.Match.
type Matcher func(pattern, name string) bool

func SelectChannels(channels []Channel, selector *string, match Matcher) []Channel {
	byName := map[string]Channel{}
	for _, c := range channels {
		byName[c.Name] = c
	}
	names := []string{}
	if selector != nil && strings.ContainsAny(*selector, "{,") {
		s := strings.Trim(*selector, "\x00\t\n\v\f\r ")
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			s = s[1 : len(s)-1]
		}
		for _, n := range strings.Split(s, ",") {
			n = strings.Trim(n, "\x00\t\n\v\f\r ")
			if n != "" {
				names = append(names, n)
			}
		}
	} else {
		for _, c := range channels {
			if selector == nil || match(*selector, c.Name) {
				names = append(names, c.Name)
			}
		}
		sort.Strings(names)
	}
	out := []Channel{}
	for _, name := range names {
		if c, ok := byName[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

type Target struct {
	Channel, Role string
	Input         any
}

func ChannelTargets(channels []Channel, role *string) []Target {
	targets := []Target{}
	for _, c := range channels {
		for _, r := range c.Roles {
			if role == nil || *role == r.Name && r.Input != nil && r.Input != false {
				targets = append(targets, Target{Channel: c.Name, Role: r.Name, Input: r.Input})
			}
		}
	}
	return targets
}

// SelectedInputs mirrors first-seen uniq, retaining target order and duplicates
// in the targets themselves. It prepares the selection boundary for mutators.
func SelectedInputs(targets []Target) []any {
	out := []any{}
	for _, target := range targets {
		found := false
		for _, input := range out {
			if reflect.DeepEqual(input, target.Input) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, target.Input)
		}
	}
	return out
}

// MachineResolver evaluates only the key mapping and this exact machine's input
// info. Its evaluator dependency supplies the existing Nix settings/flags.
type MachineResolver struct {
	Eval func(installable string) ([]byte, error)
	keys map[string]any
}

func (r *MachineResolver) Resolve(machine, role string) (any, error) {
	if r.keys == nil {
		b, err := r.Eval(".#confctl.machineKeys")
		if err != nil {
			return nil, err
		}
		if err = decode(b, &r.keys); err != nil {
			return nil, err
		}
	}
	key, known := r.keys[machine]
	if !known {
		for _, k := range r.keys {
			if k == machine {
				key, known = machine, true
				break
			}
		}
	}
	if !known {
		return nil, fmt.Errorf("Unknown machine %q", machine)
	}
	b, err := r.Eval(fmt.Sprintf(".#confctl.inputsInfo.%v", key))
	if err != nil {
		return nil, err
	}
	var info any
	if err = decode(b, &info); err != nil {
		return nil, err
	}
	mapping, ok := info.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("inputs info unavailable for machine '%s'", machine)
	}
	roleInfo, _ := mapping[role].(map[string]any)
	input := roleInfo["input"]
	if input == nil || input == false {
		return nil, fmt.Errorf("machine '%s' has no role '%s'", machine, role)
	}
	return input, nil
}
