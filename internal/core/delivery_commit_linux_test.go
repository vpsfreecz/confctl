//go:build linux

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Only Nix is simulated. Git, its index, hooks and editor are real and operate
// exclusively in a disposable repository. No network or host SSH is reachable.
type deliveryInputFixture struct {
	root, tools, git, bash, next string
	env                          []string
}

func newDeliveryInputFixture(t *testing.T) *deliveryInputFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	bash, err = filepath.Abs(bash)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	tools := filepath.Join(base, "tools")
	for _, p := range []string{root, tools, filepath.Join(base, "home"), filepath.Join(base, "template")} {
		if err = os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	next := strings.Replace(inputsSetLock, "123456789", "abcdef987654321", 1)
	f := &deliveryInputFixture{root: root, tools: tools, git: git, bash: bash, next: next}
	f.env = []string{"PATH=" + tools, "HOME=" + filepath.Join(base, "home"), "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "NO_COLOR=1", "PAGER=", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=" + filepath.Join(base, "template"), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Delivery Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Delivery Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "DELIVERY_NEXT=" + filepath.Join(base, "next.lock"), "DELIVERY_TRACE=" + filepath.Join(base, "trace"), "DELIVERY_EDITOR_MARKER=" + filepath.Join(base, "editor"), "DELIVERY_HOOK_MARKER=" + filepath.Join(base, "hook")}
	configurationFile(t, base, "next.lock", next)
	if err = os.Symlink(git, filepath.Join(tools, "git")); err != nil {
		t.Fatal(err)
	}
	f.script(t, filepath.Join(tools, "nix"), `printf '%s\n' "$*" >> "$DELIVERY_TRACE"
case "$1 $2" in
 'flake update') [[ "$#" == 3 && "$3" == a ]] || exit 98; printf '%s' "$(< "$DELIVERY_NEXT")" > flake.lock ;;
 'flake lock')
  [[ "$3" == --update-input && "$4" == a && "$5" == --override-input && "$6" == a && "$7" == *next* && "${@: -2:1}" == --output-lock-file ]] || exit 98
  printf '%s' "$(< "$DELIVERY_NEXT")" > "${@: -1}" ;;
 'eval --json') case "${@: -1}" in
  '.#confctl.settings') printf '%s' '{"nix":{"maxJobs":"auto","impureEval":false},"list":{"columns":["name"]}}' ;;
  '.#confctl.channels') printf '%s' '{"stable":{"os":"a"}}' ;;
  '.#confctl.machineKeys') printf '%s' '{"node":"node-key"}' ;;
  '.#confctl.inputsInfo.node-key') printf '%s' '{"os":{"input":"a"}}' ;;
  *) exit 98 ;;
 esac ;;
 *) exit 98 ;;
esac
`)
	editor := filepath.Join(tools, "fixture-editor")
	f.script(t, editor, `printf 'editor\n' >> "$DELIVERY_EDITOR_MARKER"
printf '\nEdited by delivery fixture\n' >> "$1"
exit "${DELIVERY_EDITOR_EXIT:-0}"
`)
	f.env = append(f.env, "GIT_EDITOR="+editor)
	f.gitRun(t, "init", "--quiet")
	configurationFile(t, root, "flake.nix", "{}\n")
	configurationFile(t, root, "flake.lock", inputsSetLock)
	configurationFile(t, root, "unrelated", "initial\n")
	f.gitRun(t, "add", "flake.nix", "flake.lock", "unrelated")
	f.gitRun(t, "commit", "--quiet", "-m", "fixture base")
	return f
}
func (f *deliveryInputFixture) script(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!"+f.bash+"\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}
func (f *deliveryInputFixture) gitRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(f.git, args...)
	cmd.Dir = f.root
	cmd.Env = f.env
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, b)
	}
	return string(b)
}
func (f *deliveryInputFixture) command(t *testing.T, argv []string) *exec.Cmd {
	t.Helper()
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIStage1Subprocess$")
	cmd.Dir = f.root
	cmd.Env = append(append([]string{}, f.env...), "CONFCTL_STAGE1_SUBPROCESS=1", "CONFCTL_STAGE1_ARGV="+string(b))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
func deliveryRun(t *testing.T, cmd *exec.Cmd) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return code, out.String(), stderr.String()
}
func deliveryLogs(t *testing.T, root string) []string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestDeliveryInputMutationsUseRealGitAndPreserveIndex(t *testing.T) {
	cases := [][]string{
		{"inputs", "update", "--commit", "--no-changelog", "a"},
		{"inputs", "channel", "update", "--commit", "--no-changelog", "stable", "os"},
		{"inputs", "machine", "update", "--commit", "--no-changelog", "node", "os"},
		{"inputs", "set", "--commit", "--no-changelog", "a", "next"},
		{"inputs", "channel", "set", "--commit", "--no-changelog", "stable", "os", "next"},
		{"inputs", "machine", "set", "--commit", "--no-changelog", "node", "os", "next"},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv[:2], "-")+"-"+strings.Join(argv[len(argv)-2:], "-"), func(t *testing.T) {
			f := newDeliveryInputFixture(t)
			before := f.gitRun(t, "rev-parse", "HEAD")
			configurationFile(t, f.root, "unrelated", "staged unrelated\n")
			f.gitRun(t, "add", "unrelated")
			configurationFile(t, f.root, "flake.nix", "{} # unstaged\n")
			f.script(t, filepath.Join(f.root, ".git/hooks/pre-commit"), `printf 'hook\n' >> "$DELIVERY_HOOK_MARKER"
[[ "$(git show :flake.lock)" == *abcdef987654321* ]] || exit 96
printf 'hook output\n'
`)
			code, out, stderr := deliveryRun(t, f.command(t, argv))
			if code != 0 {
				t.Fatalf("%q: exit=%d out=%s stderr=%s", argv, code, out, stderr)
			}
			if f.gitRun(t, "rev-parse", "HEAD") == before || strings.TrimSpace(f.gitRun(t, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")) != "flake.lock" {
				t.Fatal("optional commit did not isolate flake.lock")
			}
			if strings.TrimSpace(f.gitRun(t, "diff", "--cached", "--name-only")) != "unrelated" || configurationRead(t, f.root, "flake.nix") != "{} # unstaged\n" {
				t.Fatal("optional commit consumed unrelated index/worktree changes")
			}
			var gotLock, wantLock any
			if err := json.Unmarshal([]byte(configurationRead(t, f.root, "flake.lock")), &gotLock); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(f.next), &wantLock); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotLock, wantLock) || !strings.Contains(f.gitRun(t, "show", "HEAD:flake.lock"), "abcdef987654321") {
				t.Fatal("updated lock not committed")
			}
			if !strings.Contains(out+stderr, "hook output") || !strings.Contains(f.gitRun(t, "log", "-1", "--format=%s"), "inputs:") {
				t.Fatal("real hook/message not reached", out, stderr)
			}
			if fixtureFile(t, filepath.Dir(f.root), "hook") != "hook\n" || fixtureFile(t, filepath.Dir(f.root), "editor") != "" {
				t.Fatal("non-TTY defaults ran editor or missed hook")
			}
			if len(deliveryLogs(t, f.root)) != 0 {
				t.Fatal("successful invocation retained log")
			}
		})
	}
}

