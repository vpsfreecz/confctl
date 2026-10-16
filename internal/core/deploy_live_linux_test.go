//go:build linux

package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// These acceptance tests reach Main and the real deployment/view/process
// adapters. Existing disposable tools, controlling PTY and capture are reused.
// Each blocked tool has an explicit release file; elapsed sleeps do not prove
// live output. Root selects this PTY group only after committed review.
func deployLiveEnv(t *testing.T, f deployPublicFixture, mode string) []string {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	return []string{"CONFCTL_TTY=1", "CONFCTL_DEPLOY_PUBLIC_HOOK=1", "CONFCTL_EXTENSION_REGISTRY=" + filepath.Join(f.root, "registry.json"), "CONFCTL_EXTENSION_ROOT=" + f.root, "DEPLOY_LIVE_MODE=" + mode, "DEPLOY_LIVE_SLEEP=" + sleep}
}

func deployLiveBlocked(t *testing.T, f deployPublicFixture, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("Main completed before live observation", err)
	}
	pid, err := strconv.Atoi(f.text("live-pid"))
	if err != nil || syscall.Kill(pid, 0) != nil {
		t.Fatal("tool was not alive before release", err)
	}
}

func deployLiveRelease(t *testing.T, f deployPublicFixture) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
}

func deployLiveResult(t *testing.T, f deployPublicFixture, want int) {
	t.Helper()
	var result struct {
		Code     int
		Restored bool
	}
	if err := json.Unmarshal([]byte(f.text("result.json")), &result); err != nil || result.Code != want || !result.Restored {
		t.Fatal("deployment final status/terminal state", result, want, err)
	}
}

