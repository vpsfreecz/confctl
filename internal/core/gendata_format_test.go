package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vpsfreecz/confctl/internal/gendata"
)

// All expected bytes below are independent source predictions, not original
// Ruby captures. All entries share the corrected scalar/list encoder. Default
// consumers keep the quoted-key policy, and arrays keep their input order.
func TestGenDataNixLiteralKeyLayoutsAndDefault(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      any
		want       string
		wantQuoted string
	}{
		{"empty-map", map[string]any{}, "{\n}", "{\n}"},
		{"empty-list", []any{}, "[\n]", "[\n]"},
		{"bare", map[string]any{"node": nil, "_": true, "a-b": false, "a'b": json.Number("9007199254740993")},
			"{\n  _ = true;\n  a'b = 9007199254740993;\n  a-b = false;\n  node = null;\n}",
			"{\n  \"_\" = true;\n  \"a'b\" = 9007199254740993;\n  \"a-b\" = false;\n  \"node\" = null;\n}"},
		{"digit-tail-quotes-all", map[string]any{"alpha": nil, "ipv4": []any{}},
			"{\n  \"alpha\" = null;\n  \"ipv4\" = [\n  ];\n}",
			"{\n  \"alpha\" = null;\n  \"ipv4\" = [\n  ];\n}"},
		{"digit-first", map[string]any{"4a": nil, "alpha": true},
			"{\n  \"4a\" = null;\n  \"alpha\" = true;\n}",
			"{\n  \"4a\" = null;\n  \"alpha\" = true;\n}"},
		{"dot-empty", map[string]any{"": nil, "alpha": true, "host.example": false},
			"{\n  \"\" = null;\n  \"alpha\" = true;\n  \"host.example\" = false;\n}",
			"{\n  \"\" = null;\n  \"alpha\" = true;\n  \"host.example\" = false;\n}"},
		{"independent-nested-maps", map[string]any{"z.example": map[string]any{"node": map[string]any{"id": json.Number("1")}}, "alpha": map[string]any{"a1": nil, "safe": true}},
			"{\n  \"alpha\" = {\n    \"a1\" = null;\n    \"safe\" = true;\n  };\n  \"z.example\" = {\n    node = {\n      id = 1;\n    };\n  };\n}",
			"{\n  \"alpha\" = {\n    \"a1\" = null;\n    \"safe\" = true;\n  };\n  \"z.example\" = {\n    \"node\" = {\n      \"id\" = 1;\n    };\n  };\n}"},
		{"array-mode-and-order", []any{map[string]any{"prefix": json.Number("0"), "address": "z"}, map[string]any{"address": "a", "prefix": nil}},
			"[\n  {\n    address = \"z\";\n    prefix = 0;\n  }\n  {\n    address = \"a\";\n    prefix = null;\n  }\n]",
			"[\n  {\n    \"address\" = \"z\";\n    \"prefix\" = 0;\n  }\n  {\n    \"address\" = \"a\";\n    \"prefix\" = null;\n  }\n]"},
		{"scalar-array", []any{true, false, nil, json.Number("-1"), json.Number("9007199254740993"), float64(1.25), ""},
			// F2: negative list elements require grouping in Nix grammar.
			"[\n  true\n  false\n  null\n  (-1)\n  9007199254740993\n  1.25\n  \"\"\n]",
			"[\n  true\n  false\n  null\n  (-1)\n  9007199254740993\n  1.25\n  \"\"\n]"},
		{"string-array", []string{"z", "a"}, "[\n  \"z\"\n  \"a\"\n]", "[\n  \"z\"\n  \"a\"\n]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := genDataNixLiteral(test.value)
			if err != nil || got != test.want {
				t.Fatalf("private=%q err=%v, want=%q", got, err, test.want)
			}
			documentation, err := DocumentationNixLiteral(test.value)
			if err != nil || documentation != test.want {
				t.Fatalf("documentation=%q err=%v, want=%q", documentation, err, test.want)
			}
			quoted, err := NixLiteral(test.value)
			if err != nil || quoted != test.wantQuoted {
				t.Fatalf("default=%q err=%v, want=%q", quoted, err, test.wantQuoted)
			}
		})
	}
	for _, value := range []any{int64(1), struct{}{}, []int{1}, map[string]any{"bad": struct{}{}}} {
		_, privateErr := genDataNixLiteral(value)
		_, defaultErr := NixLiteral(value)
		_, documentationErr := DocumentationNixLiteral(value)
		if privateErr == nil || defaultErr == nil || documentationErr == nil || privateErr.Error() != defaultErr.Error() || documentationErr.Error() != defaultErr.Error() {
			t.Fatalf("error changed for %T: private=%v default=%v documentation=%v", value, privateErr, defaultErr, documentationErr)
		}
	}
}

