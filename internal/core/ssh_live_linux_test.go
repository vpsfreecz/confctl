package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
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
	e := &Engine{Root: root, Context: context.Background(), Color: "never", Inventory: []Machine{slow, fast}}
	if err := e.OpenLog("ssh"); err != nil {
		t.Fatal(err)
	}
	defer e.Log.Close()
	command := []string{"cat", "literal token"}
	view, err := e.sshLive(os.Stdout, os.Stderr, command, 2)
	if err != nil || view == nil {
		t.Fatal(view, err)
	}
	r, err := e.sshParallelProgress([]Machine{slow, fast}, command, sshInput{}, sshDeps{Run: e.sshCommandProcess}, view.advance)
	if finishErr := view.finish(e.Context, err == nil); err != nil || finishErr != nil {
		t.Fatal(err, finishErr)
	}
	if len(r) != 2 || r[0].Name != "zeta" || r[1].Name != "alpha" {
		t.Fatal("terminal view changed completion order", r)
	}
	if err := sshPrintResults(os.Stdout, r, true); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(e.Log.Name())
	if err != nil || !bytes.Contains(log, []byte("fast\toutput\n")) {
		t.Fatal("terminal substitution changed raw log", string(log), err)
	}
	b, _ := json.Marshal([]string{r[0].Name, r[1].Name})
	if err := os.WriteFile(filepath.Join(root, "result.json"), b, 0600); err != nil {
		t.Fatal(err)
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
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(waitDone) }()
	defer func() {
		cancel()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			t.Error("terminal child not reaped")
		}
	}()
	_ = slave.Close()
	sshAwaitTerminal(t, transcript, "1/2 (50%)")
	if strings.Contains(transcript.String(), "2/2 (100%)") {
		t.Fatal("progress waited for complete batch", transcript.String())
	}
	sshAwaitTerminal(t, transcript, "fast  output")
	sshPTYSize(t, master, 30, 32)
	sshAwaitTerminal(t, transcript, "Live view --------------------->")
	if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
		if waitErr != nil {
			t.Fatal(waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal child did not finish")
	}
	if err := <-readDone; !errors.Is(err, syscall.EIO) && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	text := transcript.String()
	for _, want := range []string{"Executing cat literal\\ token\r\n", "Live view", "\x1b7\x1b[H", "\x1b8", "2/2 (100%)", "zeta:\r\nExit status: 0\r\nfast\toutput\r\n\r\nalpha:"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing source display/result boundary", want, text)
		}
	}
	if strings.Contains(text, "on 2 machines") {
		t.Fatal("terminal display acquired non-TTY header", text)
	}
	b, err := os.ReadFile(filepath.Join(root, "result.json"))
	if err != nil || string(b) != `["zeta","alpha"]` {
		t.Fatal(string(b), err)
	}
}

func TestSSHLiveDisabledByEitherPipeAndEnvironment(t *testing.T) {
	_, tty := openCommitTestPTY(t)
	e := connectionEngine(t)
	t.Setenv("CONFCTL_TTY", "0")
	v, err := e.sshLive(tty, tty, []string{"cat"}, 1)
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
		v, err := e.sshLive(files[0], files[1], []string{"cat"}, 1)
		if err != nil || v != nil {
			t.Fatal(v, err)
		}
	}
}
