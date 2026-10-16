package health

import (
	"errors"
	"math/big"
	"reflect"
	"testing"
)

func property(name, value string, timeout, cooldown int64) Property {
	return Property{Name: name, Value: value, Timing: Timing{big.NewInt(timeout), big.NewInt(cooldown)}}
}

func TestSystemdSourceParsingOrderAndMinimumTiming(t *testing.T) {
	pattern := "a*.service $(literal)"
	properties := []Property{
		property("P", "first", 60, 3), property("missing", "", 2, 5), property("Empty", "", 5, 0),
		property("=x", "x", 10, 9), property("", "", 10, 9), property(" Space ", " kept \r", 10, 9),
	}
	check, err := SystemdCheck("m", properties, &pattern)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(check.Argv, []string{"systemctl", "show", pattern}) || !check.Remote || check.Timeout.Int64() != 2 || check.Cooldown.Sign() != 0 {
		t.Fatalf("group argv/timing: %#v", check)
	}
	if check.Description != "a*.service $(literal): P=first, missing=, Empty=, =x=x, =,  Space = kept \r" {
		t.Fatal(check.Description)
	}
	got := systemdFailures(check, CommandResult{Stdout: []byte("without equals\nP=first\nEmpty=\nP=last=value\r\n=x\n Space = kept \r\n\n")})
	want := []Failure{{Kind: "property", Property: "P", Actual: "last=value\r", Want: "first"}, {Kind: "missing-property", Property: "missing"}, {Kind: "missing-property", Property: ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("property distinctions/order: %#v", got)
	}
	if got := systemdFailures(check, CommandResult{Status: 1, Stdout: []byte("P=wrong")}); !reflect.DeepEqual(got, []Failure{{Kind: "systemd-status", Status: 1}}) {
		t.Fatalf("nonzero must precede property parsing: %#v", got)
	}
	_, err = SystemdCheck("m", nil, nil)
	var schema *SchemaError
	if !errors.As(err, &schema) {
		t.Fatalf("empty group silently accepted: %v", err)
	}
}
