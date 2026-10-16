package core

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Delivery checks assert CLI contracts and effects. Historical presentation
// captures remain untouched; spacing and complete help/log transcripts are not
// oracles for these tests.
func TestDeliveryEnabledCommandHelpIsSideEffectFree(t *testing.T) {
	paths := []string{"init", "add", "rename", "rediscover", "inputs ls", "inputs channel ls", "inputs update", "inputs channel update", "inputs machine update", "inputs set", "inputs channel set", "inputs machine set", "ls", "test-connection", "ssh", "cssh", "generation ls", "status"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			argv := append(strings.Fields(path), "--help", "--invalid-after-help")
			code, out, stderr, root := actualCLI(t, argv, "")
			if code != 0 || stderr != "" || !strings.Contains(out, "SYNOPSIS") {
				t.Fatalf("help %q: exit=%d stdout=%q stderr=%q", argv, code, out, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, ".confctl")); !os.IsNotExist(err) {
				t.Fatalf("help created state: %v", err)
			}
		})
	}
	for _, argv := range [][]string{{"--bad-root-option"}, {"inputs", "set", "--bad-option"}, {"ssh", "--bad-option"}, {"generation", "ls", "--bad-option"}} {
		code, _, stderr, root := actualCLI(t, argv, "")
		if code != 64 || !strings.Contains(stderr, "error:") {
			t.Fatalf("syntax error %q: %d %q", argv, code, stderr)
		}
		if _, err := os.Stat(filepath.Join(root, ".confctl")); !os.IsNotExist(err) {
			t.Fatalf("parse failure created state: %v", err)
		}
	}
}

func TestDeliveryOptionDefaultsAliasesAndArgumentBoundary(t *testing.T) {
	// These are the automation-facing defaults, not a second copy of the registry.
	cases := []struct {
		argv    []string
		options map[string]any
		args    []string
	}{
		{[]string{"inputs", "update", "--commit", "--no-editor", "--", "-literal"}, map[string]any{"commit": true, "editor": false, "changelog": true}, []string{"-literal"}},
		{[]string{"inputs", "set", "a", "rev"}, map[string]any{"commit": false, "editor": true, "changelog": true}, []string{"a", "rev"}},
		{[]string{"ssh", "-ypg", "node", "echo", "--help"}, map[string]any{"yes": true, "parallel": true, "aggregate": true, "managed": nil}, []string{"node", "echo", "--help"}},
		{[]string{"generation", "ls", "-lr", "node"}, map[string]any{"local": true, "remote": true}, []string{"node"}},
		{[]string{"status"}, map[string]any{"yes": false, "generation": nil}, nil},
	}
	for _, tc := range cases {
		inv, err := cli.BuiltinRegistry().Parse(tc.argv)
		if err != nil || !reflect.DeepEqual(inv.Args, tc.args) {
			t.Fatalf("%q: args=%q err=%v", tc.argv, inv.Args, err)
		}
		for key, want := range tc.options {
			parsed, exists := inv.Options[key]
			if !exists {
				t.Fatalf("%q: missing declared option %s", tc.argv, key)
			}
			value := parsed.Value
			var got any
			switch value.Kind {
			case cli.Boolean:
				got = value.Bool
			case cli.Text:
				got = value.String
			default:
				got = nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%q option %s=%#v want %#v", tc.argv, key, got, want)
			}
		}
	}
}

func TestDeliveryConfigurationChangesStayLocal(t *testing.T) {
	root := t.TempDir()
	for _, argv := range [][]string{{"init"}, {"add", "edge/site"}, {"rename", "edge/site", "edge/renamed"}, {"rediscover"}} {
		code, _, stderr := configurationCLI(t, root, argv, "", "022")
		if code != 0 || stderr != "" {
			t.Fatalf("%q: exit=%d stderr=%q", argv, code, stderr)
		}
	}
	for _, name := range []string{"flake.nix", "cluster/cluster.nix", "cluster/edge/renamed/module.nix", "cluster/edge/renamed/config.nix"} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("missing result %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "cluster/edge/site")); !os.IsNotExist(err) {
		t.Fatalf("rename left old directory: %v", err)
	}
	before := configurationRead(t, root, "cluster/cluster.nix")
	code, _, stderr := configurationCLI(t, root, []string{"add", "edge/renamed"}, "", "022")
	if code != 1 || !strings.Contains(stderr, "error:") || configurationRead(t, root, "cluster/cluster.nix") != before {
		t.Fatalf("duplicate add changed cluster: exit=%d stderr=%q", code, stderr)
	}
}

func TestDeliveryConnectionConfirmationPayloadAndHandledFailure(t *testing.T) {
	ms := []Machine{connectionMachine("one", "one", "one.invalid", true), connectionMachine("two", "two", "two.invalid", true)}
	declined := connectionCLI(t, []string{"ssh", "*", "printf", "literal% value"}, "n\n", ms, "exit 98\n", 0700, true, "")
	if declined.code != 1 || strings.Contains(declined.trace, "ssh\n") || !strings.Contains(declined.stderr, "Aborted") || declined.log == "" {
		t.Fatalf("decline ran a child or lost failure: %+v", declined)
	}
	completed := connectionCLI(t, []string{"test-connection", "-y", "*"}, "", ms, "printf 'probe payload\\n'; exit 255\n", 0700, true, "")
	if completed.code != 0 || strings.Count(completed.trace, "ssh\n") != 2 || !strings.Contains(completed.out, "2 failed") {
		t.Fatalf("handled host failures changed invocation status: %+v", completed)
	}
	command := connectionCLI(t, []string{"ssh", "-y", "-i", "stdin payload", "one", "echo", "literal% value", "--help"}, "", ms, "IFS= read -r input || :; printf 'payload:%s\\n' \"$input\"\n", 0700, true, "")
	if command.code != 0 || !strings.Contains(command.out, "payload:stdin payload") || !strings.Contains(command.args, "--help") || !strings.Contains(command.args, "literal") {
		t.Fatalf("forwarded argv/stdin payload: %+v", command)
	}
}
