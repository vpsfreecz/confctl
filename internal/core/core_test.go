package core

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRubyPatternContracts(t *testing.T) {
	cases := []struct {
		p, n string
		want bool
	}{{"*", "a/.x", true}, {"*", ".x", false}, {"a/*", "a/.x", true}, {"{a,b}", "b", true}, {"{a}", "a", true}, {"{a,b", "{a,b", false}, {"[x", "x", false}, {"[!x]", "y", true}, {"[^x]", "x", false}, {"\\*", "*", true}, {"alpha*", "alpha/site", true}, {"{a,{b,c}}", "c", true}, {"?.x", "a.x", true}}
	for _, c := range cases {
		if got := Match(c.p, c.n); got != c.want {
			t.Errorf("%q %q = %v want %v", c.p, c.n, got, c.want)
		}
	}
}
func TestShellWordsLiteral(t *testing.T) {
	a := []string{"uname", "a b", "$(touch /tmp/pwn)", "a'b", "", "line\nnext", "a;echo"}
	got := ShellJoin(a)
	want := "uname a\\ b \\$\\(touch\\ /tmp/pwn\\) a\\'b '' line'\n'next a\\;echo"
	if got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
func TestCarrierExecUsesOwnTarget(t *testing.T) {
	own := "own.example"
	carrier := "carrier"
	m := Machine{Name: "guest", CarrierName: &carrier, Target: Target{Host: &own, Port: 2222}}
	a, e := SSHArgs(m, []string{"uname", "-r"})
	if e != nil || strings.Join(a, " ") != "ssh -l root -p 2222 own.example uname -r" {
		t.Fatal(a, e)
	}
}
func TestSelectionPrecedenceAndMetadata(t *testing.T) {
	all := []Machine{{Name: "name", Key: "other", Managed: true, Attributes: map[string]any{"custom": map[string]any{"bool": false}}}, {Name: "other", Key: "name", Managed: true}}
	p := "name"
	ms, e := Select(all, &p, nil, nil, "all")
	if e != nil || len(ms) != 1 || ms[0].Name != "name" {
		t.Fatal(ms, e)
	}
	ms, e = Select(all, nil, []string{"custom.bool=false"}, nil, "all")
	if e != nil || len(ms) != 1 {
		t.Fatal(ms, e)
	}
}
func TestCarriedBuilderChecks(t *testing.T) {
	c := "carrier"
	m := Machine{CarrierName: &c, Spin: "nixos", Attributes: map[string]any{"healthChecks": map[string]any{"builderCommands": []any{"one"}, "machineCommands": []any{"two"}, "systemd": map[string]any{"enable": true, "systemProperties": []any{"x"}, "unitProperties": map[string]any{"unit": []any{}}}}}}
	if m.Checks() != 3 {
		t.Fatal(m.Checks())
	}
}
func TestUnknownCommandNoMutation(t *testing.T) {
	cwd, _ := os.Getwd()
	dir := t.TempDir()
	if e := os.Chdir(dir); e != nil {
		t.Fatal(e)
	}
	defer os.Chdir(cwd)
	for _, a := range [][]string{{"not-a-command"}} {
		if code := Main(context.Background(), a); code != 64 {
			t.Fatalf("%v: %d", a, code)
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 0 {
			t.Fatal("unknown command mutated state")
		}
	}
}
func TestOutputPaddingAndFalse(t *testing.T) {
	got := Table([]map[string]any{{"name": "a", "false": false, "arr": []any{"a", "b"}}}, []string{"name", "false", "arr"}, true)
	if got != "NAME   FALSE   ARR\na      -       [\"a\", \"b\"]\n" {
		t.Fatalf("%q", got)
	}
}
func TestConfirmationLines(t *testing.T) {
	e := &Engine{}
	p := filepath.Join(t.TempDir(), "input")
	_ = os.WriteFile(p, []byte("\ninvalid answer\ny\n"), 0600)
	f, _ := os.Open(p)
	defer f.Close()
	ok, err := e.Confirm(f, "", false)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
}
func TestNixEscapesInterpolation(t *testing.T) {
	s, e := NixLiteral([]any{"${danger}", "quote\"\\"})
	if e != nil || !strings.Contains(s, "\\${danger}") {
		t.Fatal(s, e)
	}
}

func TestNixLiteralScalarCorrections(t *testing.T) {
	// F1/F2 intentionally correct prior Go-only escapes and float type loss.
	// These are source-backed Nix spellings, not original runtime captures.
	for name, render := range map[string]func(any) (string, error){
		"default": NixLiteral, "gendata": genDataNixLiteral, "documentation": DocumentationNixLiteral,
	} {
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				value any
				want  string
			}{
				{"", `""`},
				{"quote\"\\", `"quote\"\\"`},
				{"\n\r\t", `"\n\r\t"`},
				{`\n`, `"\\n"`},
				{`${builtins.abort "inert"}`, `"\${builtins.abort \"inert\"}"`},
				{`\${x}`, `"\\\${x}"`},
				{"$${x}\\", `"$\${x}\\"`},
				{"\b\f\x1b\x7f\u0085\u2028😀", "\"\b\f\x1b\x7f\u0085\u2028😀\""},
				{json.Number("9007199254740993"), "9007199254740993"},
				{json.Number("184467440737095516160000000000000000000"), "184467440737095516160000000000000000000"},
				{json.Number("1.0"), "1.0"},
				{json.Number("1e3"), "1.0e3"},
				{json.Number("-2E-4"), "-2.0E-4"},
				{json.Number("1.25e+2"), "1.25e+2"},
				{json.Number("-0E+003"), "-0.0E+003"},
				{json.Number("1e400"), "1.0e400"},
				{float64(1), "1.0"},
				{math.Copysign(0, -1), "-0.0"},
				{float64(1.25), "1.25"},
			} {
				got, err := render(test.value)
				if err != nil || got != test.want {
					t.Fatalf("%T(%q): got=%q err=%v, want=%q", test.value, test.value, got, err, test.want)
				}
			}
			for _, value := range []any{
				"prefix\x00suffix", string([]byte{0xff}), math.NaN(), math.Inf(1), math.Inf(-1),
				json.Number(""), json.Number("+1"), json.Number("01"), json.Number("1."), json.Number(".5"),
				json.Number("1e"), json.Number(" 1 "), json.Number("NaN"), json.Number("1; builtins.abort \"injection\""),
				map[string]any{"valid": []any{json.Number("2"), "\x00"}},
				map[string]any{"\x00": true}, map[string]any{string([]byte{0xff}): true},
				map[string]any{"nested": map[string]any{"\x00": true}},
				map[string]any{"nested": map[string]any{string([]byte{0xff}): true}},
				int64(1), struct{}{}, []int{1},
			} {
				got, err := render(value)
				if err == nil || got != "" {
					t.Fatalf("%T invalid value: got=%q err=%v", value, got, err)
				}
			}
		})
	}
}

