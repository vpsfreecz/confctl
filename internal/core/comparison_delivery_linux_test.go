package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/vpsfreecz/confctl/internal/cli"

	"golang.org/x/sys/unix"
)

// Only the disposable helper binds a shell explicitly. Production receives the
// same value through package ldflags, without an environment override.
func TestComparisonDeliveryHelper(t *testing.T) {
	if os.Getenv("CONFCTL_COMPARISON_DELIVERY") != "1" {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("COMPARISON_ARGS")), &argv); err != nil {
		panic(err)
	}
	comparisonRuntimeShell = os.Getenv("COMPARISON_SHELL")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var before, after unix.Termios
	terminal := sshTermios(os.Stdin, syscall.TCGETS, &before) == nil
	code := Main(ctx, argv)
	if terminal {
		if err := sshTermios(os.Stdin, syscall.TCGETS, &after); err != nil {
			panic(err)
		}
	}
	sshWriteTestJSON(filepath.Join(os.Getenv("HOME"), "result.json"), struct {
		Code     int
		Restored bool
	}{code, !terminal || before == after})
	os.Exit(code)
}

type comparisonDeliveryFixture struct{ root, tools, shell, runtimeShell string }

func comparisonDelivery(t *testing.T, machines ...Machine) comparisonDeliveryFixture {
	t.Helper()
	f := comparisonDeliveryFixture{root: t.TempDir()}
	f.tools = filepath.Join(f.root, "tools")
	var err error
	f.shell, err = exec.LookPath("bash")
	if err != nil {
		t.Fatal("declared test shell required", err)
	}
	f.shell, err = filepath.Abs(f.shell)
	if err != nil {
		t.Fatal(err)
	}
	// Use sh mode for runtime command strings: an operator's interactive Bash
	// startup configuration must not replace the fixture's private tool PATH.
	f.runtimeShell, err = exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	f.runtimeShell, err = filepath.Abs(f.runtimeShell)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.tools, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("flake.nix", "{ outputs = _: {}; }\n")
	var inventory strings.Builder
	inventory.WriteByte('{')
	keys := map[string]string{}
	for i, m := range machines {
		if i != 0 {
			inventory.WriteByte(',')
		}
		name, _ := json.Marshal(m.Name)
		value, _ := json.Marshal(map[string]any{"name": m.Name, "key": m.Key, "carrier": m.CarrierName, "metaConfig": m.Attributes})
		fmt.Fprintf(&inventory, "%s:%s", name, value)
		keys[m.Name] = m.Key
	}
	inventory.WriteByte('}')
	write("machines.json", inventory.String())
	b, _ := json.Marshal(keys)
	write("keys.json", string(b))
	write("target.json", `{"os":{"rev":"target123456","url":"literal repo;$(data)"},"same":"same","unknown":{}}`)
	write("deployed.json", `{"os":{"rev":"deploy123456"},"same":"same","unknown":{}}`)
	f.tool(t, "nix", `key="${@: -1}"
printf 'nix:%s\n' "$key" >> "$TRACE"
case "$key" in
  .#confctl.settings) printf '%s\n' '{"list":{"columns":["name","managed"]},"nix":{"maxJobs":"7","impureEval":true}}' ;;
  .#confctl.machinesJson) printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$PWD" ;;
  .#confctl.machineKeys) while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < keys.json ;;
  .#confctl.inputsInfo.*)
    [[ ${FAIL_METADATA:-0} == 1 ]] && exit 23
    while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < target.json ;;
  *) printf 'unexpected evaluation: %s\n' "$*" >&2; exit 98 ;;
esac
`)
	f.tool(t, "ssh", `request="${@: -1}"
printf 'ssh:%s\n' "$request" >> "$TRACE"
case "$request" in
  'cat /proc/uptime') [[ ${OFFLINE:-0} == 1 ]] && exit 255; printf '10 0\n' ;;
  'cat /etc/confctl/inputs-info.json')
    while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line"; done < deployed.json ;;
  *) printf 'unexpected remote request: %s\n' "$request" >&2; exit 98 ;;
esac
`)
	return f
}