// Called inside terminal's interaction callback, before its deferred process
// group cleanup. Main must return and reap its marked child on its own.
func deployLiveReapedBeforeCleanup(t *testing.T, f deployPublicFixture, want int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var result struct{ Code int }
		if json.Unmarshal([]byte(f.text("result.json")), &result) == nil {
			if result.Code != want {
				t.Fatal("Main returned unexpected status before cleanup", result.Code, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Main did not finish before fixture cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(f.text("live-pid"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("production child survived Main, before fixture cleanup", pid, err)
	}
}

// A real prompt has acknowledged suspension/round teardown before writing its
// complete suffix. Resize must not produce even a timer frame while input waits.
func deployLiveQuietPrompt(t *testing.T, cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript, prompt string) {
	t.Helper()
	sshAwaitTerminal(t, transcript, prompt)
	before := transcript.String()
	sshPTYSize(t, master, 10, 96)
	if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-timer.C:
			if transcript.String() != before {
				t.Fatal("terminal repainted while prompt was suspended", transcript.String()[len(before):])
			}
			return
		case <-tick.C:
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil || transcript.String() != before {
				t.Fatal("prompt exited or repainted before answer", err, transcript.String())
			}
		}
	}
}

func deployLiveRow(label, state string) *regexp.Regexp {
	pattern := `(?m)^\r?` + regexp.QuoteMeta(label) + `[ \t]+\[`
	if state != "" {
		pattern += regexp.QuoteMeta(state)
		if state == "running" {
			pattern += `[|/\\-]?`
		}
		pattern += `\]`
	}
	return regexp.MustCompile(pattern)
}

func deployLiveFrame(t *testing.T, transcript *sshTerminalTranscript, want ...string) string {
	t.Helper()
	return deployLiveFrameRows(t, transcript, want)
}

func deployLiveFrameRows(t *testing.T, transcript *sshTerminalTranscript, want []string, rows ...*regexp.Regexp) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		frames := strings.Split(transcript.String(), "\x1b[H")
		for i := len(frames) - 1; i > 0; i-- {
			// Every case using this assertion requests 24x80. The cursor reserve
			// terminates a complete frame; a partial tail is never accepted.
			end := strings.Index(frames[i], "\x1b[24;1H\x1b[2K")
			if end < 0 || strings.Count(frames[i][:end], "\x1b[2K\r") != 23 {
				continue
			}
			frame := frames[i][:end]
			plain := ansi.Strip(frame)
			matches := true
			for _, text := range want {
				matches = matches && strings.Contains(plain, text)
			}
			for _, row := range rows {
				matches = matches && row.MatchString(plain)
			}
			if matches {
				return frame
			}
			// Only the last complete frame may satisfy the observation.
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing complete current deployment frame", want, transcript.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func deployLiveLogAboveRows(t *testing.T, frame, token string, row *regexp.Regexp) {
	t.Helper()
	plain := ansi.Strip(frame)
	top, log := strings.Index(plain, "Live view ---"), strings.Index(plain, token)
	bottom, progress := strings.Index(plain, "<---"), -1
	if match := row.FindStringIndex(plain); match != nil {
		progress = match[0]
	}
	if top < 0 || log <= top || bottom <= log || progress <= bottom {
		t.Fatal("deployment frame lost bordered logs above progress", plain)
	}
}

func TestDeployLivePTYCopyBeforeCompletion(t *testing.T) {
	for _, mode := range []string{"bulk", "one-by-one", "interactive", "failure", "cancel", "cancel-int"} {
		t.Run(mode, func(t *testing.T) {
			f := deployPublic(t, "first", "second")
			f.tool(t, "nix-copy-closure", `host="${2#root@}"
printf 'copy:%s\n' "$host" >> "$TRACE"
if [[ "$host" == second.invalid ]]; then
 printf '%s' "$$" > "$HOME/live-pid"
 printf "copying 7 paths\ncopying path '/copy-live-token'\n" >&2
 while [[ ! -f "$HOME/release" ]]; do "$DEPLOY_LIVE_SLEEP" 0.02; done
 [[ $DEPLOY_LIVE_MODE == failure ]] && exit 23
fi
printf 'copied:%s\n' "$host" >> "$HOME/effects"
`)
			args := []string{"deploy", "-y", "-gselected", "--copy-only", "--max-concurrent-copy=1"}
			if mode == "one-by-one" {
				args = append(args, "--one-by-one")
			}
			if mode == "interactive" {
				args = append(args, "--interactive")
			}
			out := f.terminal(t, args, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshPTYSize(t, master, 24, 80)
				_ = cmd.Process.Signal(syscall.SIGWINCH)
				if mode == "interactive" {
					sshAwaitTerminal(t, transcript, "Choose [yn]")
					if strings.Contains(f.text("trace"), "copy:") {
						t.Fatal("copy ran before permission")
					}
					if _, err := master.WriteString("y\ny\n"); err != nil {
						t.Fatal(err)
					}
				}
				sshAwaitTerminal(t, transcript, "/copy-live-token")
				deployLiveBlocked(t, f, cmd)
				if !strings.Contains(f.text("effects"), "copied:first.invalid") || strings.Contains(f.text("effects"), "copied:second.invalid") {
					t.Fatal("copy effects reordered", f.text("effects"))
				}
				if !strings.Contains(ansi.Strip(transcript.String()), "Copying closures:") || !strings.Contains(transcript.String(), "second") {
					t.Fatal("missing live operation/host context", transcript.String())
				}
				if mode == "bulk" {
					frame := deployLiveFrameRows(t, transcript, []string{"Command: confctl deploy", "Copying closures:", "/copy-live-token"}, deployLiveRow("All hosts", "running"), deployLiveRow("first", "succeeded"), deployLiveRow("second", "running"))
					deployLiveLogAboveRows(t, frame, "/copy-live-token", deployLiveRow("All hosts", "running"))
				}
				sshPTYSize(t, master, 12, 54)
				_ = cmd.Process.Signal(syscall.SIGWINCH)
				if mode == "cancel" || mode == "cancel-int" {
					signal := syscall.SIGTERM
					if mode == "cancel-int" {
						signal = syscall.SIGINT
					}
					if err := cmd.Process.Signal(signal); err != nil {
						t.Fatal(err)
					}
					deployLiveReapedBeforeCleanup(t, f, 1)
				} else {
					deployLiveRelease(t, f)
				}
			}, append(deployLiveEnv(t, f, mode), "NO_COLOR=1")...)
			want := 0
			if mode == "failure" || strings.HasPrefix(mode, "cancel") {
				want = 1
				if f.text("effects") != "copied:first.invalid\n" {
					t.Fatal("copy failure/cancel changed prior effects or admitted later work", f.text("effects"))
				}
			}
			deployLiveResult(t, f, want)
			if !strings.Contains(out, "/copy-live-token") || strings.Contains(f.text("effects"), "activated:") {
				t.Fatal(out, f.text("effects"))
			}
			if want == 1 && len(f.logs(t)) != 1 {
				t.Fatal("failed/canceled copy lost log")
			}
			if mode == "bulk" && regexp.MustCompile("\x1b\\[[0-9;:]*m").MatchString(out) {
				t.Fatal("NO_COLOR transcript contains SGR", out)
			}
		})
	}
}

func deployLiveInventory(t *testing.T, f deployPublicFixture, edit func(map[string]map[string]any)) {
	t.Helper()
	var inventory map[string]map[string]any
	if err := json.Unmarshal([]byte(f.text("machines.json")), &inventory); err != nil {
		t.Fatal(err)
	}
	edit(inventory)
	b, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "machines.json", string(b))
}

