package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTestConnectionParserPrecedenceAndEncounter(t *testing.T) {
	r := BuiltinRegistry()
	for _, argv := range [][]string{{"test-connection", "--help"}, {"test-connection", "-h"}, {"help", "test-connection"}} {
		inv, err := r.Parse(argv)
		if err != nil || !inv.Help || !reflect.DeepEqual(inv.HelpPath, []string{"test-connection"}) {
			t.Fatal(inv, err)
		}
	}
	for _, argv := range [][]string{{"test-connection", "--managed", "invalid", "--help"}, {"test-connection", "--unknown", "--help"}} {
		_, err := r.Parse(argv)
		var parsed *ParseError
		if !errors.As(err, &parsed) || !reflect.DeepEqual(parsed.HelpPath, []string{"test-connection"}) {
			t.Fatal(err)
		}
		if argv[1] == "--unknown" && parsed.Message != "invalid option: --unknown" {
			t.Fatal("shared ParseError mutated", parsed)
		}
	}
	inv, err := r.Parse([]string{"test-connection", "--tag", "node", "-a", "name!=beta", "--attr", "managed=true", "-t", "^absent", "-y", "--no-yes", "one", "ignored", "--help", "--managed", "invalid"})
	if err != nil || inv.Help || inv.Options["yes"].Value.Bool || !reflect.DeepEqual(inv.Options["attr"].Value.Strings, []string{"name!=beta", "managed=true"}) || !reflect.DeepEqual(inv.Options["tag"].Value.Strings, []string{"node", "^absent"}) || !reflect.DeepEqual(inv.Args, []string{"one", "ignored", "--help", "--managed", "invalid"}) {
		t.Fatal(inv, err)
	}
	if inv.Options["tag"].FirstEncounter != 1 || inv.Options["attr"].FirstEncounter != 2 || inv.Options["yes"].FirstEncounter != 5 || inv.Options["managed"].FirstEncounter != 0 {
		t.Fatal("option callback order changed", inv.Options)
	}
}

func TestCommandHelpImmediatePrecedence(t *testing.T) {
	r := BuiltinRegistry()
	for _, argv := range [][]string{
		{"ls", "--help", "--managed", "bad"},
		{"ls", "-h", "--unknown"},
		{"ls", "-HhZ"},
		{"ls", "-h", "--output"},
		{"ls", "--managed", "no", "--help", "--managed", "bad"},
		{"health-check", "--help", "-j", "bad"},
		{"inputs", "channel", "--help", "missing"},
		{"init", "--help", "extra"},
	} {
		inv, err := r.Parse(argv)
		if err != nil || !inv.Help || !reflect.DeepEqual(inv.HelpPath, inv.Command.Path) || !reflect.DeepEqual(inv.RawArgv, argv) {
			t.Fatal(argv, inv, err)
		}
		if argv[0] == "ls" && len(argv) > 2 && argv[1] == "--managed" && inv.Options["managed"].Value.String != "no" {
			t.Fatal("options before help lost", inv)
		}
	}
	for _, tc := range []struct {
		argv  []string
		error string
	}{
		{[]string{"ls", "--managed", "bad", "--help"}, "invalid argument: --managed bad"},
		{[]string{"ls", "--unknown", "-h"}, "invalid option: --unknown"},
		{[]string{"ls", "-Zh"}, "invalid option: -Z"},
		{[]string{"health-check", "-j", "bad", "--help"}, "invalid argument: -j bad"},
	} {
		inv, err := r.Parse(tc.argv)
		var parseErr *ParseError
		if !errors.As(err, &parseErr) || err.Error() != tc.error || inv.Help || !reflect.DeepEqual(parseErr.HelpPath, []string{tc.argv[0]}) {
			t.Fatal(tc.argv, inv, err)
		}
	}
	for _, tc := range []struct {
		argv, tail []string
	}{
		{[]string{"ls", "pattern", "--help", "--managed", "bad"}, []string{"pattern", "--help", "--managed", "bad"}},
		{[]string{"ls", "--", "--help", "--managed", "bad"}, []string{"--help", "--managed", "bad"}},
	} {
		inv, err := r.Parse(tc.argv)
		if err != nil || inv.Help || !reflect.DeepEqual(inv.Args, tc.tail) {
			t.Fatal(tc.argv, inv, err)
		}
	}
	inv, err := r.Parse([]string{"ls", "-o", "--help"})
	if err != nil || inv.Help || inv.Options["output"].Value.String != "--help" {
		t.Fatal("a flag argument was interpreted as help", inv, err)
	}
	if _, _, err = r.ParseGlobals([]string{"--help", "--color", "bad", "ls"}); err == nil {
		t.Fatal("global help scan changed")
	}
	globals, tail, err := r.ParseGlobals([]string{"-cnever", "--version"})
	if err != nil || globals["color"].Value.String != "never" || !reflect.DeepEqual(tail, []string{"--version"}) {
		t.Fatal("pre-registry version scan changed", globals, tail, err)
	}
}

