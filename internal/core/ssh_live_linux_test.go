package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/charmbracelet/x/ansi"
	"github.com/vpsfreecz/confctl/internal/ui"
	"golang.org/x/sys/unix"
)

type sshTerminalTranscript struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *sshTerminalTranscript) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func sshReadPTY(master *os.File) (*sshTerminalTranscript, chan error) {
	transcript := &sshTerminalTranscript{}
	done := make(chan error, 1)
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			transcript.mu.Lock()
			transcript.b.Write(b[:n])
			transcript.mu.Unlock()
			if err != nil {
				done <- err
				return
			}
		}
	}()
	return transcript, done
}

func sshPTYSize(t *testing.T, f *os.File, rows, cols uint16) {
	t.Helper()
	size := [4]uint16{rows, cols}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		t.Fatal(errno)
	}
}

func sshAwaitTerminal(t *testing.T, transcript *sshTerminalTranscript, text string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond * 10)
	defer tick.Stop()
	for {
		if strings.Contains(transcript.String(), text) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("missing terminal output", text, transcript.String())
		case <-tick.C:
		}
	}
}

func TestSSHLiveTTYChild(t *testing.T) {
	if os.Getenv("CORE_SSH_LIVE") != "1" {
		return
	}
	root := os.Getenv("CORE_SSH_LIVE_ROOT")
	slow := connectionMachine("alpha", "alpha", "slow.invalid", true)
	fast := connectionMachine("zeta", "zeta", "fast.invalid", true)
	e := &Engine{Root: root, Context: context.Background(), Color: "never", liveCommand: "confctl ssh -y -p -g * cat literal", Inventory: []Machine{slow, fast}}
	if err := e.OpenLog("ssh"); err != nil {
		t.Fatal(err)
	}
	defer e.Log.Close()
	var before, after unix.Termios
	if err := basicNativeTermios(os.Stdin, unix.TCGETS, &before); err != nil {
		t.Fatal(err)
	}
	command := []string{"cat", "literal token"}
	if err := e.SSH(sshInvocation(t, "ssh", "-y", "-p", "-g", "*", command[0], command[1]), os.Stdin, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if err := basicNativeTermios(os.Stdin, unix.TCGETS, &after); err != nil || before != after {
		t.Fatal("live SSH changed terminal mode", err)
	}

	log, err := os.ReadFile(e.Log.Name())
	if err != nil || !bytes.Contains(log, []byte("fast\toutput\n")) {
		t.Fatal("terminal substitution changed raw log", string(log), err)
	}
}

func TestSSHLiveTTYStreamsCompletionProgressAndResize(t *testing.T) {
	t.Setenv("CONFCTL_TTY", "1")
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	root := t.TempDir()
	release := filepath.Join(root, "release")
	sshExecutables(t, map[string]sshChildStimulus{
		"slow.invalid": {Barrier: 2, ReleasePath: release, Out: "slow output\n"},
		"fast.invalid": {Barrier: 2, Out: "fast\toutput\n"},
	})
	t.Setenv("CORE_SSH_LIVE", "1")
	t.Setenv("CORE_SSH_LIVE_ROOT", root)
	master, slave := openCommitTestPTY(t)
	sshPTYSize(t, slave, 24, 80)
	transcript, readDone := sshReadPTY(master)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSSHLiveTTYChild$")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	owned := startDeliveryFixture(t, cmd)
	readJoined := false
	owned.closeIO = func() error {
		_ = slave.Close()
		_ = master.Close()
		if !readJoined {
			select {
			case <-readDone:
				readJoined = true
			case <-time.After(5 * time.Second):
				return errors.New("SSH PTY reader did not join")
			}
		}
		return nil
	}

	_ = slave.Close()
	sshAwaitTerminal(t, transcript, "1/2 (50%)")
	if strings.Contains(transcript.String(), "2/2 (100%)") {
		t.Fatal("progress waited for complete batch", transcript.String())
	}
	sshAwaitTerminal(t, transcript, "fast output")
	// Full command context belongs to the wide live panel, before resize.
	for _, header := range []string{"Command: confctl ssh -y -p -g * cat literal\r\n", "SSH: cat literal\\ token\r\n"} {
		sshAwaitTerminal(t, transcript, header)
	}
	select {
	case <-owned.done:
		t.Fatal("live output was only observed after batch exit")
	default:
	}
	sshPTYSize(t, master, 30, 32)
	if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	logs := deliveryLogs(t, root)
	if len(logs) != 1 {
		t.Fatal("missing active log", logs)
	}
	sshAwaitTerminal(t, transcript, ansi.Truncate("Full log: "+logs[0], 32, "…"))
	if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err, joined := owned.wait(5 * time.Second); !joined || err != nil {
		t.Fatal("terminal child did not finish", joined, err)
	}
	select {
	case err := <-readDone:
		readJoined = true
		if !errors.Is(err, syscall.EIO) && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PTY reader did not join")
	}

	text := transcript.String()
	for _, want := range []string{"Live view", "\x1b[H", "zeta:\r\nExit status: 0\r\nfast\toutput\r\n\r\nalpha:"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing source display/result boundary", want, text)
		}
	}
	frame, after := liveLastPaint(t, text, 30)
	// Final selected fields obey the actual 32-column geometry; the full
	// percentage is not replayed outside the retained panel to evade clipping.
	finalModel, err := ui.NewLiveModel(ui.LiveHeader{}, []ui.LiveRow{
		{ID: "ssh", Label: "Commands", Current: big.NewInt(2), Total: big.NewInt(2), State: ui.LiveSucceeded},
		{ID: "host:alpha", Label: "alpha", State: ui.LiveSucceeded, Detail: "exit status 0", Target: true},
		{ID: "host:zeta", Label: "zeta", State: ui.LiveSucceeded, Detail: "exit status 0", Target: true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	finalRows := finalModel.Frame(30, 32, 0)
	for _, row := range finalRows[len(finalRows)-3:] {
		if !strings.Contains(frame, "\x1b[2K\r"+row) {
			t.Fatal("wrong final selected count/state", row, frame)
		}
	}
	if !strings.Contains(frame, ansi.Truncate("Command: confctl ssh -y -p -g * cat literal", 32, "…")) || !strings.Contains(frame, "SSH: cat literal\\ token") {
		t.Fatal("resized final header missing", frame)
	}
	if strings.Contains(after, "Command:") || strings.Contains(after, "SSH:") || strings.Contains(after, "Commands [") || strings.Count(after, "Full log: "+logs[0]) != 1 {
		t.Fatal("final panel replayed/missing full locator", after)
	}
	if strings.Contains(text, "on 2 machines") {
		t.Fatal("terminal display acquired non-TTY header", text)
	}
}

func TestSSHLiveDisabledByEitherPipeAndEnvironment(t *testing.T) {
	_, tty := openCommitTestPTY(t)
	e := connectionEngine(t)
	e.Color = "always" // Forced styling never forces live-view admission.
	t.Setenv("CONFCTL_TTY", "0")
	v, err := e.sshLive(e.Context, tty, tty, []string{"cat"}, []Machine{{Name: "host"}}, nil)
	if err != nil || v != nil {
		t.Fatal(v, err)
	}
	t.Setenv("CONFCTL_TTY", "1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for _, files := range [][2]*os.File{{tty, w}, {w, tty}, {w, w}} {
		v, err := e.sshLive(e.Context, files[0], files[1], []string{"cat"}, []Machine{{Name: "host"}}, nil)
		if err != nil || v != nil {
			t.Fatal(v, err)
		}
	}
}

func TestLiveViewTTYConstructionRefusalKeepsBorrowedTerminal(t *testing.T) {
	t.Setenv("CONFCTL_TTY", "1")
	_, tty := openCommitTestPTY(t)
	readonly, err := os.OpenFile(tty.Name(), os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	var before, after unix.Termios
	if err := basicNativeTermios(tty, unix.TCGETS, &before); err != nil {
		t.Fatal(err)
	}
	calls := 0
	v, err := (&Engine{}).openLiveView(context.Background(), readonly, tty, liveViewSpec{Rows: []ui.LiveRow{{ID: "all", Label: "Fixture", Current: big.NewInt(0)}}}, func(error) { calls++ })
	if err == nil || v != nil || calls != 1 {
		if v != nil {
			_ = v.Close()
		}
		t.Fatal("read-only terminal construction accepted", err, calls)
	}
	if !sshIsTTY(readonly) || !sshIsTTY(tty) {
		t.Fatal("construction closed borrowed descriptors")
	}
	if err := basicNativeTermios(tty, unix.TCGETS, &after); err != nil || before != after {
		t.Fatal("construction changed terminal mode", err)
	}
}
