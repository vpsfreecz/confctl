package cli

import (
	"strings"
	"testing"
)

func TestGenerationListingAvailabilityAndScopeOptions(t *testing.T) {
	r := BuiltinRegistry()
	for _, args := range [][]string{{"generation", "ls"}, {"generation", "ls", "-lr", "*", "current"}, {"generation", "ls", "--remote", "--no-local", "*", ""}} {
		inv, err := r.Parse(args)
		if err != nil || inv.Command.Handler != GenerationListHandler || r.CheckAvailability(inv) != nil {
			t.Fatal(inv, err)
		}
	}
	inv, err := r.Parse([]string{"generation", "ls", "-lr", "--no-remote", "node", "--local"})
	if err != nil || !inv.Options["local"].Value.Bool || inv.Options["remote"].Value.Bool || len(inv.Args) != 2 || inv.Args[1] != "--local" {
		t.Fatal(inv, err)
	}
	for _, path := range [][]string{{"generation"}, {"generation", "ls"}} {
		help, err := r.Help(path, 80)
		if err != nil || !strings.Contains(help, "generation") || len(path) == 2 && strings.Contains(help, "Unavailable in") {
			t.Fatal(help, err)
		}
	}
	commands := r.Commands()
	for i := range commands {
		if commands[i].Handler == GenerationListHandler {
			commands[i].Availability.Mode = Conditional
		}
	}
	if _, err := NewRegistry(r.Globals(), commands); err == nil {
		t.Fatal("accepted conditional listing handler")
	}
}

func TestGenerationMutationDeclarationsAndAvailability(t *testing.T) {
	r := BuiltinRegistry()
	for _, tc := range []struct {
		path    []string
		handler Handler
	}{
		{[]string{"generation", "rm"}, GenerationRemoveHandler},
		{[]string{"generation", "rotate"}, GenerationRotateHandler},
		{[]string{"collect-garbage"}, CollectGarbageHandler},
	} {
		t.Run(pathKey(tc.path), func(t *testing.T) {
			inv, err := r.Parse(tc.path)
			if err != nil || inv.Command.Handler != tc.handler || inv.Command.Availability.Mode != Available || r.CheckAvailability(inv) != nil {
				t.Fatal(inv, err)
			}
			workers := inv.Options["max-concurrent-gc"].Value
			if inv.Options["yes"].Value.Bool || workers.Integer.String() != "5" {
				t.Fatal("changed confirmation/worker defaults", inv.Options)
			}
			if tc.handler != CollectGarbageHandler && (!inv.Options["collect-garbage"].Value.Bool || inv.Options["local"].Value.Bool || inv.Options["remote"].Value.Bool) {
				t.Fatal("changed scope/GC defaults", inv.Options)
			}
			for _, mode := range []AvailabilityMode{Unavailable, Conditional} {
				commands := r.Commands()
				for i := range commands {
					if pathKey(commands[i].Path) == pathKey(tc.path) {
						// A valid existing switch condition must still be rejected.
						commands[i].Availability = Availability{Mode: mode, Option: "yes", Equals: Bool(true), Reason: "requires --yes"}
					}
				}
				if _, err := NewRegistry(r.Globals(), commands); err == nil {
					t.Fatal("accepted handler/availability conflict", mode)
				}
			}
		})
	}
	inv, err := r.Parse([]string{"generation", "rm", "-lr", "--no-gc", "--max-concurrent-gc=-2", "node", "", "--yes"})
	workers := inv.Options["max-concurrent-gc"].Value
	if err != nil || !inv.Options["local"].Value.Bool || !inv.Options["remote"].Value.Bool || inv.Options["collect-garbage"].Value.Bool || workers.Integer.String() != "-2" || inv.Options["yes"].Value.Bool || len(inv.Args) != 3 || inv.Args[1] != "" || inv.Args[2] != "--yes" {
		t.Fatal("changed literal arguments or option placement", inv, err)
	}
}
