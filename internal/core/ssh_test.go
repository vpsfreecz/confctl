package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

func sshInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestSSHSelectionPresenceAndConfirmation(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	all := []Machine{connectionMachine("zeta", "pick*", "zeta.invalid", true), connectionMachine("alpha", "a", "alpha.invalid", true), connectionMachine(".hidden", "h", "hidden.invalid", false), connectionMachine("carried", "c", "own.invalid", true), connectionMachine("nil", "n", "", true)}
	carrier := "zeta"
	all[3].CarrierName, all[4].Target.Host = &carrier, nil
	for _, tc := range []struct {
		args          []string
		in, want, err string
		hosts         []string
	}{
		{args: []string{"ssh", "-y", "*", "cat", "token"}, want: "zeta:\nok\n\nalpha:\nok\n\n", hosts: []string{"zeta.invalid", "alpha.invalid"}},
		{args: []string{"ssh", "-y", "pick*", "cat", "token"}, want: "zeta:\nok\n\n", hosts: []string{"zeta.invalid"}},
		{args: []string{"ssh", "-y", "--managed", "no", ".hidden", "cat", "token"}, want: ".hidden:\nok\n\n", hosts: []string{"hidden.invalid"}},
		{args: []string{"ssh", "-y", "*"}, err: "missing command"},
		{args: []string{"ssh", "-i", "", "-f", "input", "*", "cat", "token"}, err: "use one of --input-string or --input-file"},
		{args: []string{"ssh", "-i", "data", "-f", "", "*", "cat", "token"}, err: "use one of --input-string or --input-file"},
		{args: []string{"ssh", "-i", "", "-f", "", "carried", "cat", "token"}, err: "No machines to ssh to"},
		{args: []string{"ssh", "-y", "nil", "cat", "token"}, err: "No machines to ssh to"},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			e := connectionEngine(t, all...)
			var out bytes.Buffer
			hosts := []string{}
			err := e.runSSH(sshInvocation(t, tc.args...), strings.NewReader(tc.in), &out, sshDeps{Run: func(ctx context.Context, root string, argv []string, input sshInput) (Result, error) {
				hosts = append(hosts, argv[len(argv)-2])
				return Result{ExitCode: 0, Stdout: []byte("ok\n")}, nil
			}})
			if tc.err != "" {
				if err == nil || err.Error() != tc.err || out.Len() != 0 || len(hosts) != 0 {
					t.Fatal(err, out.String(), hosts)
				}
				return
			}
			if err != nil || out.String() != tc.want || !reflect.DeepEqual(hosts, tc.hosts) {
				t.Fatal(err, out.String(), hosts)
			}
		})
	}
	// An exact filtered-out name must not fall through to a matching key.
	shadow := connectionMachine("picked", "x", "unmanaged.invalid", false)
	other := connectionMachine("other", "picked", "other.invalid", true)
	e := connectionEngine(t, shadow, other)
	if _, err := e.sshMachines(sshInvocation(t, "ssh", "-y", "picked", "cat"), "empty"); err == nil || err.Error() != "empty" {
		t.Fatal(err)
	}
	e = connectionEngine(t, all[0])
	var out bytes.Buffer
	runs := 0
	deps := sshDeps{Run: func(context.Context, string, []string, sshInput) (Result, error) {
		runs++
		return Result{ExitCode: 0}, nil
	}}
	err := e.runSSH(sshInvocation(t, "ssh", "zeta", "cat", "token"), strings.NewReader("\nY\n"), &out, deps)
	if err != nil || runs != 1 || strings.Count(out.String(), "Continue? [y/N]: ") != 2 || !strings.Contains(out.String(), "Command: cat token\n") || !strings.Contains(out.String(), "NAME") {
		t.Fatal(err, runs, out.String())
	}
	for _, answer := range []string{"n\n", ""} {
		out.Reset()
		runs = 0
		err = e.runSSH(sshInvocation(t, "ssh", "-f", "missing", "zeta", "cat", "token"), strings.NewReader(answer), &out, deps)
		if err == nil || runs != 0 {
			t.Fatal(err, runs)
		}
	}
}

