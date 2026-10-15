//go:build nixliteral

package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// This opt-in finite language test must be selected explicitly after affected
// review. It imports only its generated sibling data, without flakes, fetches,
// derivations or raw literalExpression input. Missing Nix is a failure, not skip.
func TestNixLiteralRoundtrip(t *testing.T) {
	nix, err := exec.LookPath("nix")
	if err != nil {
		t.Fatalf("nixliteral requires the declared Nix executable: %v", err)
	}
	nix, err = filepath.EvalSymlinks(nix)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.ReadFile(nix)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Nix executable=%s SHA256=%x", nix, sha256.Sum256(executable))
	controls := "\b\f\x1b\x7f\u0085\u2028😀\"\\${builtins.abort \"inert\"}\n\r\t"
	keys := map[string]any{"if": true, "then": true, "else": true, "let": true, "in": true, "rec": true, "with": true, "assert": true, "inherit": true, controls: true}
	value := map[string]any{
		"controls": controls, "keys": keys,
		"names":   map[string]any{"or": true, "true": true, "false": true, "null": true},
		"numbers": []any{json.Number("-1"), json.Number("2"), json.Number("9007199254740993"), json.Number("1.0"), json.Number("1e3"), json.Number("-125E-3"), float64(-0.125), math.Copysign(0, -1)},
		"nested":  []any{[]any{json.Number("-1"), json.Number("2")}, map[string]any{"negative": float64(-1)}},
	}
	check := `let v = import ./value.nix; in {
  controls = v.controls;
  keys = v.keys;
  names = builtins.attrNames v.names;
  numbers = v.numbers;
  numberTypes = map builtins.typeOf v.numbers;
  numberLength = builtins.length v.numbers;
  nestedList = builtins.elemAt v.nested 0;
  nestedListTypes = map builtins.typeOf (builtins.elemAt v.nested 0);
  nestedListLength = builtins.length (builtins.elemAt v.nested 0);
  nestedAttributeType = builtins.typeOf (builtins.elemAt v.nested 1).negative;
}`
	for name, render := range map[string]func(any) (string, error){"default": NixLiteral, "gendata": genDataNixLiteral, "documentation": DocumentationNixLiteral} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			literal, err := render(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "value.nix"), []byte(literal), 0600); err != nil {
				t.Fatal(err)
			}
			checkPath := filepath.Join(root, "check.nix")
			if err := os.WriteFile(checkPath, []byte(check), 0600); err != nil {
				t.Fatal(err)
			}
			argv := []string{"--extra-experimental-features", "nix-command", "eval", "--json", "--file", checkPath}
			var stdout, stderr bytes.Buffer
			cmd := exec.Command(nix, argv...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			t.Logf("argv=%q fixtureSHA256=%x checkSHA256=%x status=%v stdout=%q stderr=%q", argv, sha256.Sum256([]byte(literal)), sha256.Sum256([]byte(check)), err, stdout.Bytes(), stderr.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Controls            string
				Keys                map[string]bool
				Names               []string
				Numbers             []json.Number
				NumberTypes         []string
				NumberLength        int
				NestedList          []json.Number
				NestedListTypes     []string
				NestedListLength    int
				NestedAttributeType string
			}
			decoder := json.NewDecoder(&stdout)
			decoder.UseNumber()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			wantKeys := map[string]bool{}
			for key := range keys {
				wantKeys[key] = true
			}
			if got.Controls != controls || !reflect.DeepEqual(got.Keys, wantKeys) || !reflect.DeepEqual(got.Names, []string{"false", "null", "or", "true"}) {
				t.Fatalf("strings/keys changed: %#v", got)
			}
			if got.NumberLength != 8 || !reflect.DeepEqual(got.NumberTypes, []string{"int", "int", "int", "float", "float", "float", "float", "float"}) || len(got.Numbers) != 8 {
				t.Fatalf("numeric list/type changed: %#v", got)
			}
			for i, want := range []json.Number{"-1", "2", "9007199254740993"} {
				if got.Numbers[i] != want {
					t.Fatalf("integer %d lost exact digits: %q", i, got.Numbers[i])
				}
			}
			for i, want := range []float64{1, 1000, -0.125, -0.125, 0} {
				actual, err := got.Numbers[i+3].Float64()
				if err != nil || actual != want {
					t.Fatalf("finite float %d changed: %q %v", i, got.Numbers[i+3], err)
				}
			}
			if got.NestedListLength != 2 || !reflect.DeepEqual(got.NestedList, []json.Number{"-1", "2"}) || !reflect.DeepEqual(got.NestedListTypes, []string{"int", "int"}) || got.NestedAttributeType != "float" {
				t.Fatalf("nested grouping/type changed: %#v", got)
			}
		})
	}
}
