package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/core"
	"github.com/vpsfreecz/confctl/internal/docgen"
)

func TestCommandsAndValidationNeverLoadNix(t *testing.T) {
	deps := dependencies{
		loadOptions: func(context.Context, string, string) ([]docgen.ModuleOption, error) {
			t.Fatal("moduleOptions loaded")
			return nil, nil
		},
		renderValue: func(any) (string, error) { t.Fatal("Nix value formatted"); return "", nil },
	}
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"--version", "v", "--date", "2026-10-09"}, 0},
		{[]string{"--help"}, 0}, {[]string{"--unknown"}, 2}, {[]string{"extra"}, 2},
		{[]string{"--kind", "unknown"}, 2}, {[]string{"--module-options", "fixture.json"}, 2},
		{[]string{"--kind", "options"}, 1},
		{[]string{"--kind", "options", "--version", "v", "--date", "bad"}, 1},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), test.args, &stdout, &stderr, deps)
		if code != test.code {
			t.Errorf("%v: code=%d, stderr=%s", test.args, code, stderr.String())
		}
		if len(test.args) == 4 && test.args[0] == "--version" && !strings.Contains(stdout.String(), "confctl inputs") {
			t.Fatal("builtin registry not consumed")
		}
	}
}

func TestSharedFormatterErrorsNameOptionWithoutPartialStdout(t *testing.T) {
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"NUL value", "default", "bad\x00value"},
		{"NUL nested key", "default", []any{map[string]any{"bad\x00key": "value"}}},
		{"invalid UTF-8 value", "default", string([]byte{0xff})},
		{"invalid UTF-8 nested key", "example", []any{map[string]any{string([]byte{0xff}): "value"}}},
		{"malformed number", "default", json.Number("1e")},
		{"empty number", "example", json.Number("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := docgen.ModuleOption{"name": "confctl.zFailure", "default": true, "declarations": []string{}}
			bad[test.field] = test.value
			var seen []any
			deps := dependencies{
				loadOptions: func(context.Context, string, string) ([]docgen.ModuleOption, error) {
					// Deliberately unsorted: the earlier valid option renders first.
					return []docgen.ModuleOption{bad, {"name": "confctl.aValid", "default": false, "declarations": []string{}}}, nil
				},
				renderValue: func(value any) (string, error) {
					seen = append(seen, value)
					return core.DocumentationNixLiteral(value)
				},
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"--kind", "options", "--version", "v", "--date", "2026-10-09"}, &stdout, &stderr, deps)
			if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "confctl-docs: confctl.zFailure: ") {
				t.Fatalf("formatter error: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			want := []any{false, test.value}
			if test.field == "example" {
				want = []any{false, true, test.value}
			}
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("render order = %#v, want %#v", seen, want)
			}
		})
	}
}

func TestSharedNULFailurePrecedesSiblingOpenIncludingAliases(t *testing.T) {
	for _, value := range []struct {
		name string
		json string
	}{
		{"value", `[{"name":"confctl.nul","default":"bad\u0000value","declarations":[]}]`},
		{"nested key", `[{"name":"confctl.nul","default":[{"bad\u0000key":"value"}],"declarations":[]}]`},
	} {
		for _, sibling := range []string{"absent", "regular", "symlink", "hardlink"} {
			t.Run(value.name+"/"+sibling, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "options.md")
				if err := os.WriteFile(path, []byte("prior destination\n"), 0640); err != nil {
					t.Fatal(err)
				}
				var err error
				switch sibling {
				case "regular":
					err = os.WriteFile(path+".new", []byte("prior sibling\n"), 0600)
				case "symlink":
					err = os.Symlink(path, path+".new")
				case "hardlink":
					err = os.Link(path, path+".new")
				}
				if err != nil {
					t.Fatal(err)
				}
				// Assert file identity, mode, mtime, bytes and symlink target after
				// the real formatter error. Neither alias may have been opened.
				assertDestination := unchangedFileAssertion(t, path)
				var assertSibling func()
				if sibling != "absent" {
					assertSibling = unchangedFileAssertion(t, path+".new")
				}
				deps := dependencies{
					loadOptions: func(context.Context, string, string) ([]docgen.ModuleOption, error) {
						return docgen.DecodeOptions(strings.NewReader(value.json))
					},
					renderValue: core.DocumentationNixLiteral,
				}
				var stdout, stderr bytes.Buffer
				code := run(context.Background(), []string{"--kind", "options", "--version", "v", "--date", "2026-10-09", "--output", path}, &stdout, &stderr, deps)
				if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "confctl.nul:") || !strings.Contains(stderr.String(), "NUL") {
					t.Fatalf("NUL failure: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				assertDestination()
				if assertSibling != nil {
					assertSibling()
				} else if _, err := os.Lstat(path + ".new"); !os.IsNotExist(err) {
					t.Fatalf("render failure created sibling: %v", err)
				}
			})
		}
	}
}

