package main

import (
	"encoding/json"
	"github.com/vpsfreecz/confctl/compat/harness"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDispatcherHelper(t *testing.T) {
	if os.Getenv("COMPAT_DISPATCH_HELPER") != "1" {
		return
	}
	os.Args = []string{"ssh", "-l", "root", "host", "uname -r"}
	os.Exit(run())
}
func TestExecutableSharesSemanticOccurrenceAcrossRules(t *testing.T) {
	root := t.TempDir()
	plan, trace := filepath.Join(root, "plan"), filepath.Join(root, "trace")
	if e := os.Mkdir(trace, 0700); e != nil {
		t.Fatal(e)
	}
	c := harness.Case{Rules: []harness.Rule{{Tool: "ssh", Host: "host", Contains: "uname -r", Occurrence: 1, Stdout: "first\n"}, {Tool: "ssh", Host: "host", Contains: "uname -r", Occurrence: 2, Stdout: "second\n"}}}
	if e := harness.PreparePlan(plan, c); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"first\n", "second\n"} {
		cmd := exec.Command(os.Args[0], "-test.run=TestDispatcherHelper")
		cmd.Env = append(os.Environ(), "COMPAT_DISPATCH_HELPER=1", "COMPAT_PLAN="+plan, "COMPAT_TRACE="+trace, "COMPAT_ROOT="+root)
		b, e := cmd.CombinedOutput()
		if e != nil || string(b) != want {
			t.Fatalf("%q want %q: %v", b, want, e)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(trace, "*.json"))
	seen := map[int]int{}
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		var e harness.Event
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		seen[e.Occurrence] = e.Rule
	}
	if len(paths) != 2 || seen[1] != 0 || seen[2] != 1 {
		t.Fatal("one occurrence per invocation, not per candidate", seen)
	}
}
