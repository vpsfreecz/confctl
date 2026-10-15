package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/vpsfreecz/confctl/internal/git"
)

type inheritedDescriptor struct{ Device, Inode uint64 }

func inheritedFileIdentity(f *os.File) inheritedDescriptor {
	s, err := f.Stat()
	if err != nil {
		panic(err)
	}
	v := s.Sys().(*syscall.Stat_t)
	return inheritedDescriptor{v.Dev, v.Ino}
}
func TestInheritedCommitDescriptorHelper(t *testing.T) {
	if os.Getenv("CONFCTL_INHERITED_HELPER") != "1" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	text, err := io.ReadAll(os.Stdin)
	if err != nil {
		panic(err)
	}
	record := struct {
		Descriptors       []inheritedDescriptor
		CWD, Value, Input string
		Group             int
	}{[]inheritedDescriptor{inheritedFileIdentity(os.Stdin), inheritedFileIdentity(os.Stdout), inheritedFileIdentity(os.Stderr)}, cwd, os.Getenv("CONFCTL_INHERITED_VALUE"), string(text), syscall.Getpgrp()}
	if err = json.NewEncoder(os.Stdout).Encode(record); err != nil {
		panic(err)
	}
	fmt.Fprint(os.Stderr, "inherited diagnostic\n")
	code, _ := strconv.Atoi(os.Getenv("CONFCTL_INHERITED_EXIT"))
	os.Exit(code)
}
func inheritedFiles(t *testing.T, root string) (*os.File, *os.File, *os.File) {
	t.Helper()
	var f [3]*os.File
	for i, name := range []string{"stdin", "stdout", "stderr"} {
		v, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		f[i] = v
		t.Cleanup(func() { _ = v.Close() })
	}
	return f[0], f[1], f[2]
}
func TestInheritedCommitActualDescriptorsStatusAndStartFailure(t *testing.T) {
	helper := materializeCommitRelay(t)
	t.Setenv("CONFCTL_INHERITED_HELPER", "1")
	t.Setenv("CONFCTL_INHERITED_VALUE", "literal value")
	for _, code := range []int{0, 9} {
		root := t.TempDir()
		in, out, stderr := inheritedFiles(t, root)
		if _, err := in.WriteString("input bytes\n"); err != nil {
			t.Fatal(err)
		}
		_, _ = in.Seek(0, 0)
		t.Setenv("CONFCTL_INHERITED_EXIT", strconv.Itoa(code))
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r, err := runInheritedCommitWithRelay(ctx, root, []string{os.Args[0], "-test.run=^TestInheritedCommitDescriptorHelper$"}, in, out, stderr, helper)
		cancel()
		if err != nil || r.ExitCode != code || r.Signal != "" || len(r.Stdout) != 0 || len(r.Stderr) != 0 {
			t.Fatal(r, err)
		}
		bytes, _ := os.ReadFile(out.Name())
		var record struct {
			Descriptors       []inheritedDescriptor
			CWD, Value, Input string
			Group             int
		}
		if err = json.Unmarshal(bytes, &record); err != nil {
			t.Fatal(string(bytes), err)
		}
		want := []inheritedDescriptor{inheritedFileIdentity(in), inheritedFileIdentity(out), inheritedFileIdentity(stderr)}
		for i, v := range want {
			if record.Descriptors[i] != v {
				t.Fatal("descriptor replaced by capture pipe", record, want)
			}
		}
		if record.CWD != root || record.Value != "literal value" || record.Input != "input bytes\n" || record.Group == syscall.Getpgrp() {
			t.Fatal(record)
		}
		bytes, _ = os.ReadFile(stderr.Name())
		if string(bytes) != "inherited diagnostic\n" || commitStdoutTTY(out) {
			t.Fatal(string(bytes))
		}
		r, err = runInheritedCommitWithRelay(context.Background(), root, []string{filepath.Join(root, "missing")}, in, out, stderr, helper)
		if err == nil || r.ExitCode != -1 {
			t.Fatal(r, err)
		}
	}
}
func TestInheritedCommitCancellationEscalatesAndReaps(t *testing.T) {
	root := t.TempDir()
	helper := materializeCommitRelay(t)
	t.Setenv("CORE_SUPERVISOR_HELPER", "worker")
	t.Setenv("CORE_SUPERVISOR_ROOT", root)
	in, out, stderr := inheritedFiles(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct {
		r Result
		e error
	}, 1)
	go func() {
		r, e := runInheritedCommitWithRelay(ctx, root, []string{os.Args[0], "-test.run=^TestSupervisorHelper$", "worker"}, in, out, stderr, helper)
		done <- struct {
			r Result
			e error
		}{r, e}
	}()
	until := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "worker.pid")); err == nil {
			break
		}
		if time.Now().After(until) {
			cancel()
			t.Fatal("owned worker did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.e, context.Canceled) || result.r.Signal != "killed" || result.r.ExitCode != -1 {
			t.Fatal(result)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancel did not wait/reap")
	}
	b, err := os.ReadFile(filepath.Join(root, "worker.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("owned worker remains", pid, err)
	}
}

// A disposable controlling terminal exercises the actual parent-descriptor
// Foreground mechanism and restoration. Git/editor/stop-resume parity remains a
// separate original/candidate integration gate.
func TestInheritedCommitPTYHelper(t *testing.T) {
	mode := os.Getenv("CONFCTL_INHERITED_PTY")
	if mode == "" {
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		panic(err)
	}
	defer tty.Close()
	root := os.Getenv("CONFCTL_INHERITED_PTY_ROOT")
	helper := os.Getenv("CONFCTL_INHERITED_RELAY_TEST_PATH")
	if mode == "sentinel" {
		if err = os.WriteFile(filepath.Join(root, "unexpected-git-start"), []byte("started"), 0600); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	if mode == "holder" {
		if err = os.WriteFile(filepath.Join(root, "holder-ready"), []byte("ready"), 0600); err != nil {
			panic(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "worker" || mode == "background-worker" {
		fg, err := commitTerminalGroup(tty)
		if err != nil || mode == "worker" && fg != int32(syscall.Getpgrp()) {
			panic(fmt.Sprint("worker not foreground", fg, err))
		}
		if mode == "background-worker" {
			holder, _ := strconv.Atoi(os.Getenv("CONFCTL_HOLDER_GROUP"))
			if fg != int32(holder) || fg == int32(syscall.Getpgrp()) {
				panic("background caller stole terminal")
			}
		}
		if err = os.WriteFile(filepath.Join(root, "foreground-"+mode), []byte(strconv.Itoa(int(fg))), 0600); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	original, err := commitTerminalGroup(tty)
	if err != nil || original != int32(syscall.Getpgrp()) {
		panic("caller does not own terminal")
	}
	_ = os.Setenv("CONFCTL_INHERITED_PTY", "worker")
	r, err := runInheritedCommitWithRelay(context.Background(), root, []string{os.Args[0], "-test.run=^TestInheritedCommitPTYHelper$"}, os.Stdin, os.Stdout, os.Stderr, helper)
	if err != nil || r.ExitCode != 0 {
		panic(fmt.Sprint(r, err))
	}
	restored, err := commitTerminalGroup(tty)
	if err != nil || restored != original {
		panic("foreground not restored after wait")
	}
	if _, err = runInheritedCommitWithRelay(context.Background(), root, []string{filepath.Join(root, "missing")}, os.Stdin, os.Stdout, os.Stderr, helper); err == nil {
		panic("missing executable accepted")
	}
	var ordinaryMarker *git.CommitAdapterError
	if errors.As(err, &ordinaryMarker) {
		panic("ordinary Git spawn failure classified as adapter failure")
	}
	restored, err = commitTerminalGroup(tty)
	if err != nil || restored != original {
		panic("foreground not restored after start failure")
	}
	// Compose the actual terminal adapter with the real owning Commit workflow.
	// A malformed helper must fail before Git starts and retain its diagnostic.
	_ = os.Setenv("CONFCTL_INHERITED_PTY", "sentinel")
	var adapterErr error
	err = git.Commit(context.Background(), root, "message", false, false, func(ctx context.Context, dir string, argv []string) (int, error) {
		if strings.Join(argv, " ") != "git commit -m message flake.lock" {
			panic("commit argv changed")
		}
		result, e := runInheritedCommitWithRelay(ctx, dir, []string{os.Args[0], "-test.run=^TestInheritedCommitPTYHelper$"}, os.Stdin, os.Stdout, os.Stderr, os.Getenv("CONFCTL_INHERITED_BAD_RELAY_TEST_PATH"))
		adapterErr = e
		return result.ExitCode, e
	})
	var marker *git.CommitAdapterError
	if err == nil || err != adapterErr || !errors.As(err, &marker) || !strings.Contains(err.Error(), "invalid commit relay startup record") {
		panic(fmt.Sprint("adapter diagnostic lost", err))
	}
	if _, e := os.Stat(filepath.Join(root, "unexpected-git-start")); !errors.Is(e, os.ErrNotExist) {
		panic("Git ran after adapter failure")
	}
	// This Setsid test caller is an orphaned group. Its native terminal-stop
	// relay can return without physically stopping it; actual consumed Git
	// events prove the adapter path, not real shell/job/editor parity.
	bash := os.Getenv("CONFCTL_INHERITED_BASH_TEST_PATH")
	pidPath, resumedPath := filepath.Join(root, "stopping-child.pid"), filepath.Join(root, "stopping-child.resumed")
	stopScript := `builtin printf '%s' "$BASHPID" > "$1"; builtin kill -s TSTP "$BASHPID"; builtin printf 'first\n' >> "$2"; builtin kill -s TSTP "$BASHPID"; builtin printf 'second\n' >> "$2"; exit 9`
	var consumed []commitWaitEvent
	r, err = runInheritedCommitObserved(context.Background(), root, []string{bash, "-p", "-c", stopScript, "fixture", pidPath, resumedPath}, os.Stdin, os.Stdout, os.Stderr, helper, func(pid, flags int, info *commitSiginfo) error {
		err := rawCommitWait(pid, flags, info)
		if err == nil && flags&syscall.WNOWAIT == 0 {
			e, de := decodeCommitEvent(info, pid)
			if de != nil {
				return de
			}
			if e.pid != 0 {
				consumed = append(consumed, e)
			}
		}
		return err
	})
	stops := 0
	for _, event := range consumed {
		if event.code == commitStopped && event.status == int(syscall.SIGTSTP) {
			stops++
		}
	}
	bytes, e := os.ReadFile(resumedPath)
	if err != nil || r.ExitCode != 9 || stops != 2 || e != nil || string(bytes) != "first\nsecond\n" {
		panic(fmt.Sprint("actual stop/ACK/continue failed", r, err, consumed, string(bytes), e))
	}
	r, err = runInheritedCommitWithRelay(context.Background(), root, []string{bash, "-p", "-c", `builtin kill -s KILL "$BASHPID"`}, os.Stdin, os.Stdout, os.Stderr, helper)
	if err != nil || r.ExitCode != -1 || r.Signal != "killed" {
		panic(fmt.Sprint("ordinary Git signal marked as adapter failure", r, err))
	}
	// A real stopped child and a wrong ACK exercise owned cancellation and
	// reaping after adapter failure, before the one exit-consuming Cmd.Wait.
	_ = os.Remove(resumedPath)
	r, err = runInheritedCommitWithRelay(context.Background(), root, []string{bash, "-p", "-c", stopScript, "fixture", pidPath, resumedPath}, os.Stdin, os.Stdout, os.Stderr, os.Getenv("CONFCTL_INHERITED_WRONG_RELAY_TEST_PATH"))
	if err == nil || !errors.As(err, &marker) || !strings.Contains(err.Error(), "unexpected or duplicate commit relay acknowledgement") {
		panic(fmt.Sprint("wrong ACK accepted", r, err))
	}
	pidBytes, e := os.ReadFile(pidPath)
	if e != nil {
		panic(e)
	}
	pid, e := strconv.Atoi(string(pidBytes))
	if e != nil {
		panic(e)
	}
	if e = syscall.Kill(pid, 0); e != syscall.ESRCH {
		panic(fmt.Sprint("adapter failure left owned child", pid, e))
	}
	if _, e = os.Stat(resumedPath); !errors.Is(e, os.ErrNotExist) {
		panic("wrong ACK resumed stopped child")
	}
	// Temporarily give another owned group the terminal, then call the adapter
	// from the background. No real shell/editor or stop/continue bridge is used.
	_ = os.Setenv("CONFCTL_INHERITED_PTY", "holder")
	holder := exec.Command(os.Args[0], "-test.run=^TestInheritedCommitPTYHelper$")
	holder.Stdin, holder.Stdout, holder.Stderr = os.Stdin, os.Stdout, os.Stderr
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = holder.Start(); err != nil {
		panic(err)
	}
	defer func() { _ = restoreCommitForeground(tty, original); _ = holder.Process.Kill(); _ = holder.Wait() }()
	until := time.Now().Add(30 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(root, "holder-ready")); e == nil {
			break
		}
		if time.Now().After(until) {
			panic("holder did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err = restoreCommitForeground(tty, int32(holder.Process.Pid)); err != nil {
		panic(err)
	}
	_ = os.Setenv("CONFCTL_INHERITED_PTY", "background-worker")
	_ = os.Setenv("CONFCTL_HOLDER_GROUP", strconv.Itoa(holder.Process.Pid))
	r, err = runInheritedCommitWithRelay(context.Background(), root, []string{os.Args[0], "-test.run=^TestInheritedCommitPTYHelper$"}, os.Stdin, os.Stdout, os.Stderr, helper)
	if err != nil || r.ExitCode != 0 {
		panic(fmt.Sprint(r, err))
	}
	restored, err = commitTerminalGroup(tty)
	if err != nil || restored != int32(holder.Process.Pid) {
		panic("background invocation changed foreground group")
	}
	if err = restoreCommitForeground(tty, original); err != nil {
		panic(err)
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	_ = os.Setenv("CORE_SUPERVISOR_HELPER", "worker")
	_ = os.Setenv("CORE_SUPERVISOR_ROOT", root)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		until := time.Now().Add(30 * time.Second)
		for {
			if _, e := os.Stat(filepath.Join(root, "worker.pid")); e == nil {
				break
			}
			if time.Now().After(until) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	r, err = runInheritedCommitWithRelay(ctx, root, []string{os.Args[0], "-test.run=^TestSupervisorHelper$", "worker"}, os.Stdin, os.Stdout, os.Stderr, helper)
	if !errors.Is(err, context.Canceled) || r.Signal != "killed" {
		panic(fmt.Sprint(r, err))
	}
	restored, err = commitTerminalGroup(tty)
	if err != nil || restored != original {
		panic("foreground not restored after cancellation")
	}
	if err = os.WriteFile(filepath.Join(root, "restored"), []byte(strconv.Itoa(int(restored))), 0600); err != nil {
		panic(err)
	}
	os.Exit(0)
}
func TestInheritedCommitForegroundAndRestoration(t *testing.T) {
	root := t.TempDir()
	helper := materializeCommitRelay(t)
	shebang, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	badHelper := filepath.Join(root, "bad-relay.bash")
	badBody := strings.SplitN(string(shebang), "\n", 2)[0] + "\nbuiltin printf 'invalid startup\\n' >&4\nbuiltin read -r request <&3\n"
	if err = os.WriteFile(badHelper, []byte(badBody), 0700); err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	wrongHelper := filepath.Join(root, "wrong-relay.bash")
	wrongBody := strings.SplitN(string(shebang), "\n", 2)[0] + "\nbuiltin printf '" + strings.ReplaceAll(validCommitStartup, "\n", `\n`) + "' >&4\nbuiltin read -r request <&3\nbuiltin printf 'ACK TTIN\\n' >&4\nbuiltin read -r request <&3\n"
	if err = os.WriteFile(wrongHelper, []byte(wrongBody), 0700); err != nil {
		t.Fatal(err)
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	var unlock int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	if errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number)))
	if errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	_, out, stderr := inheritedFiles(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInheritedCommitPTYHelper$")
	cmd.Env = append(os.Environ(), "CONFCTL_INHERITED_PTY=caller", "CONFCTL_INHERITED_PTY_ROOT="+root, "CONFCTL_INHERITED_RELAY_TEST_PATH="+helper, "CONFCTL_INHERITED_BAD_RELAY_TEST_PATH="+badHelper, "CONFCTL_INHERITED_WRONG_RELAY_TEST_PATH="+wrongHelper, "CONFCTL_INHERITED_BASH_TEST_PATH="+bash)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, out, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Run(); err != nil {
		b, _ := os.ReadFile(stderr.Name())
		t.Fatal(err, string(b))
	}
	worker, err := os.ReadFile(filepath.Join(root, "foreground-worker"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(filepath.Join(root, "restored"))
	if err != nil || strings.TrimSpace(string(worker)) == strings.TrimSpace(string(restored)) {
		t.Fatal("child handoff missing", string(worker), string(restored), err)
	}
	if !commitStdoutTTY(slave) {
		t.Fatal("stdout-only terminal check lost terminal")
	}
}