func TestRubyIntegerOptions(t *testing.T) {
	// Ruby 3.4.9 optparse.rb:2078-2089; the lead's pinned converter observation
	// also covers 0_10, malformed separators and both signs beyond int64.
	r := BuiltinRegistry()
	for _, tc := range []struct{ text, want string }{
		{"10", "10"}, {"010", "8"}, {"0x10", "16"}, {"1_000", "1000"},
		{"-010", "-8"}, {"+0X1_A", "26"}, {"-0b100", "-4"}, {"0_10", "8"},
		{"0", "0"}, {"-0", "0"}, {"+10", "10"}, {"-2", "-2"},
		{"9223372036854775808", "9223372036854775808"},
		{"-9223372036854775809", "-9223372036854775809"},
	} {
		for _, argv := range [][]string{
			{"health-check", "-j", tc.text},
			{"health-check", "-j" + tc.text},
			{"health-check", "--max-jobs=" + tc.text},
		} {
			inv, err := r.Parse(argv)
			v := inv.Options["max-jobs"]
			if err != nil || v.Value.Kind != Number || !v.Present || v.Value.Integer.String() != tc.want || !reflect.DeepEqual(inv.RawArgv, argv) {
				t.Fatal(argv, inv, err)
			}
		}
	}
	for _, text := range []string{"08", "0o10", "0d10", "0x_10", "0b_10", "1__0", "10_", "_10", "٠١", "", " 10", "10 ", "10\n", "0x", "+", "0b2", "1.0"} {
		inv, err := r.Parse([]string{"health-check", "--max-jobs=" + text})
		var parseErr *ParseError
		if !errors.As(err, &parseErr) || err.Error() != "invalid argument: --max-jobs "+text || inv.Help || pathKey(parseErr.HelpPath) != "health-check" {
			t.Fatal(text, inv, err)
		}
	}
	inv, err := r.Parse([]string{"health-check", "-j010", "--max-jobs=0x10"})
	n := inv.Options["max-jobs"].Value
	if err != nil || n.Integer.String() != "16" {
		t.Fatal("last integer value lost", inv, err)
	}
	for _, tc := range []struct {
		argv      []string
		key, text string
	}{
		{[]string{"build", "-j0x10"}, "max-jobs", "0x10"},
		{[]string{"build", "--cores=1_000"}, "cores", "1_000"},
		{[]string{"deploy", "--wait-online=9223372036854775808"}, "wait-online", "9223372036854775808"},
		{[]string{"build", "-j08"}, "max-jobs", "08"},
	} {
		inv, err := r.Parse(tc.argv)
		v := inv.Options[tc.key]
		if err != nil || v.Value.Kind != Text || v.Value.String != tc.text || !v.Present {
			t.Fatal("string-valued Nix option converted", tc, inv, err)
		}
	}
	for _, tc := range []struct {
		argv      []string
		key, want string
	}{
		{[]string{"deploy", "--max-concurrent-copy=-0b100"}, "max-concurrent-copy", "-4"},
		{[]string{"generation", "rm", "--max-concurrent-gc=1_000"}, "max-concurrent-gc", "1000"},
	} {
		inv, err := r.Parse(tc.argv)
		v := inv.Options[tc.key]
		if err != nil || v.Value.Kind != Number || v.Value.Integer.String() != tc.want {
			t.Fatal(tc, inv, err)
		}
	}
}

func TestOrderedParser(t *testing.T) {
	r := BuiltinRegistry()
	argv := []string{"-cnever", "ls", "-Honame,spin", "--no-hide-header", "-a", "one", "--attr=", "-ttwo", "-t", "three", "--managed=no", "alpha", "-o", "ignored", "--help"}
	inv, err := r.Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Help || inv.Options["hide-header"].Value.Bool || !inv.Options["hide-header"].Present || inv.Options["output"].Value.String != "name,spin" || inv.Globals["color"].Value.String != "never" {
		t.Fatal(inv)
	}
	if !reflect.DeepEqual(inv.Options["attr"].Value.Strings, []string{"one", ""}) || !reflect.DeepEqual(inv.Options["tag"].Value.Strings, []string{"two", "three"}) || !reflect.DeepEqual(inv.Args, []string{"alpha", "-o", "ignored", "--help"}) || !reflect.DeepEqual(inv.RawArgv, argv) {
		t.Fatal(inv)
	}
	if _, err = r.Parse([]string{"ls", "-y"}); err == nil {
		t.Fatal("undeclared option accepted")
	}
	for _, tc := range []struct {
		argv []string
		args []string
	}{
		{[]string{"ls", "--", "--help"}, []string{"--help"}},
		{[]string{"ls", "-", "--help"}, []string{"-", "--help"}},
		{[]string{"ssh", "machine", "echo", "--help"}, []string{"machine", "echo", "--help"}},
	} {
		inv, err = r.Parse(tc.argv)
		if err != nil || inv.Help || !reflect.DeepEqual(inv.Args, tc.args) {
			t.Fatal(inv, err)
		}
	}
}

