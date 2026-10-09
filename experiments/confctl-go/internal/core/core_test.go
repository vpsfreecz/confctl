package core

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRubyPatternContracts(t *testing.T) {
	cases := []struct {
		p, n string
		want bool
	}{{"*", "a/.x", true}, {"*", ".x", false}, {"a/*", "a/.x", true}, {"{a,b}", "b", true}, {"{a}", "a", true}, {"{a,b", "{a,b", false}, {"[x", "x", false}, {"[!x]", "y", true}, {"[^x]", "x", false}, {"\\*", "*", true}, {"alpha*", "alpha/site", true}, {"{a,{b,c}}", "c", true}, {"?.x", "a.x", true}}
	for _, c := range cases {
		if got := Match(c.p, c.n); got != c.want {
			t.Errorf("%q %q = %v want %v", c.p, c.n, got, c.want)
		}
	}
}
func TestShellWordsLiteral(t *testing.T) {
	a := []string{"uname", "a b", "$(touch /tmp/pwn)", "a'b", "", "line\nnext", "a;echo"}
	got := ShellJoin(a)
	want := "uname a\\ b \\$\\(touch\\ /tmp/pwn\\) a\\'b '' line'\n'next a\\;echo"
	if got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
func TestCarrierExecUsesOwnTarget(t *testing.T) {
	own := "own.example"
	carrier := "carrier"
	m := Machine{Name: "guest", CarrierName: &carrier, Target: Target{Host: &own, Port: 2222}}
	a, e := SSHArgs(m, []string{"uname", "-r"})
	if e != nil || strings.Join(a, " ") != "ssh -l root -p 2222 own.example uname -r" {
		t.Fatal(a, e)
	}
}
func TestSelectionPrecedenceAndMetadata(t *testing.T) {
	all := []Machine{{Name: "name", Key: "other", Managed: true, Attributes: map[string]any{"custom": map[string]any{"bool": false}}}, {Name: "other", Key: "name", Managed: true}}
	p := "name"
	ms, e := Select(all, &p, nil, nil, "all")
	if e != nil || len(ms) != 1 || ms[0].Name != "name" {
		t.Fatal(ms, e)
	}
	ms, e = Select(all, nil, []string{"custom.bool=false"}, nil, "all")
	if e != nil || len(ms) != 1 {
		t.Fatal(ms, e)
	}
}
func TestCarriedBuilderChecks(t *testing.T) {
	c := "carrier"
	m := Machine{CarrierName: &c, Spin: "nixos", Attributes: map[string]any{"healthChecks": map[string]any{"builderCommands": []any{"one"}, "machineCommands": []any{"two"}, "systemd": map[string]any{"enable": true, "systemProperties": []any{"x"}, "unitProperties": map[string]any{"unit": []any{}}}}}}
	if m.Checks() != 3 {
		t.Fatal(m.Checks())
	}
}
func TestUnsupportedNoMutation(t *testing.T) {
	cwd, _ := os.Getwd()
	dir := t.TempDir()
	if e := os.Chdir(dir); e != nil {
		t.Fatal(e)
	}
	defer os.Chdir(cwd)
	for _, a := range [][]string{{"build"}, {"deploy"}, {"status"}, {"status", "-g", "current"}, {"inputs", "update"}} {
		if code := Main(context.Background(), a); code != 2 {
			t.Fatalf("%v: %d", a, code)
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 0 {
			t.Fatal("unsupported mutated state")
		}
	}
}
func TestOutputPaddingAndFalse(t *testing.T) {
	got := Table([]map[string]any{{"name": "a", "false": false, "arr": []any{"a", "b"}}}, []string{"name", "false", "arr"}, true)
	if got != "NAME   FALSE   ARR\na      -       [\"a\", \"b\"]\n" {
		t.Fatalf("%q", got)
	}
}
func TestConfirmationLines(t *testing.T) {
	e := &Engine{}
	p := filepath.Join(t.TempDir(), "input")
	_ = os.WriteFile(p, []byte("\ninvalid answer\ny\n"), 0600)
	f, _ := os.Open(p)
	defer f.Close()
	ok, err := e.Confirm(f, "", false)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
}
func TestNixEscapesInterpolation(t *testing.T) {
	s, e := NixLiteral([]any{"${danger}", "quote\"\\"})
	if e != nil || !strings.Contains(s, "\\${danger}") {
		t.Fatal(s, e)
	}
}
func TestMachineOrder(t *testing.T) {
	b := []byte(`{"z":{"name":"z","clusterName":"z","metaConfig":{"managed":true,"spin":"nixos"}},"a":{"name":"a","clusterName":"a","metaConfig":{"managed":true,"spin":"nixos"}}}`)
	ms, e := DecodeMachines(b)
	if e != nil || len(ms) != 2 || ms[0].Name != "z" {
		t.Fatal(ms, e)
	}
}
func TestStatusRolesAndDuration(t *testing.T) {
	x := normalizeInputs(map[string]any{"a": "123456789", "b": map[string]any{"rev": "b", "short_rev": "B"}, "c": false})
	if x["a"]["shortRev"] != "12345678" || x["b"]["shortRev"] != "B" || inputState(x["c"], x["c"]) != "unknown" {
		t.Fatal(x)
	}
	if s, e := formatDuration(60); s != "60.0s" || e != nil {
		t.Fatal(s, e)
	}
}
func TestHelpDoesNotEvaluate(t *testing.T) {
	before := RootHelp
	if !bytes.Contains([]byte(before), []byte("collect-garbage")) {
		t.Fatal("incomplete help")
	}
	if len(Inventory) != 29 {
		t.Fatal(len(Inventory))
	}
}

func TestNumericMetadataExact(t *testing.T) {
	b := []byte(`{"m":{"name":"m","key":"k","metaConfig":{"managed":true,"custom":{"large":9007199254740993,"decimal":1.0,"array":[9007199254740993,1.0]}}}}`)
	ms, err := DecodeMachines(b)
	if err != nil {
		t.Fatal(err)
	}
	m := ms[0]
	if str(m.Attr("custom.large")) != "9007199254740993" || str(m.Attr("custom.decimal")) != "1.0" || str(m.Attr("custom.array")) != "[9007199254740993, 1.0]" {
		t.Fatal(m.Attributes)
	}
	for _, f := range []string{"custom.large=9007199254740993", "custom.decimal=1.0"} {
		got, e := Select(ms, nil, []string{f}, nil, "all")
		if e != nil || len(got) != 1 {
			t.Fatal(f, got, e)
		}
	}
}