func TestDeliveryCommitRefusalRetainsLockIndexAndLog(t *testing.T) {
	f := newDeliveryInputFixture(t)
	before := f.gitRun(t, "rev-parse", "HEAD")
	configurationFile(t, f.root, "unrelated", "staged unrelated\n")
	f.gitRun(t, "add", "unrelated")
	f.script(t, filepath.Join(f.root, ".git/hooks/pre-commit"), "printf 'hook refusal\\n' >&2\nexit 9\n")
	code, _, stderr := deliveryRun(t, f.command(t, []string{"inputs", "update", "--commit", "--no-changelog", "a"}))
	if code != 1 || !strings.Contains(stderr, "hook refusal") || !strings.Contains(stderr, "git commit failed") {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if f.gitRun(t, "rev-parse", "HEAD") != before || configurationRead(t, f.root, "flake.lock") != f.next || strings.TrimSpace(f.gitRun(t, "diff", "--cached", "--name-only")) != "unrelated" {
		t.Fatal("failed hook committed/rolled back/consumed index")
	}
	logs := deliveryLogs(t, f.root)
	if len(logs) != 1 || !strings.Contains(configurationRead(t, "/", logs[0]), "nix flake update a") {
		t.Fatal("failed commit lost update log", logs)
	}
}

func TestDeliveryNoCommitAndNoRevisionChangeLeaveGitUntouched(t *testing.T) {
	for _, noChange := range []bool{false, true} {
		t.Run(strconv.FormatBool(noChange), func(t *testing.T) {
			f := newDeliveryInputFixture(t)
			before := f.gitRun(t, "rev-parse", "HEAD")
			argv := []string{"inputs", "update", "--no-changelog", "a"}
			if noChange {
				configurationFile(t, filepath.Dir(f.root), "next.lock", inputsSetLock)
				argv = []string{"inputs", "update", "--commit", "--no-changelog", "a"}
			}
			f.script(t, filepath.Join(f.root, ".git/hooks/pre-commit"), "exit 97\n")
			code, _, stderr := deliveryRun(t, f.command(t, argv))
			if code != 0 || f.gitRun(t, "rev-parse", "HEAD") != before {
				t.Fatalf("unexpected commit: exit=%d stderr=%s", code, stderr)
			}
		})
	}
}

// These three asynchronous tests own a fresh Linux session at Start. Failure
// cleanup uses that session, independently of the production cancellation path.
// Our finite subjects do not create another session; Git may create groups.
type deliveryStartedFixture struct {
	cmd            *exec.Cmd
	session        int
	leaderStart    string
	sessionRefusal error
	done           chan struct{}
	waitErr        error
	closeIO        func() error
	once           sync.Once
	cleanupErr     error
}
type deliveryFixtureProcess struct {
	pid, parent, session int
	start                string
}

func deliveryReadFixtureProcess(pid int) (deliveryFixtureProcess, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return deliveryFixtureProcess{}, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return deliveryFixtureProcess{}, fmt.Errorf("invalid fixture process stat")
	}
	fields := strings.Fields(string(b[end+2:]))
	if len(fields) < 20 {
		return deliveryFixtureProcess{}, fmt.Errorf("short fixture process stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return deliveryFixtureProcess{}, err
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return deliveryFixtureProcess{}, err
	}
	return deliveryFixtureProcess{pid, parent, session, fields[19]}, nil
}
func startDeliveryFixture(t *testing.T, cmd *exec.Cmd) *deliveryStartedFixture {
	t.Helper()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("delivery fixture must start its own session")
	}
	// Bound exec's output-copy join if a broken subject leaves a pipe inherited.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f := &deliveryStartedFixture{cmd: cmd, session: cmd.Process.Pid, done: make(chan struct{})}
	t.Cleanup(func() {
		if err := f.cleanup(); err != nil {
			t.Errorf("fixture failure cleanup: %v", err)
		}
	})
	// Read before starting Wait: even a quick exit still has an unreaped stat.
	leader, identityErr := deliveryReadFixtureProcess(f.session)
	if identityErr == nil && leader.session == f.session {
		f.leaderStart = leader.start
	}
	go func() { f.waitErr = cmd.Wait(); close(f.done) }()
	if identityErr != nil || f.leaderStart == "" {
		t.Fatalf("fixture Start session identity unavailable: %+v: %v", leader, identityErr)
	}
	return f
}
func (f *deliveryStartedFixture) wait(limit time.Duration) (error, bool) {
	select {
	case <-f.done:
		return f.waitErr, true
	case <-time.After(limit):
		return nil, false
	}
}
func (f *deliveryStartedFixture) killMembers(includeLeader bool) error {
	if f.sessionRefusal != nil {
		return f.sessionRefusal
	}
	leader, err := deliveryReadFixtureProcess(f.session)
	if f.leaderStart == "" || (err == nil && (leader.session != f.session || leader.start != f.leaderStart)) {
		f.sessionRefusal = errors.New("refusing fixture session signals: original Start leader identity changed or unavailable")
		return f.sessionRefusal
	}
	if err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// If the leader is gone, its surviving descendants retain the kernel SID.
	// This is limited to these finite fixtures, which never leave that session;
	// a newly observed PID/start pair alone does not establish Start ownership.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	var result error
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		before, err := deliveryReadFixtureProcess(pid)
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		if before.session != f.session || (!includeLeader && pid == f.session) {
			continue
		}
		// Session membership is the Start-owned boundary; recheck this exact PID's
		// start time immediately before the signal. No pathname/pattern kill.
		now, err := deliveryReadFixtureProcess(pid)
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		if now.pid != before.pid || now.session != before.session || now.start != before.start {
			continue
		}
		if err = syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			result = errors.Join(result, err)
		}
	}
	return result
}
func (f *deliveryStartedFixture) cleanup() error {
	f.once.Do(func() {
		// Kill descendants first, giving their still-owned parents a chance to reap.
		// Never ask the production cancellation implementation to rescue its test.
		f.cleanupErr = f.killMembers(false)
		_, _ = f.wait(200 * time.Millisecond)
		f.cleanupErr = errors.Join(f.cleanupErr, f.killMembers(true))
		// Retain direct-child cleanup even if session inspection itself failed.
		if err := f.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			f.cleanupErr = errors.Join(f.cleanupErr, err)
		}
		if f.closeIO != nil {
			f.cleanupErr = errors.Join(f.cleanupErr, f.closeIO())
		}
		if _, joined := f.wait(5 * time.Second); !joined {
			f.cleanupErr = errors.Join(f.cleanupErr, errors.New("fixture Wait did not join after independent session cleanup"))
		}
	})
	return f.cleanupErr
}

