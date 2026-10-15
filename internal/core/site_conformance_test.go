//go:build siteconformance

package core

import (
	"context"
	"encoding/json"
	"fmt"
	ext "github.com/vpsfreecz/confctl/extension"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type hostReply struct {
	Stdout, Stderr string
	Exit, DelayMS  int
}

func TestKernelHandlerHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) <= separator+1 {
		return
	}
	switch os.Args[separator+1] {
	case "ssh":
		var replies map[string]hostReply
		if err := json.Unmarshal([]byte(os.Getenv("KERNEL_TEST_REPLIES")), &replies); err != nil {
			os.Exit(90)
		}
		for _, arg := range os.Args[separator+2:] {
			if reply, ok := replies[arg]; ok {
				time.Sleep(time.Duration(reply.DelayMS) * time.Millisecond)
				fmt.Fprint(os.Stdout, reply.Stdout)
				fmt.Fprint(os.Stderr, reply.Stderr)
				os.Exit(reply.Exit)
			}
		}
		os.Exit(91)
	}
}

// Exercise the explicitly supplied site executable through the core supervisor
// and reverse exec service. Only SSH is a disposable helper; no site code is linked.
func runKernelHandler(t *testing.T, names []string, initial string, replies map[string]hostReply) (string, string) {
	t.Helper()
	executable := os.Getenv("CONFCTL_TEST_SITE_EXECUTABLE")
	if executable == "" {
		t.Fatal("siteconformance requires CONFCTL_TEST_SITE_EXECUTABLE")
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("CONFCTL_TEST_SITE_EXECUTABLE must be an absolute executable path")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("CONFCTL_TEST_SITE_EXECUTABLE is not an executable regular file: %s: %v", executable, err)
	}
	root := t.TempDir()
	state := filepath.Join(root, "configs/node/kernels.json")
	if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!" + sh + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestKernelHandlerHelper$", "--", "ssh"}) + " \"$@\"\n"
	if err = os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(replies)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("KERNEL_TEST_REPLIES", string(b))
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := &Engine{Root: root, Context: ctx, Color: "never"}
	for _, name := range names {
		host := name + ".example"
		e.Inventory = append(e.Inventory, Machine{Name: name, Key: name, Spin: "vpsadminos", Managed: true, Target: Target{Host: &host, Port: 22}, Attributes: map[string]any{}})
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old; f.Close() }()
	reg := Registration{ID: "test.kernels", Protocol: ext.Version{Major: 1}, Argv: []string{executable}}
	code, err := e.Invoke(reg, "runtime.update", ext.Invocation{Root: root, Options: map[string]any{"yes": true}})
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(saved)
}

func TestKernelErrorValueRetainsPriorAndAbsentKeys(t *testing.T) {
	out, saved := runKernelHandler(t, []string{"prior", "absent", "good"}, `{"keep":{"custom":true},"prior":"old"}`, map[string]hostReply{
		"prior.example":  {Stdout: "error\n"},
		"absent.example": {Stdout: "error.\n"},
		"good.example":   {Stdout: "6.12.35.extra\n"},
	})
	want := "{\n  \"keep\": {\n    \"custom\": true\n  },\n  \"prior\": \"old\",\n  \"good\": \"6.12.35\"\n}"
	if saved != want || strings.Count(out, "error\n") != 2 || strings.Contains(out, "Error on") {
		t.Fatal(out, saved)
	}
}

func TestKernelFailureDetailsUseCompletionOrder(t *testing.T) {
	out, saved := runKernelHandler(t, []string{"slow", "fast", "good"}, `{"keep":"unchanged","slow":"old","fast":"old"}`, map[string]hostReply{
		"slow.example": {Stderr: "slow failure\n", Exit: 12, DelayMS: 400},
		"fast.example": {Stderr: "fast failure\n", Exit: 13, DelayMS: 10},
		"good.example": {Stdout: "6.12.35.extra\n", DelayMS: 100},
	})
	fast := "Error on fast: Running `ssh -l root fast.example uname\\ -r` failed with\n  exit status: 13\n  stdout: Nothing written\n  stderr: fast failure\n\n"
	slow := "Error on slow: Running `ssh -l root slow.example uname\\ -r` failed with\n  exit status: 12\n  stdout: Nothing written\n  stderr: slow failure\n\n"
	if strings.Index(out, "slow ") < 0 || strings.Index(out, "slow ") > strings.Index(out, "fast ") || !strings.Contains(out, fast+slow) {
		t.Fatal("table inventory order or error completion order lost", out)
	}
	if saved != "{\n  \"keep\": \"unchanged\",\n  \"good\": \"6.12.35\"\n}" {
		t.Fatal(saved)
	}
}
