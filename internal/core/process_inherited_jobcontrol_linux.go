package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	commitExited    = 1
	commitKilled    = 2
	commitDumped    = 3
	commitTrapped   = 4
	commitStopped   = 5
	commitContinued = 6
)

// Linux SIGCHLD siginfo is 128 bytes. uint64 storage supplies the alignment of
// pinned Go's SiginfoChild; the common header has extra padding on 64-bit ABIs.
type commitSiginfo [16]uint64
type commitWaitEvent struct{ pid, code, status int }
type commitRawWait func(int, int, *commitSiginfo) error

func decodeCommitEvent(info *commitSiginfo, pid int) (commitWaitEvent, error) {
	b := unsafe.Slice((*byte)(unsafe.Pointer(info)), 128)
	read := func(offset int) int { return int(int32(binary.NativeEndian.Uint32(b[offset:]))) }
	codeOffset := 8
	if strings.HasPrefix(runtime.GOARCH, "mips") {
		codeOffset = 4 // Linux MIPS swaps si_errno and si_code.
	}
	pidOffset := 12
	if unsafe.Sizeof(uintptr(0)) == 8 {
		pidOffset = 16
	}
	e := commitWaitEvent{read(pidOffset), read(codeOffset), read(pidOffset + 8)}
	if e.pid == 0 && read(0) == 0 && e.code == 0 && e.status == 0 {
		return e, nil // WNOHANG consume raced with a different transition.
	}
	if read(0) != int(syscall.SIGCHLD) || e.pid != pid || e.code < commitExited || e.code > commitContinued {
		return e, fmt.Errorf("invalid commit wait event: signo=%d pid=%d code=%d status=%d", read(0), e.pid, e.code, e.status)
	}
	return e, nil
}

func rawCommitWait(pid, flags int, info *commitSiginfo) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 1, uintptr(pid), uintptr(unsafe.Pointer(info)), uintptr(flags), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// Only stop/continue is consumed here. Exit remains waitable for the one Cmd.Wait.
func waitCommitEvent(pid int, wait commitRawWait) (commitWaitEvent, error) {
	call := func(flags int) (commitWaitEvent, error) {
		for {
			var info commitSiginfo
			err := wait(pid, flags, &info)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				return commitWaitEvent{}, err
			}
			return decodeCommitEvent(&info, pid)
		}
	}
	for {
		peek, err := call(syscall.WEXITED | syscall.WSTOPPED | syscall.WCONTINUED | syscall.WNOWAIT)
		if err != nil || peek.code <= commitDumped && peek.pid != 0 {
			return peek, err
		}
		if peek.pid == 0 {
			return peek, errors.New("empty blocking commit wait event")
		}
		consumed, err := call(syscall.WSTOPPED | syscall.WCONTINUED | syscall.WNOHANG)
		if err != nil {
			return consumed, err
		}
		if consumed.pid != 0 {
			if consumed.code <= commitDumped {
				return consumed, errors.New("commit consume unexpectedly reported exit")
			}
			return consumed, nil
		}
	}
}

type commitObservation struct {
	event commitWaitEvent
	err   error
}

func observeCommitChild(pid int, wait commitRawWait, events chan<- commitObservation, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		e, err := waitCommitEvent(pid, wait)
		select {
		case events <- commitObservation{e, err}:
		case <-stop:
			return
		}
		if err != nil || e.code <= commitDumped {
			return
		}
	}
}

func commitSID(pid int) (int, error) {
	sid, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}

func commitTerminalSID(file *os.File) (int, error) {
	var sid int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGSID, uintptr(unsafe.Pointer(&sid)))
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}

type commitTTY struct {
	file                         *os.File
	caller, sid, entryForeground int
	gitGroup                     int
	handed, startHandoff         bool
	ops                          *commitTerminalOps
}

// Private terminal operations permit controlled ownership races in direct
// tests. The installed adapter uses the exact descriptor and syscalls below.
type commitTerminalOps struct {
	snapshot func() (caller, sid, terminalSID, foreground int, err error)
	set      func(int) error
	probe    func(int) error
}

func (t *commitTTY) snapshot() (caller, sid, terminalSID, foreground int, err error) {
	if t.ops != nil {
		return t.ops.snapshot()
	}
	caller = syscall.Getpgrp()
	sid, err = commitSID(0)
	if err == nil {
		terminalSID, err = commitTerminalSID(t.file)
	}
	if err == nil {
		var g int32
		g, err = commitTerminalGroup(t.file)
		foreground = int(g)
	}
	return
}