func (f comparisonDeliveryFixture) tool(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.tools, name), []byte("#!"+f.shell+"\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}

func (f comparisonDeliveryFixture) command(t *testing.T, args []string, env ...string) *exec.Cmd {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestComparisonDeliveryHelper$")
	cmd.Dir = f.root
	cmd.Env = append([]string{
		"PATH=" + f.tools, "HOME=" + f.root, "XDG_CACHE_HOME=" + f.root + "/cache",
		"LANG=C.UTF-8", "PAGER=", "CONFCTL_COMPARISON_DELIVERY=1",
		"COMPARISON_ARGS=" + string(b), "COMPARISON_SHELL=" + f.runtimeShell,
		"TRACE=" + f.root + "/trace", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}, env...)
	return cmd
}

func (f comparisonDeliveryFixture) text(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.root, name))
	return string(b)
}

func comparisonDeliveryExit(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return exit.ExitCode()
}

func (f comparisonDeliveryFixture) logs(t *testing.T) []string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(f.root, ".confctl/logs/*.log"))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestComparisonDeliveryCLI(t *testing.T) {
	for _, command := range []string{"diff", "changelog"} {
		for _, tc := range []struct {
			args  []string
			input string
			code  int
			want  string
		}{
			{[]string{"--help"}, "", 0, "SYNOPSIS"},
			{[]string{"--unknown"}, "", 64, "SYNOPSIS"},
			{[]string{"-a", "invalid"}, "", 64, "SYNOPSIS"},
			{nil, "n\n", 1, "Continue?"},
			{nil, "", 1, "Continue?"},
			{[]string{"-y", "-g", ""}, "", 1, ""},
		} {
			t.Run(command+"/"+strings.Join(tc.args, "/")+"/"+tc.input, func(t *testing.T) {
				f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
				cmd := f.command(t, append([]string{command}, tc.args...))
				cmd.Stdin = strings.NewReader(tc.input)
				var out, stderr strings.Builder
				cmd.Stdout, cmd.Stderr = &out, &stderr
				if code := comparisonDeliveryExit(t, cmd.Run()); code != tc.code || !strings.Contains(out.String(), tc.want) {
					t.Fatal(code, out.String(), stderr.String())
				}
				trace := f.text("trace")
				if strings.Contains(trace, "ssh:") || strings.Contains(trace, "inputsInfo") {
					t.Fatal("refusal/help caused a query", trace)
				}
				if len(tc.args) > 0 && (tc.args[0] == "--help" || tc.args[0] == "--unknown") {
					if trace != "" || len(f.logs(t)) != 0 || strings.Contains(out.String(), "Unavailable") {
						t.Fatal(trace, out.String(), f.logs(t))
					}
				} else if len(f.logs(t)) != 1 {
					t.Fatal("operational failure should retain log", f.logs(t))
				}
			})
		}
	}
}

// A controlling terminal belongs to a newly launched disposable helper/session.
// Existing PTY/capture helpers are reused; the test launcher never runs Main.
func (f comparisonDeliveryFixture) terminal(t *testing.T, args []string, interact func(*exec.Cmd, *os.File, *sshTerminalTranscript), env ...string) string {
	t.Helper()
	master, slave := openCommitTestPTY(t)
	size := [4]uint16{8, 120, 0, 0}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		t.Fatal(errno)
	}
	transcript, readDone := sshReadPTY(master)
	cmd := f.command(t, args, env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	joined := false
	defer func() {
		// This process group was created by this fixture as a fresh session.
		// Descendant cleanup is independent of whether its leader was joined.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if !joined {
			<-done
		}
	}()
	if interact != nil {
		interact(cmd, master, transcript)
	}
	select {
	case err := <-done:
		joined = true
		if err != nil {
			var result struct{ Code int }
			if json.Unmarshal([]byte(f.text("result.json")), &result) != nil || result.Code != 1 {
				t.Fatal(err, transcript.String(), f.text("result.json"))
			}
		}
	case <-time.After(8 * time.Second):
		t.Fatal("comparison helper did not finish", transcript.String())
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("terminal output did not close", transcript.String())
	}
	var result struct {
		Code     int
		Restored bool
	}
	if err := json.Unmarshal([]byte(f.text("result.json")), &result); err != nil || !result.Restored {
		t.Fatal("terminal was not restored", result, err)
	}
	return transcript.String()
}