func TestNullEmptyAndDeclarationTypes(t *testing.T) {
	r := BuiltinRegistry()
	absent, _ := r.Parse([]string{"ls"})
	empty, err := r.Parse([]string{"ls", "-o", ""})
	if err != nil || absent.Options["output"].Value.Kind != Null || absent.Options["output"].Present || empty.Options["output"].Value.Kind != Text || !empty.Options["output"].Present || empty.Options["output"].Value.String != "" {
		t.Fatal(absent, empty, err)
	}
	for _, argv := range [][]string{{"build", "-jauto", "--cores=anything"}, {"deploy", "--wait-online=anything"}, {"health-check", "-j-2"}, {"generation", "rm", "--max-concurrent-gc=0"}, {"inputs", "channel", "set", "--allow-shared"}, {"inputs", "channel", "set", "--allow_shared"}, {"generation", "rm", "--gc", "--no-collect-garbage"}} {
		if _, err = r.Parse(argv); err != nil {
			t.Fatal(argv, err)
		}
	}
	inv, _ := r.Parse([]string{"generation", "rm", "--gc", "--no-collect-garbage"})
	if inv.Options["collect-garbage"].Value.Bool || !inv.Options["collect-garbage"].Present {
		t.Fatal(inv)
	}
	for _, argv := range [][]string{{"health-check", "-jauto"}, {"deploy", "--max-concurrent-copy=auto"}, {"ls", "--no-list"}, {"ls", "--hide-header=false"}, {"ls", "--managed", "bad"}, {"ls", "-o"}, {"generation", "rm", "-gc"}, {"init", "extra"}, {"gen-data", "vpsadmin", "all", "extra"}} {
		if _, err = r.Parse(argv); err == nil {
			t.Fatal("invalid declaration accepted", argv)
		}
	}
}

func TestNestedHelpAndAvailability(t *testing.T) {
	r := BuiltinRegistry()
	for _, argv := range [][]string{{"help", "inputs", "channel", "set"}, {"inputs", "channel", "set", "--help"}, {"inputs", "channel", "set", "-h"}, {"--help", "inputs", "channel", "set"}} {
		inv, err := r.Parse(argv)
		if err != nil || !inv.Help || pathKey(inv.HelpPath) != "inputs channel set" {
			t.Fatal(argv, inv, err)
		}
	}
	for _, argv := range [][]string{{"help", "missing"}, {"inputs", "missing"}, {"help", "--availability"}} {
		if _, err := r.Parse(argv); err == nil {
			t.Fatal(argv)
		}
	}
	for _, argv := range [][]string{{"status"}, {"status", "-gcurrent"}, {"status", "-g0"}} {
		inv, err := r.Parse(argv)
		if err != nil || r.CheckAvailability(inv) == nil {
			t.Fatal(argv, inv, err)
		}
	}
	inv, err := r.Parse([]string{"status", "-g", "none", "-y", "--no-yes", "extra", "--help"})
	if err != nil || r.CheckAvailability(inv) != nil || inv.Options["yes"].Value.Bool || inv.Help {
		t.Fatal(inv, err)
	}
	group, err := r.Parse([]string{"inputs", "channel"})
	if err != nil || !group.Help || pathKey(group.HelpPath) != "inputs channel" {
		t.Fatal(group, err)
	}
	text, _ := r.Help(group.HelpPath, 80)
	if strings.Contains(text, "Unavailable") || strings.Count(text, "(Available)") != 3 {
		t.Fatal(text)
	}
}