func (t *commitTTY) setForeground(group int) error {
	if t.ops != nil {
		return t.ops.set(group)
	}
	return restoreCommitForeground(t.file, int32(group))
}

func (t *commitTTY) probeGroup(group int) error {
	if t.ops != nil {
		return t.ops.probe(group)
	}
	return syscall.Kill(-group, 0)
}

func openCommitTTY() (*commitTTY, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	t := &commitTTY{file: f, caller: syscall.Getpgrp()}
	t.sid, err = commitSID(0)
	if err == nil {
		err = t.association()
	}
	if err == nil {
		var group int32
		group, err = commitTerminalGroup(f)
		t.entryForeground = int(group)
		if err == nil && group <= 0 {
			err = errors.New("invalid commit terminal foreground group")
		}
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return t, nil
}

func (t *commitTTY) association() error {
	caller, sid, terminalSID, _, err := t.snapshot()
	if err != nil {
		return err
	}
	if t.caller <= 0 || t.sid <= 0 || caller != t.caller || sid != t.sid || terminalSID != t.sid {
		return errors.New("commit caller/controlling-terminal association changed")
	}
	return nil
}

func (t *commitTTY) foreground() (int, error) {
	caller, sid, terminalSID, g, err := t.snapshot()
	if err == nil && (t.caller <= 0 || t.sid <= 0 || caller != t.caller || sid != t.sid || terminalSID != t.sid) {
		err = errors.New("commit caller/controlling-terminal association changed")
	}
	if err == nil && g <= 0 {
		err = errors.New("invalid commit terminal foreground group")
	}
	return g, err
}

func (t *commitTTY) beforeStart(cmd *exec.Cmd) error {
	g, err := t.foreground()
	if err != nil {
		return err
	}
	if t.entryForeground == t.caller && g == t.caller {
		cmd.SysProcAttr.Foreground, cmd.SysProcAttr.Ctty = true, int(t.file.Fd())
		t.startHandoff = true
	}
	return nil
}

func (t *commitTTY) handoff() error {
	g, err := t.foreground()
	if err != nil {
		return err
	}
	if g == t.caller {
		if err = t.setForeground(t.gitGroup); err != nil {
			return err
		}
		t.handed = true
	}
	return nil
}

func (t *commitTTY) restore(startFailed bool) error {
	if !t.handed && !(startFailed && t.startHandoff) {
		return nil
	}
	g, err := t.foreground()
	if err != nil || g == t.caller {
		return err
	}
	if t.handed && g == t.gitGroup {
		return t.setForeground(t.caller)
	}
	// Go reaps an exec-failed foreground child without exposing its PID. Only
	// that attempted foreground Start can recover its now-empty terminal group.
	if startFailed && t.startHandoff {
		probeErr := t.probeGroup(g)
		if errors.Is(probeErr, syscall.ESRCH) {
			return t.setForeground(t.caller)
		}
		if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
			return probeErr
		}
	}
	return nil // Another live foreground group is never replaced.
}

type commitSignalState struct {
	raw                      map[string]string
	blocked, ignored, caught uint64
}

func (s commitSignalState) eligible(sig syscall.Signal) bool {
	if sig <= 0 || sig > 64 {
		return false
	}
	return (s.blocked|s.ignored|s.caught)&(uint64(1)<<(uint(sig)-1)) == 0
}

var commitMaskRecord = regexp.MustCompile(`\A(SigBlk|SigIgn|SigCgt):[ \t]+([0-9a-fA-F]{16})\n\z`)
var commitThreadRecord = regexp.MustCompile(`\AThreads:[ \t]+([0-9]+)\n\z`)

func readCommitStartup(reader *bufio.Reader) (commitSignalState, error) {
	s := commitSignalState{raw: map[string]string{}}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return s, fmt.Errorf("incomplete commit relay startup: %w", err)
		}
		if line == "READY\n" {
			if len(s.raw) != 4 {
				return s, errors.New("missing commit relay startup record")
			}
			return s, nil
		}
		name := "Threads"
		if fields := commitMaskRecord.FindStringSubmatch(line); fields != nil {
			name = fields[1]
			mask, _ := strconv.ParseUint(fields[2], 16, 64)
			switch name {
			case "SigBlk":
				s.blocked = mask
			case "SigIgn":
				s.ignored = mask
			case "SigCgt":
				s.caught = mask
			}
		} else if fields := commitThreadRecord.FindStringSubmatch(line); fields != nil {
			threads, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || threads != 1 {
				return s, errors.New("commit relay must have one thread")
			}
		} else {
			return s, errors.New("invalid commit relay startup record")
		}
		if _, exists := s.raw[name]; exists {
			return s, fmt.Errorf("duplicate commit relay startup record %s", name)
		}
		s.raw[name] = line
	}
}

