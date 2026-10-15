package main

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/core"
	"github.com/vpsfreecz/confctl/internal/docgen"
)

func TestSharedDocumentationFormatterAndRawLiteral(t *testing.T) {
	options, err := docgen.DecodeOptions(strings.NewReader(`[
  {"name":"confctl.compound","default":[{"z'":"${host}","a":9007199254740993},{"key1":"quote\"\n","a":"x"}],"declarations":[]},
  {"name":"confctl.literal","default":{"_type":"literalExpression","text":"\"${alreadyNix}\""},"declarations":[]},
  {"name":"confctl.key","default":[{"${host}\"":"value"}],"declarations":[]}
]`))
	if err != nil {
		t.Fatal(err)
	}
	content, err := docgen.Options(context.Background(), func(context.Context) ([]docgen.ModuleOption, error) { return options, nil },
		core.DocumentationNixLiteral, "", docgen.Metadata{Version: "fixture", Date: "2026-10-09"})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		"*Default:*\n\n    ```nix\n    [\n      {\n        a = 9007199254740993;\n        z' = \"\\${host}\";\n      }\n      {\n        \"a\" = \"x\";\n        \"key1\" = \"quote\\\"\\n\";\n      }\n    ]\n    ```\n",
		"*Default:*\n\n    ```nix\n    \"${alreadyNix}\"\n    ```\n",
		"\"\\${host}\\\"\" = \"value\";",
	} {
		if !strings.Contains(string(content), source) {
			t.Fatalf("shared formatter/source literal differs; missing %q in:\n%s", source, content)
		}
	}
	// The ordinary formatter remains a different existing consumer policy.
	ordinary, err := core.NixLiteral(map[string]any{"z": json.Number("1"), "a": false})
	if err != nil || ordinary != "{\n  \"a\" = false;\n  \"z\" = 1;\n}" {
		t.Fatalf("default formatter changed: %q, %v", ordinary, err)
	}
}

// These are literal source expectations from the F1/F2/F3 language contract,
// not output captured from either generator or a successful Nix evaluation.
func TestSharedFormatterCorrectionsReachMarkdown(t *testing.T) {
	controls := "quote\" slash\\ \\${host} $$${x}\n\r\t\b\f\x1b\x7f\u0085\u2028😀 trail\\"
	encoded := `"quote\" slash\\ \\\${host} $$\${x}\n\r\t` + "\b\f\x1b\x7f\u0085\u2028😀" + ` trail\\"`
	for _, test := range []struct {
		name    string
		value   any
		literal string
		block   string
	}{
		{"controls and UTF-8", controls, encoded, "    ```nix\n    " + encoded + "\n    ```\n"},
		{"literal backslash then n", `\n`, `"\\n"`, "    ```nix\n    \"\\\\n\"\n    ```\n"},
		{"quoted key and value", []any{map[string]any{controls: controls}}, "[\n  {\n    " + encoded + " = " + encoded + ";\n  }\n]", "    ```nix\n    [\n      {\n        " + encoded + " = " + encoded + ";\n      }\n    ]\n    ```\n"},
		{"exponent only", json.Number("1e3"), "1.0e3", "    ```nix\n    1.0e3\n    ```\n"},
		{"negative exponent", json.Number("-2E-4"), "-2.0E-4", "    ```nix\n    -2.0E-4\n    ```\n"},
		{"decimal exponent", json.Number("1.2300e+2"), "1.2300e+2", "    ```nix\n    1.2300e+2\n    ```\n"},
		{"large integer text, beyond target range", json.Number("900719925474099312345"), "900719925474099312345", "    ```nix\n    900719925474099312345\n    ```\n"},
		{"integral float", float64(1), "1.0", "    ```nix\n    1.0\n    ```\n"},
		{"negative float zero", math.Copysign(0, -1), "-0.0", "    ```nix\n    -0.0\n    ```\n"},
		{"numeric list boundaries", []any{json.Number("-1"), json.Number("2"), json.Number("-0"), json.Number("-125E-3"), math.Copysign(0, -1), "-1", []any{json.Number("-2E-4")}}, "[\n  (-1)\n  2\n  (-0)\n  (-125.0E-3)\n  (-0.0)\n  \"-1\"\n  [\n    (-2.0E-4)\n  ]\n]", "    ```nix\n    [\n      (-1)\n      2\n      (-0)\n      (-125.0E-3)\n      (-0.0)\n      \"-1\"\n      [\n        (-2.0E-4)\n      ]\n    ]\n    ```\n"},
		{"reserved parent, independent child", []any{map[string]any{"okay": map[string]any{"z": false, "a": json.Number("1")}, "if": json.Number("2")}}, "[\n  {\n    \"if\" = 2;\n    \"okay\" = {\n      a = 1;\n      z = false;\n    };\n  }\n]", "    ```nix\n    [\n      {\n        \"if\" = 2;\n        \"okay\" = {\n          a = 1;\n          z = false;\n        };\n      }\n    ]\n    ```\n"},
		{"attribute grammar exceptions", []any{map[string]any{"true": true, "or": json.Number("1"), "null": nil, "false": false}}, "[\n  {\n    false = false;\n    null = null;\n    or = 1;\n    true = true;\n  }\n]", "    ```nix\n    [\n      {\n        false = false;\n        null = null;\n        or = 1;\n        true = true;\n      }\n    ]\n    ```\n"},
		{"backtick wrapper", "notes`log", `"notes` + "`" + `log"`, "    ```nix\n    \"notes`log\"\n    ```\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			literal, err := core.DocumentationNixLiteral(test.value)
			if err != nil || literal != test.literal {
				t.Fatalf("Nix source = %q, want %q; error = %v", literal, test.literal, err)
			}
			options := []docgen.ModuleOption{{"name": "confctl.fixture", "default": test.value, "example": test.value, "declarations": []string{}}}
			content, err := docgen.Options(context.Background(), func(context.Context) ([]docgen.ModuleOption, error) { return options, nil },
				core.DocumentationNixLiteral, "", docgen.Metadata{Version: "fixture", Date: "2026-10-09"})
			if err != nil {
				t.Fatal(err)
			}
			for _, label := range []string{"Default", "Example"} {
				if want := "*" + label + ":*\n\n" + test.block; !strings.Contains(string(content), want) {
					t.Fatalf("Markdown lost %s literal; missing %q in %q", label, want, content)
				}
			}
		})
	}
}