func TestComparisonDeliveryCurrentAndRedirected(t *testing.T) {
	for _, mode := range []string{"changed", "same", "unknown", "offline", "filtered", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := comparisonDelivery(t, connectionMachine("node", "node", "literal;$(host).invalid", true), connectionMachine("unmanaged", "unmanaged", "unused.invalid", false))
			args := []string{"diff", "-y", "-j", "auto", "--cores", "3", "node"}
			env := []string{"CONFCTL_TTY=0"}
			want := "deploy12 -> target12"
			switch mode {
			case "same":
				args = append(args, "same")
				want = "no changes"
			case "unknown":
				env = append(env, "FAIL_METADATA=1")
				want = "unknown state"
			case "offline":
				env = append(env, "OFFLINE=1")
				want = "node is offline"
			case "filtered":
				args = append(args, "absent-role")
				want = ""
			case "empty":
				args[len(args)-1] = "no-host"
				want = ""
			}
			out := f.terminal(t, args, nil, env...)
			if !strings.Contains(out, want) || strings.Contains(out, "Continue?") || strings.Contains(out, "Log file:") || len(f.logs(t)) != 0 {
				t.Fatal(out, f.logs(t))
			}
			trace := f.text("trace")
			if strings.Contains(trace, "realpath") || strings.Contains(trace, "generation") || strings.Contains(trace, "git:") || strings.Contains(trace, "unmanaged") {
				t.Fatal(trace)
			}
		})
	}
	f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
	cmd := f.command(t, []string{"diff", "-y"})
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil || out.Len() != 0 || stderr.Len() != 0 || !strings.Contains(f.text("trace"), "ssh:cat /etc/confctl/inputs-info.json") {
		t.Fatal("redirected Null output/effects", err, out.String(), stderr.String(), f.text("trace"))
	}
}

func TestComparisonDeliverySelectedAndConfirmation(t *testing.T) {
	for _, selector := range []string{"named", "0", "current"} {
		t.Run(selector, func(t *testing.T) {
			f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true), connectionMachine("missing", "missing", "missing.invalid", true))
			seedCoreGeneration(t, f.root, "node", "named", coreGenerationRecord(f.root+"/system", "2000-01-01T00:00:00Z", `{"os":"local123456"}`))
			if err := os.Symlink("named", filepath.Join(f.root, ".confctl/generations/node/current")); err != nil {
				t.Fatal(err)
			}
			out := f.terminal(t, []string{"diff", "-g", selector}, func(_ *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshAwaitTerminal(t, transcript, "Continue?")
				_, _ = master.WriteString("y\n")
				sshAwaitTerminal(t, transcript, "These hosts will be ignored.")
				_, _ = master.WriteString("y\n")
			}, "CONFCTL_TTY=0")
			trace := f.text("trace")
			if !strings.Contains(out, "Resolved host generations:") || !strings.Contains(out, "deploy12 -> local123") || strings.Contains(out, "Log file:") || strings.Count(trace, "ssh:cat /proc/uptime") != 1 || strings.Contains(trace, "inputsInfo") || strings.Contains(trace, "machineKeys") {
				t.Fatal(out, trace)
			}
		})
	}
	for _, answer := range []string{"n\n", ""} {
		f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true), connectionMachine("missing", "missing", "missing.invalid", true))
		seedCoreGeneration(t, f.root, "node", "named", coreGenerationRecord(f.root+"/system", "2000-01-01T00:00:00Z", `{"os":"local123456"}`))
		cmd := f.command(t, []string{"diff", "-g", "named"})
		cmd.Stdin = strings.NewReader("y\n" + answer)
		var out, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if code := comparisonDeliveryExit(t, cmd.Run()); code != 1 || !strings.Contains(out.String(), "These hosts will be ignored.") || strings.Contains(f.text("trace"), "ssh:") || len(f.logs(t)) != 1 {
			t.Fatal(code, out.String(), stderr.String(), f.text("trace"))
		}
	}
}

