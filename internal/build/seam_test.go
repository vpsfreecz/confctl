package build

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPhaseHooksCompleteGroupsBeforeClockAndStartBeforeGroupWork(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	f.settings = object("nix", object("legacyNixPath", true, "impureEval", true))
	first := object("z", "/z", "nixpkgs", "/nix")
	f.row("a", object("inputs", first))
	f.row("b", object("inputs", object("nixpkgs", "/different")))
	f.row("c", object("inputs", object("nixpkgs", "/nix", "z", "/z")))
	for _, host := range []string{"a", "b", "c"} {
		f.addEvalRow(".#confctl.inputs", f.keys[host], object())
	}
	prepared, started := 0, 0
	f.c.GroupsPrepared = func(groups []Group) error {
		prepared++
		if f.clockCalls != 0 || len(f.requests) != 0 || len(f.evalCalls) != 0 || len(groups) != 2 {
			t.Fatal("planning phase order", f.trace)
		}
		if !reflect.DeepEqual(hostNames(groups[0]), []string{"a", "c"}) || !reflect.DeepEqual(groups[0].Inputs, first) || !reflect.DeepEqual(hostNames(groups[1]), []string{"b"}) {
			t.Fatal(groups)
		}
		f.trace = append(f.trace, "prepared")
		return nil
	}
	f.c.GroupStart = func(index, count int, group Group) error {
		if index != started || count != 2 || f.clockCalls != 1 || len(f.requests) != started {
			t.Fatal(index, count, started, f.trace)
		}
		if index == 0 && (!reflect.DeepEqual(hostNames(group), []string{"a", "c"}) || !reflect.DeepEqual(f.trace, []string{"plan", "settings", "prepared", "clock"})) {
			t.Fatal(group, f.trace)
		}
		if index == 1 && (!reflect.DeepEqual(hostNames(group), []string{"b"}) || f.trace[len(f.trace)-1] != "current c") {
			t.Fatal(group, f.trace)
		}
		f.trace = append(f.trace, fmt.Sprintf("start %d/%d", index, count))
		started++
		return nil
	}
	out, err := Run([]string{"a", "b", "c"}, f.c)
	if err != nil || !out.Complete || prepared != 1 || started != 2 || f.clockCalls != 1 {
		t.Fatal(err, out, prepared, started)
	}
	for i, row := range f.trace {
		if strings.HasPrefix(row, "start ") {
			want := "key a"
			if row == "start 1/2" {
				want = "key b"
			}
			if f.trace[i+1] != want {
				t.Fatal("group work preceded hook", f.trace)
			}
		}
	}
}

func TestPhaseHooksOrdinaryEmptyAndLegacyZeroGroups(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			f := newFixture(t)
			f.settings = object("nix", object("legacyNixPath", legacy))
			wantCount := 1
			if legacy {
				wantCount = 0
			}
			prepared, starts := 0, 0
			f.c.GroupsPrepared = func(groups []Group) error {
				prepared++
				if len(groups) != wantCount {
					t.Fatal(groups)
				}
				f.trace = append(f.trace, "prepared")
				return nil
			}
			f.c.GroupStart = func(index, count int, group Group) error {
				starts++
				if index != 0 || count != 1 || len(group.Hosts) != 0 {
					t.Fatal(index, count, group)
				}
				f.trace = append(f.trace, "start")
				return nil
			}
			out, err := Run(nil, f.c)
			if err != nil || !out.Complete || prepared != 1 || starts != wantCount || f.clockCalls != 1 || len(f.evalCalls) != 0 || len(f.requests) != wantCount {
				t.Fatal(err, out, prepared, starts)
			}
			want := []string{"plan", "settings", "prepared", "clock"}
			if !legacy {
				want = append(want, "start", "build", "decode")
			}
			if !reflect.DeepEqual(f.trace, want) {
				t.Fatal(f.trace, want)
			}
		})
	}
}

func TestPhaseHookErrorsStopAtExactPhase(t *testing.T) {
	for _, stage := range []string{"planning", "prepared", "start"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, "a")
			failure := errors.New("phase error")
			f.c.GroupsPrepared = func([]Group) error {
				f.trace = append(f.trace, "prepared")
				if stage == "prepared" {
					return failure
				}
				return nil
			}
			f.c.GroupStart = func(int, int, Group) error { f.trace = append(f.trace, "start"); return failure }
			host := "a"
			if stage == "planning" {
				host = "missing"
			}
			out, err := Run([]string{host}, f.c)
			if err == nil || out.Complete || len(out.Generations) != 0 || len(f.requests) != 0 || len(f.evalCalls) != 0 {
				t.Fatal(err, out)
			}
			want := []string{"plan"}
			if stage != "planning" {
				want = append(want, "settings", "prepared")
			}
			if stage == "start" {
				want = append(want, "clock", "start")
			}
			if stage != "planning" && !errors.Is(err, failure) {
				t.Fatal("identity lost", err)
			}
			if !reflect.DeepEqual(f.trace, want) {
				t.Fatal(f.trace, want)
			}
			absent(t, f.root+"/.confctl")
		})
	}
}

func TestPhaseHookLaterGroupFailureRetainsEarlierWritesAndFixedCount(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	f.settings = object("nix", object("legacyNixPath", true, "impureEval", true))
	for _, host := range []string{"a", "b", "c"} {
		f.row(host, object("key", f.keys[host], "inputs", object("nixpkgs", "/"+host)))
		f.addEvalRow(".#confctl.inputs", f.keys[host], object())
	}
	failure := errors.New("second start fails")
	indexes := []int{}
	f.c.GroupStart = func(index, count int, group Group) error {
		indexes = append(indexes, index)
		if count != 3 || !reflect.DeepEqual(hostNames(group), []string{[]string{"a", "b", "c"}[index]}) {
			t.Fatal(index, count, group)
		}
		if index == 1 {
			return failure
		}
		return nil
	}
	out, err := Run([]string{"a", "b", "c"}, f.c)
	if !errors.Is(err, failure) || out.Complete || !reflect.DeepEqual(names(out.Generations), []string{"a"}) || !reflect.DeepEqual(indexes, []int{0, 1}) || len(f.requests) != 1 || f.clockCalls != 1 {
		t.Fatal(err, out, indexes)
	}
	checkLink(t, f.root+"/.confctl/generations/a/current", out.Generations[0].Generation.Name)
	absent(t, f.root+"/.confctl/generations/b")
	absent(t, f.root+"/.confctl/generations/c")
	for _, row := range f.trace {
		if row == "key b" || row == "key c" {
			t.Fatal("later group accessed", f.trace)
		}
	}
}

func TestPhaseHooksNilAreNoOps(t *testing.T) {
	f := newFixture(t, "a")
	f.c.GroupsPrepared, f.c.GroupStart = nil, nil
	out, err := Run([]string{"a"}, f.c)
	if err != nil || !out.Complete || f.clockCalls != 1 {
		t.Fatal(err, out)
	}
	if !reflect.DeepEqual(f.trace[:4], []string{"plan", "settings", "clock", "build"}) {
		t.Fatal(f.trace)
	}
}
