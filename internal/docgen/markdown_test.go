package docgen

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

var fixtureMetadata = Metadata{Version: "fixture-version", Date: "2026-10-09"}

func TestCommandsGoldenAndRegistryOwnership(t *testing.T) {
	registry, err := cli.NewRegistry(nil, []cli.CommandSpec{
		{Summary: "Manage fixture systems", Usage: "command [command options]", ArgumentPolicy: cli.GroupArguments},
		{Path: []string{"inspect"}, Summary: "Inspect declared fixtures", Usage: "[name]", Handler: cli.ListHandler,
			Availability: cli.Availability{Mode: cli.Available}, Options: []cli.OptionSpec{
				{Key: "name", Names: []string{"n", "name"}, Kind: cli.String, Metavar: "arg", Description: "Selected name", Default: cli.Str("value"), DefaultPresent: true},
			}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := registry.Commands()
	content, err := Commands(registry, fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "testdata/pandoc/commands.md", content)
	if !reflect.DeepEqual(before, registry.Commands()) {
		t.Fatal("generation mutated registry declarations")
	}
	// Changing a declaration changes the document without changing the generator.
	before[1].Summary = "Read renamed fixtures"
	before[1].Options[0].Default = cli.Str("other")
	changed, err := cli.NewRegistry(nil, before)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Commands(changed, fixtureMetadata)
	if err != nil || !strings.Contains(string(result), "Read renamed fixtures") ||
		!strings.Contains(string(result), "(default: other)") || strings.Contains(string(result), "(default: value)") {
		t.Fatalf("declaration metadata not consumed: %s, %v", result, err)
	}
}

func TestBuiltinCommandsEmbedEveryRegistryHelp(t *testing.T) {
	registry := cli.BuiltinRegistry()
	before := registry.Commands()
	content, err := Commands(registry, fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(content), "\n## ") != len(before) || len(registry.Leaves()) != 29 {
		t.Fatal("missing or duplicate command sections")
	}
	for _, command := range registry.Leaves() {
		if command.Availability.Mode != cli.Available || command.Handler == cli.None {
			t.Fatalf("builtin leaf %v lacks an available handler", command.Path)
		}
	}
	for _, command := range before {
		help, err := registry.Help(command.Path, 80)
		name := "confctl"
		if len(command.Path) > 0 {
			name += " " + strings.Join(command.Path, " ")
		}
		heading := "\n## " + codeSpan(name) + "\n\n"
		if strings.Count(string(content), heading) != 1 {
			t.Fatalf("missing or repeated scope heading %v", command.Path)
		}
		section := strings.SplitN(string(content), heading, 2)[1]
		if end := strings.Index(section, "\n## "); end >= 0 {
			section = section[:end]
		}
		if end := strings.Index(section, "\n# GENERATION SELECTION AND ROLLBACK\n"); end >= 0 {
			section = section[:end]
		}
		if err != nil || strings.Count(section, help) != 1 || section != codeBlock(help, "text") {
			t.Fatalf("missing declaration help for %v: %v", command.Path, err)
		}
	}
	if !reflect.DeepEqual(before, registry.Commands()) {
		t.Fatal("generation mutated registry declarations")
	}
}

func TestEmbeddedCommandContextAndSourceReferences(t *testing.T) {
	r := cli.BuiltinRegistry()
	before := r.Commands()
	content, err := Commands(r, fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	previous := -1
	for _, heading := range []string{"# NAME", "# SYNOPSIS", "# DESCRIPTION", "# FLAKE INPUTS", "# COMMANDS", "# GENERATION SELECTION AND ROLLBACK", "# EXECUTABLE EXTENSIONS"} {
		needle := "\n" + heading + "\n"
		at := strings.Index(s, needle)
		if at <= previous || strings.Count(s, needle) != 1 {
			t.Fatalf("context section order/count %s", heading)
		}
		previous = at
	}
	for _, fact := range []string{
		"NixOS and vpsAdminOS", "confctl.lib.mkConfctlOutputs", "flake.lock",
		"cluster.<name>.inputs.channels", "inputs.overrides", "retained confctl v3",
		"Offset `0`\ndoes not resolve the local `current` link", "unresolved link fails current selection",
		"Numeric local\nselection fails when records were excluded", "collected store closures",
		"https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/extension/README.md",
		"https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/experiments/confctl-go/README.md",
		"https://github.com/vpsfreecz/vpsfree-cz-configuration/blob/0c557149fd4e6e653cd876b16382559efbb548e4/extensions/README.md",
	} {
		if !strings.Contains(s, fact) {
			t.Fatalf("embedded owner fact/reference lost: %s", fact)
		}
	}
	if strings.Contains(s, "UserScript") || strings.Contains(s, "site/README") || !reflect.DeepEqual(before, r.Commands()) {
		t.Fatal("legacy registry example, invented source or declaration mutation")
	}
	repeated, err := Commands(r, fixtureMetadata)
	if err != nil || string(repeated) != s {
		t.Fatal("embedded context is not deterministic", err)
	}
}

func TestMarkdownLiteralDelimiters(t *testing.T) {
	for _, test := range []struct{ input, output string }{
		{"plain", "`plain`"},
		{"", "``"},
		{"a`b", "``a`b``"},
		{"a``b", "```a``b```"},
		{"`edge`", "`` `edge` ``"},
		{" padded ", "`  padded  `"},
		{"  ", "`  `"},
		{"one\ntwo`three", "``one\ntwo`three``"},
	} {
		if got := codeSpan(test.input); got != test.output {
			t.Errorf("%q: got %q, want %q", test.input, got, test.output)
		}
	}
	registry, err := cli.NewRegistry(nil, []cli.CommandSpec{
		{Summary: "Source ``` example", Usage: "command", ArgumentPolicy: cli.GroupArguments},
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := Commands(registry, fixtureMetadata)
	if err != nil || !strings.Contains(string(content), "\n````text\n") || !strings.Contains(string(content), "\n````\n\n# GENERATION SELECTION AND ROLLBACK\n") {
		t.Fatalf("source backticks close the help fence: %s, %v", content, err)
	}
}

func TestExplicitMetadata(t *testing.T) {
	for _, metadata := range []Metadata{
		{Date: "2026-10-09"}, {Version: "v"}, {Version: "v", Date: "2026-02-30"},
		{Version: "v\nheader", Date: "2026-10-09"}, {Version: "v", Date: "2026-1-9"},
	} {
		if _, err := Commands(cli.BuiltinRegistry(), metadata); err == nil {
			t.Errorf("accepted invalid metadata: %+v", metadata)
		}
	}
	if _, err := Commands(nil, fixtureMetadata); err == nil {
		t.Fatal("accepted nil registry")
	}
}

func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(path, ".json") {
		var fixture struct{ Markdown string }
		if err := json.Unmarshal(want, &fixture); err != nil {
			t.Fatal(err)
		}
		want = []byte(fixture.Markdown)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs\nGOT:\n%s\nWANT:\n%s", path, got, want)
	}
}
