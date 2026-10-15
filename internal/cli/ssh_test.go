package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestSSHFamilyDeclarationsDispatchAndAvailability(t *testing.T) {
	r := BuiltinRegistry()
	for _, tc := range []struct {
		name    string
		handler Handler
		options int
	}{{"ssh", SSHHandler, 8}, {"cssh", CSSHHandler, 4}} {
		c, ok := r.Lookup([]string{tc.name})
		if !ok || c.Handler != tc.handler || c.Availability.Mode != Available || c.ArgumentPolicy != FreeForm || len(c.Options) != tc.options {
			t.Fatal(c)
		}
		inv, err := r.Parse([]string{tc.name, "-y", "node", "--help"})
		if err != nil || inv.Help || r.CheckAvailability(inv) != nil || !reflect.DeepEqual(inv.Args, []string{"node", "--help"}) {
			t.Fatal(inv, err)
		}
		text, err := r.Help(c.Path, 80)
		if err != nil || strings.Contains(text, "Unavailable") {
			t.Fatal(text, err)
		}
		for _, mode := range []AvailabilityMode{Unavailable, Conditional} {
			commands := r.Commands()
			for i := range commands {
				if pathKey(commands[i].Path) == tc.name {
					commands[i].Availability.Mode = mode
				}
			}
			if _, err := NewRegistry(r.Globals(), commands); err == nil {
				t.Fatal("handler/availability conflict accepted", tc.name, mode)
			}
		}
	}
	inv, err := r.Parse([]string{"ssh", "-i", "", "-f", "", "--no-parallel", "-g", "--", "-literal", "cat", "--help"})
	if err != nil || inv.Help || !inv.Options["input-string"].Present || inv.Options["input-string"].Value.String != "" || !inv.Options["input-file"].Present || inv.Options["parallel"].Value.Bool || !inv.Options["aggregate"].Value.Bool || !reflect.DeepEqual(inv.Args, []string{"-literal", "cat", "--help"}) {
		t.Fatal(inv, err)
	}
}
