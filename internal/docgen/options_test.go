package docgen

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOptionsSourceDerivedGolden(t *testing.T) {
	file, err := os.Open("testdata/module-options.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options, err := DecodeOptions(file)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(options)
	var values []any
	render := func(value any) (string, error) {
		values = append(values, value)
		switch v := value.(type) {
		case nil:
			return "null", nil
		case bool:
			if v {
				return "true", nil
			}
			return "false", nil
		case json.Number:
			return string(v), nil
		case string:
			if v == "" {
				return `""`, nil
			}
		}
		return "", errors.New("fixture renderer received an unexpected value")
	}
	loads := 0
	loader := func(context.Context) ([]ModuleOption, error) { loads++; return options, nil }
	content, err := Options(context.Background(), loader, render, "/fixture/confctl", fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "testdata/pandoc/options.golden.json", content)
	after, _ := json.Marshal(options)
	if loads != 1 || string(before) != string(after) {
		t.Fatal("loader called repeatedly or its declarations mutated")
	}
	// This records actual callbacks: sorted public inputs only, no literal text.
	if !reflect.DeepEqual(values, []any{false, true, false, json.Number("9007199254740993"), true, nil, ""}) {
		t.Fatalf("value rendering order/domain: %#v", values)
	}
}

func TestDecodeOptionsExactValuesAndErrors(t *testing.T) {
	options, err := DecodeOptions(strings.NewReader(`[{"name":"confctl.number","default":900719925474099312345,"example":1.2300e+2}]`))
	if err != nil || options[0]["default"] != json.Number("900719925474099312345") || options[0]["example"] != json.Number("1.2300e+2") {
		t.Fatalf("numeric tokens rounded: %#v, %v", options, err)
	}
	for _, input := range []string{`null`, `{}`, `[`, `[] []`, `[{"name":`} {
		if _, err := DecodeOptions(strings.NewReader(input)); err == nil {
			t.Errorf("accepted malformed declarations: %s", input)
		}
	}
	if options, err := DecodeOptions(strings.NewReader(`[]`)); err != nil || len(options) != 0 {
		t.Fatalf("empty public declaration list: %#v, %v", options, err)
	}
}

func TestOptionsLiteralAndRubyExamplePresence(t *testing.T) {
	for _, value := range []any{nil, false, json.Number("0"), "", []any{}} {
		option := ModuleOption{"name": "confctl.fixture", "default": map[string]any{"_type": "literalExpression", "text": "line1\nline`2"}, "example": value, "declarations": []string{}}
		var seen []any
		render := func(value any) (string, error) { seen = append(seen, value); return "fixture", nil }
		loader := func(context.Context) ([]ModuleOption, error) { return []ModuleOption{option}, nil }
		content, err := Options(context.Background(), loader, render, "", fixtureMetadata)
		if err != nil || !strings.Contains(string(content), "*Default:*\n\n    ```nix\n    line1\n    line`2\n    ```\n") {
			t.Fatalf("literal text escaped or changed: %s, %v", content, err)
		}
		wantExample := value != nil && value != false
		if strings.Contains(string(content), "*Example:*") != wantExample || len(seen) != boolInt(wantExample) {
			t.Fatalf("Ruby presence lost for %#v: %s; calls=%#v", value, content, seen)
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestRawLiteralBypassesDataEncodingAndValidation(t *testing.T) {
	// Already-Nix source remains trusted text, even for content that the shared
	// data formatter would reject or normalize. This is no evaluation proof.
	text := "\"${alreadyNix}\"\n[ -1 1e3 ]\n\x00`raw`"
	option := ModuleOption{
		"name": "confctl.raw", "declarations": []string{},
		"default": map[string]any{"_type": "literalExpression", "text": text},
		"example": map[string]any{"_type": "literalExpression", "text": text},
	}
	content, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return []ModuleOption{option}, nil },
		func(any) (string, error) { t.Fatal("raw source reached the data formatter"); return "", nil }, "", fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Default", "Example"} {
		if want := "*" + label + ":*\n\n    ```nix\n    \"${alreadyNix}\"\n    [ -1 1e3 ]\n    \x00`raw`\n    ```\n"; !strings.Contains(string(content), want) {
			t.Fatalf("raw %s text changed; missing %q in %q", label, want, content)
		}
	}
}

func TestOptionsErrorsAndEarlyPublicFilter(t *testing.T) {
	sentinel := errors.New("loader failed")
	if _, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return nil, sentinel }, func(any) (string, error) { t.Fatal("renderer ran"); return "", nil }, "", fixtureMetadata); !errors.Is(err, sentinel) {
		t.Fatalf("loader failure: %v", err)
	}
	for _, change := range []ModuleOption{
		{"name": 7}, {"internal": "yes"}, {"description": []any{}}, {"type": false},
		{"default": map[string]any{"_type": "other", "text": "x"}},
		{"default": map[string]any{"_type": "literalExpression", "text": nil}},
		{"declarations": nil}, {"declarations": []any{1}},
	} {
		option := ModuleOption{"name": "confctl.fixture", "declarations": []string{}}
		for key, value := range change {
			option[key] = value
		}
		loader := func(context.Context) ([]ModuleOption, error) { return []ModuleOption{option}, nil }
		if _, err := Options(context.Background(), loader, func(any) (string, error) { return "null", nil }, "", fixtureMetadata); err == nil {
			t.Errorf("accepted invalid option: %#v", option)
		}
	}
	loader := func(context.Context) ([]ModuleOption, error) {
		return []ModuleOption{{"name": "unrelated.option", "default": map[string]any{"_type": "unknown"}}, {"name": "confctl.private", "internal": true}}, nil
	}
	if _, err := Options(context.Background(), loader, func(any) (string, error) { t.Fatal("excluded option rendered"); return "", nil }, "", fixtureMetadata); err != nil {
		t.Fatal(err)
	}
	option := ModuleOption{"name": "cluster.<name>.x", "declarations": []string{}}
	_, err := Options(context.Background(), func(context.Context) ([]ModuleOption, error) { return []ModuleOption{option}, nil }, func(any) (string, error) { return "", sentinel }, "", fixtureMetadata)
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "cluster.<name>.x") {
		t.Fatalf("renderer error lost option identity: %v", err)
	}
}