func unchangedFileAssertion(t *testing.T, path string) func() {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	link := ""
	if before.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		t.Helper()
		after, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("render failure changed file identity/metadata: %s", path)
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, content) {
			t.Fatalf("render failure changed file bytes: %s: %q, %v", path, got, err)
		}
		if link != "" {
			if got, err := os.Readlink(path); err != nil || got != link {
				t.Fatalf("render failure changed symlink: %s: %q, %v", path, got, err)
			}
		}
	}
}

func TestEntryFileReplacementAndLoaderFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "options.md")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	var seenRoot, seenFile string
	sentinel := errors.New("fixture loader failure")
	fail := false
	deps := dependencies{
		loadOptions: func(ctx context.Context, root, file string) ([]docgen.ModuleOption, error) {
			seenRoot, seenFile = root, file
			if fail {
				return nil, sentinel
			}
			return []docgen.ModuleOption{{"name": "confctl.fixture", "default": nil, "declarations": []string{}}}, nil
		},
		renderValue: func(any) (string, error) { return "null", nil },
	}
	args := []string{"--kind", "options", "--version", "v", "--date", "2026-10-09", "--output", path, "--source-root", "/fixture/confctl", "--module-options", "literal$path.json"}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, deps); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("file generation: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if seenRoot != "/fixture/confctl" || seenFile != "literal$path.json" {
		t.Fatalf("loader arguments changed: %q, %q", seenRoot, seenFile)
	}
	before, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(before), "*Default:*\n\n    ```nix\n    null\n    ```\n") {
		t.Fatalf("missing generated options: %s, %v", before, err)
	}
	fail = true
	if code := run(context.Background(), args, &stdout, &stderr, deps); code != 1 || !strings.Contains(stderr.String(), sentinel.Error()) {
		t.Fatalf("loader failure: code=%d, stderr=%s", code, stderr.String())
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("loader failure changed destination")
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Fatal("loader failure opened sibling")
	}
}

func TestModuleOptionsAdapterLiteralArgvAndFileRoute(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "same context")
	root := "/fixture/confctl $(literal)"
	called := 0
	evaluate := func(gotContext context.Context, program string, args []string, stderr io.Writer) ([]byte, error) {
		called++
		if gotContext != ctx || program != "nix" || !reflect.DeepEqual(args, []string{"--extra-experimental-features", "nix-command flakes", "eval", "--json", root + "#moduleOptions"}) {
			t.Fatalf("evaluation argv/context changed: %s %#v", program, args)
		}
		return []byte(`[{"name":"confctl.number","default":9007199254740993}]`), nil
	}
	loader := optionsLoader(io.Discard, evaluate)
	options, err := loader(ctx, root, "")
	if err != nil || len(options) != 1 || called != 1 {
		t.Fatalf("adapter result: %#v, %v, calls=%d", options, err, called)
	}
	file := filepath.Join(t.TempDir(), "source$.json")
	if err := os.WriteFile(file, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	options, err = loader(ctx, root, file)
	if err != nil || len(options) != 0 || called != 1 {
		t.Fatal("file route evaluated Nix")
	}
	if _, err := loader(ctx, root, file+"missing"); !os.IsNotExist(err) || called != 1 {
		t.Fatalf("file error caused evaluation fallback: %v", err)
	}
	sentinel := errors.New("fixture nix failure")
	loader = optionsLoader(io.Discard, func(context.Context, string, []string, io.Writer) ([]byte, error) { return nil, sentinel })
	if _, err := loader(ctx, root, ""); !errors.Is(err, sentinel) {
		t.Fatalf("evaluation error lost: %v", err)
	}
	loader = optionsLoader(io.Discard, func(context.Context, string, []string, io.Writer) ([]byte, error) { return []byte("not JSON"), nil })
	if _, err := loader(ctx, root, ""); err == nil {
		t.Fatal("evaluation JSON error hidden")
	}
}
