package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// These production-path PTY checks are separate from the quick callback suite.
// Every fake Nix build emits progress/raw output and blocks on an explicit FIFO;
// the parent must observe that output while that very child remains alive.
func TestBuildPresentationProductionPTY(t *testing.T) {
	for _, mode := range []string{"build", "status", "deploy", "build-eval-failed"} {
		t.Run(mode, func(t *testing.T) {
			command := strings.TrimSuffix(mode, "-eval-failed")
			failEval := mode == "build-eval-failed"
			var f buildStatusDeliveryFixture
			var env []string
			if command == "deploy" {
				d := deployPublic(t, "one", "two")
				f = d.buildStatusDeliveryFixture
				env = []string{"CONFCTL_DEPLOY_PUBLIC_HOOK=1", "CONFCTL_EXTENSION_REGISTRY=" + filepath.Join(f.root, "registry.json"), "CONFCTL_EXTENSION_ROOT=" + f.root}
			} else {
				f = buildStatusDelivery(t, "one", "two")
				f.seed(t, "one")
				f.seed(t, "two")
			}
			f.write(t, "plan.json", `{"one":{"key":"one","inputs":{"nixpkgs":"/fixture/input-one"}},"two":{"key":"two","inputs":{"nixpkgs":"/fixture/input-two"}}}`)
			for _, host := range []string{"one", "two"} {
				payload := coreGenerationRecord(f.top, "2026-10-15T10:00:00Z", `{"os":"stored-revision"}`)
				payload = strings.Replace(payload, `"inputs":{}`, `"inputs":{"nixpkgs":"/fixture/input-`+host+`"}`, 1)
				seedCoreGeneration(t, f.root, host, "selected", payload)
			}
			f.tool(t, "nix", `key="${@: -1}"
printf 'nix:%s:%s\n' "$1" "$key" >> "$TRACE"
case "$key" in
 .#confctl.settings) printf '%s\n' '{"list":{"columns":["name","managed"]},"nix":{"maxJobs":"7","impureEval":true,"legacyNixPath":true}}';;
 .#confctl.machinesJson) printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$PWD";;
 .#confctl.machineKeys) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < keys.json;;
 .#confctl.buildPlan) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < plan.json;;
 .#confctl.inputs) printf '%s\n' '{"one":{"nixpkgs":"/fixture/input-one"},"two":{"nixpkgs":"/fixture/input-two"}}';;
 .#confctl.build)
  IFS= read -r count < "$HOME/build-count"
  printf '%s\n' "$$" > "$HOME/eval-pid-$count"
  printf 'held-output-eval-%s\n' "$count" >&2
  IFS= read -r release < "$HOME/eval-release-$count"
  [[ "$release" == proceed ]] || exit 97
  [[ ${FAIL_OUTPUT_EVAL:-0} == 1 ]] && exit 7
  while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < outputs.json;;
 .#confctl.inputsInfo.*) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < target.json;;
 *)
  [[ "$1" == build ]] || exit 98
  count=0
  if [[ -f "$HOME/build-count" ]]; then IFS= read -r count < "$HOME/build-count"; fi
  count=$((count + 1))
  printf '%s\n' "$count" > "$HOME/build-count"
  printf '%s\n' "$$" > "$HOME/build-pid-$count"
  printf "these 2 derivations will be built:\nthese 3 paths will be fetched (1 MiB download):\nbuilding '/fixture/group-%s.drv'\ncopying path '/fixture/fetch-%s'\n" "$count" "$count" >&2
  for ((line=1; line<=18; line++)); do
   printf 'raw-build-%s-line-%02d\n' "$count" "$line" >&2
  done
  printf 'raw-build-%s-before-release\n' "$count" >&2
  IFS= read -r release < "$HOME/release-$count"
  [[ "$release" == proceed ]] || exit 97
  printf 'null\n';;
esac
`)
			gates := make([]*os.File, 2)
			evalGates := make([]*os.File, 2)
			for i := range gates {
				for _, entry := range []struct {
					name  string
					gates []*os.File
				}{{"release", gates}, {"eval-release", evalGates}} {
					path := filepath.Join(f.root, fmt.Sprintf("%s-%d", entry.name, i+1))
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
					gate, err := os.OpenFile(path, os.O_RDWR, 0)
					if err != nil {
						t.Fatal(err)
					}
					entry.gates[i] = gate
					t.Cleanup(func() { _ = gate.Close() })
				}
			}
			color := "never"
			if command == "build" {
				color = "always" // Explicit always overrides fixture NO_COLOR.
			}
			args := []string{"--color=" + color, command, "-y"}
			if command == "deploy" {
				args = append(args, "--copy-only")
			}
			env = append(env, "CONFCTL_TTY=1", "NO_COLOR=1")
			if failEval {
				env = append(env, "FAIL_OUTPUT_EVAL=1")
			}
			text := f.terminal(t, args, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshPTYSize(t, master, 24, 80)
				for i, gate := range gates {
					n := i + 1
					frame := buildAwaitPresentationFrame(t, transcript, fmt.Sprintf("Build group: %d/2 (1 machines)", n), fmt.Sprintf("raw-build-%d-before-release", n))
					lines := strings.Split(ansi.Strip(frame), "\n")
					if len(lines) != 23 || lines[0] != "Command: confctl "+sshShellJoin(args) || !strings.HasPrefix(lines[2], "Full log: ") || !strings.HasPrefix(lines[3], "Live view --") {
						t.Fatal("compact header or 80x24 frame changed", frame)
					}
					bottom := -1
					for j, line := range lines {
						if strings.HasPrefix(line, "<---") {
							bottom = j
						}
						if ansi.StringWidth(line) > 80 || strings.Contains(line, "/fixture/input-") || strings.Contains(line, "Building machines") || strings.Contains(line, "observed") {
							t.Fatal("prelude, old label or overwide line in latest frame", frame)
						}
					}
					if bottom < 16 || len(lines)-bottom-1 > 6 {
						t.Fatal("logs lost twelve slots or bars exceed six slots", frame)
					}
					for _, line := range lines[4:bottom] {
						if !strings.Contains(line, fmt.Sprintf("raw-build-%d-", n)) {
							t.Fatal("log region not filled before release", frame)
						}
					}
					bars := strings.Join(lines[bottom+1:], "\n")
					for _, want := range []string{"Building", "Fetching", "nix build", "2/5"} {
						if !strings.Contains(bars, want) {
							t.Fatal("progress missing below bordered log", want, frame)
						}
					}
					trackStart, trackWidth := -1, -1
					for _, row := range lines[bottom+1:] {
						if !strings.HasPrefix(row, "Building") && !strings.HasPrefix(row, "Fetching") && !strings.HasPrefix(row, "nix build") {
							continue
						}
						left, right := strings.LastIndex(row, "["), strings.LastIndex(row, "]")
						if left < 0 || right-left < 5 || strings.Contains(row[left:right], "running") {
							t.Fatal("running row lacks readable bar track", row)
						}
						start := ansi.StringWidth(row[:left])
						if trackStart >= 0 && (start != trackStart || right-left != trackWidth) {
							t.Fatal("tracks not horizontally aligned", bars)
						}
						trackStart, trackWidth = start, right-left
					}
					if color == "always" {
						if !strings.Contains(frame, "\x1b[1mCommand:") || !strings.Contains(frame, "\x1b[1mLive view ") {
							t.Fatal("confctl labels not styled", frame)
						}
					} else if strings.Contains(frame, "\x1b") {
						t.Fatal("no-color frame contains styles", frame)
					}
					rawLines := strings.Split(frame, "\n")
					if strings.Contains(strings.Join(rawLines[4:bottom], "\n"), "\x1b") {
						t.Fatal("child log styled by confctl", frame)
					}
					if i == 0 && strings.Contains(transcript.String(), "Build group: 2/2") {
						t.Fatal("second group started before first release", transcript.String())
					}
					pid, err := strconv.Atoi(strings.TrimSpace(f.text(fmt.Sprintf("build-pid-%d", n))))
					if err != nil || syscall.Kill(pid, 0) != nil || cmd.Process.Signal(syscall.Signal(0)) != nil {
						t.Fatal("observed transcript after child exit", pid, err)
					}
					if _, err := os.Stat(filepath.Join(f.root, "result.json")); !os.IsNotExist(err) {
						t.Fatal("command completed while build barrier closed", err)
					}
					if _, err := gate.WriteString("proceed\n"); err != nil {
						t.Fatal(err)
					}
					evalFrame := buildAwaitPresentationFrame(t, transcript, fmt.Sprintf("Build group: %d/2 (1 machines)", n), fmt.Sprintf("held-output-eval-%d", n))
					plainEval := ansi.Strip(evalFrame)
					for _, label := range []string{"Building", "Fetching", "nix build", "nix eval"} {
						found := false
						for _, row := range strings.Split(plainEval, "\n") {
							if strings.HasPrefix(row, label) {
								want := "succeeded"
								if label == "nix eval" {
									want = "running"
								}
								if !strings.Contains(row, want) || label == "nix eval" && !strings.Contains(row, ".#confctl.build") {
									t.Fatal("eval child did not preserve completed build rows", plainEval)
								}
								found = true
							}
						}
						if !found {
							t.Fatal("missing row during held eval", label, plainEval)
						}
					}
					evalPID, err := strconv.Atoi(strings.TrimSpace(f.text(fmt.Sprintf("eval-pid-%d", n))))
					if err != nil || syscall.Kill(evalPID, 0) != nil || syscall.Kill(pid, 0) == nil {
						t.Fatal("eval not alive or build not joined at phase transition", pid, evalPID, err)
					}
					if _, err := os.Stat(filepath.Join(f.root, "result.json")); !os.IsNotExist(err) {
						t.Fatal("command finished before held eval release", err)
					}
					if i == 0 && strings.Contains(transcript.String(), "Build group: 2/2") {
						t.Fatal("next group started while output eval was held", transcript.String())
					}
					if _, err := evalGates[i].WriteString("proceed\n"); err != nil {
						t.Fatal(err)
					}
					if failEval {
						break
					}
				}
			}, env...)
			var result struct{ Code int }
			wantCode, groups := 0, 2
			if failEval {
				wantCode, groups = 1, 1
			}
			if err := json.Unmarshal([]byte(f.text("result.json")), &result); err != nil || result.Code != wantCode {
				t.Fatal("production call-through failed", result, err, text, f.text("trace"))
			}
			if f.text("build-count") != fmt.Sprintf("%d\n", groups) || (len(f.logs(t)) != 0) != failEval {
				t.Fatal("group count/log retention changed", f.text("build-count"), text)
			}
			plain := ansi.Strip(text)
			if strings.Count(plain, "Building machines") != groups || strings.Count(plain, "nixpkgs=/fixture/input-one") != 1 || strings.Count(plain, "nixpkgs=/fixture/input-two") != groups-1 {
				t.Fatal("prelude was lost or replayed in paint/teardown", text)
			}
			for _, host := range []string{"one", "two"}[:groups] {
				if strings.Count(plain, "\n  "+host+"\r\n") != 1 {
					t.Fatal("machine list line repeated outside prelude", host, text)
				}
			}
			if command != "deploy" && !failEval {
				for _, want := range []string{"Built generations:", "one: selected", "two: selected"} {
					if !strings.Contains(plain, want) {
						t.Fatal("existing final build summary missing", want, text)
					}
				}
				if color == "always" && (!strings.Contains(text, "\x1b[1mBuilt generations:\x1b[0m") || !strings.Contains(text, "\x1b[36mselected\x1b[0m")) {
					t.Fatal("result heading/generation style missing", text)
				}
			} else if strings.Contains(plain, "Built generations:") {
				t.Fatal("new deployment summary phase introduced", text)
			}
			endMarker := "\x1b[24;1H\x1b[2K"
			last := strings.LastIndex(text, endMarker)
			if last < 0 {
				t.Fatal("missing retained final panel", text)
			}
			suffix := ansi.Strip(text[last+len(endMarker):])
			for _, duplicate := range []string{"Command:", "Build group:", "Building [", "Fetching [", "nix build [", "nix eval ["} {
				if strings.Contains(suffix, duplicate) {
					t.Fatal("selected task/header replayed after final frame", duplicate, suffix)
				}
			}
			if failEval {
				final := ansi.Strip(buildLatestPresentationFrame(text))
				for _, label := range []string{"Building", "Fetching", "nix build", "nix eval"} {
					found := false
					for _, row := range strings.Split(final, "\n") {
						if strings.HasPrefix(row, label) {
							want := "succeeded"
							if label == "nix eval" {
								want = "failed"
							}
							if !strings.Contains(row, want) {
								t.Fatal("late eval failure recolored completed build", row)
							}
							found = true
						}
					}
					if !found {
						t.Fatal("missing final task", label, final)
					}
				}
			}
			firstFrame := strings.Index(text, "\x1b[H")
			prelude := text[:max(firstFrame, 0)]
			if firstFrame < 0 || !strings.Contains(ansi.Strip(prelude), "nixpkgs=/fixture/input-one") || !strings.HasSuffix(prelude, strings.Repeat("\r\n", 24)) {
				t.Fatal("prelude not preserved before scrolling reservation", text)
			}
			if command == "deploy" && !strings.Contains(f.text("effects"), "copied:") || command == "status" && !strings.Contains(f.text("trace"), "realpath /run/current-system") {
				t.Fatal("later production phase missing", f.text("trace"), f.text("effects"), text)
			}
		})
	}
}

