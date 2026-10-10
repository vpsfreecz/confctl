package cli

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinDeclarations(t *testing.T) {
	r := BuiltinRegistry()
	if len(r.Leaves()) != 29 {
		t.Fatal(len(r.Leaves()))
	}
	// Traversal must reach each action once, without a second command inventory.
	var walk func([]string) int
	walk = func(path []string) int {
		n := 0
		for _, c := range r.Children(path) {
			if c.ArgumentPolicy == GroupArguments {
				n += walk(c.Path)
			} else if pathKey(c.Path) != "help" {
				n++
			}
		}
		return n
	}
	if n := walk(nil); n != len(r.Leaves()) {
		t.Fatal(n)
	}
	for _, c := range r.Leaves() {
		inv, err := r.Parse(c.Path)
		if err != nil || !reflect.DeepEqual(inv.Command.Path, c.Path) {
			t.Fatal(c.Path, inv, err)
		}
		if c.Handler == None && r.CheckAvailability(inv) == nil {
			t.Fatal("unimplemented action available", c.Path)
		}
	}
	for _, tc := range []struct {
		argv    []string
		key     string
		kind    ValueKind
		value   string
		present bool
	}{
		{[]string{"build", "-j", "auto"}, "max-jobs", Text, "auto", true},
		{[]string{"health-check"}, "max-jobs", Number, "5", false},
		{[]string{"deploy"}, "wait-online", Text, "600", false},
		{[]string{"inputs", "update"}, "editor", Boolean, "true", false},
		{[]string{"generation", "rm"}, "collect-garbage", Boolean, "true", false},
		{[]string{"ls"}, "output", Null, "none", false},
		{[]string{"status"}, "generation", Null, "none", false},
	} {
		inv, err := r.Parse(tc.argv)
		v := inv.Options[tc.key]
		if err != nil || v.Value.Kind != tc.kind || defaultText(v.Value) != tc.value || v.Present != tc.present {
			t.Fatal(tc, v, err)
		}
	}
}

func TestRegistryValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]CommandSpec) []CommandSpec
	}{
		{"duplicate-path", func(s []CommandSpec) []CommandSpec { return append(s, s[1]) }},
		{"leaf-as-parent", func(s []CommandSpec) []CommandSpec {
			return append(s, leaf([]string{"ls", "child"}, "child", "", nil, nil))
		}},
		{"missing-parent", func(s []CommandSpec) []CommandSpec {
			return append(s, leaf([]string{"unknown", "child"}, "child", "", nil, nil))
		}},
		{"alias", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{stringOption([]string{"same"}, ""), stringOption([]string{"same"}, "")}
			return s
		}},
		{"normalized-alias", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{stringOption([]string{"allow_shared"}, ""), stringOption([]string{"allow-shared"}, "")}
			return s
		}},
		{"negation-alias", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{switchOption([]string{"enabled"}, "", false, true), stringOption([]string{"no-enabled"}, "")}
			return s
		}},
		{"switch-default", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{{Key: "x", Names: []string{"x"}, Kind: Switch, Default: Str("false")}}
			return s
		}},
		{"integer-default", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{{Key: "count", Names: []string{"count"}, Kind: Integer, Default: Str("5")}}
			return s
		}},
		{"repeated-default", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{{Key: "x", Names: []string{"x"}, Kind: String, Multiple: true}}
			return s
		}},
		{"string-negation", func(s []CommandSpec) []CommandSpec {
			s[0].Options = []OptionSpec{{Key: "x", Names: []string{"x"}, Kind: String, Negatable: true}}
			return s
		}},
		{"available-without-handler", func(s []CommandSpec) []CommandSpec { s[2].Availability.Mode = Available; return s }},
		{"handler-condition", func(s []CommandSpec) []CommandSpec {
			s[2].Handler, s[2].Availability.Mode = StatusNone, Available
			return s
		}},
		{"variadic-position", func(s []CommandSpec) []CommandSpec {
			s[3].Arguments = []ArgumentSpec{{Name: "many", Variadic: true}, {Name: "one"}}
			return s
		}},
		{"condition-missing-option", func(s []CommandSpec) []CommandSpec {
			s[2].Handler = StatusNone
			s[2].Availability = Availability{Mode: Conditional, Option: "missing", Equals: Str("none"), Reason: "reason"}
			return s
		}},
		{"framework-option", func(s []CommandSpec) []CommandSpec {
			s[2].Options = []OptionSpec{switchOption([]string{"h", "help"}, "", false, false)}
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := BuiltinRegistry()
			if _, err := NewRegistry(r.Globals(), tc.edit(r.Commands())); err == nil {
				t.Fatal("accepted malformed declarations")
			}
		})
	}
}