func TestComparisonDeliveryGitPagerConfiguration(t *testing.T) {
	f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
	// Class selection and construction each query configuration. Both read the
	// borrowed original stdin, and completed Git status9 remains nonfatal.
	f.tool(t, "git", `[[ "$*" == 'config --get-all core.pager' ]] || exit 98
printf 'CONFIG INPUT\n' >&2
IFS= read -r token || exit 99
printf 'config:%s\n' "$token" >> "$TRACE"
printf 'pager\n'
exit 9
`)
	f.tool(t, "pager", `while IFS= read -r line; do printf '%s\n' "$line"; done`)
	out := f.terminal(t, []string{"diff", "-y", "node", "os"}, func(_ *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
		sshAwaitTerminal(t, transcript, "CONFIG INPUT")
		_, _ = master.WriteString("first token\nsecond token\n")
	}, "PAGER=absent-pager")
	trace := f.text("trace")
	if !strings.Contains(out, "deploy12 -> target12") || !strings.Contains(trace, "config:first token\nconfig:second token\nssh:") || len(f.logs(t)) != 0 {
		t.Fatal(out, trace)
	}
}

func TestComparisonDeliverySystemPager(t *testing.T) {
	for _, mode := range []string{"nonzero", "fallback", "unbound", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
			f.tool(t, "pager", `printf 'pager:%s\n' "$*" >> "$TRACE"
if ! IFS= read -r line; then
  [[ ${PAGER_DIAGNOSTIC:-0} == 1 ]] && printf 'probe diagnostic\n' >&2
  exit 0
fi
printf '%s\n' "$line"
while IFS= read -r line; do printf '%s\n' "$line"; done
exit 9
`)
			env := []string{"PAGER=pager 'literal arg;$(data)'"}
			args := []string{"diff", "-y", "node", "os"}
			switch mode {
			case "fallback":
				env = append(env, "PAGER_DIAGNOSTIC=1")
			case "unbound":
				env = append(env, "COMPARISON_SHELL=")
			case "empty":
				args[len(args)-1] = "absent-role"
			}
			out := f.terminal(t, args, nil, env...)
			trace := f.text("trace")
			switch mode {
			case "unbound":
				if !strings.Contains(out, "runtime shell is not bound") || strings.Contains(trace, "pager:") || len(f.logs(t)) != 1 {
					t.Fatal(out, trace)
				}
			case "empty":
				if strings.Contains(trace, "pager:") || strings.Contains(out, "Log file:") {
					t.Fatal(out, trace)
				}
			default:
				if !strings.Contains(out, "deploy12 -> target12") || strings.Contains(out, "Log file:") || strings.Count(trace, "pager:") != 2 || len(f.logs(t)) != 0 {
					t.Fatal(out, trace)
				}
				if mode == "fallback" && !strings.HasSuffix(trace, "pager:\n") || mode == "nonzero" && strings.Count(trace, "pager:literal arg;$(data)") != 2 {
					t.Fatal("probe/live literal arguments or fallback changed", trace)
				}
			}
		})
	}
}

func TestComparisonDeliveryBasicAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
			roles := map[string]string{}
			for i := 0; i < 20; i++ {
				roles[fmt.Sprintf("r%02d", i)] = "target"
			}
			b, _ := json.Marshal(roles)
			if err := os.WriteFile(filepath.Join(f.root, "target.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			out := f.terminal(t, []string{"diff"}, func(cmd *exec.Cmd, master *os.File, transcript *sshTerminalTranscript) {
				sshAwaitTerminal(t, transcript, "Continue?")
				if cancel {
					_, _ = master.WriteString("y\n")
				} else {
					// One write also covers confirmation read-ahead; the queued Q
					// remains available to the actual Basic input owner.
					_, _ = master.WriteString("y\nQ")
				}
				sshAwaitTerminal(t, transcript, "Press enter/return")
				if cancel {
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				}
			}, "PAGER=missing-pager")
			if !strings.Contains(out, "Press enter/return") || cancel && !strings.Contains(out, "context canceled") || !cancel && strings.Contains(out, "r19") {
				t.Fatal(out)
			}
			if cancel != (len(f.logs(t)) == 1) {
				t.Fatal("unexpected log retention", cancel, f.logs(t))
			}
		})
	}
}

