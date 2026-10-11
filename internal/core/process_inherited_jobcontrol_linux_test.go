package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func materializeCommitRelay(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../../libexec/confctl-commit-stop-relay.bash")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "#!@BASH@ -p\n"
	if !strings.HasPrefix(string(body), prefix) || fmt.Sprintf("%x", sha256.Sum256(body[len(prefix):])) != "fdb5b687cc039bb0a5c4ac0998d7b5a1132731c314bb89a48270a68569e77c5c" {
		t.Fatal("assessed relay body changed")
	}
	bash, err := exec.LookPath("bash") // Test-only declared environment, never an application fallback.
	if err != nil {
		t.Fatal(err)
	}
	bash, err = filepath.Abs(bash)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "relay.bash")
	if err = os.WriteFile(p, []byte(strings.Replace(string(body), "@BASH@", bash, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func putCommitEvent(info *commitSiginfo, e commitWaitEvent) {
	b := unsafe.Slice((*byte)(unsafe.Pointer(info)), 128)
	put := func(offset, value int) { binary.NativeEndian.PutUint32(b[offset:], uint32(value)) }
	code, pid := 8, 12
	if strings.HasPrefix(runtime.GOARCH, "mips") {
		code = 4
	}
	if unsafe.Sizeof(uintptr(0)) == 8 {
		pid = 16
	}
	put(0, int(syscall.SIGCHLD))
	put(code, e.code)
	put(pid, e.pid)
	put(pid+8, e.status)
}

func TestCommitWaitABIAndPeekConsumeRaces(t *testing.T) {
	if unsafe.Sizeof(commitSiginfo{}) != 128 || unsafe.Alignof(commitSiginfo{}) < unsafe.Alignof(uintptr(0)) {
		t.Fatal("invalid siginfo ABI")
	}
	const pid = 123
	peek, consume := syscall.WEXITED|syscall.WSTOPPED|syscall.WCONTINUED|syscall.WNOWAIT, syscall.WSTOPPED|syscall.WCONTINUED|syscall.WNOHANG
	steps := []struct {
		flags int
		e     commitWaitEvent
		err   error
	}{
		{peek, commitWaitEvent{}, syscall.EINTR},
		{peek, commitWaitEvent{pid, commitStopped, int(syscall.SIGTSTP)}, nil},
		{consume, commitWaitEvent{}, nil},
		{peek, commitWaitEvent{pid, commitStopped, int(syscall.SIGTTIN)}, nil},
		{consume, commitWaitEvent{pid, commitContinued, int(syscall.SIGCONT)}, nil},
	}
	n := 0
	e, err := waitCommitEvent(pid, func(gotPID, flags int, info *commitSiginfo) error {
		if gotPID != pid || n >= len(steps) || flags != steps[n].flags || *info != (commitSiginfo{}) {
			t.Fatalf("unscoped/nonzero wait %d %x %d", gotPID, flags, n)
		}
		s := steps[n]
		n++
		if s.e.pid != 0 {
			putCommitEvent(info, s.e)
		}
		return s.err
	})
	if err != nil || n != len(steps) || e.code != commitContinued {
		t.Fatal(e, err, n)
	}
	for _, code := range []int{commitExited, commitKilled, commitDumped} {
		n = 0
		e, err = waitCommitEvent(pid, func(gotPID, flags int, info *commitSiginfo) error {
			n++
			if gotPID != pid || flags != peek {
				t.Fatal("exit consumed", flags)
			}
			putCommitEvent(info, commitWaitEvent{pid, code, 7})
			return nil
		})
		if err != nil || e.code != code || n != 1 {
			t.Fatal(e, err, n)
		}
	}
	for _, bad := range []commitWaitEvent{{pid + 1, commitStopped, 20}, {pid, 7, 0}} {
		var info commitSiginfo
		putCommitEvent(&info, bad)
		if _, err = decodeCommitEvent(&info, pid); err == nil {
			t.Fatal("unexpected event accepted", bad)
		}
	}
	n = 0
	_, err = waitCommitEvent(pid, func(_ int, flags int, info *commitSiginfo) error {
		n++
		code := commitStopped
		if flags == consume {
			code = commitExited
		}
		putCommitEvent(info, commitWaitEvent{pid, code, 0})
		return nil
	})
	if err == nil || n != 2 {
		t.Fatal("raw exit consumption accepted", err)
	}
	_, err = waitCommitEvent(pid, func(int, int, *commitSiginfo) error { return syscall.ECHILD })
	if !errors.Is(err, syscall.ECHILD) {
		t.Fatal(err)
	}
}

func TestCommitWaitActualExitHasOneConsumingWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommitRelayProbeHelper$")
	cmd.Env = append(os.Environ(), "CONFCTL_COMMIT_RELAY_PROBE=exit")
	owned := prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owned.bind(cmd.Process.Pid)
	e, err := waitCommitEvent(cmd.Process.Pid, rawCommitWait)
	if err != nil {
		_ = owned.cancel()
	}
	waitErr := cmd.Wait()
	owned.finish()
	if err != nil || e.pid != cmd.Process.Pid || e.code != commitExited || e.status != 7 || waitErr == nil || cmd.ProcessState.ExitCode() != 7 {
		t.Fatal(e, err, waitErr)
	}
	var info commitSiginfo
	if err = rawCommitWait(cmd.Process.Pid, syscall.WEXITED|syscall.WNOHANG, &info); !errors.Is(err, syscall.ECHILD) {
		t.Fatal("child not reaped by Cmd.Wait", err)
	}
}

const validCommitStartup = "SigBlk:\t0000000000000000\nSigIgn:\t0000000000000000\nSigCgt:\t0000000000000000\nThreads:\t1\nREADY\n"

func TestCommitSignalStartupAndReplyBoundaries(t *testing.T) {
	ignored := fmt.Sprintf("SigIgn:\t%016x\n", uint64(1)<<(uint(syscall.SIGTSTP)-1))
	blocked := fmt.Sprintf("SigBlk:\t%016x\n", uint64(1)<<(uint(syscall.SIGTTIN)-1))
	text := "Threads:\t1\nSigCgt:\t0000000000000000\n" + ignored + blocked + "READY\n"
	s, err := readCommitStartup(bufio.NewReader(strings.NewReader(text)))
	if err != nil || s.raw["SigIgn"] != ignored || s.eligible(syscall.SIGTSTP) || s.eligible(syscall.SIGTTIN) || !s.eligible(syscall.SIGTTOU) {
		t.Fatal(s, err)
	}
	for _, bad := range []string{
		strings.Replace(validCommitStartup, "Threads:\t1", "Threads:\t2", 1),
		strings.Replace(validCommitStartup, "0000000000000000", "000000000000000", 1),
		"SigBlk:\t0000000000000000\n" + validCommitStartup,
		strings.Replace(validCommitStartup, "SigIgn:\t0000000000000000\n", "", 1),
		strings.TrimSuffix(validCommitStartup, "\n"), "unexpected\n" + validCommitStartup,
	} {
		if _, err = readCommitStartup(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Fatal("bad startup accepted", bad)
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU} {
		got, err := parseCommitACK("ACK " + commitStopName(sig) + "\n")
		if err != nil || got != sig {
			t.Fatal(got, err)
		}
		state := commitSignalState{caught: uint64(1) << (uint(sig) - 1)}
		if state.eligible(sig) {
			t.Fatal("caught signal eligible", sig)
		}
	}
	for _, bad := range []string{"ACK STOP\n", "ACK TSTP", "ACK TSTP extra\n", "READY\n", "ACK TTIN\r\n"} {
		if _, err = parseCommitACK(bad); err == nil {
			t.Fatal("bad ACK accepted", bad)
		}
	}
}

func TestCommitRelayCanceledBeforeStartupHasNoChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A canceled context must return before opening pipes or using either
	// deliberately unusable dependency, and cannot start a helper.
	relay, err := startCommitStopRelay(ctx, "not-an-executable", nil)
	if relay != nil || err != context.Canceled {
		t.Fatal(relay, err)
	}
}

type commitLoopFixture struct {
	ctx        context.Context
	cancel     context.CancelFunc
	events     chan commitObservation
	replies    chan commitRelayReply
	helperDone chan struct{}
	requests   chan syscall.Signal
	actions    chan string
	done       chan error
}

func newCommitLoopFixture(t *testing.T, eligible bool) *commitLoopFixture {
	t.Helper()
	f := &commitLoopFixture{events: make(chan commitObservation, 8), replies: make(chan commitRelayReply), helperDone: make(chan struct{}), requests: make(chan syscall.Signal, 8), actions: make(chan string, 8), done: make(chan error, 1)}
	f.ctx, f.cancel = context.WithTimeout(context.Background(), 15*time.Second)
	go func() {
		f.done <- commitJobLoop(f.ctx, f.events, f.replies, f.helperDone, func() error { return errors.New("helper exited") }, commitJobOps{
			request:  func(s syscall.Signal) error { f.requests <- s; return nil },
			resume:   func() error { f.actions <- "resume"; return nil },
			cont:     func() error { f.actions <- "cont"; return nil },
			eligible: func(syscall.Signal) bool { return eligible },
		})
	}()
	t.Cleanup(f.cancel)
	return f
}

func (f *commitLoopFixture) event(code int, sig syscall.Signal) {
	f.events <- commitObservation{event: commitWaitEvent{123, code, int(sig)}}
}
func (f *commitLoopFixture) request(t *testing.T, want syscall.Signal) {
	t.Helper()
	select {
	case got := <-f.requests:
		if got != want {
			t.Fatal(got, want)
		}
	case <-f.ctx.Done():
		t.Fatal("request not observed", f.ctx.Err())
	}
}
func (f *commitLoopFixture) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		f.cancel()
		return err
	case <-f.ctx.Done():
		t.Fatal("loop did not finish", f.ctx.Err())
		return f.ctx.Err()
	}
}

