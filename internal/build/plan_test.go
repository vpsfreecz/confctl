package build

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/vpsfreecz/confctl/internal/generation"
)

func object(pairs ...any) generation.Object {
	o := generation.Object{}
	for i := 0; i < len(pairs); i += 2 {
		o = append(o, generation.Member{Name: pairs[i].(string), Value: pairs[i+1]})
	}
	return o
}
func hostNames(group Group) []string {
	names := []string{}
	for _, h := range group.Hosts {
		names = append(names, h.Host)
	}
	return names
}

// Predictions from eval_host_inputs/input_build_groups at e813d3d: maps
// compare without order, the first map is displayed, and only literal true
// selects grouping. These are synthetic source fixtures, not Ruby captures.
func TestPlanSourceGroupsAndTruthiness(t *testing.T) {
	first := object("z", "/z", "a", "/a")
	reordered := object("a", "/a", "z", "/z")
	rows := object("a", object("inputs", first), "b", object("inputs", object("z", "/other")), "c", object("inputs", reordered), "d", object("inputs", nil), "e", object("inputs", false))
	groups, err := Plan([]string{"a", "b", "c", "d", "e", "a"}, rows, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups: %#v", groups)
	}
	for i, want := range [][]string{{"a", "c"}, {"b"}, {"d", "e"}} {
		if !reflect.DeepEqual(hostNames(groups[i]), want) {
			t.Fatalf("group %d: %v", i, hostNames(groups[i]))
		}
	}
	if !reflect.DeepEqual(groups[0].Inputs, first) {
		t.Fatal("first map order lost")
	}
	for _, legacy := range []any{nil, false, "true", json.Number("1")} {
		groups, err := Plan([]string{"c", "a", "b"}, rows, legacy)
		if err != nil || len(groups) != 1 || !reflect.DeepEqual(hostNames(groups[0]), []string{"c", "a", "b"}) || !reflect.DeepEqual(groups[0].Inputs, generation.Object{}) {
			t.Fatalf("ordinary %v: %#v %v", legacy, groups, err)
		}
	}
	for _, literal := range []any{"", json.Number("0"), []any{}, generation.Object{}} {
		groups, err := Plan([]string{"h"}, object("h", object("inputs", literal)), false)
		if err != nil || !reflect.DeepEqual(groups[0].Hosts[0].Inputs, literal) {
			t.Fatalf("truthy input %#v erased: %v", literal, err)
		}
	}
	for _, bad := range []generation.Object{object(), object("h", nil), object("h", false)} {
		if _, err := Plan([]string{"h"}, bad, nil); err == nil {
			t.Fatalf("accepted missing/nonobject row: %#v", bad)
		}
	}
}

func TestOrderedEqualityKeepsExactNumbers(t *testing.T) {
	a := object("n", json.Number("9007199254740993"), "x", []any{nil, false, object("a", "v")})
	b := object("x", []any{nil, false, object("a", "v")}, "n", json.Number("9007199254740993"))
	if !sameValue(a, b) || sameValue(a, object("n", json.Number("9007199254740992"), "x", value(b, "x"))) {
		t.Fatal("exact integer/order equality")
	}
	if !sameValue(json.Number("1e3"), json.Number("1000")) {
		t.Fatal("numeric token equality")
	}
}
