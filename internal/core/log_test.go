package core

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func inputLogHeader(t *testing.T, argv []string) string {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	before, err := cli.BuiltinRegistry().Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "invocation.log"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Log: f, Color: inv.Globals["color"].Value.String}
	if inv.Command.Path[len(inv.Command.Path)-1] == "set" {
		e.LogInputSetCLI(inv)
	} else {
		e.LogInputUpdateCLI(inv)
	}
	if !reflect.DeepEqual(inv, before) {
		t.Fatal("logger changed parsed invocation")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Read the complete regular header before successful command-log unlinking.
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInputLogArgumentsM02ObservedHeader(t *testing.T) {
	// Original-v11 M02/boundary-0.json log prefix, through the final header LF.
	// Only the original repeated GLI address is substituted for comparison.
	const original = `{command: ["inputs", "channel", "set"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"allow_shared" => true,
   "commit" => false,
   "changelog" => true,
   "editor" => true,
   "d" => false,
   "downgrade" => false,
   #<GLI::Command::ParentKey:0x00007d34d215cf50> =>
    {#<GLI::Command::ParentKey:0x00007d34d215cf50> => {}}},
 arguments:
  ["{beta,alpha,beta,gamma,delta}",
   "os",
   "fedcba9876543210fedcba9876543210fedcba98"]}
`
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(original))); got != "0bb8508fd5ce900c0ce360df3f8a6faa0ac7134eea912ebf8de63d1630850cc3" {
		t.Fatal("original M02 header changed", got)
	}
	argv := []string{"inputs", "channel", "set", "--allow-shared", "{beta,alpha,beta,gamma,delta}", "os", "fedcba9876543210fedcba9876543210fedcba98"}
	want := strings.ReplaceAll(original, "0x00007d34d215cf50", "0x0000000000000000")
	if got := inputLogHeader(t, argv); got != want {
		t.Fatalf("full M02 header\ngot: %s\nwant: %s", got, want)
	}
}

func TestTestConnectionLogOriginalHeaderAndEncounter(t *testing.T) {
	// Exact accepted original malformed-filter header, no parent-address field.
	const original = `{command: ["test-connection"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"a" => ["malformed"],
   "attr" => ["malformed"],
   "y" => true,
   "yes" => true,
   "managed" => nil,
   "t" => [],
   "tag" => []},
 arguments: []}
`
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(original))); got != "0fe33511833aca3da22c9dc5edd6ee9ea394bf9b06afe64fdb330d2b9515e640" {
		t.Fatal("accepted original leaf header changed", got)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-a", "malformed", "-y"}, original},
		// Synthetic finite encounter/list/ignored-argument oracle; independent
		// literal value and ordering, not copied Go output or new reference.
		{[]string{"--tag", "node", "-a", "name!=beta", "--attr", "managed=true", "-t", "^absent", "-y", "--no-yes", "one", "ignored", "--help"}, `{command: ["test-connection"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"t" => ["node", "^absent"],
   "tag" => ["node", "^absent"],
   "a" => ["name!=beta", "managed=true"],
   "attr" => ["name!=beta", "managed=true"],
   "y" => false,
   "yes" => false,
   "managed" => nil},
 arguments: ["one", "ignored", "--help"]}
`},
	} {
		inv := connectionInvocation(t, tc.args...)
		before := connectionInvocation(t, tc.args...)
		f, err := os.Create(filepath.Join(t.TempDir(), "header.log"))
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Log: f, Color: "auto"}
		e.LogTestConnectionCLI(inv)
		if !reflect.DeepEqual(inv, before) {
			t.Fatal("logger changed invocation")
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if string(b) != tc.want {
			t.Fatalf("header\ngot %s\nwant %s", b, tc.want)
		}
	}
}

// Literal source-predicted default prefixes: independent of the argument helper.
const inputLogRootSetPrefix = `{command: ["inputs", "set"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"commit" => false,
   "changelog" => true,
   "editor" => true,
   "d" => false,
   "downgrade" => false,
   #<GLI::Command::ParentKey:0x0000000000000000> => {}}`
const inputLogRootUpdatePrefix = `{command: ["inputs", "update"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"commit" => false,
   "changelog" => true,
   "editor" => true,
   "d" => false,
   "downgrade" => false,
   "all" => false,
   #<GLI::Command::ParentKey:0x0000000000000000> => {}}`
const inputLogChannelSetPrefix = `{command: ["inputs", "channel", "set"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"commit" => false,
   "changelog" => true,
   "editor" => true,
   "d" => false,
   "downgrade" => false,
   "allow_shared" => false,
   #<GLI::Command::ParentKey:0x0000000000000000> =>
    {#<GLI::Command::ParentKey:0x0000000000000000> => {}}}`
const inputLogChannelUpdatePrefix = `{command: ["inputs", "channel", "update"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"commit" => false,
   "changelog" => true,
   "editor" => true,
   "d" => false,
   "downgrade" => false,
   #<GLI::Command::ParentKey:0x0000000000000000> =>
    {#<GLI::Command::ParentKey:0x0000000000000000> => {}}}`