func TestCommitJobStopACKAssociationAndLaterStop(t *testing.T) {
	f := newCommitLoopFixture(t, true)
	f.event(commitStopped, syscall.SIGTSTP)
	f.request(t, syscall.SIGTSTP)
	// CONT and a later stop are available before the old ACK. Its signal text
	// is unchanged, so only event association prevents resuming the wrong stop.
	f.event(commitContinued, syscall.SIGCONT)
	f.event(commitStopped, syscall.SIGTSTP)
	f.replies <- commitRelayReply{sig: syscall.SIGTSTP}
	f.request(t, syscall.SIGTSTP)
	if got := <-f.actions; got != "cont" {
		t.Fatal(got)
	}
	select {
	case action := <-f.actions:
		t.Fatal("stale ACK resumed later stop", action)
	default:
	}
	f.replies <- commitRelayReply{sig: syscall.SIGTSTP}
	select {
	case got := <-f.actions:
		if got != "resume" {
			t.Fatal(got)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.event(commitStopped, syscall.SIGTTOU)
	f.request(t, syscall.SIGTTOU)
	f.replies <- commitRelayReply{sig: syscall.SIGTTOU}
	select {
	case got := <-f.actions:
		if got != "resume" {
			t.Fatal(got)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.event(commitExited, 0)
	if err := f.result(t); err != nil {
		t.Fatal(err)
	}
}

func TestCommitJobIneligibleExternalCONTAndNoStopTranslation(t *testing.T) {
	f := newCommitLoopFixture(t, false)
	f.event(commitStopped, syscall.SIGTTIN)
	f.request(t, syscall.SIGTTIN)
	f.replies <- commitRelayReply{sig: syscall.SIGTTIN}
	f.event(commitContinued, syscall.SIGCONT)
	select {
	case got := <-f.actions:
		if got != "cont" {
			t.Fatal("ineligible ACK resumed child", got)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.event(commitStopped, syscall.SIGSTOP)
	f.event(commitTrapped, syscall.SIGTSTP)
	f.event(commitExited, 0)
	if err := f.result(t); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-f.requests:
		t.Fatal("translated stop/trap", got)
	default:
	}
	select {
	case got := <-f.actions:
		t.Fatal("automatic CONT", got)
	default:
	}
}

func TestCommitJobPendingACKExitCancellationAndProtocolFailures(t *testing.T) {
	for _, kind := range []string{"exit", "cancel", "wrong", "duplicate", "reader-error", "helper-exit", "observer-error"} {
		t.Run(kind, func(t *testing.T) {
			f := newCommitLoopFixture(t, true)
			f.event(commitStopped, syscall.SIGTSTP)
			f.request(t, syscall.SIGTSTP)
			switch kind {
			case "exit":
				f.event(commitExited, 0)
			case "cancel":
				f.cancel()
			case "wrong":
				f.replies <- commitRelayReply{sig: syscall.SIGTTIN}
			case "duplicate":
				f.replies <- commitRelayReply{sig: syscall.SIGTSTP}
				select {
				case <-f.actions:
				case <-f.ctx.Done():
					t.Fatal(f.ctx.Err())
				}
				f.replies <- commitRelayReply{sig: syscall.SIGTSTP}
			case "reader-error":
				f.replies <- commitRelayReply{err: errors.New("truncated ACK")}
			case "helper-exit":
				close(f.helperDone)
			case "observer-error":
				f.events <- commitObservation{err: syscall.ECHILD}
			}
			// Cancellation already closed its guard, so collect completion without
			// selecting that same ready context as though it were a timeout.
			var err error
			select {
			case err = <-f.done:
			case <-time.After(5 * time.Second):
				t.Fatal("pending ACK blocked completion")
			}
			if kind == "exit" && err != nil || kind != "exit" && err == nil || kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(kind, err)
			}
			if kind != "duplicate" {
				select {
				case action := <-f.actions:
					t.Fatal("resumed after failure/exit", action)
				default:
				}
			}
		})
	}
	// An exit already available wins even when a duplicate ACK/helper exit is
	// available as well. No event history is inferred beyond these raw records.
	events := make(chan commitObservation, 1)
	events <- commitObservation{event: commitWaitEvent{123, commitExited, 0}}
	replies := make(chan commitRelayReply, 1)
	replies <- commitRelayReply{sig: syscall.SIGTSTP}
	done := make(chan struct{})
	close(done)
	err := commitJobLoop(context.Background(), events, replies, done, func() error { t.Fatal("exit priority lost"); return nil }, commitJobOps{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitTTYConditionalOwnershipAndFailedStart(t *testing.T) {
	for _, kind := range []string{"foreground", "background", "later-fg", "replacement", "already-caller", "start-empty", "start-live", "background-start", "changed-session", "changed-group"} {
		t.Run(kind, func(t *testing.T) {
			group, sid, ttySID, foreground := 101, 100, 100, 101
			var sets []int
			probes := 0
			tty := &commitTTY{file: os.Stdin, caller: group, sid: sid, entryForeground: foreground, gitGroup: 202}
			tty.ops = &commitTerminalOps{snapshot: func() (int, int, int, int, error) { return group, sid, ttySID, foreground, nil }, set: func(g int) error { sets = append(sets, g); foreground = g; return nil }, probe: func(int) error {
				probes++
				if kind == "start-empty" {
					return syscall.ESRCH
				}
				return nil
			}}
			cmd := exec.Command("not-executed")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if strings.HasPrefix(kind, "background") || kind == "later-fg" {
				foreground = 303
				tty.entryForeground = 303
			}
			if err := tty.beforeStart(cmd); err != nil {
				t.Fatal(err)
			}
			if cmd.SysProcAttr.Foreground != (tty.entryForeground == tty.caller) {
				t.Fatal("entry handoff", cmd.SysProcAttr)
			}
			switch kind {
			case "foreground":
				tty.handed = true
				foreground = 202
			case "replacement":
				tty.handed = true
				foreground = 303
			case "already-caller":
				tty.handed = true
			case "later-fg":
				foreground = 101
				if err := tty.handoff(); err != nil {
					t.Fatal(err)
				}
			case "background":
				if err := tty.handoff(); err != nil {
					t.Fatal(err)
				}
			case "start-empty", "start-live", "background-start":
				foreground = 404
			case "changed-session":
				ttySID = 999
			case "changed-group":
				group = 999
			}
			failed := strings.Contains(kind, "start")
			err := tty.restore(failed)
			if strings.HasPrefix(kind, "changed") {
				if _, err = tty.foreground(); err == nil {
					t.Fatal("changed association accepted")
				}
				if err = tty.handoff(); err == nil {
					t.Fatal("changed association handoff accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := []int(nil)
			if kind == "foreground" || kind == "start-empty" {
				want = []int{101}
			}
			if kind == "later-fg" {
				want = []int{202, 101}
			}
			if !reflect.DeepEqual(sets, want) {
				t.Fatal("terminal stolen or not restored", kind, sets, want)
			}
			if probes != 0 && kind != "start-empty" && kind != "start-live" {
				t.Fatal("unowned Start recovery", probes)
			}
		})
	}
}

// Runs in a disposable session; only this test fixture changes its own thread
// mask. Production eligibility observes inherited helper state without repair.
func TestCommitRelayProbeHelper(t *testing.T) {
	mode := os.Getenv("CONFCTL_COMMIT_RELAY_PROBE")
	if mode == "" {
		return
	}
	if mode == "exit" {
		os.Exit(7)
	}
	if mode == "shell-job" {
		runCommitShellSubject(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tty, err := openCommitTTY()
	if err != nil || tty == nil {
		panic(fmt.Sprint("probe has no ctty", err))
	}
	defer tty.file.Close()
	if mode == "blocked" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		mask := uint64(1) << (uint(syscall.SIGTSTP) - 1)
		var previous uint64
		_, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 0, uintptr(unsafe.Pointer(&mask)), uintptr(unsafe.Pointer(&previous)), 8, 0, 0)
		if errno != 0 {
			panic(errno)
		}
		defer func() {
			_, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 2, uintptr(unsafe.Pointer(&previous)), 0, 8, 0, 0)
			if errno != 0 {
				panic(errno)
			}
		}()
	}
	relay, err := startCommitStopRelay(ctx, os.Getenv("CONFCTL_RELAY_TEST_PATH"), tty)
	if mode == "bad-startup" || mode == "missing" {
		if err == nil {
			_ = relay.close()
			panic("invalid helper accepted")
		}
		fmt.Print(err.Error())
		return
	}
	if err != nil {
		panic(err)
	}
	identity := relay.identity
	if !reflect.DeepEqual(relay.cmd.Env, []string{"LC_ALL=C"}) || relay.cmd.SysProcAttr.Foreground || relay.cmd.SysProcAttr.Setsid || relay.cmd.SysProcAttr.Pgid != tty.caller {
		panic("helper inherited public environment/terminal authority")
	}
	// Verify the explicit inherited-file launch boundary. Bash's builtin
	// read <&3 temporarily duplicates its request pipe onto fd0 after READY.
	input, ok := relay.cmd.Stdin.(*os.File)
	if !ok || input.Name() != os.DevNull {
		panic("helper launch stdin was not the explicit null file")
	}
	if _, e := input.Stat(); !errors.Is(e, os.ErrClosed) {
		panic("parent retained helper launch stdin")
	}
	links := make(map[string]string)
	for _, fd := range []string{"1", "2", "3", "4", "0"} {
		link, e := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", identity.pid, fd))
		if e != nil || fd != "0" && !strings.HasPrefix(link, "pipe:[") {
			panic(fmt.Sprint("helper descriptor topology changed", fd, link, e))
		}
		links[fd] = link
	}
	for fd, file := range map[string]*os.File{"3": relay.request, "4": relay.reply} {
		child, e := os.Stat(fmt.Sprintf("/proc/%d/fd/%s", identity.pid, fd))
		parent, pe := file.Stat()
		if e != nil || pe != nil || !os.SameFile(child, parent) {
			panic(fmt.Sprint("helper private pipe identity changed", fd, e, pe))
		}
	}
	if links["0"] != os.DevNull && links["0"] != links["3"] {
		panic(fmt.Sprint("helper stdin is neither null nor its exact request pipe", links))
	}
	for _, f := range relay.cmd.ExtraFiles {
		if _, e := f.Stat(); !errors.Is(e, os.ErrClosed) {
			panic("parent retained child pipe end")
		}
	}
	protocolFault := mode == "wrong-ack" || mode == "duplicate-ack" || mode == "truncated-ack" || mode == "eof" || mode == "cancel-ack"
	if protocolFault {
		events := make(chan commitObservation, 1)
		events <- commitObservation{event: commitWaitEvent{123, commitStopped, int(syscall.SIGTSTP)}}
		resumes := 0
		err = commitJobLoop(ctx, events, relay.replies, relay.done, relay.exitError, commitJobOps{
			request: func(sig syscall.Signal) error {
				err := relay.requestStop(tty, sig)
				if mode == "cancel-ack" {
					cancel()
				}
				return err
			},
			resume: func() error { resumes++; return nil }, cont: func() error { return nil }, eligible: relay.signals.eligible,
		})
		if err == nil || mode == "cancel-ack" && !errors.Is(err, context.Canceled) {
			panic(fmt.Sprint("protocol failure lost", mode, err))
		}
		want := 0
		if mode == "duplicate-ack" {
			want = 1
		}
		if resumes != want {
			panic(fmt.Sprint("wrong acknowledgement resumed child", mode, resumes))
		}
	}
	if mode == "association-change" {
		tty.caller++
		if err = relay.requestStop(tty, syscall.SIGTSTP); err == nil {
			panic("changed caller association authorized relay")
		}
		tty.caller--
	}
	if mode == "identity-change" {
		relay.identity.start += "invalid"
		if err = relay.requestStop(tty, syscall.SIGTSTP); err == nil {
			panic("changed helper identity authorized relay")
		}
		relay.identity = identity
	}
	if mode == "invalid-request" {
		if _, err = relay.request.WriteString("INVALID\n"); err != nil {
			panic(err)
		}
		select {
		case reply := <-relay.replies:
			if reply.err == nil {
				panic("invalid request acknowledged")
			}
		case <-ctx.Done():
			panic(ctx.Err())
		}
		<-relay.done
		if relay.cmd.ProcessState.ExitCode() != 64 {
			panic(relay.exitError())
		}
	}
	if mode == "stopped-close" {
		if err = relay.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			panic(err)
		}
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(identity.pid, &status, syscall.WUNTRACED, nil)
		if err != nil || pid != identity.pid || !status.Stopped() {
			panic(fmt.Sprint(status, err))
		}
	}
	if mode == "kill-error" {
		kill := relay.kill
		relay.kill = func() error {
			if e := kill(); e != nil && !errors.Is(e, os.ErrProcessDone) {
				return e
			}
			return syscall.EPERM
		}
	}
	closeErr := relay.close()
	if mode == "kill-error" && !errors.Is(closeErr, syscall.EPERM) {
		panic("cleanup diagnostic lost")
	}
	if mode != "kill-error" && closeErr != nil {
		panic(closeErr)
	}
	if err = relay.close(); err != closeErr {
		panic("non-idempotent cleanup")
	}
	if childSame(identity) {
		panic("helper still live after joined Wait")
	}
	if _, err = relay.request.WriteString("TSTP\n"); !errors.Is(err, os.ErrClosed) {
		panic("request descriptor remains open")
	}
	var b [1]byte
	if _, err = relay.reply.Read(b[:]); !errors.Is(err, os.ErrClosed) {
		panic("reply descriptor remains open")
	}
	if err = json.NewEncoder(os.Stdout).Encode(relay.signals.raw); err != nil {
		panic(err)
	}
}

func openCommitTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
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
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func TestCommitRelayActualStartupStatesProtocolAndReaping(t *testing.T) {
	helper := materializeCommitRelay(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"default", "ignored", "blocked", "invalid-request", "stopped-close", "bad-startup", "missing", "wrong-ack", "duplicate-ack", "truncated-ack", "eof", "cancel-ack", "association-change", "identity-change", "kill-error"} {
		t.Run(mode, func(t *testing.T) {
			_, slave := openCommitTestPTY(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommitRelayProbeHelper$")
			if mode == "ignored" {
				cmd = exec.CommandContext(ctx, bash, "-p", "-c", `builtin trap '' TSTP; builtin exec "$@"`, "fixture", os.Args[0], "-test.run=^TestCommitRelayProbeHelper$")
			}
			path := helper
			if mode == "missing" {
				path = filepath.Join(t.TempDir(), "missing")
			}
			if mode == "bad-startup" {
				path = filepath.Join(t.TempDir(), "bad.bash")
				if err := os.WriteFile(path, []byte("#!"+bash+" -p\nbuiltin printf 'SigBlk: bad\\n' >&4\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(mode, "-ack") || mode == "eof" {
				path = filepath.Join(t.TempDir(), "fault.bash")
				body := "#!" + bash + " -p\nbuiltin printf '" + strings.ReplaceAll(validCommitStartup, "\n", `\n`) + "' >&4\nbuiltin read -r request <&3\n"
				switch mode {
				case "wrong-ack":
					body += "builtin printf 'ACK TTIN\\n' >&4\n"
				case "duplicate-ack":
					body += "builtin printf 'ACK TSTP\\nACK TSTP\\n' >&4\n"
				case "truncated-ack":
					body += "builtin printf 'ACK TSTP' >&4\nexit 0\n"
				case "eof":
					body += "exit 0\n"
				}
				body += "builtin read -r request <&3\n"
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd.Env = append(os.Environ(), "CONFCTL_COMMIT_RELAY_PROBE="+mode, "CONFCTL_RELAY_TEST_PATH="+path)
			owned := prepareChild(cmd)
			cmd.Stdin = slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			owned.bind(cmd.Process.Pid)
			err = cmd.Wait()
			owned.finish()
			bytes := output.Bytes()
			if err != nil {
				t.Fatal(mode, err, string(bytes))
			}
			if mode == "bad-startup" || mode == "missing" {
				if len(bytes) == 0 {
					t.Fatal("failure diagnostic missing")
				}
				return
			}
			// Test helpers return before the testing package's PASS line.
			line := strings.SplitN(string(bytes), "\n", 2)[0]
			var raw map[string]string
			if err = json.Unmarshal([]byte(line), &raw); err != nil {
				t.Fatal(string(bytes), err)
			}
			text := raw["SigBlk"] + raw["SigIgn"] + raw["SigCgt"] + raw["Threads"] + "READY\n"
			state, err := readCommitStartup(bufio.NewReader(strings.NewReader(text)))
			if err != nil {
				t.Fatal(raw, err)
			}
			bit := uint64(1) << (uint(syscall.SIGTSTP) - 1)
			if mode == "ignored" && state.ignored&bit == 0 || mode == "blocked" && state.blocked&bit == 0 || mode == "default" && !state.eligible(syscall.SIGTSTP) {
				t.Fatal("actual helper inheritance differs", mode, raw)
			}
		})
	}
}

func TestCommitRelayInstalledResolutionIgnoresPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := installedCommitRelay()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(executable), "..", "libexec", "confctl-commit-stop-relay.bash")
	if got != want {
		t.Fatal(got, want)
	}
}

func runCommitShellSubject(t *testing.T) {
	t.Helper()
	root := os.Getenv("CONFCTL_COMMIT_JOB_ROOT")
	caller, err := childID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := json.Marshal(callerRecord{caller.pid, caller.start, caller.group})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "caller.json"), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	// The Bash child has literal inherited terminal streams. Two terminal reads
	// give the controller distinct VSUSP/fg/input boundaries without an editor.
	script := `builtin printf ready > "$1/ready-1"; builtin read -r first; builtin printf '%s' "$first" > "$1/input-1"; builtin printf ready > "$1/ready-2"; builtin read -r second; builtin printf '%s' "$second" > "$1/input-2"`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var gitID childIdentity
	var stopped int
	r, err := runInheritedCommitObserved(ctx, root, []string{os.Getenv("CONFCTL_COMMIT_JOB_BASH"), "-p", "-c", script, "fixture", root}, os.Stdin, os.Stdout, os.Stderr, os.Getenv("CONFCTL_COMMIT_JOB_RELAY"), func(pid, flags int, info *commitSiginfo) error {
		if gitID.pid == 0 {
			var e error
			gitID, e = childID(pid)
			if e != nil {
				return e
			}
			bytes, e := json.Marshal(callerRecord{gitID.pid, gitID.start, gitID.group})
			if e != nil {
				return e
			}
			if e = os.WriteFile(filepath.Join(root, "git.json"), bytes, 0600); e != nil {
				return e
			}
		}
		e := rawCommitWait(pid, flags, info)
		if e == nil && flags&syscall.WNOWAIT == 0 {
			event, de := decodeCommitEvent(info, pid)
			if de != nil {
				return de
			}
			if event.code == commitStopped && event.status == int(syscall.SIGTSTP) {
				stopped++
			}
		}
		return e
	})
	if err != nil || r.ExitCode != 0 || stopped != 2 || ctx.Err() != nil {
		t.Fatal(r, err, stopped, ctx.Err())
	}
	if childSame(gitID) {
		t.Fatal("Git stand-in not reaped")
	}
	if !commitStdoutTTY(os.Stdout) {
		t.Fatal("subject stdout lost terminal")
	}
}

type callerRecord struct {
	PID   int
	Start string
	Group int
}

func TestInheritedCommitShellJobRepeatStopForegroundAndDescriptors(t *testing.T) {
	root := t.TempDir()
	helper := materializeCommitRelay(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	master, slave := openCommitTestPTY(t)
	var rawOpen, requested, configuredBeforeShell, before, after syscall.Termios
	phase := "raw PTY readback"
	baselineCaptured := false
	defer func() {
		if t.Failed() {
			var observed syscall.Termios
			_, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&observed)))
			group, ge := commitTerminalGroup(master)
			t.Logf("fixture phase=%s baseline_captured=%t raw_open=%+v requested=%+v configured_before_shell=%+v before_subject=%+v after_subject=%+v observed=%+v termios_error=%v foreground=%d foreground_error=%v", phase, baselineCaptured, rawOpen, requested, configuredBeforeShell, before, after, observed, e, group, ge)
		}
	}()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&rawOpen)))
	if errno != 0 {
		t.Fatal(errno)
	}
	requested = rawOpen
	// Original Ruby T4 and multi-member stop records have Cflag983231 in all
	// before/caller-before/after snapshots. Select that fixture state while
	// preserving raw speed fields, padding and every control byte.
	requested.Cflag = 983231
	requested.Lflag = (rawOpen.Lflag | syscall.ISIG | syscall.ICANON) &^ syscall.ECHO
	phase = "private PTY initialization"
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&requested)))
	if errno != 0 {
		t.Fatal(errno)
	}
	phase = "configured pre-shell readback"
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&configuredBeforeShell)))
	if errno != 0 || configuredBeforeShell != requested {
		t.Fatal("initial terminal configuration differs", errno, requested, configuredBeforeShell)
	}
	commandsRead, commandsWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer commandsRead.Close()
	defer commandsWrite.Close()
	fifo := filepath.Join(root, "peer.fifo")
	if err = syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	peerRelease, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer peerRelease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-p", "-i")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+root, "CONFCTL_COMMIT_RELAY_PROBE=shell-job", "CONFCTL_COMMIT_JOB_ROOT="+root, "CONFCTL_COMMIT_JOB_BASH="+bash, "CONFCTL_COMMIT_JOB_RELAY="+helper)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = commandsRead, slave, slave
	owned := prepareChild(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owned.bind(cmd.Process.Pid)
	_ = commandsRead.Close()
	waited := make(chan error, 1)
	go func() { err := cmd.Wait(); owned.finish(); waited <- err }()
	known := []childIdentity{}
	rootWaited := false
	// Only this disposable fixture controller kills its proven descendants on
	// assertion failure. Production never signals caller peers or scans groups.
	defer func() {
		owned.mu.Lock()
		if len(owned.ids) > 0 && childSame(owned.ids[0]) {
			owned.collect(owned.pgid)
		}
		known = append(known, owned.ids...)
		owned.mu.Unlock()
		for i := len(known) - 1; i >= 0; i-- {
			identity := known[i]
			process, e := os.FindProcess(identity.pid)
			if e == nil {
				if childSame(identity) {
					_ = process.Kill()
				}
				_ = process.Release()
			}
		}
		_ = commandsWrite.Close()
		cancel()
		if !rootWaited {
			<-waited
		}
	}()
	phase = "shell startup"
	waitFor := func(label string, condition func() bool) {
		t.Helper()
		phase = label
		for !condition() {
			select {
			case e := <-waited:
				rootWaited = true
				t.Fatalf("shell exited during %s: %v", label, e)
			case <-ctx.Done():
				t.Fatal(label, ctx.Err())
			default:
			}
			time.Sleep(5 * time.Millisecond) // Fixture watchdog, no product polling.
		}
	}
	fileExists := func(name string) bool { _, e := os.Stat(filepath.Join(root, name)); return e == nil }
	foreground := func() int {
		group, e := commitTerminalGroup(master)
		if e != nil {
			t.Fatal(e)
		}
		return int(group)
	}
	// The builtin runs only after interactive Bash initialization. It creates
	// no job, so the subject remains %1, and no subject can run before this
	// exact terminal baseline is captured.
	ready := "builtin printf '%s' shell-ready > " + ShellJoin([]string{filepath.Join(root, "shell-ready")}) + "\n"
	if _, err = commandsWrite.WriteString(ready); err != nil {
		t.Fatal(err)
	}
	waitFor("shell builtin readiness", func() bool {
		bytes, e := os.ReadFile(filepath.Join(root, "shell-ready"))
		return e == nil && string(bytes) == "shell-ready" && foreground() == cmd.Process.Pid
	})
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&before)))
	if errno != 0 || before != configuredBeforeShell {
		t.Fatal("shell initialization changed configured terminal attributes", errno, configuredBeforeShell, before)
	}
	baselineCaptured = true
	subject := ShellJoin([]string{os.Args[0], "-test.run=^TestCommitRelayProbeHelper$"})
	peer := ShellJoin([]string{bash, "-p", "-c", `builtin printf '%s' "$BASHPID" > "$1"; builtin read -r < "$2"`, "fixture", filepath.Join(root, "peer.pid"), fifo})
	// Controller commands use the private pipe. The multi-member subject job
	// explicitly retains /dev/tty streams, matching the assessed topology.
	line := "{ " + subject + " </dev/tty >/dev/tty 2>/dev/tty; builtin printf '%s' \"$?\" > " + ShellJoin([]string{filepath.Join(root, "status")}) + "; } >/dev/tty 2>/dev/tty | " + peer + "\n"
	if _, err = commandsWrite.WriteString(line); err != nil {
		t.Fatal(err)
	}
	waitFor("subject readiness", func() bool {
		caller, e1 := os.ReadFile(filepath.Join(root, "caller.json"))
		git, e2 := os.ReadFile(filepath.Join(root, "git.json"))
		peer, e3 := os.ReadFile(filepath.Join(root, "peer.pid"))
		_, e4 := strconv.Atoi(string(peer))
		return fileExists("ready-1") && e1 == nil && json.Valid(caller) && e2 == nil && json.Valid(git) && e3 == nil && e4 == nil
	})
	readIdentity := func(name string) childIdentity {
		t.Helper()
		bytes, e := os.ReadFile(filepath.Join(root, name))
		if e != nil {
			t.Fatal(e)
		}
		var record callerRecord
		if e = json.Unmarshal(bytes, &record); e != nil {
			t.Fatal(e)
		}
		identity := childIdentity{record.PID, record.Start, record.Group}
		if !childSame(identity) {
			t.Fatal("subject identity changed", identity)
		}
		return identity
	}
	caller, gitID := readIdentity("caller.json"), readIdentity("git.json")
	peerBytes, err := os.ReadFile(filepath.Join(root, "peer.pid"))
	if err != nil {
		t.Fatal(err)
	}
	peerPID, err := strconv.Atoi(string(peerBytes))
	if err != nil {
		t.Fatal(err)
	}
	peerID, err := childID(peerPID)
	if err != nil {
		t.Fatal(err)
	}
	if peerID.group != caller.group || gitID.group == caller.group {
		t.Fatal("multi-member job topology lost", caller, peerID, gitID)
	}
	known = append(known, caller, gitID, peerID)
	state := func(identity childIdentity) string {
		if !childSame(identity) {
			return "gone"
		}
		bytes, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", identity.pid))
		if e != nil {
			return "gone"
		}
		end := strings.LastIndexByte(string(bytes), ')')
		if end < 0 {
			t.Fatal("invalid owned stat")
		}
		fields := strings.Fields(string(bytes[end+2:]))
		if len(fields) == 0 {
			t.Fatal("empty owned stat")
		}
		return fields[0]
	}
	for n := 1; n <= 2; n++ {
		waitFor(fmt.Sprintf("Git foreground/read %d", n), func() bool { return foreground() == gitID.group && fileExists(fmt.Sprintf("ready-%d", n)) })
		if _, err = master.Write([]byte{before.Cc[syscall.VSUSP]}); err != nil {
			t.Fatal(err)
		}
		waitFor("whole caller job stopped", func() bool {
			return state(caller) == "T" && state(peerID) == "T" && state(gitID) == "T" && foreground() == cmd.Process.Pid
		})
		if _, err = commandsWrite.WriteString("fg %1\n"); err != nil {
			t.Fatal(err)
		}
		waitFor("owned Git foreground after fg", func() bool { return foreground() == gitID.group && state(gitID) != "T" })
		if _, err = master.Write([]byte(fmt.Sprintf("input-%d\n", n))); err != nil {
			t.Fatal(err)
		}
		waitFor("input consumed", func() bool {
			bytes, e := os.ReadFile(filepath.Join(root, fmt.Sprintf("input-%d", n)))
			return e == nil && string(bytes) == fmt.Sprintf("input-%d", n)
		})
		bytes, e := os.ReadFile(filepath.Join(root, fmt.Sprintf("input-%d", n)))
		if e != nil || string(bytes) != fmt.Sprintf("input-%d", n) {
			t.Fatal("terminal input replaced", string(bytes), e)
		}
	}
	waitFor("caller completion", func() bool {
		bytes, e := os.ReadFile(filepath.Join(root, "status"))
		return e == nil && len(bytes) != 0
	})
	status, err := os.ReadFile(filepath.Join(root, "status"))
	if err != nil || string(status) != "0" {
		t.Fatal("subject failed", string(status), err)
	}
	if foreground() != caller.group {
		t.Fatal("caller foreground not restored before peer completion")
	}
	if _, err = peerRelease.WriteString("release\n"); err != nil {
		t.Fatal(err)
	}
	waitFor("shell foreground", func() bool { return foreground() == cmd.Process.Pid })
	phase = "terminal equality after subject"
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&after)))
	if errno != 0 || before != after {
		t.Fatal("terminal attributes changed", errno, before, after)
	}
	if _, err = commandsWrite.WriteString("exit\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-waited:
		rootWaited = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if ctx.Err() != nil || childSame(caller) || childSame(gitID) || childSame(peerID) {
		t.Fatal("completed job left live work", ctx.Err())
	}
}
