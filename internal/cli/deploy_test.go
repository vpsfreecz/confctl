package cli

import (
	"reflect"
	"testing"
)

func TestDeployDeclarationsAndAvailability(t *testing.T) {
	r := BuiltinRegistry()
	inv, err := r.Parse([]string{"deploy"})
	if err != nil || inv.Command.Handler != DeployHandler || r.CheckAvailability(inv) != nil {
		t.Fatal(inv, err)
	}
	workers := inv.Options["max-concurrent-copy"].Value
	if workers.Integer.String() != "5" || inv.Options["wait-online"].Value.String != "600" || !inv.Options["health-checks"].Value.Bool || inv.Options["yes"].Value.Bool || inv.Options["generation"].Value.Kind != Null || inv.Options["max-jobs"].Value.Kind != Null {
		t.Fatal("changed deploy defaults", inv.Options)
	}
	inv, err = r.Parse([]string{"deploy", "-yi", "--copy-only", "--no-health-checks", "--max-jobs=auto", "node", "test", "--reboot"})
	if err != nil || !inv.Options["yes"].Value.Bool || !inv.Options["interactive"].Value.Bool || !inv.Options["copy-only"].Value.Bool || inv.Options["health-checks"].Value.Bool || inv.Options["max-jobs"].Value.String != "auto" || inv.Options["reboot"].Value.Bool || !reflect.DeepEqual(inv.Args, []string{"node", "test", "--reboot"}) {
		t.Fatal("changed deploy aliases, text options or argument boundary", inv, err)
	}
	for _, mode := range []AvailabilityMode{Unavailable, Conditional} {
		commands := r.Commands()
		for i := range commands {
			if commands[i].Handler == DeployHandler {
				commands[i].Availability = Availability{Mode: mode, Option: "generation", Equals: Str("stored"), Reason: "requires generation"}
			}
		}
		if _, err := NewRegistry(r.Globals(), commands); err == nil {
			t.Fatal("accepted deploy handler/availability conflict", mode)
		}
	}
}
