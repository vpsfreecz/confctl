package inputs

import (
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