func deployLiveActivationTool(t *testing.T, f deployPublicFixture) {
	t.Helper()
	f.tool(t, "ssh", `request="${@: -1}"
printf 'ssh:%s\n' "$request" >> "$TRACE"
case "$request" in
 'cat /proc/uptime') printf '600 0\n';;
 'realpath /run/current-system') printf '/previous\n';;
 'bash --norc') while IFS= read -r line; do :; done; printf '/previous\n';;
 *'/bin/switch-to-configuration dry-activate')
  printf '%s' "$$" > "$HOME/dry-pid"
  count=0; [[ -f "$HOME/dry-count" ]] && read -r count < "$HOME/dry-count"
  printf '%s\n' "$((count+1))" > "$HOME/dry-count"
  printf 'dry-%s\n' "$((count+1))";;
 *'/bin/switch-to-configuration '*|*'/fixture/rollback '*)
  printf '%s' "$$" > "$HOME/live-pid"
  printf 'activation-live-token\n'
  if [[ $DEPLOY_LIVE_MODE != profile ]]; then
   while [[ ! -f "$HOME/release" ]]; do "$DEPLOY_LIVE_SLEEP" 0.02; done
  fi
  printf 'activated\n' >> "$HOME/effects";;
 'nix-env -p '*|'carrier-env -p '*)
  if [[ $DEPLOY_LIVE_MODE == profile || $DEPLOY_LIVE_MODE == carried ]]; then
   printf '%s' "$$" > "$HOME/live-pid"
   printf 'profile-live-token\n'
   while [[ ! -f "$HOME/release" ]]; do "$DEPLOY_LIVE_SLEEP" 0.02; done
  fi
  printf 'profile\n' >> "$HOME/effects";;
 'sh -c '*cat*) printf 'switched\n';;
 'sh -c '*confirmed*) :;;
 *) printf 'unexpected request: %s\n' "$request" >&2; exit 98;;
esac
`)
}

