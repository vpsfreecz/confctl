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
	for _, args := range [][]string{{"generation", "rm"}, {"generation", "rotate"}, {"collect-garbage"}, {"build"}, {"status"}, {"status", "-gcurrent"}, {"status", "-g0"}} {
		inv, err := r.Parse(args)
		if err != nil || r.CheckAvailability(inv) == nil {
			t.Fatal("broadened availability", args, err)
		}
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
