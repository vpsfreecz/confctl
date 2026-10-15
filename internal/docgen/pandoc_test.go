package docgen

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/core"
)

// These expectations describe authored Markdown only. No Pandoc, md2man,
// original Ruby provider or real moduleOptions evaluation runs in this tier.
func TestPandocMetadataQuotedScalarsAndHeadingLevels(t *testing.T) {
	m := Metadata{Date: "2026-10-09", Version: "v: \"quoted\" \\ # [x] {y}\t\x1b <&>"}
	content, err := Commands(cli.BuiltinRegistry(), m)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(content), "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatal("YAML metadata boundary", string(content))
	}
	want := []string{"title", "section", "date", "footer", "header"}
	values := []string{"confctl", "8", m.Date, m.Version, "confctl"}
	lines := strings.Split(strings.TrimSuffix(parts[1], "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatal("unexpected metadata fields", lines)
	}
	for i, line := range lines {
		key, scalar, ok := strings.Cut(line, ": ")
		var value string
		if !ok || key != want[i] || json.Unmarshal([]byte(scalar), &value) != nil || value != values[i] {
			t.Fatalf("metadata %q is not the required JSON string", line)
		}
	}
	if !strings.Contains(parts[1], `\u001b`) || !strings.Contains(parts[1], `\t`) || strings.Contains(parts[1], `\x1b`) {
		t.Fatal("Go-only escapes leaked into YAML", parts[1])
	}
	if !strings.HasPrefix(parts[2], "\n# NAME\n") || !strings.Contains(parts[2], "\n# FLAKE INPUTS\n") ||
		!strings.Contains(parts[2], "\n# COMMANDS\n\n## `confctl`\n") ||
		strings.Index(parts[2], "\n# FLAKE INPUTS\n") > strings.Index(parts[2], "\n# COMMANDS\n") || strings.Contains(parts[2], "\n### ") {
		t.Fatal("manual/command heading hierarchy")
	}
	repeated, err := Commands(cli.BuiltinRegistry(), m)
	if err != nil || string(repeated) != string(content) {
		t.Fatal("explicit metadata was not deterministic", err)
	}
}