func TestRegistryMetadataControlsParseHelpAndDefaults(t *testing.T) {
	base := BuiltinRegistry()
	specs := base.Commands()
	for i := range specs {
		if pathKey(specs[i].Path) != "ls" {
			continue
		}
		specs[i].Summary = "Changed list description"
		specs[i].Usage = "[selector]"
		for j := range specs[i].Options {
			o := &specs[i].Options[j]
			if o.Key == "output" {
				o.Names = []string{"q", "columns"}
				o.Description = "Changed columns"
				o.Default = Str("name")
			}
			if o.Key == "attr" {
				o.Default = List("baseline")
			}
		}
	}
	r, err := NewRegistry(base.Globals(), specs)
	if err != nil {
		t.Fatal(err)
	}
	for i := range specs {
		specs[i].Summary = "must not mutate registry"
	}
	text, _ := r.Help([]string{"ls"}, 80)
	if !strings.Contains(text, "Changed list description") || !strings.Contains(text, "[command options] [selector]") || !strings.Contains(text, "-q, --columns=arg") || !strings.Contains(text, "Changed columns (default: name)") {
		t.Fatal(text)
	}
	first, err := r.Parse([]string{"ls", "-q", "spin", "-a", "one"})
	if err != nil || first.Options["output"].Value.String != "spin" || !first.Options["output"].Present {
		t.Fatal(first, err)
	}
	first.Options["attr"].Value.Strings[0] = "mutated"
	second, err := r.Parse([]string{"ls", "-a", "two"})
	if err != nil || second.Options["output"].Value.String != "name" || second.Options["output"].Present || !reflect.DeepEqual(second.Options["attr"].Value.Strings, []string{"baseline", "two"}) {
		t.Fatal(second, err)
	}
	if _, err = r.Parse([]string{"ls", "--output", "spin"}); err == nil {
		t.Fatal("old alias remained authoritative")
	}
	if _, err = base.Parse([]string{"ls", "--output", "spin"}); err != nil {
		t.Fatal("derived registry mutated original", err)
	}
	copy, _ := r.Lookup([]string{"ls"})
	copy.Options[0].Names[0] = "mutation"
	text2, _ := r.Help([]string{"ls"}, 80)
	if text2 != text {
		t.Fatal("lookup leaked mutable declarations")
	}
}

