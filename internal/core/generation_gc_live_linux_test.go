//go:build linux

package core

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestGenerationGCLiveAdmissionUsesBothOriginalStreams(t *testing.T) {
	_, terminal := openCommitTestPTY(t)
	for _, mode := range []string{"disabled", "stdout-pipe", "stderr-pipe"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CONFCTL_TTY", "1")
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			var stdout, stderr io.Writer = terminal, terminal
			switch mode {
			case "disabled":
				t.Setenv("CONFCTL_TTY", "0")
			case "stdout-pipe":
				stdout = write
			case "stderr-pipe":
				stderr = write
			}
			e := gcAdapterEngine(t)
			view, tty, err := e.generationGCAdapter(stdout, stderr, nil).Open(e.Context, gcViewSpec{Header: "GC admission"})
			if err != nil || tty || view == nil {
				t.Fatal(view, tty, err)
			}
			if _, text := view.(*generationGCTextView); !text {
				t.Fatal("redirected output acquired a live view")
			}
			if mode == "stdout-pipe" {
				if err := write.Close(); err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(read)
				if err != nil || string(data) != "GC admission\n" {
					t.Fatal(string(data), err)
				}
			}
		})
	}
}

// These cases reach Main, the real GC dependency factory and its live TTY
// admission. Only remote Nix/SSH effects use the existing confined fixture.
// Execute this PTY tier after committed review, under the root's watcher.
func TestGenerationGCLiveProductionTTY(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		fail  bool
		color bool
	}{
		{"direct", []string{"collect-garbage", "-y"}, false, false},
		{"remove", []string{"generation", "rm", "-ry"}, false, false},
		{"rotate", []string{"generation", "rotate", "-ry"}, false, false},
		{"nonzero", []string{"collect-garbage", "-y"}, true, false},
		{"always", []string{"--color=always", "collect-garbage", "-y"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := retentionDelivery(t, "node")
			fifo := filepath.Join(f.root, "gc-release")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			// Owning both ends lets the parent release without a blocking open,
			// while the fixture child still blocks until this explicit write.
			release, err := os.OpenFile(fifo, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer release.Close()
			path := filepath.Join(f.tools, "ssh")
			script, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original := "  printf '1 store paths deleted\\n'"
			blocked := `  printf "deleting '/nix/store/gc-live-held-path'\n"
  printf '\033[35mgc-live-child-held\033[0m\n'
  IFS= read -r permission < "$HOME/gc-release"
  [[ "$permission" == release ]] || exit 98
  printf '1 store paths deleted\n'`
			if strings.Count(string(script), original) != 1 {
				t.Fatal("retention fixture GC seam changed")
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(script), original, blocked, 1)), 0700); err != nil {
				t.Fatal(err)
			}
			env := []string{"CONFCTL_TTY=1", "NO_COLOR=1"}
			if tc.fail {
				env = append(env, "FAIL_GC_HOST=node.invalid")
			}
			text := f.terminal(t, tc.args, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshPTYSize(t, master, 24, 100)
				if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
					t.Fatal(err)
				}
				sshAwaitTerminal(t, transcript, "gc-live-child-held")
				sshAwaitTerminal(t, transcript, "Deleted paths")
				sshAwaitTerminal(t, transcript, "gc-live-held-path")
				sshAwaitTerminal(t, transcript, "1/?")
				raw := transcript.String()
				plain := ansi.Strip(raw)
				if !regexp.MustCompile(`Deleted paths[^\r\n]*1/\?`).MatchString(plain) {
					t.Fatal("path count missing before release", raw)
				}
				if strings.Contains(raw, "\x1b[35m") {
					t.Fatal("child ANSI reached the live display", raw)
				}
				if tc.color && !strings.Contains(raw, "\x1b[33mnode\x1b[0m") {
					t.Fatal("always did not style the target despite NO_COLOR", raw)
				}
				if err := syscall.Kill(cmd.Process.Pid, 0); err != nil || strings.Contains(f.text("trace"), "gc-finished:") {
					t.Fatal("screen was observed only after GC completed", err, f.text("trace"))
				}
				t.Logf("GC held-child %s raw: %q\nGC held-child %s plain:\n%s", tc.name, raw, tc.name, plain)
				// Resize the active production view while its child stays blocked.
				sshPTYSize(t, master, 12, 72)
				if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
					t.Fatal(err)
				}
				if _, err := release.WriteString("release\n"); err != nil {
					t.Fatal(err)
				}
			}, env...)
			plain := ansi.Strip(text)
			var result struct{ Code int }
			if err := json.Unmarshal([]byte(f.text("result.json")), &result); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.fail {
				want = 1
			}
			if result.Code != want || !strings.Contains(plain, "Collecting garbage: node") || !strings.Contains(plain, "Deleted paths: 1") || strings.Count(f.text("trace"), "gc-finished:node.invalid") != 1 {
				t.Fatal(result, text, f.text("trace"))
			}
			if !strings.Contains(plain, "Command: confctl") || !strings.Contains(plain, "Full log: ") {
				t.Fatal("compact GC header lost invocation or log context", plain)
			}
			if !tc.color && regexp.MustCompile("\x1b\\[[0-9;]*m").MatchString(text) {
				t.Fatal("NO_COLOR presentation contains styling", text)
			}
			if tc.fail {
				if !strings.Contains(text, "0 succeeded, 1 failed") || len(f.logs(t)) != 1 {
					t.Fatal("ordinary nonzero lost final failure/log", text, f.logs(t))
				}
				log, err := os.ReadFile(f.logs(t)[0])
				if err != nil || strings.Count(string(log), "gc-live-held-path") != 1 {
					t.Fatal("semantic view changed or duplicated raw log", string(log), err)
				}
			}
			if tc.name == "remove" || tc.name == "rotate" {
				trace := f.text("trace")
				if !strings.Contains(trace, "--delete-generations") || strings.Index(trace, "--delete-generations") > strings.Index(trace, "nix-collect-garbage") {
					t.Fatal("presentation changed removal-before-GC ordering", trace)
				}
			}
		})
	}
}
