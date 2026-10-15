package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type sshInheritedRecord struct {
	PID, Group, Foreground, UID int
	CWD, Sentinel, Input        string
	FD                          [3]inheritedDescriptor
	Argv                        []string
}

type sshInheritedResult struct {
	Result                   Result
	Error                    string
	Before, After            unix.Termios
	PeerAlive, GroupRetained bool
}

func sshWriteTestJSON(path string, value any) {
	b, err := json.Marshal(value)
	if err == nil {
		err = os.WriteFile(path, b, 0600)
	}
	if err != nil {
		panic(err)
	}
}

// Fake native executable roles, launched only by this finite test family. No
// production SSH, local shell, NSS lookup, network or Git runner is exercised.
func TestSSHInheritedChild(t *testing.T) {
	role := os.Getenv("CORE_SSH_INHERITED_ROLE")
	if role == "" {
		return
	}
	root, mode := os.Getenv("CORE_SSH_INHERITED_ROOT"), os.Getenv("CORE_SSH_INHERITED_MODE")
	switch role {
	case "peer":
		sshWriteTestJSON(filepath.Join(root, "peer.json"), os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	case "leaf":
		cwd, _ := os.Getwd()
		group, err := sshTerminalGroup(os.Stdin)
		if err != nil {
			panic(err)
		}
		record := sshInheritedRecord{PID: os.Getpid(), Group: syscall.Getpgrp(), Foreground: int(group), UID: os.Getuid(), CWD: cwd, Sentinel: os.Getenv("CORE_SSH_SENTINEL"), Argv: os.Args[2:]}
		record.FD = [3]inheritedDescriptor{inheritedFileIdentity(os.Stdin), inheritedFileIdentity(os.Stdout), inheritedFileIdentity(os.Stderr)}
		var attrs unix.Termios
		if err := sshTermios(os.Stdin, syscall.TCGETS, &attrs); err != nil {
			panic(err)
		}
		attrs.Lflag &^= syscall.ECHO
		if err := sshTermios(os.Stdin, syscall.TCSETS, &attrs); err != nil {
			panic(err)
		}
		if mode == "cancel" {
			signal.Ignore(syscall.SIGTERM)
		}
		sshWriteTestJSON(filepath.Join(root, "leaf.json"), record)
		fmt.Fprintln(os.Stdout, "SSH READY")
		fmt.Fprintln(os.Stderr, "SSH STDERR")
		if mode == "cancel" || mode == "interrupt" {
			for {
				time.Sleep(time.Hour)
			}
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			panic(err)
		}
		record.Input = line
		sshWriteTestJSON(filepath.Join(root, "leaf.json"), record)
		fmt.Fprintf(os.Stdout, "SSH INPUT %s", line)
		os.Exit(9) // ignored false result is tested at the family/CLI boundary
	case "runner":
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigs)
		go func() {
			select {
			case <-sigs:
				cancel()
			case <-ctx.Done():
			}
		}()
		peer := exec.Command(os.Args[0], "-test.run=^TestSSHInheritedChild$")
		peer.Env = append(os.Environ(), "CORE_SSH_INHERITED_ROLE=peer")
		peer.Stdin, peer.Stdout, peer.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := peer.Start(); err != nil {
			panic(err)
		}
		var summary sshInheritedResult
		if err := sshTermios(os.Stdin, syscall.TCGETS, &summary.Before); err != nil {
			panic(err)
		}
		os.Setenv("CORE_SSH_INHERITED_ROLE", "leaf")
		argv := []string{os.Args[0], "-test.run=^TestSSHInheritedChild$", "--", "literal arg%25", ""}
		if mode == "spawn" {
			argv = []string{filepath.Join(root, "missing-executable")}
		}
		var err error
		summary.Result, err = runInheritedSSH(ctx, root, argv, os.Stdin, os.Stdout, os.Stderr)
		if err != nil {
			summary.Error = err.Error()
		}
		group, groupErr := sshTerminalGroup(os.Stdin)
		summary.GroupRetained = groupErr == nil && int(group) == syscall.Getpgrp()
		if err := sshTermios(os.Stdin, syscall.TCGETS, &summary.After); err != nil {
			panic(err)
		}
		summary.PeerAlive = peer.Process.Signal(syscall.Signal(0)) == nil
		_ = peer.Process.Kill()
		_ = peer.Wait()
		sshWriteTestJSON(filepath.Join(root, "result.json"), summary)
	default:
		panic("unexpected inherited role")
	}
	os.Exit(0)
}

