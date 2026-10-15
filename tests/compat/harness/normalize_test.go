package harness

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizerRejectsBehaviorFields(t *testing.T) {
	for _, f := range []string{"exit", "signal", "after", "events.argv", "events.rule", "stderr"} {
		if _, e := Normalize(Observation{}, Normalization{VolatileFields: []string{f}}, "/tmp/case"); e == nil {
			t.Fatalf("accepted erasure of %s", f)
		}
	}
}
func TestRootNormalizationIsExplicit(t *testing.T) {
	o := Observation{Stdout: "/tmp/case/file", Exit: 255, Signal: "terminated", After: map[string]File{"file": {Text: "/tmp/case/not-volatile"}}}
	n, e := Normalize(o, Normalization{RootFields: []string{"stdout"}}, "/tmp/case")
	if e != nil {
		t.Fatal(e)
	}
	if n.Stdout != "${ROOT}/file" || n.Exit != 255 || n.Signal != o.Signal || !reflect.DeepEqual(o.After, n.After) {
		t.Fatalf("behavior changed: %#v", n)
	}
}
func TestPathHashNotNormalized(t *testing.T) {
	o := Observation{Stdout: "/tmp/case confctl-12345678"}
	n, _ := Normalize(o, Normalization{RootFields: []string{"stdout"}}, "/tmp/case")
	if n.Stdout != "${ROOT} confctl-12345678" {
		t.Fatal(n)
	}
}
func TestSameHostOrderRetained(t *testing.T) {
	o := Observation{Events: []Event{{Host: "b", Rule: 2}, {Host: "a", Rule: 0}, {Host: "b", Rule: 3}, {Host: "a", Rule: 1}}}
	n, _ := Normalize(o, Normalization{ConcurrentEvents: true}, "")
	if n.Events[0].Rule != 0 || n.Events[1].Rule != 1 || n.Events[2].Rule != 2 || n.Events[3].Rule != 3 {
		t.Fatal(n)
	}
}
func TestSafeInitialization(t *testing.T) {
	if e := WriteFiles(t.TempDir(), map[string]File{"../escape": {Text: "bad"}}); e == nil {
		t.Fatal("escaped root")
	}
}
func TestSemanticOccurrenceShared(t *testing.T) {
	root := t.TempDir()
	for want := 1; want <= 2; want++ {
		n, e := NextOccurrence(root, "ssh-host-uname")
		if e != nil || n != want {
			t.Fatalf("occurrence %d, %v", n, e)
		}
	}
	n, e := NextOccurrence(root, "ssh-other-uname")
	if e != nil || n != 1 {
		t.Fatalf("other identity %d %v", n, e)
	}
}
func TestCausalBarrierBeforeSorting(t *testing.T) {
	events := []Event{{Tool: "nix", Start: 1, End: 10}, {Tool: "ssh", Start: 9, End: 20}}
	if e := AssertCausal(events, []Constraint{{Before: Selector{Tool: "nix"}, After: Selector{Tool: "ssh"}}}); e == nil {
		t.Fatal("lost global barrier")
	}
}
func TestRunRootFields(t *testing.T) {
	o := Observation{Events: []Event{{Env: map[string]string{"HOME": "/tmp/run/home", "PWD": "/tmp/run/config"}}}}
	n, e := Normalize(o, Normalization{RootFields: []string{"events.env.PWD"}, RunRootFields: []string{"events.env.HOME"}}, "/tmp/run/config")
	if e != nil || n.Events[0].Env["HOME"] != "${RUN_ROOT}/home" || n.Events[0].Env["PWD"] != "${ROOT}" {
		t.Fatal(n, e)
	}
}
func TestSyntheticInventory(t *testing.T) {
	for _, n := range []int{1, 10, 100, 1000} {
		c := Synthetic(n, "status-none", false)
		if len(c.Selected) != n || len(c.Rules) != n*5+3 {
			t.Fatal(n, len(c.Rules))
		}
		if c.Argv[2] != "none" {
			t.Fatal(c.Argv)
		}
	}
}
func TestInputModeIsSemantic(t *testing.T) {
	r := Rule{Tool: "ssh", Host: "host", Contains: "bash --norc", ReadStdin: true}
	if !MatchRequest(r, "ssh", "host", []string{"-l", "root", "host", "bash --norc"}, "/") || MatchRequest(r, "ssh", "host", []string{"host", "uname -r"}, "/") {
		t.Fatal("input mode selected wrong request")
	}
}
func TestLogUUIDBijectionAndNoHashErasure(t *testing.T) {
	o := Observation{After: map[string]File{".confctl/logs/2026-10-09--12-00-00-confctl-ls.log": {Text: "[deadbeef] Running nix deadbeef /nix/store/deadbeef-path\n[0123abcd] Running ssh\n[deadbeef] Finished in 0.123 seconds with exit status 255 (failed)\n#<GLI::Command::ParentKey:0x00007ae36be5c350> => {}\n0x00007ae36be5c350\n"}, "kernels.json": {Text: "deadbeef"}}}
	n, e := Normalize(o, Normalization{LogFields: []string{"command-uuid", "gli-parent-address", "process-duration"}}, "/")
	if e != nil {
		t.Fatal(e)
	}
	s := n.After[".confctl/logs/2026-10-09--12-00-00-confctl-ls.log"].Text
	if strings.Count(s, "${COMMAND_ID:1}") != 2 || strings.Count(s, "${COMMAND_ID:2}") != 1 || !strings.Contains(s, "exit status 255 (failed)") || !strings.Contains(s, "/nix/store/deadbeef-path") || !strings.Contains(s, "\n0x00007ae36be5c350\n") || n.After["kernels.json"].Text != "deadbeef" {
		t.Fatal(s)
	}
}
func TestLogFilenameBijectionPreservesTwoFiles(t *testing.T) {
	o := Observation{After: map[string]File{".confctl/logs/2026-10-09--12-00-00-confctl-ls.log": {}, ".confctl/logs/2026-10-09--12-00-01-confctl-ls.log": {}}}
	n, e := Normalize(o, Normalization{LogFields: []string{"filename-time"}}, "/")
	if e != nil || len(n.After) != 2 {
		t.Fatal(n, e)
	}
}
func TestRequestPlanIsCompactAndPreservesOrder(t *testing.T) {
	c := Synthetic(1000, "status-none", false)
	dir := t.TempDir()
	if e := PreparePlan(dir, c); e != nil {
		t.Fatal(e)
	}
	p, e := LoadPlan(dir, "ssh", "node0999.example", []string{"node0999.example", "cat /proc/uptime"})
	if e != nil || len(p) != 4 {
		t.Fatal(len(p), e)
	}
	p, e = LoadPlan(dir, "nix", "", []string{"eval", ".#confctl.inputsInfo.m999"})
	if e != nil || len(p) != 1 {
		t.Fatal(len(p), e)
	}
}