func TestSSHCommandAndInteractiveLiteralArguments(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "fixtures/ssh config%25.conf")
	m := connectionMachine("machine", "key", "literal target%25.invalid", true)
	m.Target.Port = 2222
	cmd := []string{"cat", "space value", "single'quote", `"double"`, `\back`, "$dollar", "%percent", "semi;value", "", "line\nbreak"}
	argv, err := sshCommandArgs(m, cmd)
	want := []string{"ssh", "-F", "fixtures/ssh config%25.conf", "-l", "root", "-p", "2222", "literal target%25.invalid", "cat space\\ value single\\'quote \\\"double\\\" \\\\back \\$dollar \\%percent semi\\;value '' line'\n'break"}
	if err != nil || !reflect.DeepEqual(argv, want) {
		t.Fatal(argv, err, want)
	}
	interactive, err := sshInteractiveArgs(m, nil)
	if err != nil || !reflect.DeepEqual(interactive, want[:len(want)-1]) {
		t.Fatal(interactive, err)
	}
	for _, a := range interactive {
		if a == "-t" || a == "-tt" || a == "-o" {
			t.Fatal("ordinary SSH acquired test-connection/PTY options", interactive)
		}
	}
	local := connectionMachine("local", "local", "localhost", true)
	local.Target.Port = 2200
	argv, err = sshCommandArgs(local, cmd)
	if err != nil || !reflect.DeepEqual(argv, cmd) {
		t.Fatal(argv, err)
	}
	t.Setenv("SHELL", "/wrong/environment-shell")
	interactive, err = sshInteractiveArgs(local, func() (string, error) { return "/root/passwd-shell", nil })
	if err != nil || !reflect.DeepEqual(interactive, []string{"/root/passwd-shell"}) {
		t.Fatal(interactive, err)
	}
	// Zero arguments follow the command branch, with a literal empty remote
	// command string. The uncharacterized prompt variant remains an open gate.
	e := connectionEngine(t, m)
	var out bytes.Buffer
	err = e.runSSH(sshInvocation(t, "ssh", "-y"), strings.NewReader(""), &out, sshDeps{Run: func(ctx context.Context, root string, a []string, input sshInput) (Result, error) {
		if a[len(a)-1] != "" {
			t.Fatal(a)
		}
		return Result{ExitCode: 0}, nil
	}})
	if err != nil || out.String() != "machine:\n\n\n" {
		t.Fatal(err, out.String())
	}
	out.Reset()
	err = e.runSSH(sshInvocation(t, "ssh"), strings.NewReader("y\n"), &out, sshDeps{Run: func(context.Context, string, []string, sshInput) (Result, error) {
		t.Fatal("zero-positional confirmation failure ran a command")
		return Result{}, nil
	}})
	if err == nil || err.Error() != "undefined method 'map' for nil" || !strings.Contains(out.String(), "NAME") || strings.Contains(out.String(), "Continue?") {
		t.Fatal(err, out.String())
	}
	out.Reset()
	err = e.runSSH(sshInvocation(t, "ssh", "-y", "-p"), strings.NewReader(""), &out, sshDeps{Run: func(context.Context, string, []string, sshInput) (Result, error) {
		t.Fatal("zero-positional parallel header failure ran a command")
		return Result{}, nil
	}})
	if err == nil || err.Error() != "undefined method 'map' for nil" || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	for _, tc := range []struct {
		command []string
		want    string
	}{{nil, "Cmd requires command argument"}, {[]string{""}, "No command provided"}} {
		if _, err := sshCommandArgs(local, tc.command); err == nil || err.Error() != tc.want {
			t.Fatal(err)
		}
	}
}

