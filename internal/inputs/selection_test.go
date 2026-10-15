package inputs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestChannelOrderingSelectionAndTargets(t *testing.T) {
	channels, err := DecodeChannels([]byte(`{"z":{"os":"shared","confctl":"own"},"a":{"confctl":"own","os":"other"},"nil":null,"z":{"os":"shared","confctl":"own"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// The production adapter supplies core.Match. This small predicate isolates
	// the selector's ordering and literal-list branch from pattern implementation.
	match := func(p, n string) bool { return p == "*" || p == n }
	for _, tc := range []struct {
		selector *string
		want     []string
	}{
		{nil, []string{"a", "nil", "z"}},
		{stringPtr("*"), []string{"a", "nil", "z"}},
		{stringPtr(" { z, a, z,, missing } \x00"), []string{"z", "a", "z"}},
		{stringPtr("z,a"), []string{"z", "a"}},
		{stringPtr("{*}"), []string{}},
		{stringPtr("{z}"), []string{"z"}},
		{stringPtr("a"), []string{"a"}},
		{stringPtr(""), []string{}},
	} {
		selected := SelectChannels(channels, tc.selector, match)
		names := []string{}
		for _, c := range selected {
			names = append(names, c.Name)
		}
		if !reflect.DeepEqual(names, tc.want) {
			t.Fatal(tc.selector, names, tc.want)
		}
	}
	selected := SelectChannels(channels, stringPtr("{z,a,z}"), match)
	targets := ChannelTargets(selected, nil)
	want := []Target{{"z", "os", "shared"}, {"z", "confctl", "own"}, {"a", "confctl", "own"}, {"a", "os", "other"}, {"z", "os", "shared"}, {"z", "confctl", "own"}}
	if !reflect.DeepEqual(targets, want) || !reflect.DeepEqual(SelectedInputs(targets), []any{"shared", "own", "other"}) {
		t.Fatal(targets, SelectedInputs(targets))
	}
	if got := ChannelTargets(selected, stringPtr("os")); !reflect.DeepEqual(got, []Target{want[0], want[3], want[4]}) {
		t.Fatal(got)
	}
	if got := ChannelTargets(selected, stringPtr("absent")); len(got) != 0 {
		t.Fatal(got)
	}
}

func stringPtr(s string) *string { return &s }

func TestChannelsNonobjectAndCompleteJSON(t *testing.T) {
	for _, b := range []string{`[]`, `null`, `false`, `1`, `"scalar"`} {
		channels, err := DecodeChannels([]byte(b))
		if err != nil || len(channels) != 0 {
			t.Fatal(b, channels, err)
		}
	}
	for _, b := range []string{`bad`, `{} []`, `{"a":`} {
		if _, err := DecodeChannels([]byte(b)); err == nil {
			t.Fatal("accepted invalid channel JSON", b)
		}
	}
}

func TestMachineExactNameKeyAndRoleBoundary(t *testing.T) {
	var calls []string
	r := MachineResolver{Eval: func(s string) ([]byte, error) {
		calls = append(calls, s)
		switch s {
		case ".#confctl.machineKeys":
			return []byte(`{"alpha/site":"key-alpha","key-alpha":"key-collision"}`), nil
		case ".#confctl.inputsInfo.key-alpha":
			return []byte(`{"os":{"input":"nixpkgs"},"empty":{"input":""},"null":null}`), nil
		case ".#confctl.inputsInfo.key-collision":
			return []byte(`{"os":{"input":"collision"}}`), nil
		default:
			return nil, errors.New("unexpected installable: " + s)
		}
	}}
	if got, err := r.Resolve("alpha/site", "os"); err != nil || got != "nixpkgs" {
		t.Fatal(got, err)
	}
	if got, err := r.Resolve("key-alpha", "os"); err != nil || got != "collision" {
		t.Fatal("name must win over another machine's key", got, err)
	}
	if got, err := r.Resolve("key-collision", "os"); err != nil || got != "collision" {
		t.Fatal("exact key lookup", got, err)
	}
	if got, err := r.Resolve("alpha/site", "empty"); err != nil || got != "" {
		t.Fatal("empty string is Ruby truthy", got, err)
	}
	before := len(calls)
	if _, err := r.Resolve("alpha/*", "os"); err == nil || err.Error() != `Unknown machine "alpha/*"` || len(calls) != before {
		t.Fatal("pattern unexpectedly resolved or evaluated", calls, err)
	}
	if _, err := r.Resolve("alpha/site", "null"); err == nil || err.Error() != "machine 'alpha/site' has no role 'null'" {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls[:4], []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key-alpha", ".#confctl.inputsInfo.key-collision", ".#confctl.inputsInfo.key-collision"}) {
		t.Fatal(calls)
	}
}

func TestMachineUnavailableRoleAndEvaluationFailures(t *testing.T) {
	for _, tc := range []struct{ value, message string }{
		{`null`, "inputs info unavailable for machine 'host'"},
		{`[]`, "inputs info unavailable for machine 'host'"},
		{`{"os":false}`, "machine 'host' has no role 'os'"},
		{`{"os":{"input":false}}`, "machine 'host' has no role 'os'"},
		{`{"other":{"input":"input"}}`, "machine 'host' has no role 'os'"},
	} {
		r := MachineResolver{Eval: func(s string) ([]byte, error) {
			if strings.HasSuffix(s, "machineKeys") {
				return []byte(`{"host":"key"}`), nil
			}
			return []byte(tc.value), nil
		}}
		if _, err := r.Resolve("host", "os"); err == nil || err.Error() != tc.message {
			t.Fatal(tc.value, err)
		}
	}
	failure := errors.New("evaluation failed")
	for _, stage := range []string{"keys", "info"} {
		r := MachineResolver{Eval: func(s string) ([]byte, error) {
			if stage == "keys" || strings.Contains(s, "inputsInfo") {
				return nil, failure
			}
			return []byte(`{"host":"key"}`), nil
		}}
		if _, err := r.Resolve("host", "os"); err != failure {
			t.Fatal("evaluation error lost", stage, err)
		}
	}
}

func TestMachineResolverRetriesRejectedKeyMapping(t *testing.T) {
	for _, bad := range []string{
		`{"a":"prefix-a","b":"prefix-b"} {}`,
		`{"a":"prefix-a","b":"prefix-b"} bad`,
		`{"a":`,
		`[]`,
	} {
		t.Run(bad, func(t *testing.T) {
			var calls []string
			keyCalls := 0
			r := MachineResolver{Eval: func(installable string) ([]byte, error) {
				calls = append(calls, installable)
				switch installable {
				case ".#confctl.machineKeys":
					keyCalls++
					if keyCalls == 1 {
						return []byte(bad), nil
					}
					return []byte(`{"a":"key-a","b":"key-b"}`), nil
				case ".#confctl.inputsInfo.key-b":
					return []byte(`{"os":{"input":"valid-b"}}`), nil
				case ".#confctl.inputsInfo.prefix-b":
					return []byte(`{"os":{"input":"poisoned-prefix"}}`), nil
				default:
					return nil, errors.New("unexpected installable: " + installable)
				}
			}}
			if _, err := r.Resolve("a", "os"); err == nil {
				t.Fatal("invalid mapping accepted")
			}
			publishedPrefix := r.keys != nil
			got, err := r.Resolve("b", "os")
			want := []string{".#confctl.machineKeys", ".#confctl.machineKeys", ".#confctl.inputsInfo.key-b"}
			if publishedPrefix || err != nil || got != "valid-b" || !reflect.DeepEqual(calls, want) {
				t.Fatalf("failed mapping cached prefix=%v; next got%v err%v trace%v; want%v", publishedPrefix, got, err, calls, want)
			}
		})
	}
}

func TestMachineResolverRawMetadataAndSharedKeyLookup(t *testing.T) {
	raw := []byte(" \n" + `{"z":{"rev":"revision-z","nested":{"b":9007199254740993,"a":[999999999999999999999999,1.000000000000000000001]}},"os":{"input":"nixpkgs"},"a":"legacy-revision"}` + "\t\n")
	var calls []string
	r := MachineResolver{Eval: func(s string) ([]byte, error) {
		calls = append(calls, s)
		switch s {
		case ".#confctl.machineKeys":
			return []byte(`{"alpha/site":"key-alpha","key-alpha":"key-collision"}`), nil
		case ".#confctl.inputsInfo.key-alpha", ".#confctl.inputsInfo.key-collision":
			return raw, nil
		default:
			return nil, errors.New("unexpected installable: " + s)
		}
	}}
	for _, machine := range []string{"alpha/site", "key-alpha", "key-collision", "alpha/site"} {
		got, err := r.InputInfo(machine)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("raw metadata changed for %q: %q err%v", machine, got, err)
		}
	}
	if got, err := r.Resolve("alpha/site", "os"); err != nil || got != "nixpkgs" {
		t.Fatal("Resolve no longer delegates through shared keys", got, err)
	}
	want := []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key-alpha", ".#confctl.inputsInfo.key-collision", ".#confctl.inputsInfo.key-collision", ".#confctl.inputsInfo.key-alpha", ".#confctl.inputsInfo.key-alpha"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal(calls, want)
	}
	before := len(calls)
	if got, err := r.InputInfo("alpha/*"); got != nil || err == nil || err.Error() != `Unknown machine "alpha/*"` || len(calls) != before {
		t.Fatal("unknown host dispatched metadata", got, err, calls)
	}
}

func TestMachineResolverRawMetadataIsNotDecodedOrCached(t *testing.T) {
	for _, raw := range []string{"", `bad JSON`, `{} []`, `null`, `[]`} {
		t.Run(raw, func(t *testing.T) {
			calls := []string{}
			metadataCalls := 0
			r := MachineResolver{Eval: func(s string) ([]byte, error) {
				calls = append(calls, s)
				if s == ".#confctl.machineKeys" {
					return []byte(`{"host":"key"}`), nil
				}
				metadataCalls++
				if metadataCalls == 1 {
					return []byte(raw), nil
				}
				return []byte(`{"os":{"input":"later"}}`), nil
			}}
			got, err := r.InputInfo("host")
			if err != nil || string(got) != raw {
				t.Fatal("raw boundary decoded metadata", got, err)
			}
			input, err := r.Resolve("host", "os")
			want := []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key", ".#confctl.inputsInfo.key"}
			if err != nil || input != "later" || !reflect.DeepEqual(calls, want) {
				t.Fatal(input, err, calls)
			}
		})
	}
}

func TestMachineResolverRetainsKeysAfterMetadataFailures(t *testing.T) {
	for _, failure := range []error{errors.New("metadata failed"), context.Canceled, context.DeadlineExceeded} {
		for _, resolve := range []bool{false, true} {
			calls := []string{}
			partial := []byte("partial raw bytes")
			r := MachineResolver{Eval: func(s string) ([]byte, error) {
				calls = append(calls, s)
				switch s {
				case ".#confctl.machineKeys":
					return []byte(`{"a":"key-a","b":"key-b"}`), nil
				case ".#confctl.inputsInfo.key-a":
					return partial, failure
				case ".#confctl.inputsInfo.key-b":
					return []byte(`{"os":{"input":"good-b"}}`), nil
				}
				return nil, errors.New("unexpected installable: " + s)
			}}
			if resolve {
				if got, err := r.Resolve("a", "os"); got != nil || err != failure {
					t.Fatal("Resolve changed evaluation failure", got, err)
				}
			} else if got, err := r.InputInfo("a"); !bytes.Equal(got, partial) || err != failure {
				t.Fatal("raw evaluation result changed", got, err)
			}
			if got, err := r.Resolve("b", "os"); err != nil || got != "good-b" {
				t.Fatal(got, err)
			}
			want := []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key-a", ".#confctl.inputsInfo.key-b"}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal("successful keys discarded after metadata error", calls, want)
			}
		}
	}
	for _, raw := range []string{`bad JSON`, `{} []`} {
		calls := []string{}
		r := MachineResolver{Eval: func(s string) ([]byte, error) {
			calls = append(calls, s)
			if s == ".#confctl.machineKeys" {
				return []byte(`{"a":"key-a","b":"key-b"}`), nil
			}
			if s == ".#confctl.inputsInfo.key-a" {
				return []byte(raw), nil
			}
			return []byte(`{"os":{"input":"good-b"}}`), nil
		}}
		if _, err := r.Resolve("a", "os"); err == nil {
			t.Fatal("invalid metadata accepted")
		}
		if got, err := r.Resolve("b", "os"); err != nil || got != "good-b" {
			t.Fatal(got, err)
		}
		if !reflect.DeepEqual(calls, []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key-a", ".#confctl.inputsInfo.key-b"}) {
			t.Fatal("metadata decode error dropped keys", calls)
		}
	}
}

func TestMachineResolverRetriesKeyEvaluationFailures(t *testing.T) {
	for _, failure := range []error{errors.New("mapping eval failed"), context.Canceled, context.DeadlineExceeded} {
		calls := []string{}
		keyCalls := 0
		r := MachineResolver{Eval: func(s string) ([]byte, error) {
			calls = append(calls, s)
			if s == ".#confctl.machineKeys" {
				keyCalls++
				if keyCalls == 1 {
					return []byte(`{"a":"prefix-a"}`), failure
				}
				return []byte(`{"a":"key-a","b":"key-b"}`), nil
			}
			return []byte(`{"os":{"input":"good-b"}}`), nil
		}}
		if got, err := r.InputInfo("a"); got != nil || err != failure || r.keys != nil {
			t.Fatal(got, err, r.keys)
		}
		if got, err := r.Resolve("b", "os"); err != nil || got != "good-b" {
			t.Fatal(got, err)
		}
		if !reflect.DeepEqual(calls, []string{".#confctl.machineKeys", ".#confctl.machineKeys", ".#confctl.inputsInfo.key-b"}) {
			t.Fatal(calls)
		}
	}
}

func TestMachineResolverLiteralKeyFormattingAndRoleValues(t *testing.T) {
	for _, tc := range []struct{ mapping, installable string }{
		{`{"host":"key 'quoted';$(literal)\\path"}`, ".#confctl.inputsInfo.key 'quoted';$(literal)\\path"},
		{`{"host":9007199254740993}`, ".#confctl.inputsInfo.9007199254740993"},
		{`{"host":null}`, ".#confctl.inputsInfo.<nil>"},
		{`{"host":false}`, ".#confctl.inputsInfo.false"},
	} {
		calls := []string{}
		r := MachineResolver{Eval: func(s string) ([]byte, error) {
			calls = append(calls, s)
			if s == ".#confctl.machineKeys" {
				return []byte(tc.mapping), nil
			}
			if s != tc.installable {
				t.Fatalf("literal key formatting changed: %q want%q", s, tc.installable)
			}
			return []byte(`{"os":{"input":9007199254740993},"empty":{"input":""},"true":{"input":true},"array":{"input":["literal",false]},"object":{"input":{"raw":"value"}},"nil":{"input":null}}`), nil
		}}
		for _, role := range []string{"os", "empty", "true", "array", "object"} {
			got, err := r.Resolve("host", role)
			if err != nil {
				t.Fatal(role, err)
			}
			want := map[string]any{"os": json.Number("9007199254740993"), "empty": "", "true": true, "array": []any{"literal", false}, "object": map[string]any{"raw": "value"}}[role]
			if !reflect.DeepEqual(got, want) {
				t.Fatal(role, got, want)
			}
		}
		if _, err := r.Resolve("host", "nil"); err == nil || err.Error() != "machine 'host' has no role 'nil'" {
			t.Fatal(err)
		}
		if calls[0] != ".#confctl.machineKeys" || len(calls) != 7 {
			t.Fatal(calls)
		}
		for _, s := range calls[1:] {
			if s != tc.installable {
				t.Fatal(calls)
			}
		}
	}
}

func TestSharedInputUsesOtherRoleOutsideSelectionAndSortedMessage(t *testing.T) {
	all := []Channel{
		{Name: "z", Roles: []Role{{"os", "b"}, {"confctl", "b"}, {"extra", "a"}}},
		{Name: "a", Roles: []Role{{"os", "a"}, {"confctl", "b"}}},
		{Name: "outside", Roles: []Role{{"z", "b"}, {"a", "b"}, {"os", "a"}}},
	}
	selected := []Channel{all[0], all[1], all[0]}
	targets := ChannelTargets(selected, stringPtr("os"))
	if !reflect.DeepEqual(targets, []Target{{"z", "os", "b"}, {"a", "os", "a"}, {"z", "os", "b"}}) || !reflect.DeepEqual(SelectedInputs(targets), []any{"b", "a"}) {
		t.Fatal(targets, SelectedInputs(targets))
	}
	uses := SharedInputUses(all, selected, "os", []any{"b", "a", "b"})
	want := []SharedInputUse{
		{Input: "b", Uses: []ChannelRoleUse{{"z", "confctl"}, {"a", "confctl"}, {"outside", "z"}, {"outside", "a"}}},
		{Input: "a", Uses: []ChannelRoleUse{{"z", "extra"}, {"outside", "os"}}},
	}
	if !reflect.DeepEqual(uses, want) {
		t.Fatal(uses)
	}
	// Literal diagnostic layout is owned by cli/inputs/channels.rb; no warning
	// or refusal suffix belongs to this private helper.
	message, err := SharedInputMessage(uses, setTestArgument)
	expected := "selected input(s) are shared outside the requested channel/role: a (also used by outside/os, z/extra); b (also used by a/confctl, outside/a, outside/z, z/confctl)"
	if err != nil || message != expected || !reflect.DeepEqual(uses, want) {
		t.Fatal(message, err, uses)
	}
	if len(SharedInputUses(all, all, "confctl", []any{"absent"})) != 0 {
		t.Fatal("absent sharing")
	}
}

func TestSharedInputUsesRetainsRawValuesUntilSorting(t *testing.T) {
	name := []any{"opaque", false}
	all := []Channel{{Name: "selected", Roles: []Role{{"os", name}, {"other", []any{"opaque", false}}}}, {Name: "outside", Roles: []Role{{"os", "string"}}}}
	uses := SharedInputUses(all, all[:1], "os", []any{name, "string"})
	if len(uses) != 2 || !reflect.DeepEqual(uses[0].Input, name) || !reflect.DeepEqual(uses[0].Uses, []ChannelRoleUse{{"selected", "other"}}) {
		t.Fatal(uses)
	}
	called := false
	_, err := SharedInputMessage(uses, func(any) string { called = true; return "coerced" })
	if err == nil || called {
		t.Fatal("raw mixed keys were stringified before sorting", err, called)
	}
	uses = uses[:1]
	message, err := SharedInputMessage(uses, func(v any) string {
		if !reflect.DeepEqual(v, name) {
			t.Fatal(v)
		}
		return "raw"
	})
	if err != nil || message != "selected input(s) are shared outside the requested channel/role: raw (also used by selected/other)" {
		t.Fatal(message, err)
	}
}