func TestIndependentSameHostProfileInterleavings(t *testing.T) {
	a := []Event{{Tool: "nix", Start: 1, End: 2}, {Tool: "ssh", Host: "h", Argv: []string{"uptime"}, Start: 3, End: 4, Occurrence: 1}, {Tool: "ssh", Host: "h", Argv: []string{"uptime"}, Start: 5, End: 6, Occurrence: 2}, {Tool: "ssh", Host: "h", Argv: []string{"bash"}, Stdin: "profile-a", Start: 7, End: 8, Occurrence: 1}, {Tool: "ssh", Host: "h", Argv: []string{"bash"}, Stdin: "profile-b", Start: 9, End: 10, Occurrence: 1}, {Tool: "ssh", Host: "h", Argv: []string{"inputs-a"}, Start: 11, End: 12, Occurrence: 1}, {Tool: "ssh", Host: "h", Argv: []string{"inputs-b"}, Start: 13, End: 14, Occurrence: 1}}
	n := Normalization{IndependentSSH: []SSHFlow{{Name: "a", Steps: []Selector{{Tool: "ssh", Host: "h", Contains: "uptime"}, {Tool: "ssh", Host: "h", Contains: "bash", StdinContains: "profile-a"}, {Tool: "ssh", Host: "h", Contains: "inputs-a"}}}, {Name: "b", Steps: []Selector{{Tool: "ssh", Host: "h", Contains: "uptime"}, {Tool: "ssh", Host: "h", Contains: "bash", StdinContains: "profile-b"}, {Tool: "ssh", Host: "h", Contains: "inputs-b"}}}}, VolatileFields: []string{"events.start_ns", "events.end_ns"}}
	b := append([]Event(nil), a...)
	b[3], b[4] = b[4], b[3]
	b[3].Start, b[3].End = 7, 8
	b[4].Start, b[4].End = 9, 10
	x, e := Normalize(Observation{Events: a}, n, "")
	if e != nil {
		t.Fatal(e)
	}
	y, e := Normalize(Observation{Events: b}, n, "")
	if e != nil || Equal(x, y) != nil {
		t.Fatal("valid interleaving", e, Equal(x, y))
	}
	b[3], b[6] = b[6], b[3]
	b[3].Start, b[3].End = 7, 8
	b[6].Start, b[6].End = 13, 14
	if _, e = Normalize(Observation{Events: b}, n, ""); e == nil {
		t.Fatal("profile order swap hidden")
	}
	a[1].Start = 1
	if _, e = Normalize(Observation{Events: a}, n, ""); e == nil {
		t.Fatal("setup barrier hidden")
	}
}