func TestDeployLivePTYActivationAndPromptBeforeCompletion(t *testing.T) {
	for _, mode := range []string{"activation", "profile", "rollback", "carried", "retry", "skip", "abort", "prompt-cancel", "prompt-int", "prompt-eof"} {
		t.Run(mode, func(t *testing.T) {
			f := deployPublic(t, "node", "carried")
			deployLiveActivationTool(t, f)
			args := []string{"deploy", "-y", "-gselected", "--no-health-checks", "--disable-auto-rollback"}
			host := "node"
			if mode == "carried" {
				host = "carried"
				deployLiveInventory(t, f, func(ms map[string]map[string]any) { ms["carried"]["carrier"] = "node" })
			}
			if mode == "rollback" {
				args = args[:len(args)-1]
				deployLiveInventory(t, f, func(ms map[string]map[string]any) {
					ms["node"]["metaConfig"].(map[string]any)["autoRollback"] = map[string]any{"enable": true, "timeout": 1}
				})
				path := ".confctl/generations/node/selected/generation.json"
				var record map[string]any
				if err := json.Unmarshal([]byte(f.text(path)), &record); err != nil {
					t.Fatal(err)
				}
				record["auto_rollback"] = "/fixture/rollback"
				b, _ := json.Marshal(record)
				f.write(t, path, string(b))
			}
			interactive := mode == "retry" || mode == "skip" || mode == "abort" || strings.HasPrefix(mode, "prompt-")
			if interactive {
				args = append(args, "--interactive", "--dry-activate-first")
			}
			args = append(args, host)
			if mode == "activation" {
				args = append([]string{"--color=always"}, args...)
			}
			out := f.terminal(t, args, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshPTYSize(t, master, 24, 80)
				_ = cmd.Process.Signal(syscall.SIGWINCH)
				if interactive {
					sshAwaitTerminal(t, transcript, "Choose [yn]")
					_, _ = master.WriteString("y\n")
					sshAwaitTerminal(t, transcript, "Choose [ysar]: ")
					if strings.Contains(f.text("effects"), "activated") {
						t.Fatal("real activation ran before permission")
					}
					if mode == "prompt-cancel" || mode == "prompt-int" {
						pid, err := strconv.Atoi(f.text("dry-pid"))
						if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
							t.Fatal("dry activation child was not reaped before prompt", err)
						}
						deployLiveQuietPrompt(t, cmd, master, transcript, "Choose [ysar]: ")
						signal := syscall.SIGTERM
						if mode == "prompt-int" {
							signal = syscall.SIGINT
						}
						if err := cmd.Process.Signal(signal); err != nil {
							t.Fatal(err)
						}
						return
					}
					if mode == "prompt-eof" {
						_, _ = master.Write([]byte{4})
						return
					}
					if mode == "skip" || mode == "abort" {
						_, _ = master.WriteString(map[string]string{"skip": "s\n", "abort": "a\n"}[mode])
						return
					}
					_, _ = master.WriteString("r\n")
					sshAwaitTerminal(t, transcript, "dry-2")
					_, _ = master.WriteString("y\n")
				}
				token := "activation-live-token"
				if mode == "profile" || mode == "carried" {
					token = "profile-live-token"
				}
				sshAwaitTerminal(t, transcript, token)
				deployLiveBlocked(t, f, cmd)
				if !strings.Contains(ansi.Strip(transcript.String()), "Activating: "+host+" (switch; selected)") {
					t.Fatal("activation context absent")
				}
				frame := deployLiveFrame(t, transcript, "Command: confctl", "Activating: "+host+" (switch; selected)", token)
				// Raw activation output is still plain even when confctl's own
				// header, machine label and terminal state are colored.
				plain := ansi.Strip(frame)
				bottom := strings.Index(plain, "<---")
				if bottom < 0 || !deployLiveRow(host, "").MatchString(plain[bottom:]) {
					t.Fatal("activation row was not below the log", plain)
				}
				deployLiveLogAboveRows(t, frame, token, deployLiveRow(host, ""))
				if mode == "activation" {
					for _, styled := range []string{"\x1b[1mActivating:\x1b[0m", "\x1b[33mnode\x1b[0m", "\x1b[33mswitch\x1b[0m", "\x1b[36mselected\x1b[0m"} {
						if !strings.Contains(frame, styled) {
							t.Fatal("color-on activation lost semantic style", styled, frame)
						}
					}
					for _, line := range strings.Split(frame, "\x1b[2K\r") {
						if strings.Contains(line, token) && strings.Contains(line, "\x1b[") {
							t.Fatal("child log received confctl styling", line)
						}
					}
				}
				deployLiveRelease(t, f)
			}, append(deployLiveEnv(t, f, mode), "NO_COLOR=1")...)
			want := 0
			if mode == "abort" || strings.HasPrefix(mode, "prompt-") {
				want = 1
			}
			deployLiveResult(t, f, want)
			if interactive && mode != "retry" && strings.Contains(f.text("effects"), "activated") {
				t.Fatal("skip/abort activated", out)
			}
			if interactive && mode != "retry" && f.text("effects") != "copied:node.invalid\n" {
				t.Fatal("prompt outcome changed prior copy or admitted profile effects", f.text("effects"))
			}
			if want == 1 && len(f.logs(t)) != 1 {
				t.Fatal("failed/canceled activation prompt lost log")
			}
			if mode == "retry" && f.text("dry-count") != "2\n" {
				t.Fatal("dry retry sequencing changed", f.text("trace"))
			}
		})
	}
}