func TestComparisonDeliverySystemEarlyCloseAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			f := comparisonDelivery(t, connectionMachine("first", "first", "first.invalid", true), connectionMachine("later", "later", "later.invalid", true))
			roles := map[string]string{}
			for i := 0; i < 100; i++ {
				roles[fmt.Sprintf("r%03d", i)+strings.Repeat("x", 2048)] = "target"
			}
			b, _ := json.Marshal(roles)
			if err := os.WriteFile(filepath.Join(f.root, "target.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			f.tool(t, "pager", `if ! IFS= read -r line; then exit 0; fi
printf '%s\n' "$$" > "$HOME/pager.pid"
printf 'PAGER READY\n'
if [[ ${WAIT_FOR_CANCEL:-0} == 1 ]]; then
  deadline=$((SECONDS + 15))
  while ((SECONDS < deadline)); do :; done
  exit 99
fi
exit 0
`)
			var interact func(*exec.Cmd, *os.File, *sshTerminalTranscript)
			env := []string{"PAGER=pager"}
			if cancel {
				env = append(env, "WAIT_FOR_CANCEL=1")
				interact = func(cmd *exec.Cmd, _ *os.File, transcript *sshTerminalTranscript) {
					sshAwaitTerminal(t, transcript, "PAGER READY")
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				}
			}
			out := f.terminal(t, []string{"diff", "-y"}, interact, env...)
			if !strings.Contains(out, "PAGER READY") || strings.Count(f.text("trace"), "ssh:cat /proc/uptime") != 1 {
				t.Fatal(out, f.text("trace"))
			}
			if cancel != (len(f.logs(t)) == 1) || cancel && !strings.Contains(out, "context canceled") {
				t.Fatal(out, f.logs(t))
			}
			pid, err := strconv.Atoi(strings.TrimSpace(f.text("pager.pid")))
			if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("fixture pager was not reaped", pid, err)
			}
		})
	}
}

func TestComparisonDeliveryChangelogFailureAndRedirected(t *testing.T) {
	f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
	f.tool(t, "git", "printf 'git:%s\\n' \"$*\" >> \"$TRACE\"\nprintf 'fixture Git failure\\n' >&2\nexit 7\n")
	out := f.terminal(t, []string{"changelog", "-y", "node", "os"}, nil, "CONFCTL_TTY=0")
	mirrors, err := filepath.Glob(filepath.Join(f.root, "cache/confctl/git-mirrors/*"))
	if err != nil || len(mirrors) != 1 || !strings.Contains(out, "fixture Git failure") || strings.Contains(out, "Log file:") || len(f.logs(t)) != 0 {
		t.Fatal("per-role failure or clone residue changed", out, mirrors, err)
	}
	cmd := f.command(t, []string{"changelog", "-y", "node", "os"})
	var redirected, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &redirected, &stderr
	if err := cmd.Run(); err != nil || redirected.Len() != 0 || stderr.Len() != 0 || !strings.Contains(f.text("trace"), "fetch --no-show-forced-updates --quiet") {
		t.Fatal("redirected changelog must retain query/mirror effects", err, redirected.String(), stderr.String(), f.text("trace"))
	}
}

func comparisonDeliveryGit(t *testing.T, git, repo, home string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", repo}, args...)...)
	cmd.Dir = repo
	// Git setup has the same owned-environment boundary as the Main helper.
	// In particular, inherit no repository, index, object or config overrides.
	cmd.Env = []string{
		"PATH=" + filepath.Dir(git), "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(args, err, string(b))
	}
	return strings.TrimSpace(string(b))
}

func comparisonDeliveryGitRepository(t *testing.T, git, root string) (repo, from, to string) {
	t.Helper()
	repo = filepath.Join(root, "repo with spaces;literal")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string { return comparisonDeliveryGit(t, git, repo, root, args...) }
	run("init", "--quiet")
	file := filepath.Join(repo, "file")
	if err := os.WriteFile(file, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "file")
	run("commit", "--quiet", "-m", "Original fixture")
	from = run("rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("updated payload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("commit", "--quiet", "-am", "Updated fixture")
	to = run("rev-parse", "HEAD")
	return
}

func TestComparisonDeliveryGitSetupIgnoresExternalEnvironment(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("declared Git required", err)
	}
	external := t.TempDir()
	externalRepo, _, _ := comparisonDeliveryGitRepository(t, git, external)
	externalIndex := filepath.Join(external, "external.index")
	b, err := os.ReadFile(filepath.Join(externalRepo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(externalIndex, b, 0600); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(external, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nprintf injected > "+ShellJoin([]string{filepath.Join(external, "hook-ran")})+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// Snapshot this small disposable external repository, alternate index and
	// injected hook, so a leaked write cannot hide behind an unchanged HEAD.
	snapshot := func() string {
		t.Helper()
		var out strings.Builder
		err := filepath.Walk(external, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			var data []byte
			if !info.IsDir() {
				data, err = os.ReadFile(path)
				if err != nil {
					return err
				}
			}
			fmt.Fprintf(&out, "%q %v %x\n", strings.TrimPrefix(path, external), info.Mode(), data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	before := snapshot()
	t.Cleanup(func() {
		if snapshot() != before {
			t.Error("fixture setup changed the external repository/index/config hook")
		}
	})
	for key, value := range map[string]string{
		"HOME": external, "XDG_CONFIG_HOME": external,
		"GIT_INDEX_FILE": externalIndex, "GIT_DIR": filepath.Join(externalRepo, ".git"),
		"GIT_WORK_TREE": externalRepo, "GIT_COMMON_DIR": filepath.Join(externalRepo, ".git"),
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": hooks,
	} {
		t.Setenv(key, value)
	}
	local := t.TempDir()
	repo, from, to := comparisonDeliveryGitRepository(t, git, local)
	run := func(args ...string) string { return comparisonDeliveryGit(t, git, repo, local, args...) }
	if from == to || run("rev-list", "--count", "HEAD") != "2" || run("rev-parse", "--absolute-git-dir") != filepath.Join(repo, ".git") || run("show", from+":file") != "original" || run("show", to+":file") != "updated payload" {
		t.Fatal("both fixture commits must belong to the local repository")
	}
}

func TestComparisonDeliveryChangelogLocalGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("declared Git required", err)
	}
	for _, args := range [][]string{{"changelog", "-y", "node", "os"}, {"changelog", "-y", "-v", "-p", "node", "os"}, {"changelog", "-y", "-d", "node", "os"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			f := comparisonDelivery(t, connectionMachine("node", "node", "fixture.invalid", true))
			repo, from, to := comparisonDeliveryGitRepository(t, git, f.root)
			for name, rev := range map[string]string{"deployed.json": from, "target.json": to} {
				b, _ := json.Marshal(map[string]any{"os": map[string]string{"rev": rev, "url": repo}})
				if err := os.WriteFile(filepath.Join(f.root, name), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Bind the local transport helper explicitly inside this fixture:
			// Git's compiled shell can otherwise load operator PATH startup code.
			uploadPack := filepath.Join(filepath.Dir(git), "git-upload-pack")
			f.tool(t, "git", "printf 'git:%s\\n' \"$*\" >> \"$TRACE\"\n"+
				"if [[ $1 == clone ]]; then exec "+ShellJoin([]string{git})+" \"$@\" --upload-pack="+ShellJoin([]string{uploadPack})+"; fi\n"+
				"exec "+ShellJoin([]string{git, "-c", "remote.origin.uploadpack=" + uploadPack})+" \"$@\"\n")
			out := f.terminal(t, args, nil, "CONFCTL_TTY=0")
			if !strings.Contains(out, "git log for ") || strings.Contains(out, "Log file:") || len(f.logs(t)) != 0 {
				t.Fatal(out)
			}
			trace := f.text("trace")
			if !strings.Contains(trace, "clone --quiet --mirror "+repo) || !strings.Contains(trace, "log --no-decorate --left-right --cherry-mark") {
				t.Fatal(trace)
			}
			if len(args) > 3 && args[2] == "-d" {
				if !strings.Contains(out, to+".."+from) {
					t.Fatal(out)
				}
			} else if !strings.Contains(out, "Updated fixture") {
				t.Fatal("Git payload was lost", out)
			}
			if strings.Contains(strings.Join(args, " "), "-p") && (!strings.Contains(out, "+updated payload") || strings.Contains(trace, "--max-count=50")) {
				t.Fatal(out, trace)
			}
		})
	}
}

func TestComparisonDeliveryPublicHelpAvailable(t *testing.T) {
	for _, name := range []string{"diff", "changelog"} {
		inv, err := cli.BuiltinRegistry().Parse([]string{name, "--help"})
		if err != nil || !inv.Help || inv.Command.Handler != cli.ComparisonHandler {
			t.Fatal(inv, err)
		}
	}
}
