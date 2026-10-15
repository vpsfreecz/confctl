package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Only the test executable takes this route, allowing real Start/Wait and FD
// inheritance checks without a shell, real nix-env or any production profile.
func TestMain(m *testing.M) {
	if os.Getenv("CARRIER_ENV_TEST_CHILD") == "1" {
		os.Exit(fakeChild())
	}
	os.Exit(m.Run())
}

type childRecord struct {
	Argv  []string
	CWD   string
	Value string
	Input []byte
}

func fakeChild() int {
	name := filepath.Base(os.Args[0])
	first := name == "nix-env"
	key := "SECOND"
	if first {
		key = "FIRST"
	}
	var input []byte
	if os.Getenv("CARRIER_ENV_TEST_"+key+"_INPUT") != "skip" {
		var err error
		input, err = io.ReadAll(os.Stdin)
		if err != nil {
			return 90
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 91
	}
	record := childRecord{Argv: os.Args, CWD: cwd, Value: os.Getenv("CARRIER_ENV_TEST_VALUE"), Input: input}
	b, err := json.Marshal(record)
	if err != nil || os.WriteFile(filepath.Join(cwd, key+".json"), b, 0600) != nil {
		return 92
	}
	if first {
		if len(os.Args) != 5 || os.Args[1] != "-p" {
			return 93
		}
		if err = os.WriteFile(os.Args[2], []byte("profile effect\n"), 0600); err != nil {
			return 94
		}
	} else {
		if len(os.Args) != 1 {
			return 95
		}
		if err = os.WriteFile(filepath.Join(cwd, "callback-effect"), []byte("callback effect\n"), 0600); err != nil {
			return 96
		}
	}
	_, _ = os.Stdout.Write([]byte(name + ":stdout\x00\xff\n"))
	_, _ = os.Stderr.Write([]byte(name + ":stderr\n"))
	code, _ := strconv.Atoi(os.Getenv("CARRIER_ENV_TEST_" + key + "_EXIT"))
	return code
}

func TestCarrierParserAndWorkflow(t *testing.T) {
	// These are original57 condition nominations and selected candidate diagnostic
	// contracts, not recorded Ruby outcomes or historical fixture replacements.
	const profile = "profiles/confctl-rack:node"
	const generation = "generations/netboot-7"
	base := []string{"-p", profile, "--set", generation}
	callback := "/private/callback"
	usage := "Usage: /actual/carrier-env <options>\n  -p, --profile PROFILE: Profile path\n  --set GENERATION: Set profile to generation\n  --delete-generations GENERATIONS: Delete generations\n"
	cases := []struct {
		name                      string
		args                      []string
		ordered                   bool
		profile, generation, flag string
		stdout, stderr            string
		status, fail              int
	}{
		{name: "CE01-set", args: base, profile: profile, generation: generation, flag: "--set"},
		{name: "CE02-delete", args: []string{"-p", profile, "--delete-generations", "6"}, profile: profile, generation: "6", flag: "--delete-generations"},
		{name: "CE03-delete-opaque", args: []string{"-p", profile, "--delete-generations", "5 6"}, profile: profile, generation: "5 6", flag: "--delete-generations"},
		{name: "CE04-last-action-delete", args: []string{"--set", "a", "-p", profile, "--delete-generations", "b"}, profile: profile, generation: "b", flag: "--delete-generations"},
		{name: "CE05-last-action-set", args: []string{"--delete-generations", "a", "--set", "b", "-p", profile}, profile: profile, generation: "b", flag: "--set"},
		{name: "CE06-no-action", args: []string{"-p", profile}, status: 1, stderr: "No action specified\n"},
		{name: "CE07-no-profile", args: []string{"--set", generation}, status: 1, stderr: "Profile not set\n"},
		{name: "CE08-leftover", args: append(append([]string{}, base...), "operand"), status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE09-help", args: []string{"--help"}, stdout: help("/actual/carrier-env")},
		{name: "CE10-unknown", args: []string{"--unknown"}, status: 1, stderr: "invalid option: --unknown\n"},
		{name: "CE11-first-nonzero", args: base, profile: profile, generation: generation, flag: "--set", fail: 1, status: 1},
		{name: "CE12-first-missing", args: base, profile: profile, generation: generation, flag: "--set", fail: 1, status: 1},
		{name: "CE13-second-nonzero", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE14-last-command-success", args: base, profile: profile, generation: generation, flag: "--set"},
		{name: "CE15-profile-spaces", args: []string{"-p", "profile with spaces", "--set", generation}, profile: "profile with spaces", generation: generation, flag: "--set"},
		{name: "CE16-repeated-profile", args: []string{"-p", "old", "-p", profile, "--set", generation}, profile: profile, generation: generation, flag: "--set"},
		{name: "CE17-empty-profile", args: []string{"-p", "", "--set", generation}, generation: generation, flag: "--set"},
		{name: "CE18-empty-generation", args: []string{"-p", profile, "--set", ""}, profile: profile, flag: "--set"},
		{name: "CE19-both-empty", args: []string{"-p", "", "--set", ""}, flag: "--set"},
		{name: "CE20-attached-short", args: []string{"-p" + profile, "--set=" + generation}, profile: profile, generation: generation, flag: "--set"},
		{name: "CE21-attached-delete", args: []string{"--profile=" + profile, "--delete-generations=5 6"}, profile: profile, generation: "5 6", flag: "--delete-generations"},
		{name: "CE22-opaque-generation", args: []string{"-p", profile, "--set", "'$x;\\opaque\" value"}, profile: profile, generation: "'$x;\\opaque\" value", flag: "--set"},
		{name: "CE23-opaque-profile", args: []string{"-p", "'$x;\\opaque\" value", "--set", generation}, profile: "'$x;\\opaque\" value", generation: generation, flag: "--set"},
		{name: "CE24-leading-hyphen-generation", args: []string{"-p", profile, "--set", "--opaque"}, profile: profile, generation: "--opaque", flag: "--set"},
		{name: "CE25-leading-hyphen-profile", args: []string{"-p", "--opaque", "--set", generation}, profile: "--opaque", generation: generation, flag: "--set"},
		{name: "CE26-missing-set", args: []string{"--set"}, status: 1, stderr: "missing argument: --set\n"},
		{name: "CE27-missing-delete", args: []string{"--delete-generations"}, status: 1, stderr: "missing argument: --delete-generations\n"},
		{name: "CE28-missing-profile", args: []string{"--profile"}, status: 1, stderr: "missing argument: --profile\n"},
		{name: "CE29-both-absent", status: 1, stderr: "No action specified\n"},
		{name: "CE30-permute-operand-first", args: append([]string{"operand"}, base...), status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE31-order-empty-present", args: append([]string{"operand"}, base...), ordered: true, status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE32-order-one-present", args: append([]string{"operand"}, base...), ordered: true, status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE33-permute-late-help", args: []string{"operand", "--help"}, stdout: help("/actual/carrier-env")},
		{name: "CE34-order-late-help", args: []string{"operand", "--help"}, ordered: true, status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE35-terminator-before", args: append([]string{"--"}, base...), status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE36-empty-terminator", args: append(append([]string{}, base...), "--"), profile: profile, generation: generation, flag: "--set"},
		{name: "CE37-terminator-operand", args: []string{"--", "operand"}, status: 1, stdout: usage, stderr: "Too many arguments\n"},
		{name: "CE38-abbreviation", args: []string{"--pro", profile, "--s", generation}, profile: profile, generation: generation, flag: "--set"},
		{name: "CE39-underscore", args: []string{"--profile", profile, "--delete_generations", "6"}, profile: profile, generation: "6", flag: "--delete-generations"},
		{name: "CE40-ambiguous", args: []string{"--*-completion"}, status: 1, stderr: "ambiguous option: --*-completion\nDid you mean?  *-completion-zsh\n"},
		{name: "CE41-short-help", args: []string{"-h"}, stdout: help("/actual/carrier-env")},
		{name: "CE42-version", args: []string{"--version"}, status: 1, stderr: "carrier-env: version unknown\n"},
		{name: "CE43-bash", args: []string{"--*-completion-bash", "--"}, stdout: "--profile\n--set\n--delete-generations\n"},
		{name: "CE44-zsh", args: []string{"--*-completion-zsh=carrier-env:_carrier_env"}, stdout: zshCompletion("carrier-env:_carrier_env")},
		{name: "CE45-completion-missing", args: []string{"--*-completion-bash"}, status: 1, stderr: "missing argument: --*-completion-bash\n"},
		{name: "CE46-stdin-first-then-eof", args: base, profile: profile, generation: generation, flag: "--set"},
		{name: "CE47-stdin-no-read-first", args: base, profile: profile, generation: generation, flag: "--set"},
		{name: "CE48-first-term-injected", args: base, profile: profile, generation: generation, flag: "--set", fail: 1, status: 1},
		{name: "CE49-first-int-injected", args: base, profile: profile, generation: generation, flag: "--set", fail: 1, status: 1},
		{name: "CE50-first-kill-injected", args: base, profile: profile, generation: generation, flag: "--set", fail: 1, status: 1},
		{name: "CE51-second-term-injected", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE52-second-int-injected", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE53-second-kill-injected", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE54-second-missing", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE55-second-nonexec", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE56-recorded-second-failure", args: base, profile: profile, generation: generation, flag: "--set", fail: 2, status: 1},
		{name: "CE57-empty-delete", args: []string{"-p", profile, "--delete-generations", ""}, profile: profile, flag: "--delete-generations"},
	}
	if len(cases) != 57 {
		t.Fatal("condition nominations must remain complete")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errout bytes.Buffer
			calls := [][]string{}
			result := run("/actual/carrier-env", c.args, c.ordered, callback, func(program string, args []string) error {
				calls = append(calls, append([]string{program}, args...))
				if c.fail == len(calls) {
					return errors.New("injected failure; not a native signal outcome")
				}
				return nil
			}, &out, &errout)
			wantErr := c.stderr
			expected := [][]string{}
			if c.flag != "" {
				expected = append(expected, []string{"nix-env", "-p", c.profile, c.flag, c.generation})
				if c.fail != 1 {
					expected = append(expected, []string{callback})
				}
			}
			if c.fail != 0 {
				wantErr = strings.Join(expected[len(expected)-1], " ") + " failed\n"
			}
			if result != c.status || out.String() != c.stdout || errout.String() != wantErr || !reflect.DeepEqual(calls, expected) {
				t.Fatalf("status=%d stdout=%q stderr=%q calls=%q; expected%d %q %q %q", result, out.String(), errout.String(), calls, c.status, c.stdout, wantErr, expected)
			}
		})
	}
}

func TestCarrierPresentationLiteral(t *testing.T) {
	// Explicit candidate presentation policy; these bytes are not Ruby captures.
	for _, argv0 := range []string{"/one/carrier-env", "/different parent/helper.rb", "relative/name.multi.ext"} {
		_, got := parse(argv0, []string{"--help"}, false)
		expected := "Usage: " + argv0 + " <options>\n    -p, --profile PROFILE            Profile path\n        --set GENERATION             Set profile to generation\n        --delete-generations GENERATIONS\n                                     Delete generations\n"
		if got == nil || got.stdout != expected || got.status != 0 || got.stderr != "" {
			t.Fatal("literal help")
		}
		_, extra := parse(argv0, []string{"operand"}, false)
		if extra.stdout == got.stdout || extra.stderr != "Too many arguments\n" || extra.status != 1 {
			t.Fatal("distinct corrective usage")
		}
	}
	if programName("/dir/carrier-env.rb") != "carrier-env" || programName("/dir/.hidden") != ".hidden" || programName("/dir/name.multi.ext") != "name.multi" {
		t.Fatal("basename/suffix")
	}
	expectedZsh := "#compdef carrier-env:_carrier_env\n\ntypeset -A opt_args\nlocal context state line\n\n_arguments -s -S \\\n  \"-p[Profile path]\" \\\n  \"--profile[Profile path]\" \\\n  \"--set[Set profile to generation]\" \\\n  \"--delete-generations[Delete generations]\" \\\n  '*:file:_files' && return 0\n"
	if zshCompletion("carrier-env:_carrier_env") != expectedZsh {
		t.Fatal("literal supplied Zsh name")
	}
	for _, arg := range []string{"--version", "--version=PKG", "--version=", "--version=opaque ; '$value"} {
		calls := 0
		var out, errout bytes.Buffer
		status := run("/actual/name.rb", []string{arg}, false, "/callback", func(string, []string) error { calls++; return nil }, &out, &errout)
		if status != 1 || calls != 0 || out.Len() != 0 || errout.String() != "name: version unknown\n" {
			t.Fatal("selected unsupported package-query policy")
		}
	}
}

func TestCarrierCompletionPrefixes(t *testing.T) {
	// Source-backed candidate contracts, not additional Ruby observations.
	for _, c := range []struct{ word, want string }{
		{"--", "--profile\n--set\n--delete-generations\n"},
		{"-", "--profile\n-p\n--set\n--delete-generations\n"},
		{"-p", "-p\n"},
		{"--PROF", "--profile\n"},
		{"--d-g", "--delete-generations\n"},
		{"--D-G", "--delete-generations\n"},
		{"-P", ""},
		{"--profile\xff", ""},
		{"--profile=value", "--profile\n"},
		{"", ""},
		{"ordinary", ""},
	} {
		if got := bashCompletion(c.word); got != c.want {
			t.Fatalf("word%q got%q want%q", c.word, got, c.want)
		}
	}
}

func TestCarrierWordSegmentsAndZshNames(t *testing.T) {
	// Static provenance: pinned optparse.rb Completion.regexp and complete
	// expand word segments/fold long case, with exact search before expansion.
	// These source-derived expectations are not additional Ruby observations.
	_, malformed := parse("/fixture/carrier-env", []string{"--profile\xff"}, false)
	if malformed == nil || malformed.status != 1 || malformed.stderr != "invalid option: --profile\xff\n" {
		t.Fatalf("ordinary malformed option diagnostic %+v", malformed)
	}
	for _, args := range [][]string{
		{"--PROFILE", "opaque profile", "--set", "opaque generation"},
		{"-p", "opaque profile", "--d-g", "6"},
		{"--P", "opaque profile", "--D_G=5 6"},
	} {
		var out, stderr bytes.Buffer
		var calls [][]string
		status := run("/fixture/carrier-env", args, false, "/callback", func(program string, argv []string) error {
			calls = append(calls, append([]string{program}, argv...))
			return nil
		}, &out, &stderr)
		flag, value := "--delete-generations", "6"
		if args[0] == "--PROFILE" {
			flag, value = "--set", "opaque generation"
		}
		if args[0] == "--P" {
			value = "5 6"
		}
		want := [][]string{{"nix-env", "-p", "opaque profile", flag, value}, {"/callback"}}
		if status != 0 || !reflect.DeepEqual(calls, want) || out.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("args=%q status=%d calls=%q stderr=%q", args, status, calls, stderr.String())
		}
	}
	for _, name := range longOptions {
		if got := longMatches(name); !reflect.DeepEqual(got, []string{name}) {
			t.Fatalf("exact match %q=%q", name, got)
		}
	}
	if got := longMatches("*-completion"); len(got) != 2 {
		t.Fatalf("ambiguous completion %q", got)
	}
	for _, tc := range []struct {
		args []string
		name string
	}{
		{[]string{"--*-completion-zsh"}, ""},
		{[]string{"--*-completion-zsh="}, ""},
		{[]string{"--*-completion-zsh=carrier-env:_carrier_env"}, "carrier-env:_carrier_env"},
	} {
		_, p := parse("/actual/carrier-env", tc.args, false)
		if p == nil || p.status != 0 || p.stderr != "" || p.stdout != zshCompletion(tc.name) || !strings.HasPrefix(p.stdout, "#compdef "+tc.name+"\n") {
			t.Fatalf("Zsh args=%q presentation=%+v", tc.args, p)
		}
	}
	_, standalone := parse("/actual/carrier-env", []string{"--"}, false)
	if standalone == nil || standalone.status != 1 || standalone.stderr != "No action specified\n" {
		t.Fatalf("standalone terminator %+v", standalone)
	}
}

func TestCarrierInheritedActions(t *testing.T) {
	for _, tc := range []struct {
		name                                                  string
		first, second                                         string
		skipFirst, missingFirst, missingSecond, nonexecSecond bool
	}{
		{name: "success-and-input-first"},
		{name: "input-no-read-first", skipFirst: true},
		{name: "first-fails-after-effect", first: "7"},
		{name: "second-fails-after-effects", second: "23"},
		{name: "missing-first", missingFirst: true},
		{name: "missing-callback-after-first", missingSecond: true},
		{name: "nonexec-callback-after-first", nonexecSecond: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "profiles"), 0700); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.missingFirst {
				if err = os.Symlink(executable, filepath.Join(bin, "nix-env")); err != nil {
					t.Fatal(err)
				}
			}
			callback := filepath.Join(bin, "on-change")
			if tc.nonexecSecond {
				err = os.WriteFile(callback, []byte("nonexecutable\n"), 0600)
			} else if !tc.missingSecond {
				err = os.Symlink(executable, callback)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("CARRIER_ENV_TEST_CHILD", "1")
			t.Setenv("CARRIER_ENV_TEST_VALUE", "literal inherited value")
			t.Setenv("CARRIER_ENV_TEST_FIRST_EXIT", tc.first)
			t.Setenv("CARRIER_ENV_TEST_SECOND_EXIT", tc.second)
			t.Setenv("CARRIER_ENV_TEST_FIRST_INPUT", "read")
			if tc.skipFirst {
				t.Setenv("CARRIER_ENV_TEST_FIRST_INPUT", "skip")
			}
			t.Setenv("CARRIER_ENV_TEST_SECOND_INPUT", "read")
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Chdir(root); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Chdir(cwd); err != nil {
					t.Error(err)
				}
			}()
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("literal stdin\x00\xff\n")
			if _, err = writer.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err = writer.Close(); err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			out, err := os.Create(filepath.Join(root, "stdout"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			errout, err := os.Create(filepath.Join(root, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer errout.Close()
			oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
			os.Stdin, os.Stdout, os.Stderr = input, out, errout
			defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()
			status := run("actual helper", []string{"-p", "profiles/fake", "--set", "opaque ; '$value"}, false, callback, runInherited, out, errout)
			failed := tc.first != "" || tc.second != "" || tc.missingFirst || tc.missingSecond || tc.nonexecSecond
			if (status == 1) != failed {
				t.Fatalf("status%d failed%v", status, failed)
			}
			read := func(name string) []byte {
				b, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			var first, second childRecord
			if !tc.missingFirst {
				if err = json.Unmarshal(read("FIRST.json"), &first); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(first.Argv, []string{"nix-env", "-p", "profiles/fake", "--set", "opaque ; '$value"}) || first.CWD != root || first.Value != "literal inherited value" {
					t.Fatal("literal argv/env/CWD inheritance")
				}
				if tc.skipFirst {
					if first.Input != nil {
						t.Fatal("first consumed input")
					}
				} else if !bytes.Equal(first.Input, payload) {
					t.Fatal("first whole stdin")
				}
				if !bytes.Equal(read("profiles/fake"), []byte("profile effect\n")) {
					t.Fatal("first effect")
				}
			}
			secondRan := !tc.missingFirst && tc.first == "" && !tc.missingSecond && !tc.nonexecSecond
			if secondRan {
				if err = json.Unmarshal(read("SECOND.json"), &second); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(second.Argv, []string{callback}) || second.CWD != root || second.Value != first.Value {
					t.Fatal("callback inherited argv/env/CWD")
				}
				if tc.skipFirst {
					if !bytes.Equal(second.Input, payload) {
						t.Fatal("second whole stdin")
					}
				} else if len(second.Input) != 0 {
					t.Fatal("second not EOF")
				}
				if !bytes.Equal(read("callback-effect"), []byte("callback effect\n")) {
					t.Fatal("second effect")
				}
			} else if _, err = os.Stat(filepath.Join(root, "SECOND.json")); !os.IsNotExist(err) {
				t.Fatal("unexpected second call")
			}
			wantOut, wantErr := "", ""
			if !tc.missingFirst {
				wantOut = "nix-env:stdout\x00\xff\n"
				wantErr = "nix-env:stderr\n"
			}
			if secondRan {
				wantOut += "on-change:stdout\x00\xff\n"
				wantErr += "on-change:stderr\n"
			}
			if failed {
				if tc.missingFirst || tc.first != "" {
					wantErr += "nix-env -p profiles/fake --set opaque ; '$value failed\n"
				} else {
					wantErr += callback + " failed\n"
				}
			}
			if !bytes.Equal(read("stdout"), []byte(wantOut)) || !bytes.Equal(read("stderr"), []byte(wantErr)) {
				t.Fatal("separate byte-exact streams and appended actual failed command")
			}
		})
	}
}