type sshChildStimulus struct {
	Read                   bool
	Out, Err, Tail         string
	Exit, DelayMS, Barrier int
	WritePath, WriteText   string
	ReleasePath            string
	Wait                   bool
}

type sshChildRecord struct {
	Tool, Host, CWD, Input, StdinMode string
	Argv                              []string
	Env                               map[string]string
	RegularStdin                      bool
	PID                               int
	OutputDescriptors                 [2]inheritedDescriptor
}

// A real disposable executable boundary. It never executes the remote command,
// reads /proc, calls SSH, realizes Nix, or launches a GUI.
func TestSSHProcessChild(t *testing.T) {
	if os.Getenv("CORE_SSH_CHILD") != "1" {
		return
	}
	marker := 0
	for os.Args[marker] != "--" {
		marker++
	}
	tool, argv := os.Args[marker+1], os.Args[marker+2:]
	host := ""
	if tool == "ssh" {
		for i := 0; i < len(argv); i++ {
			if argv[i] == "-F" || argv[i] == "-l" || argv[i] == "-p" {
				i++
				continue
			}
			host = argv[i]
			break
		}
	}
	var stimuli map[string]sshChildStimulus
	if err := json.Unmarshal([]byte(os.Getenv("CORE_SSH_STIMULI")), &stimuli); err != nil {
		panic(err)
	}
	key := host
	if key == "" {
		key = tool
	}
	stim, ok := stimuli[key]
	if !ok {
		panic("unexpected test request " + key)
	}
	cwd, _ := os.Getwd()
	stat, _ := os.Stdin.Stat()
	r := sshChildRecord{Tool: tool, Host: host, CWD: cwd, Argv: argv, Env: map[string]string{}, RegularStdin: stat.Mode().IsRegular(), PID: os.Getpid()}
	r.OutputDescriptors = [2]inheritedDescriptor{inheritedFileIdentity(os.Stdout), inheritedFileIdentity(os.Stderr)}
	for _, k := range []string{"CONFCTL_SSH_CONFIG", "NIX_SSHOPTS", "shellHook"} {
		if value, present := os.LookupEnv(k); present {
			r.Env[k] = value
		}
	}
	if stim.Read {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			panic(err)
		}
		r.Input, r.StdinMode = string(b), "read-to-eof"
	} else {
		_ = syscall.SetNonblock(0, true)
		b := make([]byte, 1)
		n, err := syscall.Read(0, b)
		_ = syscall.SetNonblock(0, false)
		switch {
		case n == 0 && err == nil:
			r.StdinMode = "eof"
		case err == syscall.EAGAIN:
			r.StdinMode = "open"
		default:
			panic(fmt.Sprint("unexpected input probe", n, err))
		}
	}
	trace := os.Getenv("CORE_SSH_TRACE")
	base := filepath.Join(trace, key)
	index := 1
	if b, err := os.ReadFile(base + ".count"); err == nil {
		index, _ = strconv.Atoi(string(b))
		index++
	}
	_ = os.WriteFile(base+".count", []byte(strconv.Itoa(index)), 0600)
	if stim.Barrier != 0 {
		_ = os.WriteFile(base+".ready", []byte("ready"), 0600)
		until := time.Now().Add(4 * time.Second)
		for {
			files, _ := filepath.Glob(filepath.Join(trace, "*.ready"))
			if len(files) == stim.Barrier {
				break
			}
			if time.Now().After(until) {
				os.Exit(124)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if stim.DelayMS != 0 {
		time.Sleep(time.Duration(stim.DelayMS) * time.Millisecond)
	}
	if stim.WritePath != "" {
		if err := os.WriteFile(stim.WritePath, []byte(stim.WriteText), 0600); err != nil {
			panic(err)
		}
	}
	b, _ := json.Marshal(r)
	_ = os.WriteFile(fmt.Sprintf("%s-%d.json", base, index), b, 0600)
	if stim.ReleasePath != "" {
		until := time.Now().Add(4 * time.Second)
		for {
			if _, err := os.Stat(stim.ReleasePath); err == nil {
				break
			}
			if time.Now().After(until) {
				os.Exit(124)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if stim.Wait {
		for {
			time.Sleep(time.Hour)
		}
	}
	fmt.Fprint(os.Stdout, stim.Out)
	fmt.Fprint(os.Stderr, stim.Err)
	fmt.Fprint(os.Stdout, stim.Tail)
	os.Exit(stim.Exit)
}

func sshExecutables(t *testing.T, stimuli map[string]sshChildStimulus) string {
	t.Helper()
	bin, trace := t.TempDir(), t.TempDir()
	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"ssh", "cat", "nix-shell", "getent"} {
		script := "#!" + sh + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestSSHProcessChild$", "--", tool}) + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(stimuli)
	t.Setenv("PATH", bin)
	t.Setenv("CORE_SSH_CHILD", "1")
	t.Setenv("CORE_SSH_TRACE", trace)
	t.Setenv("CORE_SSH_STIMULI", string(b))
	return trace
}

func sshRecord(t *testing.T, trace, key string, index int) sshChildRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(trace, fmt.Sprintf("%s-%d.json", key, index)))
	if err != nil {
		t.Fatal(err)
	}
	var r sshChildRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSSHActualStreamsInputAndSerialHostFailures(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "fixtures/ssh config%25.conf")
	machines := []Machine{connectionMachine("zeta", "z", "zeta.invalid", true), connectionMachine("beta", "b", "beta.invalid", true), connectionMachine("alpha", "a", "alpha.invalid", true)}
	machines[1].Target.Port = 2222
	trace := sshExecutables(t, map[string]sshChildStimulus{
		"zeta.invalid":  {Out: "OUT1\n", Err: "ERR1\n", Tail: "OUT2\n"},
		"beta.invalid":  {Out: "FAIL-OUT\n", Err: "FAIL-ERR\n", Exit: 7},
		"alpha.invalid": {Out: "255-OUT\n", Err: "255-ERR\n", Exit: 255},
	})
	e := connectionEngine(t, machines...)
	if err := e.OpenLog("ssh"); err != nil {
		t.Fatal(err)
	}
	defer e.Log.Close()
	var out bytes.Buffer
	if err := e.SSH(sshInvocation(t, "ssh", "-y", "*", "cat", "token"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "zeta:\nOUT1\nERR1\nOUT2\n\nbeta:\n\n\nalpha:\n\n\n" {
		t.Fatal(out.String())
	}
	for _, host := range []string{"zeta.invalid", "beta.invalid", "alpha.invalid"} {
		r := sshRecord(t, trace, host, 1)
		if r.CWD != e.Root || r.StdinMode != "open" || r.Env["CONFCTL_SSH_CONFIG"] != "fixtures/ssh config%25.conf" {
			t.Fatal(r)
		}
		if r.Argv[len(r.Argv)-1] != "cat token" {
			t.Fatal(r.Argv)
		}
	}
	log, _ := os.ReadFile(e.Log.Name())
	for _, text := range []string{"OUT1", "ERR1", "OUT2", "FAIL-OUT", "FAIL-ERR", "255-OUT", "255-ERR", "exit status 7 (failed)", "exit status 255 (failed)"} {
		if !strings.Contains(string(log), text) {
			t.Fatalf("log lost %q: %s", text, log)
		}
	}
	// Source-backed input matrix exercises actual descriptor types and bytes.
	for _, tc := range []struct {
		name string
		args []string
		text string
		file bool
	}{
		{"empty-string", []string{"-i", ""}, "", false},
		{"literal-string", []string{"-i", "first\nsecond\n"}, "first\nsecond\n", false},
		{"existing-file", []string{"-f", "input.txt"}, "file bytes\n", true},
		{"missing-file", []string{"-f", "missing.txt"}, "missing.txt", true},
		{"empty-file-option", []string{"-f", ""}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := sshExecutables(t, map[string]sshChildStimulus{"cat": {Read: true}})
			e := connectionEngine(t, connectionMachine("local", "l", "localhost", true))
			if tc.name == "existing-file" {
				_ = os.WriteFile(filepath.Join(e.Root, "input.txt"), []byte(tc.text), 0600)
			}
			args := append([]string{"ssh", "-y"}, tc.args...)
			args = append(args, "local", "cat", "token")
			var out bytes.Buffer
			if err := e.SSH(sshInvocation(t, args...), strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			r := sshRecord(t, trace, "cat", 1)
			if r.Input != tc.text || r.RegularStdin != tc.file || r.StdinMode != "read-to-eof" || !reflect.DeepEqual(r.Argv, []string{"token"}) {
				t.Fatal(r)
			}
		})
	}
}

func TestSSHInputFileReopensAtEachHost(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	e := connectionEngine(t, connectionMachine("zeta", "z", "zeta.invalid", true), connectionMachine("alpha", "a", "alpha.invalid", true))
	_ = os.WriteFile(filepath.Join(e.Root, "input.txt"), []byte("first file bytes\n"), 0600)
	trace := sshExecutables(t, map[string]sshChildStimulus{"zeta.invalid": {Read: true, WritePath: "input.txt", WriteText: "second file bytes\n"}, "alpha.invalid": {Read: true}})
	var out bytes.Buffer
	if err := e.SSH(sshInvocation(t, "ssh", "-y", "-f", "input.txt", "*", "cat", "token"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if sshRecord(t, trace, "zeta.invalid", 1).Input != "first file bytes\n" || sshRecord(t, trace, "alpha.invalid", 1).Input != "second file bytes\n" {
		t.Fatal("file was not reopened per host")
	}
	// First missing-path conversion happens before the first response creates it.
	trace = sshExecutables(t, map[string]sshChildStimulus{"zeta.invalid": {Read: true, WritePath: "created.txt", WriteText: "created after first spawn\n"}, "alpha.invalid": {Read: true}})
	out.Reset()
	if err := e.SSH(sshInvocation(t, "ssh", "-y", "-f", "created.txt", "*", "cat", "token"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if sshRecord(t, trace, "zeta.invalid", 1).Input != "created.txt" || sshRecord(t, trace, "alpha.invalid", 1).Input != "created after first spawn\n" {
		t.Fatal("wrong missing-file conversion timing")
	}
}

func TestSSHInputFilePreservesPhysicalSymlinkParentTraversal(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	for _, parallel := range []bool{false, true} {
		t.Run(strconv.FormatBool(parallel), func(t *testing.T) {
			e := connectionEngine(t, connectionMachine("one", "one", "one.invalid", true), connectionMachine("two", "two", "two.invalid", true))
			physicalDir := filepath.Join(e.Root, "physical")
			if err := os.MkdirAll(filepath.Join(physicalDir, "child"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("physical/child", filepath.Join(e.Root, "link")); err != nil {
				t.Fatal(err)
			}
			const physical = "physical input\x00bytes\n"
			const lexical = "wrong lexical input\n"
			if err := os.WriteFile(filepath.Join(physicalDir, "input.txt"), []byte(physical), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.Root, "input.txt"), []byte(lexical), 0600); err != nil {
				t.Fatal(err)
			}
			trace := sshExecutables(t, map[string]sshChildStimulus{
				"one.invalid": {Read: true, Out: "stdout\n", Err: "stderr\n"},
				"two.invalid": {Read: true, Out: "stdout\n", Err: "stderr\n"},
			})
			argv := []string{"ssh", "-y", "-f", "link/../input.txt"}
			if parallel {
				argv = append(argv, "-p")
			}
			argv = append(argv, "*", "cat", "token")
			var out bytes.Buffer
			if err := e.SSH(sshInvocation(t, argv...), strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			for _, host := range []string{"one.invalid", "two.invalid"} {
				record := sshRecord(t, trace, host, 1)
				if record.Input != physical || !record.RegularStdin || record.OutputDescriptors[0] != record.OutputDescriptors[1] {
					t.Fatal("physical file bytes or merged child descriptors changed", record)
				}
			}
			if strings.Count(out.String(), "stdout\nstderr\n") != 2 {
				t.Fatal(out.String())
			}
			// Both files remain present and distinct; a missing-file conversion
			// or lexical-path read cannot accidentally pass this regression.
			for path, want := range map[string]string{filepath.Join(physicalDir, "input.txt"): physical, filepath.Join(e.Root, "input.txt"): lexical} {
				b, err := os.ReadFile(path)
				if err != nil || string(b) != want {
					t.Fatal(path, string(b), err)
				}
			}
		})
	}
}

func TestSSHInputFileOpenFailureStopsBeforeSpawn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions cannot characterize open failure as root")
	}
	trace := sshExecutables(t, map[string]sshChildStimulus{"zeta.invalid": {Read: true}, "alpha.invalid": {Read: true}})
	e := connectionEngine(t, connectionMachine("zeta", "z", "zeta.invalid", true), connectionMachine("alpha", "a", "alpha.invalid", true))
	path := filepath.Join(e.Root, "denied.txt")
	if err := os.WriteFile(path, []byte("retained input"), 0000); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := e.SSH(sshInvocation(t, "ssh", "-y", "-f", "denied.txt", "*", "cat", "token"), strings.NewReader(""), &out)
	if !errors.Is(err, os.ErrPermission) || out.String() != "zeta:\n" {
		t.Fatal(err, out.String())
	}
	if entries, _ := os.ReadDir(trace); len(entries) != 0 {
		t.Fatal("spawn occurred before file open", entries)
	}
	if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != 0 {
		t.Fatal("input file changed on error", stat, err)
	}
}

func TestSSHParallelSpawnFailureCancelsActivePeer(t *testing.T) {
	trace := sshExecutables(t, map[string]sshChildStimulus{"cat": {Wait: true}})
	if err := os.Remove(filepath.Join(os.Getenv("PATH"), "ssh")); err != nil {
		t.Fatal(err)
	}
	e := connectionEngine(t, connectionMachine("local", "l", "localhost", true), connectionMachine("remote", "r", "remote.invalid", true))
	deps := sshDeps{Run: func(ctx context.Context, root string, argv []string, input sshInput) (Result, error) {
		if argv[0] == "ssh" {
			until := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(trace, "cat-1.json")); err == nil {
					break
				}
				if time.Now().After(until) {
					return Result{}, errors.New("peer readiness failure")
				}
				time.Sleep(time.Millisecond)
			}
		}
		return e.sshCommandProcess(ctx, root, argv, input)
	}}
	var out bytes.Buffer
	err := e.runSSH(sshInvocation(t, "ssh", "-y", "-p", "*", "cat", "token"), strings.NewReader(""), &out, deps)
	var missing *exec.Error
	if !errors.As(err, &missing) || !errors.Is(err, exec.ErrNotFound) || out.String() != "Executing cat token on 2 machines\n" {
		t.Fatal(err, out.String())
	}
	pid := sshRecord(t, trace, "cat", 1).PID
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("cancelled active peer not reaped", pid, err)
	}
}

func TestSSHParallelCompletionAndAggregateGroups(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	for _, aggregate := range []bool{false, true} {
		t.Run(strconv.FormatBool(aggregate), func(t *testing.T) {
			trace := sshExecutables(t, map[string]sshChildStimulus{
				"zeta.invalid":  {Out: "same\n", Exit: 7, DelayMS: 400, Barrier: 4},
				"beta.invalid":  {Out: "same\n", DelayMS: 0, Barrier: 4},
				"alpha.invalid": {Out: "same\n", Exit: 7, DelayMS: 200, Barrier: 4},
				"gamma.invalid": {Out: "different\n", Exit: 7, DelayMS: 100, Barrier: 4},
			})
			e := connectionEngine(t, connectionMachine("zeta", "z", "zeta.invalid", true), connectionMachine("beta", "b", "beta.invalid", true), connectionMachine("alpha", "a", "alpha.invalid", true), connectionMachine("gamma", "g", "gamma.invalid", true))
			args := []string{"ssh", "-y", "-p"}
			if aggregate {
				args = append(args, "-g")
			}
			args = append(args, "*", "cat", "token")
			var out bytes.Buffer
			if err := e.SSH(sshInvocation(t, args...), strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			want := "Executing cat token on 4 machines\nbeta:\nsame\n\ngamma:\n\n\nalpha:\n\n\nzeta:\n\n\n"
			if aggregate {
				want = "Executing cat token on 4 machines\nbeta:\nExit status: 0\nsame\n\ngamma:\nExit status: 7\ndifferent\n\nalpha, zeta:\nExit status: 7\nsame\n\n"
			}
			if out.String() != want {
				t.Fatal(out.String())
			}
			for _, host := range []string{"zeta.invalid", "beta.invalid", "alpha.invalid", "gamma.invalid"} {
				_ = sshRecord(t, trace, host, 1)
			}
		})
	}
}

func TestSSHSerialAggregateAndTransportFailures(t *testing.T) {
	e := connectionEngine(t, connectionMachine("zeta", "z", "zeta.invalid", true), connectionMachine("alpha", "a", "alpha.invalid", true), connectionMachine("beta", "b", "beta.invalid", true))
	var out bytes.Buffer
	deps := sshDeps{Run: func(_ context.Context, _ string, argv []string, _ sshInput) (Result, error) {
		status := 7
		if argv[len(argv)-2] == "beta.invalid" {
			status = 255
		}
		return Result{ExitCode: status, Stdout: []byte("same\n")}, nil
	}}
	if err := e.runSSH(sshInvocation(t, "ssh", "-y", "-g", "*", "cat", "token"), strings.NewReader(""), &out, deps); err != nil {
		t.Fatal(err)
	}
	if out.String() != "alpha, zeta:\nExit status: 7\nsame\n\nbeta:\nExit status: 255\nsame\n\n" {
		t.Fatal(out.String())
	}
	failure := errors.New("transport failed")
	for _, r := range []Result{{ExitCode: 255}, {ExitCode: 0, waitErr: failure}, {ExitCode: -1}, {ExitCode: -1, signal: syscall.SIGTERM}} {
		status, err := sshStatus(context.Background(), r, nil)
		if r.waitErr != nil && !errors.Is(err, failure) || r.ExitCode == -1 && r.signal == 0 && err == nil || r.signal != 0 && (err != nil || status != int(syscall.SIGTERM)) || r.ExitCode == 255 && (err != nil || status != 255) {
			t.Fatal(status, err, r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sshStatus(ctx, Result{ExitCode: 255}, failure); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	out.Reset()
	calls := 0
	deps.Run = func(context.Context, string, []string, sshInput) (Result, error) {
		calls++
		return Result{ExitCode: 255}, failure
	}
	if err := e.runSSH(sshInvocation(t, "ssh", "-y", "*", "cat", "token"), strings.NewReader(""), &out, deps); !errors.Is(err, failure) || calls != 1 || out.String() != "zeta:\n" {
		t.Fatal(err, calls, out.String())
	}
	if _, err := e.sshCommandProcess(context.Background(), e.Root, []string{filepath.Join(e.Root, "missing")}, sshInput{}); err == nil {
		t.Fatal("spawn failure treated as host exit")
	}
}

func TestSSHActualCancellationReapsAndPreservesFailure(t *testing.T) {
	trace := sshExecutables(t, map[string]sshChildStimulus{"cat": {Wait: true}})
	e := connectionEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.sshCommandProcess(ctx, e.Root, []string{"cat", "token"}, sshInput{}); done <- err }()
	until := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(trace, "cat-1.json")); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child never became ready")
		}
		time.Sleep(time.Millisecond)
	}
	pid := sshRecord(t, trace, "cat", 1).PID
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("owned child did not finish")
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("child not reaped", pid, err)
	}
}

func TestSSHInteractiveAdapterIgnoresHostNonzero(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	e := connectionEngine(t, connectionMachine("one", "one", "one.invalid", true))
	var out bytes.Buffer
	in := strings.NewReader("")
	called := false
	err := e.runSSH(sshInvocation(t, "ssh", "-y", "one"), in, &out, sshDeps{Interactive: func(ctx context.Context, root string, argv []string, stdin io.Reader, stdout io.Writer) (Result, error) {
		called = true
		if stdin != in || stdout != &out || root != e.Root || !reflect.DeepEqual(argv, []string{"ssh", "-l", "root", "one.invalid"}) {
			t.Fatal(argv)
		}
		return Result{ExitCode: 9}, nil
	}})
	if err != nil || !called || out.Len() != 0 {
		t.Fatal(err, called, out.String())
	}
}

func TestCSSHActualLaunchShapeEnvironmentAndIgnoredFalse(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "fixtures/ssh config%25.conf")
	t.Setenv("shellHook", "caller sentinel")
	for _, status := range []int{0, 9} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			trace := sshExecutables(t, map[string]sshChildStimulus{"nix-shell": {Out: "out\n", Err: "err\n", Exit: status}})
			m := connectionMachine("alpha", "a", "literal host%25.invalid", true)
			m.Target.Port = 2222
			e := connectionEngine(t, connectionMachine("zeta", "z", "zeta.invalid", true), m, connectionMachine("local", "l", "localhost", true))
			var out bytes.Buffer
			if err := e.CSSH(sshInvocation(t, "cssh", "-y", "*"), strings.NewReader(""), &out); err != nil || out.Len() != 0 {
				t.Fatal(err, out.String())
			}
			r := sshRecord(t, trace, "nix-shell", 1)
			if !reflect.DeepEqual(r.Argv, []string{"-p", "perlPackages.AppClusterSSH", "--command", "cssh -l root zeta.invalid literal host%25.invalid localhost"}) || r.CWD != e.Root || r.Env["shellHook"] != "" {
				t.Fatal(r)
			}
			if _, present := r.Env["shellHook"]; !present {
				t.Fatal("pinned TTY empty export was silently unset")
			}
			if os.Getenv("shellHook") != "caller sentinel" {
				t.Fatal("caller environment mutated")
			}
		})
	}
	// Declining uses the ordinary table/prompt and never launches nix-shell.
	trace := sshExecutables(t, map[string]sshChildStimulus{"nix-shell": {Exit: 126}})
	e := connectionEngine(t, connectionMachine("one", "o", "one.invalid", true))
	var out bytes.Buffer
	if err := e.CSSH(sshInvocation(t, "cssh", "one"), strings.NewReader("n\n"), &out); err == nil || err.Error() != "Aborted" {
		t.Fatal(err)
	}
	if files, _ := os.ReadDir(trace); len(files) != 0 {
		t.Fatal(files)
	}
}

// Keep exec.ExitError in this adapter's status vocabulary: it is a completed
// child result, unlike arbitrary Wait errors or a failed spawn.
func TestSSHStatusCompletedExitError(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestSSHProcessChild$", "--", "cat", "token")
	trace := sshExecutables(t, map[string]sshChildStimulus{"cat": {Exit: 7}})
	_ = trace
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if status, err := sshStatus(context.Background(), Result{ExitCode: 7, waitErr: err}, nil); status != 7 || err != nil {
		t.Fatal(status, err)
	}
}