func TestGenDataReservedKeysAndRepresentableKeyBytes(t *testing.T) {
	for _, key := range []string{"if", "then", "else", "let", "in", "rec", "with", "assert", "inherit"} {
		value := map[string]any{key: json.Number("1"), "okay": map[string]any{"or": true}}
		// The complete parent map is quoted; its child makes its own decision.
		for name, render := range map[string]func(any) (string, error){"gendata": genDataNixLiteral, "documentation": DocumentationNixLiteral} {
			got, err := render(value)
			keyLine := "  \"" + key + "\" = 1;\n"
			childLine := "  \"okay\" = {\n    or = true;\n  };\n"
			want := "{\n" + keyLine + childLine + "}"
			if key > "okay" {
				want = "{\n" + childLine + keyLine + "}"
			}
			if err != nil || got != want {
				t.Fatalf("%s reserved %s: got=%q err=%v want=%q", name, key, got, err, want)
			}
		}
	}
	value := map[string]any{"or": json.Number("1"), "true": false, "false": true, "null": nil}
	want := "{\n  false = true;\n  null = null;\n  or = 1;\n  true = false;\n}"
	for _, render := range []func(any) (string, error){genDataNixLiteral, DocumentationNixLiteral} {
		got, err := render(value)
		if err != nil || got != want {
			t.Fatalf("admissible names: got=%q err=%v want=%q", got, err, want)
		}
	}
	key := "\b\f\x1b\x7f\u0085\u2028😀"
	for _, render := range []func(any) (string, error){NixLiteral, genDataNixLiteral, DocumentationNixLiteral} {
		got, err := render(map[string]any{key: "value"})
		want := "{\n  \"\b\f\x1b\x7f\u0085\u2028😀\" = \"value\";\n}"
		if err != nil || got != want {
			t.Fatalf("literal key bytes: got=%q err=%v want=%q", got, err, want)
		}
	}
}

func TestGenDataBareKeyFullASCIIPolicy(t *testing.T) {
	for _, key := range []string{"_", "a", "Z", "a-b", "a'b", "A_Z", "_-''"} {
		if !genDataBareKey(key) {
			t.Errorf("ordinary bare key %q rejected", key)
		}
	}
	for _, key := range []string{"", "4", "4a", "a4", "-a", "'a", "a.b", "a b", "a\nb", "a\n", "a\r", "é", "a\x00", "${k}", "a\"b", "a\\b"} {
		if genDataBareKey(key) {
			t.Errorf("unsafe/full-key case %q accepted bare", key)
		}
	}
}

func TestGenDataIntentionalSafeLiteralEscaping(t *testing.T) {
	// Ruby interpolates quoted keys raw and String#dump does not protect Nix
	// interpolation. These safe-literal corrections are intentional, not O.
	value := map[string]any{"a\"b": "quote\"\\", "d${key}": "${value}\n\t", "safe": map[string]any{"name": "${name}"}}
	want := "{\n  \"a\\\"b\" = \"quote\\\"\\\\\";\n  \"d\\${key}\" = \"\\${value}\\n\\t\";\n  \"safe\" = {\n    name = \"\\${name}\";\n  };\n}"
	got, err := genDataNixLiteral(value)
	if err != nil || got != want {
		t.Fatalf("private escaped=%q err=%v, want=%q", got, err, want)
	}
	got, err = DocumentationNixLiteral(value)
	if err != nil || got != want {
		t.Fatalf("documentation escaped=%q err=%v, want=%q", got, err, want)
	}
	wantQuoted := "{\n  \"a\\\"b\" = \"quote\\\"\\\\\";\n  \"d\\${key}\" = \"\\${value}\\n\\t\";\n  \"safe\" = {\n    \"name\" = \"\\${name}\";\n  };\n}"
	got, err = NixLiteral(value)
	if err != nil || got != wantQuoted {
		t.Fatalf("default escaped=%q err=%v, want=%q", got, err, wantQuoted)
	}
}

func gdField[T any](value T) gendata.Field[T] {
	return gendata.Field[T]{Present: true, Value: &value}
}

type genDataSource struct {
	showIDs []gendata.Field[json.Number]
	shows   []gendata.VPS
	list    func() ([]gendata.Network, error)
}

