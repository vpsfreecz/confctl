package harness

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestProcessHelper(t *testing.T) {
	switch os.Getenv("COMPAT_HELPER") {
	case "successful-parent-closed", "successful-parent-pty":
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessHelper")
		cmd.Env = append(os.Environ(), "COMPAT_HELPER=orphan-worker")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if os.Getenv("COMPAT_HELPER") == "successful-parent-pty" {
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		}
		if e := cmd.Start(); e != nil {
			os.Exit(8)
		}
		until := time.Now().Add(time.Second)
		for {
			if _, e := os.Stat(os.Getenv("COMPAT_CHILD_FILE")); e == nil {
				break
			}
			if time.Now().After(until) {
				os.Exit(9)
			}
			time.Sleep(time.Millisecond)
		}
		// Keep the root alive across observer ticks so ownership is proven.
		time.Sleep(100 * time.Millisecond)
		fmt.Println(cmd.Process.Pid)
		os.Exit(0)
	case "orphan-worker":
		signal.Ignore(syscall.SIGTERM)
		_ = os.WriteFile(os.Getenv("COMPAT_CHILD_FILE"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			time.Sleep(time.Hour)
		}
	case "write-netboot":
		if e := os.WriteFile("cluster/netbootable.nix", []byte("generated\n"), 0600); e != nil {
			os.Exit(8)
		}
		os.Exit(0)
	case "empty-success":
		os.Exit(0)
	case "separate-group":
		cmd := exec.Command(os.Args[0], "-test.run=TestProcessHelper")
		cmd.Env = append(os.Environ(), "COMPAT_HELPER=ignore-term")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(8)
		}
		signal.Ignore(syscall.SIGTERM)
		_ = cmd.Wait()
		os.Exit(0)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println(os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	case "pty", "pty-bytes":
		for _, fd := range []uintptr{0, 1, 2} {
			var size [4]uint16
			_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
			if e != 0 {
				os.Exit(7)
			}
		}
		if os.Getenv("COMPAT_HELPER") == "pty-bytes" {
			fmt.Fprint(os.Stdout, "out:\x00\x1b[31mą\x1b[0m\n")
			fmt.Fprint(os.Stderr, "err:\x00\x1b[32mž\x1b[0m\n")
			os.Exit(0)
		}
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Fprint(os.Stdout, "stdout:"+line)
		fmt.Fprint(os.Stderr, "stderr\n")
		os.Exit(0)
	}
}

func TestPTYDrainDeadlineAfterSuccessfulRootWait(t *testing.T) {
	master, slave, err := ptyPair()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	raw, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	var eno syscall.Errno
	if err = raw.Control(func(fd uintptr) { flags, _, eno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0) }); err != nil || eno != 0 || flags&syscall.O_NONBLOCK == 0 {
		t.Fatal("PTY setup disabled nonblocking master polling", flags, eno, err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
	cmd.Env = append(os.Environ(), "COMPAT_HELPER=empty-success", "GORACE=atexit_sleep_ms=0")
	cmd.Stdout, cmd.Stderr = slave, slave
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	// A retained controller slave deliberately keeps this real PTY open after
	// successful Wait. It is a stream-drain test, not a process ownership claim.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, master) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err = drainPTY(ctx, &wg, master); err == nil || !strings.Contains(err.Error(), "PTY drain canceled") {
		t.Fatal("post-Wait drain escaped capture deadline", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("PTY readers not released by deadline")
	}
}

func TestPTYCapturePreservesExactBytes(t *testing.T) {
	c := helperCase("pty-bytes", 3000, true)
	c.Env["GORACE"] = "atexit_sleep_ms=0"
	o, _, err := Run(context.Background(), c, os.Args[0], "", t.TempDir(), "", "")
	// These are literal PTY bytes, including ONLCR, NUL, ANSI and UTF-8. No
	// normalizer or pipe substitution participates in this capture assertion.
	if err != nil || o.Exit != 0 || o.Stdout != "out:\x00\x1b[31mą\x1b[0m\r\n" || o.Stderr != "err:\x00\x1b[32mž\x1b[0m\r\n" {
		t.Fatalf("PTY bytes changed: stdout=%q stderr=%q exit=%d error=%v", o.Stdout, o.Stderr, o.Exit, err)
	}
}

func TestSuccessfulParentRejectsAndReapsDescendants(t *testing.T) {
	for _, mode := range []string{"successful-parent-closed", "successful-parent-pty"} {
		t.Run(mode, func(t *testing.T) {
			c := helperCase(mode, 3000, mode == "successful-parent-pty")
			c.Env["COMPAT_CHILD_FILE"] = "${ROOT}/child.pid"
			c.Env["GORACE"] = "atexit_sleep_ms=0"
			start := time.Now()
			o, _, err := Run(context.Background(), c, os.Args[0], "", t.TempDir(), "", "")
			if err == nil || !strings.Contains(err.Error(), "leftover live descendants") || o.Exit != 0 {
				t.Fatal("successful root accepted with live child work", o.Exit, err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("successful root PTY/capture cleanup exceeded its bound")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(o.Stdout))
			if err != nil {
				t.Fatal(o.Stdout, err)
			}
			if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatal("successful root left a separate-group child", pid, err)
			}
		})
	}
}

func TestInstalledSuccessfulParentRejectsAndReapsDescendant(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	env := append(os.Environ(), "COMPAT_HELPER=successful-parent-closed", "GORACE=atexit_sleep_ms=0", "COMPAT_CHILD_FILE="+filepath.Join(root, "child.pid"))
	o, _, err := RunInstalled(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, root, env)
	if err == nil || !strings.Contains(err.Error(), "leftover live descendants") || o.Exit != 0 {
		t.Fatal("installed capture accepted live child work", o.Exit, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(o.Stdout))
	if err != nil {
		t.Fatal(o.Stdout, err)
	}
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("installed descendant not reaped", pid, err)
	}
}

func TestWarmMutationResetRestoresNetbootBeforeState(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			c := helperCase("write-netboot", 3000, false)
			c.Files = map[string]File{"cluster/.keep": {}, "data/.keep": {}}
			if initial {
				c.Files["cluster/netbootable.nix"] = File{Text: "initial\n", Mode: 0600}
			}
			cache := t.TempDir()
			for i := 0; i < 2; i++ {
				o, _, err := RunCached(context.Background(), c, os.Args[0], "", t.TempDir(), "", "", cache)
				if err != nil {
					t.Fatal(err)
				}
				before, present := o.Before["cluster/netbootable.nix"]
				if present != initial || (initial && before.Text != "initial\n") || o.After["cluster/netbootable.nix"].Text != "generated\n" {
					t.Fatal("warm mutation inputs were retained", o.Before, o.After)
				}
				if i == 0 {
					if err = os.WriteFile(filepath.Join(cache, "config/data/netbootable.nix"), []byte("unrelated cache\n"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if o.Before["data/netbootable.nix"].Text != "unrelated cache\n" {
					t.Fatal("reset changed unrelated warm state")
				}
			}
		})
	}
}
func helperCase(mode string, deadline int, tty bool) Case {
	return Case{Schema: 1, ID: "process-" + mode, Tier: "fixture-process", Sources: []Source{{Revision: OracleRevision, Path: "lib/confctl/machine_control.rb", Lines: "157-183"}}, Argv: []string{"-test.run=TestProcessHelper"}, Env: map[string]string{"COMPAT_HELPER": mode}, DeadlineMS: deadline, TTY: tty, Stdin: "yes\n", Files: map[string]File{}}
}
func TestDriverCancellationReapsIgnoringChild(t *testing.T) {
	c := helperCase("ignore-term", 80, false)
	o, _, e := Run(context.Background(), c, os.Args[0], "", t.TempDir(), "", "")
	if e == nil || !strings.Contains(e.Error(), "deadline") {
		t.Fatal("deadline was not a failure", e)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(o.Stdout))
	if err != nil {
		t.Fatal(o.Stdout, err)
	}
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("child still exists", pid, err)
	}
	if o.Signal != "killed" {
		t.Fatal(o.Signal)
	}
}
func TestPTYHasRealThreeTTYDescriptors(t *testing.T) {
	o, _, e := Run(context.Background(), helperCase("pty", 5000, true), os.Args[0], "", t.TempDir(), "", "")
	if e != nil {
		t.Fatal(e)
	}
	if o.Exit != 0 || o.Stdout != "stdout:yes\r\n" || o.Stderr != "stderr\r\n" {
		t.Fatalf("%#v", o)
	}
}

func TestDriverReapsSeparateGroupDescendant(t *testing.T) {
	o, _, e := Run(context.Background(), helperCase("separate-group", 150, false), os.Args[0], "", t.TempDir(), "", "")
	if e == nil {
		t.Fatal("expected deadline")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(o.Stdout))
	if err != nil {
		t.Fatal(o.Stdout, err)
	}
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("descendant not reaped: %d %v", pid, err)
	}
}