// The painter's final cursor/erase suffix proves a frame was received in full.
// Inspect the latest complete frame, never a token in old scrollback.
func buildLatestPresentationFrame(text string) string {
	end := strings.LastIndex(text, "\x1b[24;1H\x1b[2K")
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(text[:end], "\x1b[H")
	if start < 0 {
		return ""
	}
	parts := strings.Split(text[start+3:end], "\x1b[2K\r")
	if len(parts) < 24 {
		return ""
	}
	lines := make([]string, 23)
	for i := range lines {
		lines[i] = strings.TrimRight(parts[i+1], "\r\n")
	}
	return strings.Join(lines, "\n")
}

func TestBuildPresentationFrameExtraction(t *testing.T) {
	paint := func(label string) string {
		var out strings.Builder
		out.WriteString("\x1b[H")
		for i := range 23 {
			fmt.Fprintf(&out, "\x1b[2K\r%s-%d\r\n", label, i)
		}
		out.WriteString("\x1b[24;1H\x1b[2K")
		return out.String()
	}
	prelude := "Building machines\r\n  prelude-host\r\nwith inputs\r\n  input=/prelude/path\r\n"
	old := prelude + paint("old")
	if got := buildLatestPresentationFrame(old + "\x1b[H\x1b[2K\rpartial"); !strings.HasPrefix(got, "old-0\n") || strings.Contains(got, "prelude") || strings.Contains(got, "partial") {
		t.Fatal("incomplete paint or scrollback treated as frame", got)
	}
	if got := buildLatestPresentationFrame(old + paint("latest")); len(strings.Split(got, "\n")) != 23 || !strings.HasSuffix(got, "latest-22") || strings.Contains(got, "old-") {
		t.Fatal("did not select latest complete bounded frame", got)
	}
}

func buildAwaitPresentationFrame(t *testing.T, transcript *sshTerminalTranscript, header, raw string) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		frame := buildLatestPresentationFrame(transcript.String())
		plain := ansi.Strip(frame)
		if strings.Contains(plain, header) && strings.Contains(plain, raw) && strings.Contains(plain, "2/5") {
			return frame
		}
		select {
		case <-deadline.C:
			t.Fatal("missing completed live frame before child release", header, raw, frame, transcript.String())
		case <-tick.C:
		}
	}
}
