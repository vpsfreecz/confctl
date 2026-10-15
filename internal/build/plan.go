// Package build owns private, ordered build orchestration. Its callbacks own
// Nix execution, JSON decoding and key quoting; it neither starts processes nor
// imports core. Generation writes retain their sequential partial effects.
package build

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"

	"github.com/vpsfreecz/confctl/internal/generation"
)

// HostPlan retains the decoded source row and Ruby-truthy inputs value. Invalid
// shapes are not silently replaced with empty maps or prevalidated as a batch.
type HostPlan struct {
	Host   string
	Row    generation.Object
	Inputs any
}

type Group struct {
	Hosts  []HostPlan
	Inputs any
}

func value(o generation.Object, name string) any { v, _ := o.Lookup(name); return v }
func truth(v any) bool                           { return v != nil && v != false }
func literalTrue(v any) bool                     { b, ok := v.(bool); return ok && b }

func nixSetting(settings generation.Object, name string) (any, error) {
	v := value(settings, "nix")
	if v == nil {
		return nil, nil
	}
	o, ok := v.(generation.Object)
	if !ok {
		return nil, fmt.Errorf("nix settings are not an object")
	}
	return value(o, name), nil
}

// Plan implements eval_host_inputs followed by input_build_groups. Duplicate
// hosts replace their value without changing their first insertion position.
func Plan(hosts []string, rows generation.Object, legacy any) ([]Group, error) {
	selected := []HostPlan{}
	indexes := map[string]int{}
	for _, host := range hosts {
		v, found := rows.Lookup(host)
		if !found {
			return nil, fmt.Errorf("build plan: key not found: %q", host)
		}
		row, ok := v.(generation.Object)
		if !ok {
			return nil, fmt.Errorf("build plan for %q is not an object", host)
		}
		inputs := value(row, "inputs")
		if !truth(inputs) {
			inputs = generation.Object{}
		}
		h := HostPlan{host, row, inputs}
		if i, found := indexes[host]; found {
			selected[i] = h
		} else {
			indexes[host] = len(selected)
			selected = append(selected, h)
		}
	}
	if !literalTrue(legacy) {
		return []Group{{Hosts: selected, Inputs: generation.Object{}}}, nil
	}
	groups := []Group{}
	for _, h := range selected {
		found := false
		for i := range groups {
			if sameValue(groups[i].Inputs, h.Inputs) {
				groups[i].Hosts = append(groups[i].Hosts, h)
				found = true
				break
			}
		}
		if !found {
			groups = append(groups, Group{Hosts: []HostPlan{h}, Inputs: h.Inputs})
		}
	}
	return groups, nil
}

// Ruby hash equality ignores insertion order; JSON numeric equality is numeric,
// without converting exact tokens to float64. The first map remains displayed.
func sameValue(a, b any) bool {
	switch a := a.(type) {
	case generation.Object:
		b, ok := b.(generation.Object)
		if !ok || len(a) != len(b) {
			return false
		}
		for _, m := range a {
			v, found := b.Lookup(m.Name)
			if !found || !sameValue(m.Value, v) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !sameValue(a[i], b[i]) {
				return false
			}
		}
		return true
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		left, lok := new(big.Rat).SetString(string(a))
		right, rok := new(big.Rat).SetString(string(b))
		return lok && rok && left.Cmp(right) == 0
	default:
		return reflect.DeepEqual(a, b)
	}
}

func unique(keys []string) []string {
	ret := []string{}
	seen := map[string]bool{}
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			ret = append(ret, key)
		}
	}
	return ret
}