func TestPandocBuiltinProductAndFrameworkCoverage(t *testing.T) {
	r := cli.BuiltinRegistry()
	before := r.Commands()
	content, err := Commands(r, fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	products, groups, frameworkHelp := 0, 0, 0
	for _, command := range before {
		if command.ArgumentPolicy == cli.GroupArguments {
			groups++
		} else if strings.Join(command.Path, " ") == "help" {
			frameworkHelp++
		} else {
			products++
			if command.Availability.Mode != cli.Available || command.Handler == cli.None {
				t.Fatalf("builtin product %v lacks an available handler", command.Path)
			}
		}
	}
	if products != 29 || frameworkHelp != 1 || groups == 0 || strings.Count(string(content), "\n## ") != products+groups+frameworkHelp {
		t.Fatal("declaration scope census", products, groups, frameworkHelp)
	}
	if !reflect.DeepEqual(before, r.Commands()) {
		t.Fatal("generation mutated registry declarations")
	}
	// The existing scoped Help test checks each complete help block once.
	// Shared option spellings can legitimately recur in different blocks.
}

func TestPandocSyntheticAvailabilityHelp(t *testing.T) {
	// Framework help supports conditional declarations without assigning an
	// executable product handler a policy that NewRegistry rejects.
	r, err := cli.NewRegistry(nil, []cli.CommandSpec{
		{Summary: "Manage availability fixtures", ArgumentPolicy: cli.GroupArguments},
		{Path: []string{"help"}, Summary: "Show conditional fixture help",
			Options: []cli.OptionSpec{
				{Key: "generation", Names: []string{"generation"}, Kind: cli.String, Default: cli.Str("none")},
			},
			Availability: cli.Availability{Mode: cli.Conditional, Option: "generation", Equals: cli.Str("none"), Reason: "Fixture condition"},
		},
		{Path: []string{"offline"}, Summary: "Describe an unavailable fixture",
			Availability: cli.Availability{Mode: cli.Unavailable, Reason: "Unavailable in fixture registry."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := r.Commands()
	content, err := Commands(r, fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(content), "\n## ") != len(before) {
		t.Fatal("missing or duplicate fixture sections")
	}
	for _, command := range before {
		help, err := r.Help(command.Path, 80)
		if err != nil || strings.Count(string(content), codeBlock(help, "text")) != 1 {
			t.Fatalf("missing or repeated fixture help for %v: %v", command.Path, err)
		}
	}
	for _, marker := range []string{
		"AVAILABILITY\n    Execution requires --generation none.",
		"AVAILABILITY\n    Unavailable in fixture registry.",
	} {
		if strings.Count(string(content), marker) != 1 {
			t.Fatalf("missing or repeated availability text %q", marker)
		}
	}
	if !reflect.DeepEqual(before, r.Commands()) {
		t.Fatal("generation mutated fixture declarations")
	}
}

func TestPandocCompleteFencedLiteralBytes(t *testing.T) {
	var fixtures struct {
		Cases []struct{ Input, Markdown string }
	}
	data, err := os.ReadFile("testdata/pandoc/literals.golden.json")
	if err != nil || json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("literal fixture", err)
	}
	for _, test := range fixtures.Cases {
		if got := codeBlock(test.Input, "nix"); got != test.Markdown {
			t.Errorf("literal %q: got %q, want %q", test.Input, got, test.Markdown)
		}
		option := ModuleOption{"name": "confctl.literal", "description": "", "default": map[string]any{"_type": "literalExpression", "text": test.Input}, "declarations": []string{}}
		content, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return []ModuleOption{option}, nil }, func(any) (string, error) {
			t.Fatal("trusted literal sent to value renderer")
			return "", nil
		}, "", fixtureMetadata)
		if err != nil || !strings.Contains(string(content), "*Default:*\n\n"+indentDefinition(test.Markdown)) {
			t.Fatalf("complete default block %q: %q %v", test.Input, content, err)
		}
	}
}

func TestPandocDescriptionBlocksAndTagOrder(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"before <literal>one\n  two\n\nthree\n</literal> after", "before \n\n```text\none\n  two\n\nthree\n```\n\n after"},
		{"<option>one\n````\nend</option>", "`````text\none\n````\nend\n`````\n\n"},
		{"<option><literal>a\nb</literal></option>", "```text\n`a\nb`\n```\n\n"},
		{"<literal><option>a\nb</option></literal>", "<literal>\n\n```text\na\nb\n```\n\n</literal>"},
		{"<literal>a&#10;&amp;lt;</literal>", "```text\na\n&lt;\n```\n\n"},
		{"&lt;literal&gt;a&#10;b&lt;/literal&gt;", "<literal>a\nb</literal>"},
		{"<literal>`edge`</literal> and <option>two``parts</option>", "`` `edge` `` and ```two``parts```"},
	} {
		if got := markdownDescription(test.input); got != test.want {
			t.Errorf("description %q: got %q, want %q", test.input, got, test.want)
		}
	}
	input := "First paragraph.\n\n  - nested bullet\n    relative indent\n\nLast paragraph.\n"
	if got := indentDescription(input); got != "First paragraph.\n\n      - nested bullet\n        relative indent\n\n    Last paragraph.\n" {
		t.Fatal("relative paragraph indentation", got)
	}
}

func TestPandocMultilineDescriptionRemainsInsideDefinition(t *testing.T) {
	description := "First paragraph.\n\n  - nested bullet\n    continued\n\nBefore <literal>.dot\n'apostrophe\n\\backslash\n````\n\nlast\n</literal> after\n"
	option := ModuleOption{"name": "confctl.tags", "description": description, "type": "string", "default": nil, "declarations": []string{}}
	content, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return []ModuleOption{option}, nil }, core.DocumentationNixLiteral, "", fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	// Literal source expectation, including the definition indentation. The
	// leading control lines are content, not handwritten roff escaping.
	want := "`confctl.tags`\n\n:   First paragraph.\n\n      - nested bullet\n        continued\n\n    Before \n\n    `````text\n    .dot\n    'apostrophe\n    \\backslash\n    ````\n\n    last\n    `````\n\n     after\n\n\n    *Type:* string\n\n    *Default:*\n\n    ```nix\n    null\n    ```\n\n    *Declared by:* \n\n"
	if !strings.Contains(string(content), want) {
		t.Fatalf("multiline description left or changed its definition; missing %q in %q", want, content)
	}
}

