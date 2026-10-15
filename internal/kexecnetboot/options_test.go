package kexecnetboot

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOriginalHelpAndFiniteCompletion(t *testing.T) {
	want, err := os.ReadFile("testdata/original-help.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--help", "-h"} {
		_, p := Parse("/literal/kexec-netboot", []string{flag}, false)
		if p == nil || p.Status != 0 || p.Stdout != string(want) {
			t.Fatalf("%s: %#v", flag, p)
		}
	}
	_, p := Parse("/actual/invoked-name", []string{"--help"}, false)
	if !strings.HasPrefix(p.Stdout, "Usage: /actual/invoked-name [options]\n") {
		t.Fatal(p)
	}
	_, p = Parse("/actual/prog.rb", []string{"--version"}, false)
	if p.Status != 1 || p.Stderr != "prog: version unknown\n" || p.Stdout != "" {
		t.Fatal(p)
	}
	_, p = Parse("name", []string{"--*-completion-bash", "-"}, false)
	wantWords := "--server-url\n-s\n--machine\n-m\n--generation\n-g\n--variant\n-v\n--interactive\n-i\n--append\n-a\n--exec\n-e\n--unload\n-u\n--no-sync\n-y\n"
	if p.Stdout != wantWords {
		t.Fatal(p.Stdout)
	}
	_, p = Parse("name", []string{"--*-completion-bash", "--s-u"}, false)
	if p.Stdout != "--server-url\n" {
		t.Fatal(p.Stdout)
	}
	for _, tc := range []struct{ arg, want string }{{"--*-completion-zsh", "#compdef \n"}, {"--*-completion-zsh=", "#compdef \n"}, {"--*-completion-zsh=chosen", "#compdef chosen\n"}} {
		_, p := Parse("invoked", []string{tc.arg}, false)
		if !strings.HasPrefix(p.Stdout, tc.want) || !strings.Contains(p.Stdout, `"--generation[Select generation by number or negative offset]"`) {
			t.Fatal(p)
		}
	}
}

func TestParserPermutationPresenceExpansionAndOpaqueValues(t *testing.T) {
	for _, args := range [][]string{{"tail", "-s", "", "--SERVER_U=opaque?x=y", "--m=node", "--g", "-1", "--v", "rescue", "-ei", "--no_sync"}, {"-sfirst", "--server-url=opaque?x=y", "-mnode", "-g-1", "-vrescue", "-eiy"}} {
		o, p := Parse("name", args, false)
		if p != nil || o.Server == nil || *o.Server != "opaque?x=y" || *o.Machine != "node" || *o.Generation != "-1" || *o.Variant != "rescue" || !o.Exec || !o.Interactive {
			t.Fatalf("%v %#v %#v", args, o, p)
		}
	}
	o, p := Parse("name", []string{"tail", "--exec"}, true)
	if p != nil || o.Exec {
		t.Fatal(o, p)
	}
	o, p = Parse("name", []string{"--", "--exec"}, false)
	if p != nil || o.Exec {
		t.Fatal(o, p)
	}
	o, p = Parse("name", []string{"--variant=", "--append= $() ; '\"", "--machine="}, false)
	if p != nil || o.Variant == nil || *o.Variant != "" || o.Machine == nil || *o.Machine != "" || o.Append != " $() ; '\"" {
		t.Fatal(o, p)
	}
	for _, tc := range []struct {
		args []string
		err  string
	}{{[]string{"--unknown"}, "invalid option: --unknown\n"}, {[]string{"--server-url"}, "missing argument: --server-url\n"}, {[]string{"--exec=yes"}, "needless argument: --exec=yes\n"}, {[]string{"--*-completion"}, "ambiguous option: --*-completion\nDid you mean?  *-completion-zsh\n"}} {
		_, p := Parse("name", tc.args, false)
		if p == nil || p.Stderr != tc.err || p.Status != 1 {
			t.Fatal(tc, p)
		}
	}
	_, p = Parse("name", []string{"--ver"}, false)
	if p == nil || p.Status != 1 {
		t.Fatal(p)
	}
	if !reflect.DeepEqual(matchLong("v"), []string{"variant"}) {
		t.Fatal("implicit version shadowed declared variant")
	}
}
