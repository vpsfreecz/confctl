package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/vpsfreecz/confctl/internal/deploy"
)

// This child is the real health command executed by the production Process
// owner. The slow command stays alive until the PTY parent observes progress.
func TestHealthLiveCommandChild(t *testing.T) {
	if os.Getenv("CONFCTL_HEALTH_LIVE_CHILD") != "1" {
		return
	}
	args := os.Args
	mode := args[len(args)-1]
	root := os.Getenv("CONFCTL_HEALTH_LIVE_ROOT")
	if mode == "fast" {
		marker := filepath.Join(root, "failed-once")
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			if err := os.WriteFile(marker, []byte("attempt"), 0600); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(os.Stderr, "HEALTH_RAW_FAILURE_ONLY")
			os.Exit(7)
		}
		fmt.Fprintln(os.Stdout, "HEALTH_RAW_SUCCESS_ONLY")
		return
	}
	if mode != "slow" {
		t.Fatal("unexpected health command", mode)
	}
	if err := os.WriteFile(filepath.Join(root, "slow-started"), []byte("alive"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("health child release never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHealthLivePresentationChild(t *testing.T) {
	mode := os.Getenv("CONFCTL_HEALTH_LIVE")
	if mode != "standalone" && mode != "deployment" {
		return
	}
	root := os.Getenv("CONFCTL_HEALTH_LIVE_ROOT")
	command := func(mode string) string {
		return strings.Replace(healthCommandJSON(os.Args[0], "-test.run=^TestHealthLiveCommandChild$", "--", mode), `"cooldown":3`, `"cooldown":0`, 1)
	}
	e := healthCLIEngine(t, healthMachineJSON("live-machine", "nixos", `{"builderCommands":[`+command("fast")+`,`+command("slow")+`]}`))
	e.Root = root
	e.Color = os.Getenv("CONFCTL_HEALTH_LIVE_COLOR")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	e.Context = ctx
	if err := e.OpenLog("health-check"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseLog(false)
	logPath := e.Log.Name()
	if err := os.WriteFile(filepath.Join(root, "log-path"), []byte(logPath), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "standalone" {
		result, err := e.HealthCheck(healthInvocation(t, "--yes", "-j", "2"), strings.NewReader(""), os.Stdout, os.Stderr)
		if err != nil || !result.RetainLog || len(result.Report.Results) != 2 || !result.Report.Results[0].Passed() || !result.Report.Results[1].Passed() {
			t.Fatal(result, err)
		}
		return
	}
	// Reach deployment's actual RunHealth closure and real deployProcess, not a
	// fake view or copied presentation callback. No host activation is performed.
	m := e.Inventory[0]
	d := e.deployOperations(ctx, []Machine{m}, []Machine{m}, func(context.Context) (string, error) { return "", io.EOF }, os.Stdout, os.Stderr, deployInvocation(t, "--yes"))
	checks, err := m.healthSpecs()
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.RunHealth(ctx, []deploy.Machine{{Name: m.Name}}, checks)
	if err != nil || !report.RetainLog || len(report.Results) != 2 || !report.Results[0].Passed() || !report.Results[1].Passed() {
		t.Fatal(report, err)
	}
}

// Root selects execution after committed independent review. These ordinary
// tests use the existing production PTY/process helpers.
func TestHealthLiveProductionStandaloneAndDeploymentBeforeChildExit(t *testing.T) {
	for _, mode := range []string{"standalone", "deployment"} {
		for _, color := range []string{"always", "never"} {
			t.Run(mode+"/"+color, func(t *testing.T) {
				t.Setenv("CONFCTL_TTY", "1")
				t.Setenv("NO_COLOR", "1")
				t.Setenv("CONFCTL_HEALTH_LIVE_COLOR", color)
				t.Setenv("CONFCTL_HEALTH_LIVE", mode)
				t.Setenv("CONFCTL_HEALTH_LIVE_CHILD", "1")
				root := t.TempDir()
				t.Setenv("CONFCTL_HEALTH_LIVE_ROOT", root)
				master, slave := openCommitTestPTY(t)
				sshPTYSize(t, slave, 24, 80)
				transcript, readDone := sshReadPTY(master)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHealthLivePresentationChild$")
				cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
				cmd.Env = os.Environ()
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				joined := false
				defer func() {
					cancel()
					if !joined {
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("health presentation child not joined")
						}
					}
				}()
				_ = slave.Close()
				sshAwaitTerminal(t, transcript, "2 checks on")
				sshAwaitTerminal(t, transcript, "1/2")
				sshAwaitTerminal(t, transcript, "failed #1")
				sshAwaitTerminal(t, transcript, "live-machine")
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(root, "slow-started")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("health progress did not reach blocked real child")
					}
					time.Sleep(10 * time.Millisecond)
				}
				select {
				case err := <-done:
					joined = true
					t.Fatal("terminal progress only arrived after operation exit", err, transcript.String())
				default:
				}
				before := ansi.Strip(transcript.String())
				if strings.Contains(before, "HEALTH_RAW_") || strings.Contains(before, "2 checks passed, 0 failed") {
					t.Fatal("semantic health view tailed child output or finished early", before)
				}
				frame := healthCurrentPTYFrame(t, transcript)
				plain := ansi.Strip(frame)
				title, bottom := strings.Index(plain, "Live view"), strings.Index(plain, "<---")
				progress := regexp.MustCompile("(?m)^\r?" + regexp.QuoteMeta("Health checks") + ` +\[running[|/\\-]\]`).FindStringIndex(plain)
				if title < 0 || bottom <= title || progress == nil || progress[0] <= bottom || !strings.Contains(plain, "Health checks: 2 checks on live-machine") || !strings.Contains(plain, "1/2") {
					t.Fatal("current health frame lost compact context/log/bar order", frame)
				}
				if color == "always" {
					if !strings.Contains(frame, "\x1b[33mlive-machine\x1b[0m") || !strings.Contains(frame, "\x1b[1mLive view") {
						t.Fatal("health frame lost semantic styles under explicit always", frame)
					}
				} else if strings.Contains(frame, "\x1b[0m") {
					t.Fatal("color-never health frame gained source styling", frame)
				}
				sshPTYSize(t, master, 12, 72)
				if err := os.WriteFile(filepath.Join(root, "release"), []byte("release"), 0600); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					joined = true
					if err != nil {
						t.Fatal(err, transcript.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("health child did not exit after release")
				}
				if err := <-readDone; !errors.Is(err, syscall.EIO) && !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
				if !strings.Contains(ansi.Strip(transcript.String()), "2 checks passed, 0 failed") {
					t.Fatal(transcript.String())
				}
				logPath, err := os.ReadFile(filepath.Join(root, "log-path"))
				if err != nil {
					t.Fatal(err)
				}
				log, err := os.ReadFile(string(logPath))
				if err != nil || !bytes.Contains(log, []byte("HEALTH_RAW_FAILURE_ONLY")) || !bytes.Contains(log, []byte("HEALTH_RAW_SUCCESS_ONLY")) {
					t.Fatal("semantic display lost raw failed-attempt log", string(log), err)
				}
			})
		}
	}
}

// Inspect the latest complete 80x24 repaint while both real health processes
// and the release barrier are still owned by the existing production fixture.
func healthCurrentPTYFrame(t *testing.T, transcript *sshTerminalTranscript) string {
	t.Helper()
	frame := deployLiveFrame(t, transcript, "1/2", "failed #1")
	rows := strings.Split(frame, "\n")
	title, bottom := -1, -1
	for j, row := range rows {
		line := strings.Trim(ansi.Strip(row), "\r")
		if ansi.StringWidth(line) > 80 {
			t.Fatal("health frame exceeds width", line)
		}
		if strings.Contains(line, "Live view") {
			title = j
		}
		if strings.HasPrefix(line, "<---") {
			bottom = j
		}
	}
	if title < 0 || bottom-title-1 < 12 || len(rows)-bottom-1 > 6 {
		t.Fatal("health viewport lacks Ruby log region", ansi.Strip(frame))
	}
	return frame
}

func TestHealthCurrentPTYFrameRejectsIncompleteOrStale(t *testing.T) {
	const reserve = "\x1b[24;1H\x1b[2K"
	paint := func(progress, last string) string {
		rows := make([]string, 23)
		rows[0] = "Health checks: 2 checks on live-machine"
		rows[2] = "Live view --->"
		rows[18] = "<---"
		rows[19] = "Health checks [" + progress + "]"
		rows[20] = "live-machine failed #1"
		rows[22] = last
		return "\x1b[H\x1b[2K\r" + strings.Join(rows, "\n\x1b[2K\r") + reserve
	}
	old := paint("1/2", "older")
	valid := paint("1/2", "released")
	for _, tc := range []struct {
		name, initial string
	}{
		{"partial-final-row", strings.TrimSuffix(old, "older"+reserve) + "part"},
		{"newer-complete-nonmatching", old + paint("0/2", "newer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transcript := &sshTerminalTranscript{}
				transcript.b.WriteString(tc.initial)
				frames := make(chan string, 1)
				go func() { frames <- healthCurrentPTYFrame(t, transcript) }()
				// The observer has read the initial transcript and either returned
				// or blocked on its polling timer; no wall-clock race is involved.
				synctest.Wait()
				var frame string
				select {
				case frame = <-frames:
					t.Error("health observer accepted incomplete or stale frame", frame)
				default:
				}
				transcript.mu.Lock()
				transcript.b.WriteString(valid)
				transcript.mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				synctest.Wait()
				if frame == "" {
					select {
					case frame = <-frames:
					default:
						t.Fatal("health observer did not admit the released complete frame")
					}
				}
				if want := strings.TrimSuffix(strings.TrimPrefix(valid, "\x1b[H"), reserve); frame != want {
					t.Error("health observer did not select the current valid frame", frame)
				}
			})
		})
	}
}
