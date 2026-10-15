//go:build siteconformance

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	ext "github.com/vpsfreecz/confctl/extension"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestNetbootInventoryHelper(t *testing.T) {
	if os.Getenv("CONFCTL_NETBOOT_INVENTORY_HELPER") != "1" {
		return
	}
	// This is a finite injected process. Refresh also evaluates argument
	// settings even when Engine.SettingsCache already exists.
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	settings := []string{"eval", "--json", "--no-write-lock-file", "--no-update-lock-file", "--option", "max-jobs", "auto", ".#confctl.settings"}
	inventory := []string{"build", "--json", "--no-link", "--no-write-lock-file", "--no-update-lock-file", "--option", "max-jobs", "auto", ".#confctl.machinesJson"}
	switch {
	case reflect.DeepEqual(args, settings):
		body, err := os.ReadFile(os.Getenv("CONFCTL_NETBOOT_SETTINGS_FILE"))
		if err != nil {
			os.Exit(91)
		}
		if _, err := os.Stdout.Write(body); err != nil {
			os.Exit(92)
		}
	case reflect.DeepEqual(args, inventory):
		if err := json.NewEncoder(os.Stdout).Encode([]map[string]any{{"outputs": map[string]string{"out": os.Getenv("CONFCTL_NETBOOT_INVENTORY_FILE")}}}); err != nil {
			os.Exit(92)
		}
	default:
		os.Exit(90)
	}
	os.Exit(0)
}

func TestNetbootRealLiteralServiceConformance(t *testing.T) {
	executable := os.Getenv("CONFCTL_TEST_SITE_EXECUTABLE")
	info, err := os.Stat(executable)
	if !filepath.IsAbs(executable) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("siteconformance requires explicit absolute CONFCTL_TEST_SITE_EXECUTABLE: %q: %v", executable, err)
	}
	for _, invalid := range []bool{false, true} {
		root := t.TempDir()
		state := filepath.Join(root, "cluster/netbootable.nix")
		if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(state, []byte("OLD\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		name := "host\"\\${inert}\b\f\x1b\u0085\u2028😀"
		if invalid {
			name = "host\x00"
		}
		var inventory bytes.Buffer
		inventory.WriteByte('{')
		for i, entry := range []struct {
			name, carrier string
			enabled       bool
		}{{name, "", true}, {"carried", "carrier", true}, {"disabled", "", false}, {"second", "", true}} {
			if i != 0 {
				inventory.WriteByte(',')
			}
			key, _ := json.Marshal(fmt.Sprintf("entry%d", i))
			inventory.Write(key)
			inventory.WriteByte(':')
			row := map[string]any{"name": entry.name, "clusterName": entry.name, "metaConfig": map[string]any{
				"spin": "nixos", "managed": false, "netboot": map[string]any{"enable": entry.enabled},
			}}
			if entry.carrier != "" {
				row["carrier"] = entry.carrier
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			inventory.Write(encoded)
		}
		inventory.WriteByte('}')
		inventoryPath := filepath.Join(root, "inventory.json")
		if err := os.WriteFile(inventoryPath, inventory.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		settingsPath := filepath.Join(root, "settings.json")
		if err := os.WriteFile(settingsPath, []byte(`{"nix":{"maxJobs":"auto","impureEval":false}}`), 0600); err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		sh, err := exec.LookPath("sh")
		if err != nil {
			t.Fatal(err)
		}
		launcher := "#!" + sh + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestNetbootInventoryHelper$", "--"}) + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, "nix"), []byte(launcher), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CONFCTL_NETBOOT_INVENTORY_HELPER", "1")
		t.Setenv("CONFCTL_NETBOOT_INVENTORY_FILE", inventoryPath)
		t.Setenv("CONFCTL_NETBOOT_SETTINGS_FILE", settingsPath)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		engine := &Engine{Root: root, Context: ctx, Color: "never", Inventory: []Machine{{Name: "stale"}}, SettingsCache: map[string]any{"nix": map[string]any{"maxJobs": "auto", "impureEval": false}}}
		out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = out
		reg := Registration{ID: "test.netboot", Protocol: ext.Version{Major: 1}, Argv: []string{executable}}
		code, invokeErr := engine.Invoke(reg, "netboot.rediscover", ext.Invocation{Root: root, Event: "rediscover.after-write"})
		os.Stdout = original
		cancel()
		_ = out.Close()
		printed, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		if invalid {
			var formatterError *ext.RPCError
			if code != 1 || !errors.As(invokeErr, &formatterError) || formatterError.Message != "unsupported NUL in Nix string" || len(printed) != 0 || string(saved) != "OLD\n" {
				t.Fatalf("NUL site name succeeded/message/write: code=%d err=%v out=%q state=%q", code, invokeErr, printed, saved)
			}
		} else {
			want := "# This file is auto-generated on confctl rediscover, changes will be lost\n" +
				"[\n  \"host\\\"\\\\\\${inert}\b\f\x1b\u0085\u2028😀\"\n  \"second\"\n]\n"
			if code != 0 || invokeErr != nil || string(saved) != want || string(printed) != "replace cluster/netbootable.nix\n" {
				t.Fatalf("site literal/order: code=%d err=%v state=%q want=%q out=%q", code, invokeErr, saved, want, printed)
			}
		}
	}
}