func TestLiteralValidationPrecedesValueRendering(t *testing.T) {
	loader := func(context.Context) ([]ModuleOption, error) {
		return []ModuleOption{
			{"name": "confctl.a", "default": false, "declarations": []string{}},
			{"name": "cluster.<name>.z", "default": map[string]any{"_type": "unsupported"}, "declarations": []string{}},
		}, nil
	}
	_, err := Options(context.Background(), loader, func(any) (string, error) {
		t.Fatal("value rendered before all declaration expressions were extracted")
		return "", nil
	}, "", fixtureMetadata)
	if err == nil || !strings.Contains(err.Error(), "cluster.<name>.z") {
		t.Fatalf("literal extraction error lost: %v", err)
	}
}

func TestEqualNameOrderingAndSystemPrefix(t *testing.T) {
	loader := func(context.Context) ([]ModuleOption, error) {
		return []ModuleOption{
			{"name": "confctl.same", "description": "first", "default": nil, "declarations": []string{}},
			{"name": "confctl.inputsInformation", "description": "system prefix", "default": nil, "declarations": []string{}},
			{"name": "confctl.same", "description": "second", "default": nil, "declarations": []string{}},
		}, nil
	}
	content, err := Options(context.Background(), loader, func(any) (string, error) { return "null", nil }, "", fixtureMetadata)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	if strings.Index(s, ":   first\n") > strings.Index(s, ":   second\n") || strings.Count(s, "\n`confctl.same`\n\n:   ") != 2 {
		t.Fatal("equal-name options were reordered or deduplicated")
	}
	if strings.Index(s, "system prefix") < strings.Index(s, "# MACHINE SYSTEM CONFIGURATION") {
		t.Fatal("source starts-with system prefix was narrowed")
	}
}

func TestDescriptionsAndDeclarations(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"Use <literal>x</literal> and <option>y</option> &amp; &lt;b&gt; &quot;q&quot; &apos;s&apos; &#65; &#x42; &nbsp;", "Use `x` and `y` & <b> \"q\" 's' A B &nbsp;"},
		{"<literal>two`parts</literal>", "``two`parts``"},
		{"<literal>&#96;edge&#96;</literal>", "`` `edge` ``"},
		{"&lt;literal&gt;source&lt;/literal&gt;", "<literal>source</literal>"},
		{"<literal><option>x</option></literal>", "<literal>`x`</literal>"},
		{"<option><literal>x</literal></option>", "`` `x` ``"},
		{"<literal>&amp;lt;</literal><option>&amp;lt;</option>", "`&lt;``&lt;`"},
		{"<option></option><literal>x<option>y</literal>", "<option></option><literal>x<option>y</literal>"},
		{"&amp;lt;", "&lt;"},
	} {
		if got := markdownDescription(test.input); got != test.want {
			t.Errorf("%q: got %q, want %q", test.input, got, test.want)
		}
	}
	for _, test := range []struct{ path, want string }{
		{"/fixture/confctl/nix/modules/confctl/foo.nix", "<confctl/nix/modules/confctl/foo.nix>"},
		{"/fixture/confctl/nix/modules/cluster", "<confctl/nix/modules/cluster>"},
		{"/nix/store/abc-source/nix/modules/cluster/default.nix", "<confctl/nix/modules/cluster/default.nix>"},
		{"/fixture/confctl-other/nix/modules/confctl/x.nix", "/fixture/confctl-other/nix/modules/confctl/x.nix"},
		{"/nix/store/abc-source/nix/modules/confctl-other/x.nix", "/nix/store/abc-source/nix/modules/confctl-other/x.nix"},
		{"/fixture/confctl/modules/x.nix", "/fixture/confctl/modules/x.nix"},
	} {
		if got := rewriteDeclaration(test.path, "/fixture/confctl"); got != test.want {
			t.Errorf("declaration %s: got %s, want %s", test.path, got, test.want)
		}
	}
	if got := indentDescription("first\nsecond\n\nthird\n\n"); got != "first\n    second\n\n    third\n\n" {
		t.Fatalf("definition indentation: %q", got)
	}
}