func TestDeployLivePTYRebootBeforeCompletion(t *testing.T) {
	for _, mode := range []string{"up", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := deployPublic(t, "node")
			f.tool(t, "ssh", `request="${@: -1}"
case "$request" in
 'cat /proc/uptime')
  if [[ ! -f "$HOME/rebooted" ]]; then printf '600 0\n'
  elif [[ ! -f "$HOME/release" ]]; then exit 255
  else printf '1 0\n'; fi;;
 'realpath /run/current-system') printf '/previous\n';;
 'bash --norc') while IFS= read -r line; do :; done; printf '/previous\n';;
 *'/bin/switch-to-configuration boot'|'nix-env -p '*) :;;
 'reboot') printf 'rebooted' > "$HOME/rebooted";;
 *) exit 98;;
esac
`)
			wait := "wait"
			if mode == "timeout" {
				wait = "1"
			}
			args := []string{"deploy", "-y", "-gselected", "--no-health-checks", "--reboot", "--wait-online=" + wait, "node", "boot"}
			out := f.terminal(t, args, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshPTYSize(t, master, 24, 80)
				_ = cmd.Process.Signal(syscall.SIGWINCH)
				sshAwaitTerminal(t, transcript, "waiting-up")
				if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
					t.Fatal("reboot wait already completed")
				}
				if !strings.Contains(ansi.Strip(transcript.String()), "Rebooting: node (boot)") || !strings.Contains(transcript.String(), "elapsed") {
					t.Fatal("reboot progress/context missing", transcript.String())
				}
				deployLiveFrameRows(t, transcript, []string{"Command: confctl deploy", "Rebooting: node (boot)", "waiting-up", "elapsed"}, deployLiveRow("node", "running"))
				if mode == "cancel" {
					_ = cmd.Process.Signal(syscall.SIGTERM)
				}
				if mode == "up" {
					deployLiveRelease(t, f)
				}
			}, deployLiveEnv(t, f, mode)...)
			want := 0
			if mode != "up" {
				want = 1
			}
			deployLiveResult(t, f, want)
			if mode == "up" && !strings.Contains(out, "up") {
				t.Fatal(out)
			}
			if mode != "up" && len(f.logs(t)) != 1 {
				t.Fatal(fmt.Sprintf("%s lost retained log", mode))
			}
		})
	}
}

func TestDeployLivePTYAllCurrent(t *testing.T) {
	f := deployPublic(t, "first", "second")
	f.tool(t, "ssh", `request="${@: -1}"
printf 'query:%s\n' "$request" >> "$TRACE"
case "$request" in
 'cat /proc/uptime') printf '600 0\n';;
 'realpath /run/current-system') printf '%s/top\n' "$PWD";;
 'bash --norc') while IFS= read -r line; do :; done
  printf '%s/top\n/nix/var/nix/profiles/system-1-link;%s/top;1\n' "$PWD" "$PWD";;
 *) printf 'unexpected request: %s\n' "$request" >&2; exit 98;;
esac
`)
	out := f.terminal(t, []string{"deploy", "-y", "-gselected", "--disable-auto-rollback", "--no-health-checks"}, nil, deployLiveEnv(t, f, "all-current")...)
	deployLiveResult(t, f, 0)
	if strings.Count(out, "current-skipped") != 2 || strings.Contains(ansi.Strip(out), "Copying closures:") || strings.Contains(ansi.Strip(out), "Activating:") || strings.Contains(f.text("trace"), "copy:") || f.text("effects") != "" || len(f.logs(t)) != 0 {
		t.Fatal("all-current work opened a view or ran a copy/activation child", out, f.text("trace"), f.text("effects"))
	}
}

