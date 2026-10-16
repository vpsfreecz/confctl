//go:build linux

package core

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func deployHealthAwaitChoice(t *testing.T, transcript *sshTerminalTranscript, offset int, prompt string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		text := transcript.String()
		if at := strings.Index(text[offset:], prompt); at >= 0 {
			return offset + at + len(prompt)
		}
		if time.Now().After(deadline) {
			t.Fatal("missing next deployment health choice", text[offset:])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// This composes Main's real one-by-one deployment and round/subset choice loop.
// Persistent probe failure separates explicit operator retries from the health
// runner's own automatic attempt policy (timeout zero means one attempt).
func TestDeployLivePTYHealthChoices(t *testing.T) {
	for _, test := range []struct {
		name, answers string
		fail, pass    int
		code          int
	}{
		{"failed-only", "fc", 2, 1, 0},
		{"all", "rc", 2, 2, 0},
		{"continue", "c", 1, 1, 0},
		{"abort", "a", 1, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := deployPublic(t, "first", "second")
			var terminalTranscript *sshTerminalTranscript
			defer func() {
				if !t.Failed() {
					return
				}
				diagnostic := func(name, text string) {
					const limit = 32 << 10
					if len(text) > limit {
						text = text[:limit] + "\n[diagnostic truncated]"
					}
					t.Logf("%s:\n%s", name, text)
				}
				if terminalTranscript != nil {
					diagnostic("terminal transcript", terminalTranscript.String())
				}
				for _, name := range []string{"result.json", "trace", "effects", "health-requests"} {
					diagnostic(name, f.text(name))
				}
				for _, path := range f.logs(t) {
					data, err := os.ReadFile(path)
					t.Logf("retained log %s: read error %v", path, err)
					diagnostic("log data", string(data))
				}
			}()
			deployLiveInventory(t, f, func(ms map[string]map[string]any) {
				var checks []any
				for _, name := range []string{"fail", "pass"} {
					var command any
					if err := json.Unmarshal([]byte(healthImmediateCommand("health-choice-probe", name)), &command); err != nil {
						t.Fatal(err)
					}
					checks = append(checks, command)
				}
				ms["first"]["metaConfig"].(map[string]any)["healthChecks"] = map[string]any{"builderCommands": checks}
				// The later host has no checks, but still needs the evaluated field.
				ms["second"]["metaConfig"].(map[string]any)["healthChecks"] = map[string]any{"builderCommands": []any{}}
			})
			f.tool(t, "health-choice-probe", `printf '%s\n' "$1" >> "$HOME/health-requests"
case "$1" in
 fail) printf 'HEALTH_CHOICE_RAW_FAILURE\n' >&2; exit 7;;
 pass) printf 'HEALTH_CHOICE_RAW_SUCCESS\n';;
 *) exit 98;;
esac
`)
			args := []string{"deploy", "-y", "-gselected", "--one-by-one", "--interactive", "--disable-auto-rollback", "--max-jobs=1"}
			out := f.terminal(t, args, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				terminalTranscript = transcript
				sshAwaitTerminal(t, transcript, "Choose [yn]: ")
				// Existing copy, activation and health admission read the same
				// retained input. No future retry answer is queued prematurely.
				if _, err := master.WriteString("y\ny\ny\n"); err != nil {
					t.Fatal(err)
				}
				previous := 0
				for round, answer := range test.answers {
					prompt := "Choose [crfa]: "
					// Use a suffix-scoped transcript so a previous choice cannot
					// satisfy the wait for this completed round's prompt.
					promptEnd := deployHealthAwaitChoice(t, transcript, previous, prompt)
					text := transcript.String()
					segment := ansi.Strip(text[previous:promptEnd])
					summary := "1 checks passed, 1 failed"
					header := "Health checks: 2 checks on first"
					wantRequests := "fail\npass\n"
					if round > 0 {
						if test.answers[0] == 'f' {
							summary = "0 checks passed, 1 failed"
							header = "Health checks: 1 checks on first"
							wantRequests += "fail\n"
						} else {
							wantRequests += "fail\npass\n"
						}
					}
					if !strings.Contains(segment, header) || strings.LastIndex(segment, summary) < 0 || strings.LastIndex(segment, summary) > strings.LastIndex(segment, prompt) || f.text("health-requests") != wantRequests {
						t.Fatal("round summary/subset did not precede the actual choice", segment, f.text("health-requests"))
					}
					if strings.Contains(f.text("effects"), "second.invalid") || !strings.Contains(f.text("effects"), "profile:first.invalid") {
						t.Fatal("health choice lost prior effects or admitted later machine", f.text("effects"))
					}
					before := f.text("health-requests")
					deployLiveQuietPrompt(t, cmd, master, transcript, prompt)
					if f.text("health-requests") != before {
						t.Fatal("next round started before operator answer")
					}
					previous = len(transcript.String())
					input := string(answer) + "\n"
					if answer == 'c' {
						input += "y\ny\n" // Only continue admits second copy/activation.
					}
					if _, err := master.WriteString(input); err != nil {
						t.Fatal(err)
					}
				}
			}, deployLiveEnv(t, f, "health-choice")...)
			deployLiveResult(t, f, test.code)
			requests := f.text("health-requests")
			if strings.Count(requests, "fail\n") != test.fail || strings.Count(requests, "pass\n") != test.pass || strings.Contains(out, "HEALTH_CHOICE_RAW_") {
				t.Fatal("explicit retries changed check membership/counts or displayed raw data", requests, out)
			}
			later := strings.Contains(f.text("effects"), "profile:second.invalid")
			if later != (test.code == 0) || len(f.logs(t)) != 1 {
				t.Fatal("continue/abort effects or retained failure log changed", f.text("effects"), f.logs(t))
			}
			if test.code == 1 && strings.Contains(f.text("effects"), "second.invalid") {
				t.Fatal("abort admitted a later machine effect", f.text("effects"))
			}
			log, err := os.ReadFile(f.logs(t)[0])
			if err != nil || !strings.Contains(string(log), "HEALTH_CHOICE_RAW_FAILURE") || !strings.Contains(string(log), "HEALTH_CHOICE_RAW_SUCCESS") {
				t.Fatal("health raw failure/success was not retained", err)
			}
		})
	}
}
