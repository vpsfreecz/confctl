package core

import (
	"os"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func TestSSHFamilyActualCLICommandAndInteractiveNonzero(t *testing.T) {
	m := connectionMachine("node", "node", "literal%host.invalid", true)
	m.Target.Port = 2222
	m.Attributes["host"].(map[string]any)["port"] = 2222
	for _, tc := range []struct {
		argv            []string
		body, out, args string
	}{
		{[]string{"ssh", "-y", "node", "cat", "quoted value%"}, "printf 'stdout\\n'; printf 'stderr\\n' >&2; exit 0\n", "node:\nstdout\nstderr\n\n", "cat quoted\\ value\\%"},
		{[]string{"ssh", "-y", "node", "cat", "token"}, "printf 'merged failure\\n' >&2; exit 255\n", "node:\n\n\n", "cat token"},
		{[]string{"ssh", "-y", "node"}, "printf 'inherited stdout\\n'; printf 'inherited stderr\\n' >&2; exit 9\n", "inherited stdout\n", ""},
	} {
		t.Run(strings.Join(tc.argv, "/"), func(t *testing.T) {
			r := connectionCLI(t, tc.argv, "", []Machine{m}, tc.body, 0700, true, "private ssh%quote'/config")
			want := []string{"-F", "private ssh%quote'/config", "-l", "root", "-p", "2222", "literal%host.invalid"}
			if len(tc.argv) > 3 {
				want = append(want, tc.args)
			}
			stderr := ""
			if len(tc.argv) == 3 {
				stderr = "inherited stderr\n"
			}
			if r.code != 0 || r.out != tc.out || r.stderr != stderr || r.log != "" || r.trace != "settings-auto\nsettings-seven\nmachines\nssh\n" || r.args != strings.Join(want, "\x00")+"\x00" {
				t.Fatal(r, want)
			}
		})
	}
}

func TestSSHFamilyActualCLILocalRootShellAndCSSH(t *testing.T) {
	local := connectionMachine("local", "l", "localhost", true)
	body := `if [[ "$0" == */getent ]]; then
  printf 'uidzero:x:0:0::/root:%s/tools/cat\n' "$PWD"
  exit 0
fi
printf 'local root shell\n'
exit 9
`
	r := connectionCLI(t, []string{"ssh", "-y", "local"}, "", []Machine{local}, body, 0700, true, "")
	if r.code != 0 || r.out != "local root shell\n" || r.stderr != "" || r.log != "" || r.trace != "settings-auto\nsettings-seven\nmachines\ngetent\ncat\n" || r.args != "passwd\x000\x00\x00" {
		t.Fatal(r)
	}
	remote := connectionMachine("node", "n", "literal host%25.invalid", true)
	remote.Target.Port = 2222
	r = connectionCLI(t, []string{"cssh", "-y", "*"}, "", []Machine{remote, local}, "[[ ${shellHook+x} == x && $shellHook == '' ]] || exit 99\nexit 9\n", 0700, true, "ignored-config")
	want := []string{"-p", "perlPackages.AppClusterSSH", "--command", "cssh -l root literal host%25.invalid localhost"}
	if r.code != 0 || r.out != "" || r.stderr != "" || r.log != "" || r.trace != "settings-auto\nsettings-seven\nmachines\nnix-shell\n" || r.args != strings.Join(want, "\x00")+"\x00" {
		t.Fatal(r)
	}
}

func TestSSHFamilyActualCLIErrorsPromptLogsAndHelp(t *testing.T) {
	ms := []Machine{connectionMachine("one", "o", "one.invalid", true), connectionMachine("two", "t", "two.invalid", true)}
	for _, tc := range []struct {
		argv             []string
		input, errorText string
		code             int
		help             bool
	}{
		{[]string{"ssh", "-y", "*"}, "", "missing command", 64, true},
		{[]string{"ssh", "-i", "", "-f", "", "*", "cat"}, "", "use one of --input-string or --input-file", 64, true},
		{[]string{"ssh", "-a", "invalid", "*", "cat"}, "", "Invalid filter ''", 64, true},
		{[]string{"ssh", "missing", "cat"}, "", "No machines to ssh to", 1, false},
		{[]string{"cssh", "missing"}, "", "No machines to open cssh to", 1, false},
		{[]string{"ssh", "one", "cat"}, "n\n", "Aborted", 1, false},
		{[]string{"cssh", "one"}, "", "end of file reached", 1, false},
		{[]string{"ssh"}, "y\n", "undefined method 'map' for nil", 1, false},
		{[]string{"ssh", "-y", "-p"}, "", "undefined method 'map' for nil", 1, false},
	} {
		t.Run(strings.Join(tc.argv, "/"), func(t *testing.T) {
			r := connectionCLI(t, tc.argv, tc.input, ms, "exit 98\n", 0700, true, "")
			if r.code != tc.code || !strings.Contains(r.stderr, "error: "+tc.errorText+"\n") || !strings.Contains(r.stderr, "Log file: ") || r.log == "" || strings.Contains(r.trace, "ssh\n") || strings.Contains(r.trace, "nix-shell\n") {
				t.Fatal(r)
			}
			if !strings.Contains(r.log, `{command: ["`+tc.argv[0]+`"]`) || !strings.Contains(r.log, "command_options:") {
				t.Fatal(r.log)
			}
			if tc.help {
				help, _ := cli.BuiltinRegistry().Help([]string{tc.argv[0]}, 80)
				if !strings.HasSuffix(r.out, help) {
					t.Fatal(r.out, help)
				}
			}
		})
	}
	for _, name := range []string{"ssh", "cssh"} {
		help, _ := cli.BuiltinRegistry().Help([]string{name}, 80)
		r := connectionCLI(t, []string{name, "--unknown"}, "", ms, "exit 98\n", 0700, false, "")
		if r.code != 64 || r.stderr != "error: Unknown option --unknown\n\n" || r.out != help || r.log != "" || r.trace != "" {
			t.Fatal(r)
		}
		r = connectionCLI(t, []string{name, "--help"}, "", ms, "exit 98\n", 0700, false, "")
		if r.code != 0 || r.out != help || r.stderr != "" || r.trace != "" || strings.Contains(help, "Unavailable") {
			t.Fatal(r)
		}
	}
}

func TestSSHFamilyActualCLIParallelAndFileInput(t *testing.T) {
	ms := []Machine{connectionMachine("one", "one", "one.invalid", true), connectionMachine("two", "two", "two.invalid", true)}
	r := connectionCLI(t, []string{"ssh", "-y", "-p", "-g", "-i", "", "*", "cat", "token"}, "", ms, "IFS= read -r token && exit 98\nprintf 'same\\n'\n", 0700, true, "")
	if r.code != 0 || r.stderr != "" || r.out != "Executing cat token on 2 machines\none, two:\nExit status: 0\nsame\n\n" || r.log != "" {
		t.Fatal(r)
	}
	// A literal missing filename reaches the installed-dependency-compatible
	// temporary-file stdin path; the finite child reads its bytes, no host cat.
	r = connectionCLI(t, []string{"ssh", "-y", "-f", "missing literal%file", "one", "cat"}, "", ms, "input=''; IFS= read -r input; printf '%s\\n' \"$input\"\n", 0700, true, "")
	if r.code != 0 || r.stderr != "" || r.out != "one:\nmissing literal%file\n\n" {
		t.Fatal(r)
	}
	if _, err := os.Stat(r.root + "/missing literal%file"); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