func TestDeployLivePTYRebootAbsence(t *testing.T) {
	for _, mode := range []string{"nowait", "local", "carried"} {
		t.Run(mode, func(t *testing.T) {
			f := deployPublic(t, "node", "carried")
			f.tool(t, "ssh", `request="${@: -1}"
printf 'ssh:%s\n' "$request" >> "$TRACE"
case "$request" in
 'cat /proc/uptime')
  if [[ -f "$HOME/reboot-effect" ]]; then printf 'post-reboot-probe\n' >> "$HOME/probes"
  else printf 'query-probe\n' >> "$HOME/probes"; fi
  printf '600 0\n';;
 'realpath /run/current-system'|'realpath /nix/var/nix/profiles/confctl-') printf '/previous\n';;
 'bash --norc') while IFS= read -r line; do :; done; printf '/previous\n';;
 *'/bin/switch-to-configuration boot') printf 'activation\n' >> "$HOME/effects";;
 'nix-env -p '*) printf 'profile\n' >> "$HOME/effects";;
 'reboot') printf 'reboot\n' > "$HOME/reboot-effect";;
 *) printf 'unexpected request: %s\n' "$request" >&2; exit 98;;
esac
`)
			host, wait := "node", "wait"
			if mode == "nowait" {
				wait = "nowait"
			}
			if mode == "carried" {
				host = "carried"
				deployLiveInventory(t, f, func(ms map[string]map[string]any) { ms["carried"]["carrier"] = "node" })
			}
			if mode == "local" {
				deployLiveInventory(t, f, func(ms map[string]map[string]any) {
					ms["node"]["metaConfig"].(map[string]any)["host"].(map[string]any)["target"] = "localhost"
				})
				// Local commands still use only disposable tools and the stored
				// generation's private toplevel; no host profile/reboot is reached.
				f.tool(t, "cat", `[[ "$1" == /proc/uptime ]] || exit 98
printf 'query-probe\n' >> "$HOME/probes"
printf '600 0\n'
`)
				f.tool(t, "realpath", `[[ "$1" == /run/current-system ]] || exit 98
printf '/previous\n'
`)
				f.tool(t, "bash", `[[ "$1" == --norc ]] || exit 98
while IFS= read -r line; do :; done
printf '/previous\n'
`)
				f.tool(t, "nix-env", `printf 'profile\n' >> "$HOME/effects"`)
				f.tool(t, "switch-to-configuration", `[[ "$1" == boot ]] || exit 98
printf 'activation\n' >> "$HOME/effects"
`)
				if err := os.Mkdir(filepath.Join(f.top, "bin"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.tools, "switch-to-configuration"), filepath.Join(f.top, "bin/switch-to-configuration")); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"deploy", "-y", "-gselected", "--no-health-checks", "--reboot", "--wait-online=" + wait, host, "boot"}
			out := f.terminal(t, args, nil, deployLiveEnv(t, f, mode)...)
			deployLiveResult(t, f, 0)
			if strings.Contains(ansi.Strip(out), "Rebooting:") || strings.Contains(out, "waiting-down") || strings.Contains(out, "waiting-up") || f.text("probes") != "query-probe\n" || len(f.logs(t)) != 0 {
				t.Fatal("no-wait/skipped reboot opened a wait view or added uptime probes", out, f.text("probes"))
			}
			if mode == "nowait" {
				if f.text("reboot-effect") != "reboot\n" || !strings.Contains(f.text("effects"), "activation\nprofile\n") {
					t.Fatal("no-wait lost ordinary reboot/activation effects", f.text("effects"), f.text("reboot-effect"))
				}
			} else if f.text("reboot-effect") != "" || !strings.Contains(out, "reboot-skipped") {
				t.Fatal("local/carried skip ran reboot", out, f.text("reboot-effect"))
			}
			if mode == "local" && (strings.Contains(f.text("trace"), "copy:") || !strings.Contains(f.text("effects"), "activation\nprofile\n")) {
				t.Fatal("local no-op copy or disposable activation changed", f.text("trace"), f.text("effects"))
			}
			if mode == "carried" && strings.Contains(f.text("effects"), "activation\n") {
				t.Fatal("carried boot unexpectedly activated a host", f.text("effects"))
			}
		})
	}
}