func (s *genDataSource) ShowVPS(_ context.Context, id gendata.Field[json.Number]) (gendata.VPS, error) {
	s.showIDs = append(s.showIDs, id)
	return s.shows[len(s.showIDs)-1], nil
}

func (s *genDataSource) ListNetworks(context.Context) ([]gendata.Network, error) {
	if s.list == nil {
		return nil, errors.New("unexpected list")
	}
	return s.list()
}

func TestGenDataGeneratorNormalFilesAndAllOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data/vpsadmin/networks"), 0755); err != nil {
		t.Fatal(err)
	}
	vps := gendata.VPS{Node: gdField(gendata.Node{
		ID: gdField(json.Number("9007199254740993")), Name: gdField("node"), DomainName: gdField("n"),
		Location: gdField(gendata.Location{Domain: gdField("loc"), Environment: gdField(gendata.Environment{Domain: gdField("test")})}),
	})}
	source := &genDataSource{shows: []gendata.VPS{vps, vps}}
	wantContainers := "{\n  \"z.loc.test\" = {\n    node = {\n      domain = \"test\";\n      fqdn = \"n.test\";\n      id = 9007199254740993;\n      location = \"loc\";\n      name = \"node\";\n    };\n  };\n}\n"
	lists := 0
	source.list = func() ([]gendata.Network, error) {
		lists++
		body, err := os.ReadFile(filepath.Join(root, "data/vpsadmin/containers.nix"))
		if err != nil || string(body) != wantContainers {
			t.Fatalf("network requested before containers replacement: body=%q err=%v", body, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "data/vpsadmin/containers.nix.new")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("containers .new remains at network call: %v", err)
		}
		return []gendata.Network{
			{IPVersion: gdField[any](float64(6)), Address: gdField("2001:db8:2::"), Prefix: gdField(json.Number("64"))},
			{IPVersion: gdField[any](json.Number("4")), Address: gdField("192.0.2.0"), Prefix: gdField(json.Number("24"))},
			{IPVersion: gdField[any](float64(6)), Address: gdField("2001:db8:1::")},
		}, nil
	}
	machine := gendata.MachineInput{Name: "diagnostic", Container: gdField(gendata.ContainerRef{ID: gdField(json.Number("31"))}), HostName: gdField("z"), HostLocation: gdField("loc"), HostDomain: gdField("test")}
	g := gendata.Generator{Root: root, Source: source, Literal: genDataNixLiteral}
	if err := g.All(context.Background(), []gendata.MachineInput{machine, machine}); err != nil {
		t.Fatal(err)
	}
	if len(source.showIDs) != 2 || !source.showIDs[0].Present || *source.showIDs[0].Value != "31" || *source.showIDs[1].Value != "31" || lists != 1 {
		t.Fatalf("requests: shows=%#v lists=%d", source.showIDs, lists)
	}
	wantNetworks := "{\n  \"ipv4\" = [\n    {\n      address = \"192.0.2.0\";\n      prefix = 24;\n    }\n  ];\n  \"ipv6\" = [\n    {\n      address = \"2001:db8:2::\";\n      prefix = 64;\n    }\n    {\n      address = \"2001:db8:1::\";\n      prefix = null;\n    }\n  ];\n}\n"
	for relative, want := range map[string]string{"data/vpsadmin/containers.nix": wantContainers, "data/vpsadmin/networks/containers.nix": wantNetworks} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || string(body) != want {
			t.Fatalf("%s: body=%q err=%v, want=%q", relative, body, err, want)
		}
		if _, err := os.Lstat(filepath.Join(root, relative) + ".new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected .new after success for %s: %v", relative, err)
		}
	}
}

func TestGenDataGeneratorEmptyFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data/vpsadmin/networks"), 0755); err != nil {
		t.Fatal(err)
	}
	source := &genDataSource{list: func() ([]gendata.Network, error) { return nil, nil }}
	g := gendata.Generator{Root: root, Source: source, Literal: genDataNixLiteral}
	if err := g.All(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for relative, want := range map[string]string{
		"data/vpsadmin/containers.nix":          "{\n}\n",
		"data/vpsadmin/networks/containers.nix": "{\n  \"ipv4\" = [\n  ];\n  \"ipv6\" = [\n  ];\n}\n",
	} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || string(body) != want {
			t.Fatalf("%s: body=%q err=%v, want=%q", relative, body, err, want)
		}
	}
}