func commitStopName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTSTP:
		return "TSTP"
	case syscall.SIGTTIN:
		return "TTIN"
	case syscall.SIGTTOU:
		return "TTOU"
	}
	return ""
}

func parseCommitACK(line string) (syscall.Signal, error) {
	for _, sig := range []syscall.Signal{syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU} {
		if line == "ACK "+commitStopName(sig)+"\n" {
			return sig, nil
		}
	}
	return 0, errors.New("invalid commit relay acknowledgement")
}

type commitRelayReply struct {
	state *commitSignalState
	sig   syscall.Signal
	err   error
}

type commitStopRelay struct {
	cmd                          *exec.Cmd
	request, reply               *os.File
	identity                     childIdentity
	signals                      commitSignalState
	replies                      chan commitRelayReply
	readerStop, readerDone, done chan struct{}
	stdout, stderr               bytes.Buffer
	waitErr, closeErr            error
	once                         sync.Once
	kill                         func() error
}

func installedCommitRelay() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	// os.Executable names the actual Go ELF also behind makeWrapper's script.
	return filepath.Join(filepath.Dir(executable), "..", "libexec", "confctl-commit-stop-relay.bash"), nil
}

func startCommitStopRelay(ctx context.Context, path string, tty *commitTTY) (*commitStopRelay, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	replyRead, replyWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, requestRead.Close(), requestWrite.Close())
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		return nil, errors.Join(err, requestRead.Close(), requestWrite.Close(), replyRead.Close(), replyWrite.Close())
	}
	r := &commitStopRelay{request: requestWrite, reply: replyRead, replies: make(chan commitRelayReply), readerStop: make(chan struct{}), readerDone: make(chan struct{}), done: make(chan struct{})}
	r.cmd = exec.Command(path)
	r.cmd.Env, r.cmd.Stdin = []string{"LC_ALL=C"}, input
	r.cmd.Stdout, r.cmd.Stderr = &r.stdout, &r.stderr
	r.cmd.ExtraFiles = []*os.File{requestRead, replyWrite}
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: tty.caller}
	err = r.cmd.Start()
	endsErr := errors.Join(input.Close(), requestRead.Close(), replyWrite.Close())
	if err != nil {
		return nil, errors.Join(err, endsErr, requestWrite.Close(), replyRead.Close())
	}
	r.kill = r.cmd.Process.Kill
	go func() { r.waitErr = r.cmd.Wait(); close(r.done) }()
	go r.readReplies()
	r.identity, err = childID(r.cmd.Process.Pid)
	err = errors.Join(endsErr, err)
	if err == nil {
		err = r.association(tty)
	}
	if err == nil {
		select {
		case record := <-r.replies:
			err = record.err
			if record.state == nil && err == nil {
				err = errors.New("missing commit relay startup boundary")
			}
			if record.state != nil {
				r.signals = *record.state
			}
		case <-r.done:
			err = r.exitError()
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, errors.Join(err, r.close())
	}
	return r, nil
}

func (r *commitStopRelay) readReplies() {
	defer close(r.readerDone)
	reader := bufio.NewReader(r.reply)
	s, err := readCommitStartup(reader)
	if !r.sendReply(commitRelayReply{state: &s, err: err}) || err != nil {
		return
	}
	for {
		line, err := reader.ReadString('\n')
		var sig syscall.Signal
		if err == nil {
			sig, err = parseCommitACK(line)
		} else {
			err = fmt.Errorf("incomplete commit relay acknowledgement: %w", err)
		}
		if !r.sendReply(commitRelayReply{sig: sig, err: err}) || err != nil {
			return
		}
	}
}

func (r *commitStopRelay) sendReply(reply commitRelayReply) bool {
	select {
	case r.replies <- reply:
		return true
	case <-r.readerStop:
		return false
	}
}

