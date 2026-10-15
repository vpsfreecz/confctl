package core

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSSHMergeAtChildFDAndCapturedProcessUnchanged(t *testing.T) {
	trace := sshExecutables(t, map[string]sshChildStimulus{"cat": {Out: "out-one\n", Err: "err-one\n", Tail: "out-two\n"}})
	e := connectionEngine(t)
	r, err := e.sshCommandProcess(context.Background(), e.Root, []string{"cat", "token"}, sshInput{})
	if err != nil || string(r.Stdout) != "out-one\nerr-one\nout-two\n" || len(r.Stderr) != 0 {
		t.Fatal(r, err)
	}
	merged := sshRecord(t, trace, "cat", 1).OutputDescriptors
	if merged[0] != merged[1] {
		t.Fatal("streams combined after exit instead of at child FD", merged)
	}
	r, err = Process(context.Background(), e.Root, []string{"cat", "token"}, nil)
	if err != nil || string(r.Stdout) != "out-one\nout-two\n" || string(r.Stderr) != "err-one\n" {
		t.Fatal("ordinary captured Process changed", r, err)
	}
	separate := sshRecord(t, trace, "cat", 2).OutputDescriptors
	if separate[0] == separate[1] {
		t.Fatal("ordinary Process acquired SSH merging", separate)
	}
	r, err = e.testConnectionProcess(context.Background(), e.Root, []string{"cat", "token"}, nil)
	if err != nil || string(r.Stdout) != "out-one\nout-two\n" || string(r.Stderr) != "err-one\n" {
		t.Fatal("test-connection captured streams changed", r, err)
	}
}

func TestSSHLogTypedOptionsPresenceAndEncounterOrder(t *testing.T) {
	inv := sshInvocation(t, "ssh", "-f", "input-file", "-i", "", "--no-parallel", "-g", "-y", "node", "cat", "token")
	e := connectionEngine(t)
	e.Color = inv.Globals["color"].Value.String
	if err := e.OpenLog("ssh"); err != nil {
		t.Fatal(err)
	}
	before := sshInvocation(t, inv.RawArgv...)
	if err := e.LogSSHCLI(inv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv, before) {
		t.Fatal("command log changed invocation")
	}
	if err := e.Log.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.Log.Name())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{command: ["ssh"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"f" => "input-file",
   "input-file" => "input-file",
   "i" => "",
   "input-string" => "",
   "p" => false,
   "parallel" => false,
   "g" => true,
   "aggregate" => true,
   "y" => true,
   "yes" => true,
   "managed" => nil,
   "a" => [],
   "attr" => [],
   "t" => [],
   "tag" => []},
 arguments: ["node", "cat", "token"]}
`
	if string(b) != want {
		t.Fatal(string(b))
	}
	inv = sshInvocation(t, "ssh", "-y", "node", "cat")
	f, err := os.Create(e.Log.Name())
	if err != nil {
		t.Fatal(err)
	}
	e.Log, e.logOwner = f, nativeEngineLog(f)
	if err := e.LogSSHCLI(inv); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	b, _ = os.ReadFile(f.Name())
	for _, text := range []string{`"input-string" => nil`, `"input-file" => nil`} {
		if !strings.Contains(string(b), text) {
			t.Fatal("absence conflated with empty", string(b))
		}
	}
}

func TestCSSHProcessLogRetainsSourceEnvironmentShellSpelling(t *testing.T) {
	sshExecutables(t, map[string]sshChildStimulus{"nix-shell": {Out: "out\n", Err: "err\n", Exit: 9}})
	e := connectionEngine(t, connectionMachine("one", "one", "one.invalid", true))
	if err := e.OpenLog("cssh"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.CSSH(sshInvocation(t, "cssh", "-y", "one"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	_ = e.Log.Close()
	b, _ := os.ReadFile(e.Log.Name())
	if !strings.Contains(string(b), `Running ( export shellHook="" ; nix-shell -p perlPackages.AppClusterSSH --command cssh\ -l\ root\ one.invalid )`+"\n") || !strings.Contains(string(b), "exit status 9 (failed)") {
		t.Fatal(string(b))
	}
}