func TestSSHInheritedPTYForegroundSignalsCancelReapAndRestoration(t *testing.T) {
	for _, mode := range []string{"read", "interrupt", "cancel", "spawn"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			master, slave := openCommitTestPTY(t)
			identity := inheritedFileIdentity(slave)
			transcript, readDone := sshReadPTY(master)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSSHInheritedChild$")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CORE_SSH_INHERITED_ROOT="+root, "CORE_SSH_INHERITED_MODE="+mode, "CORE_SSH_INHERITED_ROLE=runner", "CORE_SSH_SENTINEL=inherited%value")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = slave.Close()
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			defer func() {
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("runner not reaped")
				}
			}()
			if mode != "spawn" {
				sshAwaitTerminal(t, transcript, "SSH READY")
				switch mode {
				case "read":
					_, _ = master.WriteString("finite input\n")
				case "interrupt":
					_, _ = master.Write([]byte{3}) // terminal VINTR to this owned job
				case "cancel":
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case <-done:
				if waitErr != nil {
					t.Fatal(waitErr, transcript.String())
				}
			case <-time.After(8 * time.Second):
				t.Fatal("inherited run did not finish", transcript.String())
			}
			<-readDone
			var result sshInheritedResult
			b, err := os.ReadFile(filepath.Join(root, "result.json"))
			if err != nil || json.Unmarshal(b, &result) != nil {
				t.Fatal(string(b), err)
			}
			if (mode != "interrupt" && !result.PeerAlive) || !result.GroupRetained || result.Before != result.After {
				t.Fatal("caller group, peer or terminal attributes changed", result)
			}
			switch mode {
			case "read":
				if result.Error != "" || result.Result.ExitCode != 9 || !strings.Contains(transcript.String(), "SSH INPUT finite input\r\n") {
					t.Fatal(result, transcript.String())
				}
			case "interrupt":
				if result.Error != context.Canceled.Error() {
					t.Fatal(result)
				}
			case "cancel":
				if result.Error != context.Canceled.Error() || result.Result.Signal != syscall.SIGKILL.String() {
					t.Fatal(result)
				}
			case "spawn":
				if result.Error == "" || result.Result.ExitCode != -1 {
					t.Fatal(result)
				}
			}
			if mode != "spawn" {
				var record sshInheritedRecord
				b, err = os.ReadFile(filepath.Join(root, "leaf.json"))
				if err != nil || json.Unmarshal(b, &record) != nil {
					t.Fatal(string(b), err)
				}
				if record.Group != cmd.Process.Pid || record.Foreground != record.Group || record.CWD != root || record.UID != os.Getuid() || record.Sentinel != "inherited%value" || record.FD != [3]inheritedDescriptor{identity, identity, identity} || !reflect.DeepEqual(record.Argv, []string{"--", "literal arg%25", ""}) {
					t.Fatal("literal inherited boundary changed", record)
				}
				if err := syscall.Kill(record.PID, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatal("SSH child not reaped", record.PID, err)
				}
			}
		})
	}
}

func TestSSHInheritedPipeFailureAndCanceledBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	in, err := os.Create(filepath.Join(root, "in"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(filepath.Join(root, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	r, err := runInheritedSSH(context.Background(), root, []string{filepath.Join(root, "absent")}, in, out, out)
	if err == nil || r.ExitCode != -1 {
		t.Fatal(r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err = runInheritedSSH(ctx, root, []string{filepath.Join(root, "absent")}, in, out, out)
	if !errors.Is(err, context.Canceled) || r.ExitCode != -1 {
		t.Fatal(r, err)
	}
}

func TestSSHRootShellNSSLiteralRequestAndFailures(t *testing.T) {
	for _, tc := range []struct {
		output        string
		status        int
		want, failure string
	}{
		{"root:x:0:0:Root:/root:/literal/root shell\n", 0, "/literal/root shell", ""},
		{"alias:x:0:0::/:/bin/other\n", 0, "/bin/other", ""},
		{"root:x:1:0::/:/bad\n", 0, "", "invalid root passwd record"},
		{"", 2, "", "exit status 2"},
	} {
		t.Run(strconv.Itoa(tc.status)+tc.output, func(t *testing.T) {
			trace := sshExecutables(t, map[string]sshChildStimulus{"getent": {Out: tc.output, Exit: tc.status}})
			e := connectionEngine(t)
			t.Setenv("SHELL", "/wrong/environment-shell")
			shell, err := e.sshRootShell()
			if tc.failure == "" {
				if err != nil || shell != tc.want {
					t.Fatal(shell, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.failure) {
				t.Fatal(shell, err)
			}
			record := sshRecord(t, trace, "getent", 1)
			if !reflect.DeepEqual(record.Argv, []string{"passwd", "0"}) || record.CWD != e.Root || record.StdinMode != "open" {
				t.Fatal(record)
			}
		})
	}
	t.Setenv("PATH", t.TempDir())
	e := connectionEngine(t)
	if _, err := e.sshRootShell(); err == nil {
		t.Fatal("missing getent silently guessed a shell")
	}
	if _, err := e.sshInteractiveProcess(context.Background(), e.Root, []string{"ssh"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("interactive streams were copied through pipes")
	}
}