func TestPandocSelectedDefinitionsSharedValuesAndDeclarationChanges(t *testing.T) {
	options := []ModuleOption{
		{"name": "confctl.z", "description": "last", "type": "string", "default": "${value} \"quoted\" café", "example": false, "declarations": []string{}},
		{"name": "cluster.<name>.x", "description": "machine", "type": "list", "default": []any{json.Number("-1"), json.Number("1e3"), json.Number("900719925474099312345")}, "example": []any{}, "declarations": []string{"/fixture/confctl/nix/modules/cluster/default.nix"}},
		{"name": "confctl.a", "description": "first", "type": "boolean", "default": false, "example": json.Number("0"), "declarations": []string{}},
		{"name": "confctl.a", "description": "second", "type": "string", "default": true, "example": "", "declarations": []string{}},
		{"name": "confctl.programs.x", "description": "system", "type": "list", "default": []any{map[string]any{"if": json.Number("1.2300e+2"), "quote\"": "${name}"}}, "example": nil, "declarations": []string{"/nix/store/fixture-source/nix/modules/confctl/x.nix", "/other/x.nix"}},
		{"name": "confctl.hidden", "internal": true, "default": map[string]any{"_type": "not rendered"}},
		{"name": "unrelated.x", "default": map[string]any{"_type": "not rendered"}},
	}
	before, _ := json.Marshal(options)
	content, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return options, nil }, core.DocumentationNixLiteral, "/fixture/confctl", fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	// This is an independent finite input expectation, not a second catalogue.
	terms := []string{"confctl.a", "confctl.a", "confctl.z", "cluster.<name>.x", "confctl.programs.x"}
	offset := 0
	for _, term := range terms {
		needle := "\n`" + term + "`\n\n:   "
		at := strings.Index(s[offset:], needle)
		if at < 0 {
			t.Fatalf("missing ordered definition %s", term)
		}
		offset += at + len(needle)
	}
	if strings.Count(s, "\n:   ") != len(terms) || strings.Index(s, ":   first") > strings.Index(s, ":   second") || strings.Count(s, "*Example:*") != 3 {
		t.Fatal("definition count, duplicate order or example presence", s)
	}
	for _, literal := range []string{"[\n  (-1)\n  1.0e3\n  900719925474099312345\n]", `"\${value} \"quoted\" café"`, "0", `""`, "[\n]"} {
		if !strings.Contains(s, indentDefinition(codeBlock(literal, "nix"))) {
			t.Fatalf("shared value content omitted %q in %s", literal, s)
		}
	}
	if !strings.Contains(s, `"if" = 1.2300e+2;`) || !strings.Contains(s, `"quote\"" = "\${name}";`) || !strings.Contains(s, "*Declared by:* `<confctl/nix/modules/confctl/x.nix>`, `/other/x.nix`") {
		t.Fatal("shared reserved/key/value/declaration encoding", s)
	}
	if strings.Index(s, "\n`cluster.<name>.x`\n") < strings.Index(s, "\n# MACHINE CONFIGURATION\n") || strings.Index(s, "\n`confctl.programs.x`\n") < strings.Index(s, "\n# MACHINE SYSTEM CONFIGURATION\n") {
		t.Fatal("definition escaped its group")
	}
	after, _ := json.Marshal(options)
	if string(before) != string(after) {
		t.Fatal("declarations mutated")
	}
	options[2]["description"] = "changed declaration"
	changed, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return options, nil }, core.DocumentationNixLiteral, "", fixtureMetadata)
	if err != nil || !strings.Contains(string(changed), ":   changed declaration") || string(changed) == s {
		t.Fatal("changed declaration not reflected", err)
	}
}