func TestDeliveryRealEditorOnDisposableTerminal(t *testing.T) {
	for _, mode := range []string{"default", "no-editor", "refused"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeliveryInputFixture(t)
			before := f.gitRun(t, "rev-parse", "HEAD")
			// Give the unmodified Main its ordinary adjacent installed-helper layout.
			prefix := t.TempDir()
			if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(prefix, "libexec"), 0700); err != nil {
				t.Fatal(err)
			}
			src, err := os.Open(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			binary := filepath.Join(prefix, "bin", "subject")
			dst, err := os.OpenFile(binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
			if err != nil {
				t.Fatal(err)
			}
			_, copyErr := io.Copy(dst, src)
			closeErr := dst.Close()
			if copyErr != nil || closeErr != nil {
				t.Fatal(copyErr, closeErr)
			}
			body, err := os.ReadFile("../../libexec/confctl-commit-stop-relay.bash")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(prefix, "libexec", "confctl-commit-stop-relay.bash"), bytes.Replace(body, []byte("@BASH@"), []byte(f.bash), 1), 0700); err != nil {
				t.Fatal(err)
			}
			argv := []string{"inputs", "update", "--commit", "--no-changelog"}
			if mode == "no-editor" {
				argv = append(argv, "--no-editor")
			}
			argv = append(argv, "a")
			if mode == "refused" {
				f.env = append(f.env, "DELIVERY_EDITOR_EXIT=9")
			}
			cmd := f.command(t, argv)
			cmd.Path = binary
			cmd.Args[0] = binary
			master, slave := openCommitTestPTY(t)
			cmd.Stdin = slave
			cmd.Stdout = slave
			cmd.Stderr = slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			var transcript bytes.Buffer
			drained := make(chan struct{})
			owned := startDeliveryFixture(t, cmd)
			owned.closeIO = func() error {
				_ = slave.Close()
				_ = master.Close()
				select {
				case <-drained:
					return nil
				case <-time.After(5 * time.Second):
					return errors.New("PTY output drain did not join after closing master")
				}
			}
			go func() { _, _ = io.Copy(&transcript, master); close(drained) }()
			_ = slave.Close()
			waitErr, joined := owned.wait(15 * time.Second)
			if !joined {
				t.Fatal("PTY editor fixture did not finish before deadline")
			}
			// Close and join the drain even if a descendant holds the slave.
			if err := owned.closeIO(); err != nil {
				t.Fatal(err)
			}
			owned.closeIO = nil
			code := 0
			if waitErr != nil {
				var exit *exec.ExitError
				if !errors.As(waitErr, &exit) {
					t.Fatal(waitErr)
				}
				code = exit.ExitCode()
			}
			wantCode := 0
			if mode == "refused" {
				wantCode = 1
			}
			if code != wantCode {
				t.Fatalf("terminal commit exit=%d want=%d: %s", code, wantCode, transcript.String())
			}
			marker := fixtureFile(t, filepath.Dir(f.root), "editor")
			if (marker != "") == (mode == "no-editor") {
				t.Fatalf("editor selection %s: %q", mode, marker)
			}
			if mode == "refused" {
				if f.gitRun(t, "rev-parse", "HEAD") != before || configurationRead(t, f.root, "flake.lock") != f.next || len(deliveryLogs(t, f.root)) != 1 {
					t.Fatal("editor refusal lost effects/log")
				}
			} else if f.gitRun(t, "rev-parse", "HEAD") == before {
				t.Fatal("editor commit absent")
			}
			if mode == "default" && !strings.Contains(f.gitRun(t, "log", "-1", "--format=%B"), "Edited by delivery fixture") {
				t.Fatal("editor message edit not committed")
			}
		})
	}
}