func TestInputLogArgumentsShortHeaders(t *testing.T) {
	// Original-v11 M01 short header; the retained update PP tests remain separate.
	const original = `{command: ["inputs", "channel", "set"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"commit" => false,
   "changelog" => true,
   "editor" => true,
   "d" => false,
   "downgrade" => false,
   "allow_shared" => false,
   #<GLI::Command::ParentKey:0x000072c1e227c4e8> =>
    {#<GLI::Command::ParentKey:0x000072c1e227c4e8> => {}}},
 arguments: ["{gh,g}", "os", "topic/x?y=1&z=two words"]}
`
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(original))); got != "347f5e41a6af7bfe35653f12c2ce904b6f4f18d975c317a8f42650b35712c68c" {
		t.Fatal("original M01 header changed", got)
	}
	got := inputLogHeader(t, []string{"inputs", "channel", "set", "{gh,g}", "os", "topic/x?y=1&z=two words"})
	if want := strings.ReplaceAll(original, "0x000072c1e227c4e8", "0x0000000000000000"); got != want {
		t.Fatalf("full M01 header\ngot: %s\nwant: %s", got, want)
	}
	for _, caller := range []struct {
		path   []string
		prefix string
	}{
		{[]string{"inputs", "set"}, inputLogRootSetPrefix},
		{[]string{"inputs", "update"}, inputLogRootUpdatePrefix},
		{[]string{"inputs", "channel", "set"}, inputLogChannelSetPrefix},
		{[]string{"inputs", "channel", "update"}, inputLogChannelUpdatePrefix},
	} {
		for _, tc := range []struct {
			args   []string
			suffix string
		}{
			{nil, ",\n arguments: []}\n"},
			{[]string{"one"}, ",\n arguments: [\"one\"]}\n"},
		} {
			argv := append(append([]string(nil), caller.path...), tc.args...)
			if got := inputLogHeader(t, argv); got != caller.prefix+tc.suffix {
				t.Fatalf("source-predicted short header %q\ngot: %s\nwant: %s", argv, got, caller.prefix+tc.suffix)
			}
		}
	}
}

func TestInputLogArgumentsLayoutBoundaries(t *testing.T) {
	// Source-predicted finite layouts, not additional original observations.
	// A includes brackets/separators; the final hash brace alone breaks at 67/77.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"A66-inline-equality", []string{strings.Repeat("a", 57), "b"}, ",\n arguments: [\"" + strings.Repeat("a", 57) + "\", \"b\"]}\n"},
		{"A67-final-brace-field-overflow", []string{strings.Repeat("a", 58), "b"}, ",\n arguments:\n  [\"" + strings.Repeat("a", 58) + "\", \"b\"]}\n"},
		{"A76-flat-equality", []string{strings.Repeat("a", 67), "b"}, ",\n arguments:\n  [\"" + strings.Repeat("a", 67) + "\", \"b\"]}\n"},
		{"A77-final-brace-array-overflow", []string{strings.Repeat("a", 68), "b"}, ",\n arguments:\n  [\"" + strings.Repeat("a", 68) + "\",\n   \"b\"]}\n"},
		{"single-indivisible-overflow", []string{strings.Repeat("a", 90)}, ",\n arguments:\n  [\"" + strings.Repeat("a", 90) + "\"]}\n"},
		{"long-indivisible-first-order", []string{strings.Repeat("a", 90), "b", "c"}, ",\n arguments:\n  [\"" + strings.Repeat("a", 90) + "\",\n   \"b\",\n   \"c\"]}\n"},
		{"unicode-A66-character-not-byte-width", []string{strings.Repeat("雪", 57), "b"}, ",\n arguments: [\"" + strings.Repeat("雪", 57) + "\", \"b\"]}\n"},
		{"unicode-A77-character-width", []string{strings.Repeat("é", 68), "b"}, ",\n arguments:\n  [\"" + strings.Repeat("é", 68) + "\",\n   \"b\"]}\n"},
		{"quote-backslash-order", []string{"quote\"\\end", "tail"}, ",\n arguments: [\"quote\\\"\\\\end\", \"tail\"]}\n"},
		{"atomic-LF-vs-literal-backslash-n-only", []string{"a\nb", `a\nb`, "\t"}, ",\n arguments: [\"a\\nb\", \"a\\\\nb\", \"\\t\"]}\n"},
	}
	for _, caller := range []struct {
		path   []string
		prefix string
	}{
		{[]string{"inputs", "set"}, inputLogRootSetPrefix},
		{[]string{"inputs", "update"}, inputLogRootUpdatePrefix},
		{[]string{"inputs", "channel", "set"}, inputLogChannelSetPrefix},
		{[]string{"inputs", "channel", "update"}, inputLogChannelUpdatePrefix},
	} {
		for _, tc := range cases {
			t.Run(strings.Join(caller.path, "-")+"/"+tc.name, func(t *testing.T) {
				argv := append(append([]string(nil), caller.path...), tc.args...)
				if got := inputLogHeader(t, argv); got != caller.prefix+tc.want {
					t.Fatalf("source-predicted layout\ngot: %s\nwant: %s", got, caller.prefix+tc.want)
				}
			})
		}
	}
}