func TestInputsOptionFirstEncounterAndFinalValue(t *testing.T) {
	r := BuiltinRegistry()
	type explicit struct {
		first int
		value bool
	}
	for _, tc := range []struct {
		flags []string
		want  map[string]explicit
	}{
		{[]string{"--all", "--editor", "-d", "--commit", "--no-all", "--no-downgrade", "--no-commit"}, map[string]explicit{"all": {1, false}, "editor": {2, true}, "downgrade": {3, false}, "commit": {4, false}}},
		{[]string{"--downgrade", "--no-editor", "--all", "-d", "--no-changelog", "--no-downgrade", "--editor"}, map[string]explicit{"downgrade": {1, false}, "editor": {2, true}, "all": {3, true}, "changelog": {5, false}}},
		{[]string{"-d", "--no-downgrade", "--commit", "--all", "--no-commit"}, map[string]explicit{"downgrade": {1, false}, "commit": {3, false}, "all": {4, true}}},
	} {
		argv := append(append([]string{"inputs", "update"}, tc.flags...), "a")
		inv, err := r.Parse(argv)
		if err != nil {
			t.Fatal(argv, err)
		}
		for key, parsed := range inv.Options {
			want, present := tc.want[key]
			if parsed.FirstEncounter != want.first || parsed.Present != present || present && parsed.Value.Bool != want.value {
				t.Fatal(argv, key, parsed, want)
			}
		}
		// Invocation metadata belongs to this parse, including its scalar ordinal.
		all := inv.Options["all"]
		all.FirstEncounter = 99
		all.Value.Bool = !all.Value.Bool
		inv.Options["all"] = all
		inv.Command.Options[0].Names[0] = "changed"
		fresh, err := r.Parse(argv)
		want := tc.want["all"]
		if err != nil || fresh.Options["all"].FirstEncounter != want.first || fresh.Options["all"].Value.Bool != want.value || fresh.Command.Options[0].Names[0] == "changed" {
			t.Fatal("parse metadata leaked", fresh, err)
		}
	}
	defaults, err := r.Parse([]string{"inputs", "update", "a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range defaults.Command.Options {
		parsed := defaults.Options[spec.Key]
		if parsed.Present || parsed.FirstEncounter != 0 || parsed.Value.Bool != spec.Default.Bool {
			t.Fatal("default acquired insertion metadata", spec.Key, parsed)
		}
	}
}

func TestOptionFirstEncounterOnlySuccessfulCallbacks(t *testing.T) {
	r := BuiltinRegistry()
	inv, err := r.Parse([]string{"health-check", "--yes", "--max-jobs=bad"})
	if err == nil || inv.Options["yes"].FirstEncounter != 1 || !inv.Options["yes"].Present || inv.Options["max-jobs"].FirstEncounter != 0 || inv.Options["max-jobs"].Present {
		t.Fatal("failed conversion inserted option", inv, err)
	}
	inv, err = r.Parse([]string{"inputs", "update", "--commit", "--commit=bad"})
	if err == nil || inv.Options["commit"].FirstEncounter != 1 || !inv.Options["commit"].Value.Bool {
		t.Fatal("failed repeat changed successful insertion", inv, err)
	}
	inv, err = r.Parse([]string{"-cnever", "inputs", "update", "--all", "a"})
	if err != nil || inv.Globals["color"].FirstEncounter != 1 || inv.Options["all"].FirstEncounter != 1 {
		t.Fatal("scan counters leaked across globals/command", inv, err)
	}
}

func TestInputsSetOptionsAliasesNegationsAndLiteralRevision(t *testing.T) {
	r := BuiltinRegistry()
	for _, path := range [][]string{{"inputs", "set"}, {"inputs", "channel", "set"}, {"inputs", "machine", "set"}} {
		positional := []string{"a", "opaque?/# --rev"}
		if len(path) == 3 {
			positional = []string{"a", "os", "opaque?/# --rev"}
		}
		inv, err := r.Parse(append(append([]string{}, path...), positional...))
		if err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]bool{"commit": false, "changelog": true, "editor": true, "downgrade": false} {
			p := inv.Options[key]
			if p.Value.Bool != want || p.Present || p.FirstEncounter != 0 {
				t.Fatal(path, key, p)
			}
		}
		flags := []string{"--commit", "--no-commit", "--no-changelog", "--no-editor", "-d", "--no-downgrade", "--downgrade"}
		if pathKey(path) == "inputs channel set" {
			flags = append(flags, "--allow-shared", "--no-allow_shared", "--allow_shared")
		}
		argv := append(append(append([]string{}, path...), flags...), positional...)
		inv, err = r.Parse(argv)
		if err != nil || !reflect.DeepEqual(inv.Args, positional) || inv.Options["commit"].Value.Bool || inv.Options["changelog"].Value.Bool || inv.Options["editor"].Value.Bool || !inv.Options["downgrade"].Value.Bool || inv.Options["downgrade"].FirstEncounter != 5 {
			t.Fatal(argv, inv, err)
		}
		if pathKey(path) == "inputs channel set" && !inv.Options["allow_shared"].Value.Bool {
			t.Fatal(inv)
		}
		inv, err = r.Parse(append(append([]string{}, path...), "--", "--help", "--commit", ""))
		if err != nil || inv.Help || !reflect.DeepEqual(inv.Args, []string{"--help", "--commit", ""}) {
			t.Fatal("setter parser boundary", inv, err)
		}
	}
}