func TestDeliveryCancellationJoinsOrdinaryOwnedProcessTree(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeliveryProcessCancellationSubject$")
	cmd.Env = append(os.Environ(), "CONFCTL_DELIVERY_PROCESS_ROOT="+root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	owned := startDeliveryFixture(t, cmd)
	err, joined := owned.wait(20 * time.Second)
	if !joined {
		t.Fatal("production Process cancellation did not finish before deadline")
	}
	if err != nil {
		t.Fatalf("Process cancellation subject: %v\n%s", err, output.String())
	}
}

func TestDeliveryProcessCancellationSubject(t *testing.T) {
	root := os.Getenv("CONFCTL_DELIVERY_PROCESS_ROOT")
	if root == "" {
		return
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	script := `root=$1
exec 3<&0
trap 'wait "$worker"; printf cleaned > "$root/cleaned"; exit 0' TERM
"$BASH" --noprofile --norc -c 'trap "exit 0" TERM; printf "%s" "$$" > "$1"; IFS= read -r line' child "$root/child.pid" <&3 &
worker=$!
printf '%s' "$$" > "$root/parent.pid"
wait "$worker"
`
	ctx, cancel := context.WithCancel(context.Background())
	triggered := make(chan bool, 1)
	go func() {
		ready := false
		defer func() { cancel(); triggered <- ready }()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				return
			case <-tick.C:
				p, pe := os.ReadFile(filepath.Join(root, "parent.pid"))
				c, ce := os.ReadFile(filepath.Join(root, "child.pid"))
				parentPID, parentErr := strconv.Atoi(string(p))
				childPID, childErr := strconv.Atoi(string(c))
				if pe == nil && ce == nil && parentErr == nil && childErr == nil && parentPID > 0 && childPID > 0 {
					ready = true
					return
				}
			}
		}
	}()
	_, err = Process(ctx, root, []string{bash, "--noprofile", "--norc", "-c", script, "fixture", root}, nil)
	cancel()
	ready := <-triggered // Join the trigger on success and early Process failure.
	if !ready {
		t.Fatal("owned child did not start")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became handled success: %v", err)
	}
	if fixtureFile(t, root, "cleaned") != "cleaned" {
		t.Fatal("ordinary TERM trap did not complete")
	}
	for _, name := range []string{"parent.pid", "child.pid"} {
		pid, e := strconv.Atoi(fixtureFile(t, root, name))
		if e != nil {
			t.Fatal(e)
		}
		if e = syscall.Kill(pid, 0); e != syscall.ESRCH {
			t.Fatalf("owned PID %d remains: %v", pid, e)
		}
	}
}