func TestNumericDeclarationOwnershipAndAvailability(t *testing.T) {
	// Nonzero multiword values expose shallow-copy aliases that scalar defaults miss.
	const huge = "340282366920938463463374607431768211455"
	number := func() Value {
		v := Value{Kind: Number}
		if _, ok := v.Integer.SetString(huge, 10); !ok {
			t.Fatal("invalid test declaration")
		}
		return v
	}
	base := BuiltinRegistry()
	builtinHelp, err := base.Help([]string{"health-check"}, 80)
	if err != nil || !strings.Contains(builtinHelp, "default: 5") {
		t.Fatal("builtin numeric default help changed", builtinHelp, err)
	}
	globals := append(base.Globals(), OptionSpec{Key: "limit", Names: []string{"limit"}, Kind: Integer, Default: number(), Metavar: "count"})
	root, _ := base.Lookup(nil)
	command, _ := base.Lookup([]string{"status"})
	command.Options = []OptionSpec{{Key: "count", Names: []string{"count"}, Kind: Integer, Default: number(), Metavar: "count"}}
	command.Availability = Availability{Mode: Conditional, Option: "count", Equals: number(), Reason: "different count"}
	specs := []CommandSpec{root, command}
	r, err := NewRegistry(globals, specs)
	if err != nil {
		t.Fatal(err)
	}
	help, err := r.Help(command.Path, 80)
	if err != nil || !strings.Contains(help, "default: "+huge) {
		t.Fatal(help, err)
	}
	// Mutate constructor inputs and every declaration-returning boundary.
	globals[len(globals)-1].Default.Integer.SetInt64(1)
	specs[1].Options[0].Default.Integer.SetInt64(2)
	specs[1].Availability.Equals.Integer.SetInt64(3)
	returnedGlobals := r.Globals()
	returnedGlobals[len(returnedGlobals)-1].Default.Integer.SetInt64(4)
	lookup, _ := r.Lookup(command.Path)
	for _, declarations := range [][]CommandSpec{r.Commands(), r.Children(nil), r.Leaves(), {lookup}} {
		for i := range declarations {
			if pathKey(declarations[i].Path) == "status" {
				declarations[i].Options[0].Default.Integer.SetInt64(5)
				declarations[i].Availability.Equals.Integer.SetInt64(6)
			}
		}
	}
	first, err := r.Parse([]string{"status"})
	if err != nil || r.CheckAvailability(first) != nil {
		t.Fatal(first, err)
	}
	v := first.Options["count"]
	v.Value.Integer.SetInt64(7)
	first.Options["count"] = v
	v = first.Globals["limit"]
	v.Value.Integer.SetInt64(8)
	first.Globals["limit"] = v
	first.Command.Options[0].Default.Integer.SetInt64(9)
	first.Command.Availability.Equals.Integer.SetInt64(10)
	second, err := r.Parse([]string{"status"})
	for _, v := range []ParsedValue{second.Globals["limit"], second.Options["count"]} {
		if v.Value.Kind != Number || v.Value.Integer.String() != huge || v.Present {
			t.Fatal("numeric declaration storage leaked", v)
		}
	}
	helpAfter, helpErr := r.Help(command.Path, 80)
	stored, _ := r.Lookup(command.Path)
	if err != nil || helpErr != nil || helpAfter != help || stored.Options[0].Default.Integer.String() != huge || stored.Availability.Equals.Integer.String() != huge || r.CheckAvailability(second) != nil {
		t.Fatal("mutation changed registry/help/condition", second, stored, helpAfter, err, helpErr)
	}
	for _, text := range []string{huge, "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFE"} {
		inv, err := r.Parse([]string{"status", "--count=" + text})
		if err != nil || (r.CheckAvailability(inv) == nil) != (text != "0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFE") {
			t.Fatal("numeric comparison changed", text, inv, err)
		}
	}
	second.Options["count"] = ParsedValue{Value: Str(huge), Present: true}
	if r.CheckAvailability(second) == nil {
		t.Fatal("Text matched a numeric condition")
	}
	zero := Value{Kind: Number}
	if defaultText(zero) != "0" || defaultText(Value{}) != "none" || defaultText(Int(-5)) != "-5" {
		t.Fatal("zero/null/literal integer semantics changed")
	}
}

func TestHelpReferences(t *testing.T) {
	r := BuiltinRegistry()
	b, err := os.ReadFile("../core/help_root.txt")
	if err != nil {
		t.Fatal(err)
	}
	root, err := r.Help(nil, 80)
	if err != nil || root != string(b) || len(root) != 1269 {
		t.Fatalf("root differs from immutable reference: %v\n%q", err, root)
	}
	ls, err := r.Help([]string{"ls"}, 80)
	b, readErr := os.ReadFile("../core/help_ls.txt")
	if err != nil || readErr != nil || ls != string(b) {
		t.Fatalf("ls differs from test reference: %v %v\n%q", err, readErr, ls)
	}
	for _, c := range r.Commands() {
		text, err := r.Help(c.Path, 80)
		if err != nil || !strings.Contains(text, c.Summary) {
			t.Fatal(c.Path, text, err)
		}
		if c.ArgumentPolicy != GroupArguments && c.Availability.Mode == Unavailable && !strings.Contains(text, "Unavailable in confctl-go-prototype.") {
			t.Fatal(c.Path, text)
		}
	}
	status, _ := r.Help([]string{"status"}, 80)
	if !strings.Contains(status, "Execution requires --generation none.") {
		t.Fatal(status)
	}
	if _, err := r.Help([]string{"unknown"}, 80); err == nil {
		t.Fatal("unknown help succeeded")
	}
}

func TestCapabilityTableMatchesREADME(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	start := "<!-- command-registry-capabilities:start -->\n"
	end := "<!-- command-registry-capabilities:end -->"
	_, tail, ok := strings.Cut(string(b), start)
	if !ok {
		t.Fatal("missing registry capability table")
	}
	table, _, ok := strings.Cut(tail, end)
	if !ok || table != BuiltinRegistry().CapabilityTable() {
		t.Fatal("README capability table diverged from registry", table)
	}
}