func (r *commitStopRelay) association(t *commitTTY) error {
	if err := t.association(); err != nil {
		return err
	}
	if !childSame(r.identity) {
		return errors.New("commit relay identity no longer owned")
	}
	sid, err := commitSID(r.identity.pid)
	if err != nil {
		return err
	}
	if r.identity.group != t.caller || sid != t.sid {
		return errors.New("commit relay group/session association changed")
	}
	return nil
}

func (r *commitStopRelay) requestStop(t *commitTTY, sig syscall.Signal) error {
	if err := r.association(t); err != nil {
		return err
	}
	name := commitStopName(sig)
	if name == "" {
		return errors.New("invalid commit stop request")
	}
	_, err := io.WriteString(r.request, name+"\n")
	return err
}

func (r *commitStopRelay) exitError() error {
	return fmt.Errorf("commit stop relay exited unexpectedly: %v; stdout=%q stderr=%q", r.waitErr, r.stdout.String(), r.stderr.String())
}

func (r *commitStopRelay) close() error {
	r.once.Do(func() {
		close(r.readerStop)
		r.closeErr = errors.Join(r.request.Close(), r.reply.Close())
		// os.Process synchronizes signalling against its one concurrent Wait
		// (pidfd or sigMu). Never signal the helper's caller-owned group.
		killErr := r.kill()
		if !errors.Is(killErr, os.ErrProcessDone) {
			r.closeErr = errors.Join(r.closeErr, killErr)
		}
		<-r.done
		<-r.readerDone
	})
	return r.closeErr
}

type commitJobOps struct {
	request  func(syscall.Signal) error
	resume   func() error
	cont     func() error
	eligible func(syscall.Signal) bool
}

// Only this loop owns transition/request association and terminal actions.
// Its child observer and reply reader run independently, never inside a lock.
func commitJobLoop(ctx context.Context, events <-chan commitObservation, replies <-chan commitRelayReply, helperDone <-chan struct{}, helperError func() error, ops commitJobOps) error {
	var transition, pending uint64
	var stopped, relayed bool
	var stopSignal, pendingSignal syscall.Signal
	apply := func(record commitObservation) (bool, error) {
		if record.err != nil {
			return true, record.err
		}
		e := record.event
		if e.code <= commitDumped {
			return true, nil
		}
		transition++
		switch e.code {
		case commitStopped, commitTrapped:
			stopped, relayed, stopSignal = true, e.code == commitTrapped, syscall.Signal(e.status)
		case commitContinued:
			stopped = false
			if err := ctx.Err(); err != nil {
				return true, err
			}
			if err := ops.cont(); err != nil {
				return true, err
			}
		}
		return false, nil
	}
	drain := func() (bool, error) {
		for {
			if err := ctx.Err(); err != nil {
				return true, err
			}
			select {
			case record, ok := <-events:
				if !ok {
					return true, errors.New("commit child observer closed without exit")
				}
				if done, err := apply(record); done {
					return true, err
				}
			default:
				return false, nil
			}
		}
	}
	ack := func(reply commitRelayReply) error {
		if reply.err != nil {
			return reply.err
		}
		if pending == 0 || reply.state != nil || reply.sig != pendingSignal {
			return errors.New("unexpected or duplicate commit relay acknowledgement")
		}
		matching := stopped && pending == transition
		pending = 0
		if matching && ops.eligible(reply.sig) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := ops.resume(); err != nil {
				return err
			}
			stopped = false
		}
		return nil
	}
	for {
		if done, err := drain(); done {
			return err
		}
		// Process queued replies before issuing a new request. This also rejects
		// duplicates rather than associating them with a later transition.
		select {
		case reply, ok := <-replies:
			if !ok {
				return errors.New("commit relay replies closed")
			}
			if done, err := drain(); done {
				return err
			}
			if err := ack(reply); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case <-helperDone:
			if done, err := drain(); done {
				return err
			}
			return helperError()
		default:
		}
		if stopped && !relayed && pending == 0 && commitStopName(stopSignal) != "" {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := ops.request(stopSignal); err != nil {
				return err
			}
			pending, pendingSignal, relayed = transition, stopSignal, true
		}
		select {
		case record, ok := <-events:
			if !ok {
				return errors.New("commit child observer closed without exit")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if done, err := apply(record); done {
				return err
			}
		case reply, ok := <-replies:
			if !ok {
				return errors.New("commit relay replies closed")
			}
			if done, err := drain(); done {
				return err
			}
			if err := ack(reply); err != nil {
				return err
			}
		case <-helperDone:
			if done, err := drain(); done {
				return err
			}
			return helperError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