// This subprocess gives Main a cancellable context without borrowing the test
// runner's terminal or changing its process-wide signal handlers.
func TestDeliveryCancelableMain(t *testing.T) {
	if os.Getenv("CONFCTL_DELIVERY_CANCEL_MAIN") != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("CONFCTL_STAGE1_ARGV")), &argv); err != nil {
		t.Fatal(err)
	}
	os.Exit(Main(ctx, argv))
}

func TestDeliveryCancellationDuringRealCommitHookRetainsEffects(t *testing.T) {
	f := newDeliveryInputFixture(t)
	before := f.gitRun(t, "rev-parse", "HEAD")
	configurationFile(t, f.root, "unrelated", "staged unrelated\n")
	f.gitRun(t, "add", "unrelated")
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(sleep, filepath.Join(f.tools, "sleep")); err != nil {
		t.Fatal(err)
	}
	f.script(t, filepath.Join(f.root, ".git/hooks/pre-commit"), `trap 'wait "$worker"; exit 1' TERM
sleep 30 &
worker=$!
printf '%s' "$$" > "$DELIVERY_HOOK_MARKER"
wait "$worker"
`)
	cmd := f.command(t, []string{"inputs", "update", "--commit", "--no-changelog", "a"})
	cmd.Args[1] = "-test.run=^TestDeliveryCancelableMain$"
	cmd.Env = append(cmd.Env, "CONFCTL_DELIVERY_CANCEL_MAIN=1")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	owned := startDeliveryFixture(t, cmd)
	deadline := time.Now().Add(10 * time.Second)
	marker := filepath.Join(filepath.Dir(f.root), "hook")
	for {
		if b, e := os.ReadFile(marker); e == nil && len(b) > 0 {
			break
		}
		select {
		case <-owned.done:
			t.Fatalf("CLI exited before hook: %v; %s", owned.waitErr, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("real hook did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var joined bool
	err, joined = owned.wait(10 * time.Second)
	if !joined {
		t.Fatal("cancelled input commit did not join")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("cancellation status: %v stdout=%s stderr=%s", err, out.String(), stderr.String())
	}
	if f.gitRun(t, "rev-parse", "HEAD") != before || configurationRead(t, f.root, "flake.lock") != f.next || strings.TrimSpace(f.gitRun(t, "diff", "--cached", "--name-only")) != "unrelated" {
		t.Fatal("cancelled hook changed HEAD/index or lost completed lock write")
	}
	if len(deliveryLogs(t, f.root)) != 1 {
		t.Fatal("cancelled commit lost diagnostic log")
	}
	if _, err = os.Stat(filepath.Join(f.root, ".git/index.lock")); !os.IsNotExist(err) {
		t.Fatalf("Git index lock left behind: %v", err)
	}
}

// Exercise the fallback itself: a separate-group child ignores TERM and holds
// inherited output open. The subject waits/reaps it but never self-cancels.
func TestDeliveryFixtureCleanupSubject(t *testing.T) {
	mode := os.Getenv("CONFCTL_DELIVERY_CLEANUP_SUBJECT")
	if mode == "" {
		return
	}
	if mode == "child" {
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeliveryFixtureCleanupSubject$")
	cmd.Env = append(os.Environ(), "CONFCTL_DELIVERY_CLEANUP_SUBJECT=child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("CONFCTL_DELIVERY_CLEANUP_ROOT")
	if err := os.WriteFile(filepath.Join(root, "child.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	_ = cmd.Wait() // Reap even when independent parent cleanup kills this child.
	for {          // Also require the independent owner to kill this leader.
		time.Sleep(time.Hour)
	}
}

func TestDeliveryFixtureFailureCleanupKillsSessionAndJoins(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeliveryFixtureCleanupSubject$")
	cmd.Env = append(os.Environ(), "CONFCTL_DELIVERY_CLEANUP_SUBJECT=parent", "CONFCTL_DELIVERY_CLEANUP_ROOT="+root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	owned := startDeliveryFixture(t, cmd)
	peer := exec.Command(os.Args[0], "-test.run=^TestDeliveryFixtureCleanupSubject$")
	peer.Env = append(os.Environ(), "CONFCTL_DELIVERY_CLEANUP_SUBJECT=child")
	peer.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = startDeliveryFixture(t, peer)
	deadline := time.Now().Add(5 * time.Second)
	childPID := 0
	for childPID == 0 {
		b, _ := os.ReadFile(filepath.Join(root, "child.pid"))
		pid, err := strconv.Atoi(string(b))
		if err == nil && pid > 0 {
			childPID = pid
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup fault child did not start")
		}
		time.Sleep(time.Millisecond)
	}
	child, err := deliveryReadFixtureProcess(childPID)
	if err != nil || child.session != owned.session || child.parent != cmd.Process.Pid {
		t.Fatal("fault child outside owned session", child, err)
	}
	group, err := syscall.Getpgid(childPID)
	if err != nil || group != childPID || group == owned.session {
		t.Fatal("fault fixture must use a second process group", group, err)
	}
	// A second blocked drain models the PTY read on the failure path.
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	owned.closeIO = func() error {
		_ = read.Close()
		_ = write.Close()
		select {
		case <-drained:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("fault drain not joined")
		}
	}
	go func() { _, _ = io.Copy(io.Discard, read); close(drained) }()
	if _, finished := owned.wait(20 * time.Millisecond); finished {
		t.Fatalf("fault subject unexpectedly completed: %s", output.String())
	}
	if err = owned.cleanup(); err != nil {
		t.Fatal(err)
	}
	waitErr, joined := owned.wait(time.Second)
	if !joined {
		t.Fatal("fallback left Wait goroutine")
	}
	var killed *exec.ExitError
	if !errors.As(waitErr, &killed) || killed.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("fault leader did not require independent kill: %v", waitErr)
	}
	select {
	case <-drained:
	default:
		t.Fatal("fallback left output drain")
	}
	if err = syscall.Kill(childPID, 0); err != syscall.ESRCH {
		t.Fatalf("fault child was not reaped: %v", err)
	}
	if err = syscall.Kill(cmd.Process.Pid, 0); err != syscall.ESRCH {
		t.Fatalf("fixture leader was not reaped: %v", err)
	}
	if err = peer.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("cleanup touched peer in another owned session: %v", err)
	}
}