func TestNixLiteralNumericListGrammar(t *testing.T) {
	value := []any{json.Number("-1"), json.Number("2"), json.Number("-0"), json.Number("-125E-3"), math.Copysign(0, -1), "-1", []any{json.Number("-2"), float64(-1), map[string]any{"negative": json.Number("-3")}}}
	for name, render := range map[string]func(any) (string, error){
		"default": NixLiteral, "gendata": genDataNixLiteral, "documentation": DocumentationNixLiteral,
	} {
		key := "negative"
		if name == "default" {
			key = `"negative"`
		}
		want := "[\n  (-1)\n  2\n  (-0)\n  (-125.0E-3)\n  (-0.0)\n  \"-1\"\n  [\n    (-2)\n    (-1.0)\n    {\n      " + key + " = -3;\n    }\n  ]\n]"
		got, err := render(value)
		if err != nil || got != want {
			t.Fatalf("%s: got=%q err=%v, want=%q", name, got, err, want)
		}
	}
}
func TestMachineOrder(t *testing.T) {
	b := []byte(`{"z":{"name":"z","clusterName":"z","metaConfig":{"managed":true,"spin":"nixos"}},"a":{"name":"a","clusterName":"a","metaConfig":{"managed":true,"spin":"nixos"}}}`)
	ms, e := DecodeMachines(b)
	if e != nil || len(ms) != 2 || ms[0].Name != "z" {
		t.Fatal(ms, e)
	}
}
func TestStatusRolesAndDuration(t *testing.T) {
	x := normalizeInputs(map[string]any{"a": "123456789", "b": map[string]any{"rev": "b", "short_rev": "B"}, "c": false})
	if x["a"]["shortRev"] != "12345678" || x["b"]["shortRev"] != "B" || inputState(x["c"], x["c"]) != "unknown" {
		t.Fatal(x)
	}
	if s, e := formatDuration(60); s != "60.0s" || e != nil {
		t.Fatal(s, e)
	}
}

func TestNumericMetadataExact(t *testing.T) {
	b := []byte(`{"m":{"name":"m","key":"k","metaConfig":{"managed":true,"custom":{"large":9007199254740993,"decimal":1.0,"array":[9007199254740993,1.0]}}}}`)
	ms, err := DecodeMachines(b)
	if err != nil {
		t.Fatal(err)
	}
	m := ms[0]
	if str(m.Attr("custom.large")) != "9007199254740993" || str(m.Attr("custom.decimal")) != "1.0" || str(m.Attr("custom.array")) != "[9007199254740993, 1.0]" {
		t.Fatal(m.Attributes)
	}
	for _, f := range []string{"custom.large=9007199254740993", "custom.decimal=1.0"} {
		got, e := Select(ms, nil, []string{f}, nil, "all")
		if e != nil || len(got) != 1 {
			t.Fatal(f, got, e)
		}
	}
}
