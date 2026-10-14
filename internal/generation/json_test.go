package generation_test

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/generation"
)

// Literal fixtures exercise the existing ordered/admission contract through
// actual external-package access. They are not new Ruby provider observations,
// universal Ruby Float equivalence or generation-record validation.
func TestDecodeJSONValueGenericRoots(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         any
	}{
		{"object", `{}`, generation.Object{}},
		{"array", `[]`, []any{}},
		{"string", `"literal"`, "literal"},
		{"number", `-0`, json.Number("-0")},
		{"true", `true`, true},
		{"false", `false`, false},
		{"null", " \t\r\nnull\n ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := generation.DecodeJSONValue([]byte(tc.source))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v (%T), %v; want %#v (%T)", got, got, err, tc.want, tc.want)
			}
		})
	}
	// Fields that would fail LoadBuild are ordinary data at this boundary.
	got, err := generation.DecodeJSONValue([]byte(`{"mode":"unknown","date":null,"toplevel":false,"extra":[]}`))
	want := generation.Object{{Name: "mode", Value: "unknown"}, {Name: "date", Value: nil}, {Name: "toplevel", Value: false}, {Name: "extra", Value: []any{}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
}

func TestDecodeJSONValueNestedOrderDuplicateReplacementAndExactTokens(t *testing.T) {
	source := `{"z":{"old":true},"a":[{"beta":9007199254740993,"alpha":[1.23456789123456789123,1e1000,1E+03,-0,-0.0,-0e+00]},[],{},null],"\u007a":{"new":999999999999999999999999999999,"nested":{"y":0,"x":1,"y":2}}}`
	want := generation.Object{
		{Name: "z", Value: generation.Object{
			{Name: "new", Value: json.Number("999999999999999999999999999999")},
			{Name: "nested", Value: generation.Object{{Name: "y", Value: json.Number("2")}, {Name: "x", Value: json.Number("1")}}},
		}},
		{Name: "a", Value: []any{
			generation.Object{
				{Name: "beta", Value: json.Number("9007199254740993")},
				{Name: "alpha", Value: []any{json.Number("1.23456789123456789123"), json.Number("1e1000"), json.Number("1E+03"), json.Number("-0"), json.Number("-0.0"), json.Number("-0e+00")}},
			}, []any{}, generation.Object{}, nil,
		}},
	}
	got, err := generation.DecodeJSONValue([]byte(source))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, %v; want %#v", got, err, want)
	}
	root, ok := got.(generation.Object)
	if !ok {
		t.Fatalf("object type %T", got)
	}
	value, found := root.Lookup("z")
	if !found || !reflect.DeepEqual(value, want[0].Value) {
		t.Fatal("external Lookup lost final replacement", value)
	}
	if _, found := value.(generation.Object).Lookup("old"); found {
		t.Fatal("duplicate values were merged")
	}
	for _, source := range []string{`{"x":"\ud800","x":"valid"}`, `{"x":[1,],"x":[]}`} {
		value, err := generation.DecodeJSONValue([]byte(source))
		if err == nil || value != nil {
			t.Fatalf("invalid overwritten value accepted: %#v %v", value, err)
		}
	}
}

func TestDecodeJSONValueWholeInputAndUnwrappedErrors(t *testing.T) {
	for _, source := range []string{"", " \t\r\n", "{", "[1", `{"x":}`, `[1,]`, "01", `"unfinished`, "null true", "{}{}", "{}junk"} {
		value, err := generation.DecodeJSONValue([]byte(source))
		if err == nil || value != nil {
			t.Fatalf("accepted %q: %#v %v", source, value, err)
		}
		var recordError *generation.RecordError
		if errors.As(err, &recordError) {
			t.Fatalf("generic error wrapped as record: %v", err)
		}
	}
	value, err := generation.DecodeJSONValue(nil)
	if value != nil || !errors.Is(err, io.EOF) {
		t.Fatalf("empty input %#v, %v", value, err)
	}
	value, err = generation.DecodeJSONValue([]byte("@"))
	var syntaxError *json.SyntaxError
	if value != nil || !errors.As(err, &syntaxError) {
		t.Fatalf("syntax error lost: %#v, %T %v", value, err, err)
	}
	value, err = generation.DecodeJSONValue([]byte("null"))
	if value != nil || err != nil {
		t.Fatalf("null success confused with failure: %#v %v", value, err)
	}
}

func TestDecodeJSONValueFiniteInheritedStringAdmission(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{`"\ud800\udc00"`, "\U00010000"},
		{`"\udc00"`, "\xed\xb0\x80"},
		{`"x\udfff\udc00"`, "x\xed\xbf\xbf\xed\xb0\x80"},
		{`"a\n\udc00\t\ud800\udc00"`, "a\n\xed\xb0\x80\t\U00010000"},
		{`"\\ud800"`, `\ud800`},
		{`"x\u0000y"`, "x\x00y"},
	} {
		value, err := generation.DecodeJSONValue([]byte(tc.source))
		if err != nil || value != tc.want {
			t.Fatalf("%s bytes %x, %v; want %x", tc.source, value, err, tc.want)
		}
	}
	value, err := generation.DecodeJSONValue([]byte(`{"\udc00":0,"\u0000":"x\u0000y","\ud800\udc00":false,"\\ud800":true}`))
	want := generation.Object{{Name: "\xed\xb0\x80", Value: json.Number("0")}, {Name: "\x00", Value: "x\x00y"}, {Name: "\U00010000", Value: false}, {Name: `\ud800`, Value: true}}
	if err != nil || !reflect.DeepEqual(value, want) {
		t.Fatalf("key bytes/model %#v, %v; want %#v", value, err, want)
	}
	for _, source := range []string{`"\ud800"`, `"\ud800x"`, `"\ud800\u0041"`, `"\ud800\ud800"`, `"\udc00\ud800"`, `{"\ud800":0}`} {
		value, err := generation.DecodeJSONValue([]byte(source))
		if err == nil || value != nil {
			t.Fatalf("invalid surrogate accepted %s: %#v %v", source, value, err)
		}
	}
	// This selected case remains encoding/json's raw-UTF8 replacement behavior,
	// not a new UTF8 rejection rule or a Ruby malformed-encoding parity claim.
	value, err = generation.DecodeJSONValue([]byte{'"', 0xff, '"'})
	if err != nil || value != "\ufffd" {
		t.Fatalf("raw UTF8 policy changed: %#v %v", value, err)
	}
}

func TestDecodeJSONValueGenericRootNonemptyDepthBoundary(t *testing.T) {
	array := func(depth int, leaf string) string {
		return strings.Repeat("[", depth) + leaf + strings.Repeat("]", depth)
	}
	object := func(depth int, leaf string) string {
		return strings.Repeat(`{"x":`, depth) + leaf + strings.Repeat("}", depth)
	}
	mixed := func(depth int) string {
		value := "0"
		for i := 0; i < depth; i++ {
			if i%2 == 0 {
				value = "[" + value + "]"
			} else {
				value = `{"x":` + value + "}"
			}
		}
		return value
	}
	// These are generic-root fixtures. The existing record tests retain their
	// additional nonempty record wrapper and are deliberately not rewritten.
	for _, tc := range []struct {
		name, source string
		admitted     bool
	}{
		{"array100", array(100, "0"), true}, {"array101", array(101, "0"), false},
		{"object100", object(100, "0"), true}, {"object101", object(101, "0"), false},
		{"mixed100", mixed(100), true}, {"mixed101", mixed(101), false},
		{"array100_empty_array", array(100, "[]"), true}, {"array100_empty_object", array(100, "{}"), true},
		{"object100_empty_array", object(100, "[]"), true}, {"object100_empty_object", object(100, "{}"), true},
		{"array100_nonempty_leaf", array(100, "[0]"), false}, {"object100_nonempty_leaf", object(100, `{"x":0}`), false},
		{"array101_empty_leaf", array(101, "[]"), false}, {"object101_empty_leaf", object(101, "{}"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := generation.DecodeJSONValue([]byte(tc.source))
			if tc.admitted {
				if err != nil || value == nil {
					t.Fatalf("boundary rejected %v", err)
				}
			} else {
				if err == nil || value != nil || !strings.Contains(err.Error(), "nesting of 101 is too deep") {
					t.Fatalf("boundary admitted/wrong error %#v %v", value, err)
				}
			}
		})
	}
}
