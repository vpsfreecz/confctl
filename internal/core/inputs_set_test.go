package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	inputgit "github.com/vpsfreecz/confctl/internal/git"
	"github.com/vpsfreecz/confctl/internal/inputs"
)

const inputsSetLock = `{"nodes":{"root":{"inputs":{"a":"a","b":"b","alias":["a","ignored"]}},"a":{"locked":{"type":"git","url":"https://repo","rev":"123456789"},"original":{"type":"git","url":"https://repo","ref":"main"}},"b":{"locked":{"type":"github","owner":"org","repo":"b","rev":"987654321"},"original":{"type":"github","owner":"org","repo":"b","ref":"stable"}}}}`

func inputsSetRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	configurationFile(t, root, "flake.nix", "{}")
	configurationFile(t, root, "flake.lock", inputsSetLock)
	return root
}
func inputsSetInvocation(t *testing.T, argv ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(argv)
	if err != nil || inv.Command.Handler != cli.InputsSetHandler {
		t.Fatal(argv, inv, err)
	}
	return inv
}
func inputsSetChanged(rev string) string { return strings.Replace(inputsSetLock, "123456789", rev, 1) }

// The injected boundary still calls actual inputs.Set and its filesystem owner.
// It writes only the fresh TEMP named by that call, never a preparatory lock.
func inputsSetWriter(t *testing.T, texts []string, calls *[][]string) func(context.Context, string, []string) (inputs.Output, error) {
	t.Helper()
	return func(ctx context.Context, root string, argv []string) (inputs.Output, error) {
		*calls = append(*calls, append([]string(nil), argv...))
		index := len(*calls) - 1
		if index >= len(texts) || len(argv) != 10 || !reflect.DeepEqual(argv[:4], []string{"nix", "flake", "lock", "--update-input"}) || argv[5] != "--override-input" || argv[4] != argv[6] || argv[8] != "--output-lock-file" {
			t.Fatal("unexpected Set request", argv)
		}
		temp := argv[9]
		if !regexp.MustCompile(fmt.Sprintf(`^%s/flake\.lock\.tmp-%d-[0-9a-f]{12}$`, regexp.QuoteMeta(root), os.Getpid())).MatchString(temp) {
			t.Fatal("TEMP identity", temp)
		}
		if _, err := os.Lstat(temp); !os.IsNotExist(err) {
			t.Fatal("precreated TEMP", err)
		}
		return inputs.Output{}, os.WriteFile(temp, []byte(texts[index]), 0644)
	}
}
func inputsSetDeps(run func(context.Context, string, []string) (inputs.Output, error)) inputs.SetDeps {
	return inputs.SetDeps{Run: run, Argument: str}
}
func inputsSetNoTemps(t *testing.T, root string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "flake.lock.tmp-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal("TEMP cleanup", paths, err)
	}
}

func TestInputsSetRootActualEngineNoEvaluationAndOpaqueRevision(t *testing.T) {
	tools, marker := configurationTools(t)
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	root := inputsSetRoot(t)
	var calls [][]string
	var out strings.Builder
	deps := inputsSetDeps(inputsSetWriter(t, []string{inputsSetChanged("abcdef987654321")}, &calls))
	inv := inputsSetInvocation(t, "inputs", "set", "a", "opaque?rev/# ")
	e := &Engine{Root: root, Context: context.Background()}
	if err := e.setInputs(inv, deps, func(s string) { out.WriteString(s) }); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Updated inputs (1):\n  a: 12345678 -> abcdef98\n" || calls[0][7] != "git+https://repo?rev=opaque?rev/# " {
		t.Fatal(out.String(), calls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("root evaluated or used another tool", err)
	}
	if !strings.Contains(configurationRead(t, root, "flake.lock"), `"ref": "main"`) {
		t.Fatal("original metadata not restored")
	}
	inputsSetNoTemps(t, root)
}

func inputsSetChannelEngine(t *testing.T, root, channels string, impure bool) *Engine {
	t.Helper()
	settings := strings.Replace(inputsSettings, `"impureEval":true`, fmt.Sprintf(`"impureEval":%t`, impure), 1)
	rules := []inputsNixRule{{Argv: inputsEvalArgv(".#confctl.settings", false, "auto"), Stdout: settings}, {Argv: inputsEvalArgv(".#confctl.settings", false, "7"), Stdout: settings}, {Argv: inputsEvalArgv(".#confctl.channels", impure, "7"), Stdout: channels}}
	tools, calls := inputsTools(t, rules)
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	t.Cleanup(func() { inputsCalls(t, calls, rules) })
	return &Engine{Root: root, Context: context.Background()}
}
func TestInputsSetChannelsSelectionDedupSharingAndImmediateWarning(t *testing.T) {
	for _, tc := range []struct{ selector, channels, want string }{
		{"{z,a,z}", `{"a":{"os":"a"},"z":{"os":"a"}}`, "Configuring os in z -> abcdef98\nConfiguring os in a -> abcdef98\nConfiguring os in z -> abcdef98\n"},
		{"*", `{"z":{"os":"a"},"a":{"os":"a"}}`, "Configuring os in a -> abcdef98\nConfiguring os in z -> abcdef98\n"},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			root := inputsSetRoot(t)
			e := inputsSetChannelEngine(t, root, tc.channels, false)
			var calls [][]string
			var out strings.Builder
			deps := inputsSetDeps(inputsSetWriter(t, []string{inputsSetChanged("abcdef987654321")}, &calls))
			if err := e.setInputs(inputsSetInvocation(t, "inputs", "channel", "set", tc.selector, "os", "rev"), deps, func(s string) { out.WriteString(s) }); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || out.String() != tc.want {
				t.Fatal(calls, out.String())
			}
		})
	}
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			root := inputsSetRoot(t)
			before := configurationRead(t, root, "flake.lock")
			e := inputsSetChannelEngine(t, root, `{"z":{"os":"a","other":"a"},"outside":{"z":"a","a":"a"}}`, true)
			var out strings.Builder
			sentinel := errors.New("process refused")
			called := false
			deps := inputsSetDeps(func(context.Context, string, []string) (inputs.Output, error) {
				called = true
				if out.String() != "Warning: selected input(s) are shared outside the requested channel/role: a (also used by outside/a, outside/z, z/other)\n" {
					t.Fatal("warning not emitted before Set", out.String())
				}
				return inputs.Output{}, sentinel
			})
			argv := []string{"inputs", "channel", "set"}
			if allowed {
				argv = append(argv, "--allow-shared")
			}
			argv = append(argv, "z", "os", "rev")
			err := e.setInputs(inputsSetInvocation(t, argv...), deps, func(s string) { out.WriteString(s) })
			message := "selected input(s) are shared outside the requested channel/role: a (also used by outside/a, outside/z, z/other)"
			if allowed {
				if err != sentinel || !called || out.String() != "Warning: "+message+"\n" {
					t.Fatal(err, called, out.String())
				}
			} else if called || out.Len() != 0 || err == nil || err.Error() != message+" (use --allow-shared to proceed)" {
				t.Fatal(err, called, out.String())
			}
			if configurationRead(t, root, "flake.lock") != before {
				t.Fatal("sharing/process refusal mutated lock")
			}
			inputsSetNoTemps(t, root)
		})
	}
}

func TestInputsSetFinalSummaryReadAfterCommitAndRowRetention(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			root := inputsSetRoot(t)
			e := inputsSetChannelEngine(t, root, `{"first":{"os":"a"},"second":{"os":"b"}}`, false)
			var calls [][]string
			var out strings.Builder
			changed := strings.Replace(inputsSetChanged("abcdef987654321"), `"rev":"987654321"`, `"rev":"bbbbbbbb9"`, 1)
			deps := inputsSetDeps(inputsSetWriter(t, []string{changed, changed}, &calls))
			deps.Commit = func(ctx context.Context, r, message string, editor bool) error {
				if !editor || !strings.HasPrefix(message, "inputs: set ") {
					t.Fatal("SetAction/default editor", message, editor)
				}
				final := strings.Replace(changed, "abcdef987654321", "postcommit1", 1)
				if malformed {
					final = strings.Replace(final, `"rev":"bbbbbbbb9"`, `"rev":123`, 1)
				} else {
					final = strings.Replace(final, "bbbbbbbb9", "", 1)
				}
				return os.WriteFile(filepath.Join(r, "flake.lock"), []byte(final), 0644)
			}
			err := e.setInputs(inputsSetInvocation(t, "inputs", "channel", "set", "--commit", "--no-changelog", "{first,second}", "os", "rev"), deps, func(s string) { out.WriteString(s) })
			want := "Configuring os in first -> postcomm\n"
			if malformed {
				if err == nil || err.Error() != "expected input revision string" || out.String() != want {
					t.Fatal("later row lost earlier stdout", err, out.String())
				}
			} else {
				if err != nil || out.String() != want+"Configuring os in second -> \n" {
					t.Fatal("final reread/empty truthiness", err, out.String())
				}
			}
			if len(calls) != 2 {
				t.Fatal(calls)
			}
			inputsSetNoTemps(t, root)
		})
	}
}

func TestInputsSetMachineNameBeforeKeyAndOriginalLabel(t *testing.T) {
	root := inputsSetRoot(t)
	rules := append(inputsSettingsRules(), inputsNixRule{Argv: inputsEvalArgv(".#confctl.machineKeys", true, "7"), Stdout: `{"key":"actual","other":"key"}`}, inputsNixRule{Argv: inputsEvalArgv(".#confctl.inputsInfo.actual", true, "7"), Stdout: `{"os":{"input":"a"}}`})
	tools, calls := inputsTools(t, rules)
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	var requests [][]string
	var out strings.Builder
	e := &Engine{Root: root, Context: context.Background()}
	if err := e.setInputs(inputsSetInvocation(t, "inputs", "machine", "set", "key", "os", "rev"), inputsSetDeps(inputsSetWriter(t, []string{inputsSetChanged("abcdef987654321")}, &requests)), func(s string) { out.WriteString(s) }); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Configuring os in key -> abcdef98\n" || len(requests) != 1 || requests[0][4] != "a" {
		t.Fatal(out.String(), requests)
	}
	inputsCalls(t, calls, rules)
}

func TestInputsSetNoChangeOptionsCommitAndCancellation(t *testing.T) {
	for _, route := range []string{"root", "channel", "machine"} {
		t.Run(route, func(t *testing.T) {
			root := inputsSetRoot(t)
			e := &Engine{Root: root, Context: context.Background()}
			argv := []string{"inputs", "set", "--commit", "a", "rev"}
			if route == "channel" {
				e = inputsSetChannelEngine(t, root, `{"a":{"os":"a"}}`, false)
				argv = []string{"inputs", "channel", "set", "--commit", "a", "os", "rev"}
			}
			if route == "machine" {
				rules := append(inputsSettingsRules(), inputsNixRule{Argv: inputsEvalArgv(".#confctl.machineKeys", true, "7"), Stdout: `{"name":"key"}`}, inputsNixRule{Argv: inputsEvalArgv(".#confctl.inputsInfo.key", true, "7"), Stdout: `{"os":{"input":"a"}}`})
				tools, calls := inputsTools(t, rules)
				t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
				t.Cleanup(func() { inputsCalls(t, calls, rules) })
				argv = []string{"inputs", "machine", "set", "--commit", "name", "os", "rev"}
			}
			var calls [][]string
			var out strings.Builder
			metadata := strings.Replace(inputsSetLock, "https://repo", "https://metadata", 1)
			deps := inputsSetDeps(inputsSetWriter(t, []string{metadata}, &calls))
			deps.Commit = func(context.Context, string, string, bool) error { t.Fatal("metadata-only commit"); return nil }
			if err := e.setInputs(inputsSetInvocation(t, argv...), deps, func(s string) { out.WriteString(s) }); err != nil {
				t.Fatal(err)
			}
			want := "No changes.\n"
			if route == "channel" {
				want = "Configuring os in a -> 12345678\n"
			}
			if route == "machine" {
				want = "Configuring os in name -> 12345678\n"
			}
			if out.String() != want {
				t.Fatal(out.String())
			}
		})
	}
	for _, stage := range []string{"before", "run", "changelog", "commit"} {
		t.Run(stage, func(t *testing.T) {
			root := inputsSetRoot(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before" {
				cancel()
			}
			var calls [][]string
			var out strings.Builder
			write := inputsSetWriter(t, []string{inputsSetChanged("abcdef987654321")}, &calls)
			deps := inputsSetDeps(func(ctx context.Context, root string, argv []string) (inputs.Output, error) {
				o, err := write(ctx, root, argv)
				if stage == "run" {
					cancel()
					return o, errors.New("ordinary error loses to cancellation")
				}
				return o, err
			})
			deps.Changelog = func(context.Context, string, string, string) (string, error) {
				if stage != "changelog" && stage != "commit" {
					t.Fatal("unexpected changelog")
				}
				if stage == "changelog" {
					cancel()
				}
				return "", errors.New("changelog refusal")
			}
			deps.Commit = func(context.Context, string, string, bool) error {
				if stage != "commit" {
					t.Fatal("commit after cancellation")
				}
				cancel()
				return errors.New("ordinary commit failure")
			}
			err := (&Engine{Root: root, Context: ctx}).setInputs(inputsSetInvocation(t, "inputs", "set", "--commit", "a", "rev"), deps, func(s string) { out.WriteString(s) })
			if !errors.Is(err, context.Canceled) || out.Len() != 0 {
				t.Fatal(stage, err, out.String())
			}
			want := inputsSetLock
			if stage == "changelog" || stage == "commit" {
				if !strings.Contains(configurationRead(t, root, "flake.lock"), "abcdef987654321") {
					t.Fatal("completed replacement rolled back")
				}
			} else if configurationRead(t, root, "flake.lock") != want {
				t.Fatal("canceled before replacement mutated lock")
			}
			inputsSetNoTemps(t, root)
		})
	}
	for _, downgrade := range []bool{false, true} {
		t.Run(fmt.Sprint(downgrade), func(t *testing.T) {
			root := inputsSetRoot(t)
			var calls [][]string
			var out strings.Builder
			committed := false
			deps := inputsSetDeps(inputsSetWriter(t, []string{inputsSetChanged("abcdef987654321")}, &calls))
			deps.Changelog = func(ctx context.Context, url, old, next string) (string, error) {
				want := []string{"123456789", "abcdef987654321"}
				if downgrade {
					want[0], want[1] = want[1], want[0]
				}
				if url != "https://repo" || !reflect.DeepEqual([]string{old, next}, want) {
					t.Fatal(url, old, next)
				}
				return "", errors.New("finite changelog failure")
			}
			deps.Commit = func(ctx context.Context, root, message string, editor bool) error {
				committed = true
				if editor || !strings.HasPrefix(message, "inputs: set a to abcdef98\n") || !strings.Contains(message, "finite changelog failure") {
					t.Fatal(message, editor)
				}
				return errors.New("git commit failed")
			}
			argv := []string{"inputs", "set", "--commit", "--no-editor"}
			if downgrade {
				argv = append(argv, "-d")
			}
			argv = append(argv, "a", "rev")
			err := (&Engine{Root: root, Context: context.Background()}).setInputs(inputsSetInvocation(t, argv...), deps, func(s string) { out.WriteString(s) })
			if err == nil || err.Error() != "git commit failed" || !committed || out.Len() != 0 || !strings.Contains(configurationRead(t, root, "flake.lock"), "abcdef987654321") {
				t.Fatal(err, committed, out.String())
			}
		})
	}
}

func TestInputsSetTypedDiagnosticPresentationOnly(t *testing.T) {
	open := &os.PathError{Op: "open", Path: "temp", Err: syscall.ENOENT}
	cases := []struct {
		phase              inputs.SetPhase
		cause              error
		path, target, want string
	}{
		{inputs.FirstRead, open, "temp", "", "No such file or directory @ rb_sysopen - temp"},
		{inputs.SnapshotRead, open, "temp", "", "No such file or directory @ rb_sysopen - temp"},
		{inputs.TempRead, open, "temp", "", "No such file or directory @ rb_sysopen - temp"},
		{inputs.FinalRead, open, "temp", "", "No such file or directory @ rb_sysopen - temp"},
		{inputs.TempWrite, open, "temp", "", "No such file or directory @ rb_sysopen - temp"},
		{inputs.Rename, &os.LinkError{Op: "rename", Old: "from", New: "to", Err: syscall.EISDIR}, "from", "to", "Is a directory @ rb_file_s_rename - (from, to)"},
		{inputs.Rename, &os.LinkError{Op: "rename", Old: "from", New: "to", Err: syscall.EEXIST}, "from", "to", "File exists - to"},
	}
	for _, tc := range cases {
		err := &inputs.SetError{Phase: tc.phase, Path: tc.path, Target: tc.target, Err: tc.cause}
		wrapped := fmt.Errorf("outer: %w", err)
		if inputSetDiagnostic(err) != tc.want || inputSetDiagnostic(wrapped) != tc.want || !errors.Is(wrapped, tc.cause) {
			t.Fatal(tc, inputSetDiagnostic(wrapped))
		}
	}
	for _, tc := range []struct {
		phase        inputs.SetPhase
		cause        error
		path, target string
	}{
		{inputs.Validate, open, "temp", ""}, {inputs.Restore, open, "temp", ""}, {inputs.Format, open, "temp", ""}, {inputs.Diff, open, "temp", ""},
		{inputs.TempRead, open, "wrong", ""}, {inputs.TempWrite, &os.PathError{Op: "write", Path: "temp", Err: syscall.EIO}, "temp", ""},
		{inputs.TempRead, errors.New("expected object key, got 'not-json' at line 1 column 2"), "temp", ""},
		{inputs.Rename, &os.LinkError{Op: "rename", Old: "wrong", New: "to", Err: syscall.EISDIR}, "from", "to"},
		{inputs.Rename, &os.LinkError{Op: "rename", Old: "from", New: "wrong", Err: syscall.EISDIR}, "from", "to"},
		{inputs.Rename, &os.LinkError{Op: "link", Old: "from", New: "to", Err: syscall.EISDIR}, "from", "to"},
		{inputs.TempWrite, errors.New("close failure"), "temp", ""},
	} {
		err := fmt.Errorf("outer: %w", &inputs.SetError{Phase: tc.phase, Path: tc.path, Target: tc.target, Err: tc.cause})
		if got := inputSetDiagnostic(err); got != err.Error() {
			t.Fatal("unmapped cause changed", tc, got)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, &inputgit.CommitAdapterError{Err: errors.New("helper failed")}} {
		if inputSetDiagnostic(err) != err.Error() {
			t.Fatal(err)
		}
	}
}

// Finite process rules validate the actual dynamic TEMP operand; the test child
// writes it only when instructed. All other requests, including real tools,
// refuse. These are unit stimuli, not accepted original observations.
type inputsSetRule struct {
	Argv           []string
	Text           *string
	Stdout, Stderr string
	Exit           int
	Retry          bool
}

func TestInputsSetNixHelper(t *testing.T) {
	if os.Getenv("CONFCTL_SET_TOOL") != "1" {
		return
	}
	var argv []string
	for i, arg := range os.Args {
		if arg == "--" {
			argv = os.Args[i+1:]
			break
		}
	}
	path := os.Getenv("CONFCTL_INPUTS_CALLS")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var a []string
		if err = json.Unmarshal([]byte(line), &a); err != nil {
			panic(err)
		}
		calls = append(calls, a)
	}
	var rules []inputsSetRule
	if err = json.Unmarshal([]byte(os.Getenv("CONFCTL_SET_RULES")), &rules); err != nil {
		panic(err)
	}
	i := len(calls)
	if i >= len(rules) {
		panic("unlisted request")
	}
	r := rules[i]
	want := append([]string(nil), r.Argv...)
	for j, a := range want {
		if a == "${TEMP}" {
			if j != len(want)-1 || want[j-1] != "--output-lock-file" || len(argv) != len(want) {
				panic("invalid finite TEMP rule")
			}
			root, err := os.Getwd()
			if err != nil {
				panic(err)
			}
			temp := argv[j]
			if !regexp.MustCompile(fmt.Sprintf(`^%s/flake\.lock\.tmp-%d-[0-9a-f]{12}$`, regexp.QuoteMeta(root), os.Getppid())).MatchString(temp) {
				panic("wrong TEMP identity")
			}
			if r.Retry {
				if i == 0 || calls[i-1][len(calls[i-1])-1] != temp {
					panic("retry changed TEMP")
				}
			} else if _, err = os.Lstat(temp); !os.IsNotExist(err) {
				panic("precreated TEMP")
			}
			want[j] = temp
		}
	}
	if !reflect.DeepEqual(argv, want) {
		fmt.Fprintf(os.Stderr, "unexpected setter request %d: %q want %q\n", i, argv, want)
		os.Exit(97)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	err = json.NewEncoder(f).Encode(argv)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		panic("record write")
	}
	if r.Text != nil {
		if len(argv) == 0 || argv[len(argv)-2] != "--output-lock-file" {
			panic("write outside TEMP")
		}
		if err = os.WriteFile(argv[len(argv)-1], []byte(*r.Text), 0644); err != nil {
			panic(err)
		}
	}
	fmt.Print(r.Stdout)
	fmt.Fprint(os.Stderr, r.Stderr)
	os.Exit(r.Exit)
}
func TestInputsSetMainSubprocess(t *testing.T) {
	if os.Getenv("CONFCTL_SET_MAIN") != "1" {
		return
	}
	// Setsid on the independently launched subject is the test's no-ctty owner.
	if f, err := os.Open("/dev/tty"); err == nil {
		f.Close()
		panic("setter fake-commit subject has controlling terminal")
	} else if !errors.Is(err, syscall.ENXIO) {
		panic(err)
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("CONFCTL_STAGE1_ARGV")), &argv); err != nil {
		panic(err)
	}
	os.Exit(Main(context.Background(), argv))
}
func inputsSetCLI(t *testing.T, root string, argv []string, rules []inputsSetRule) (int, string, string) {
	t.Helper()
	tools, marker := configurationTools(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!" + sh + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestInputsSetNixHelper$", "--"}) + " \"$@\"\n"
	for _, tool := range []string{"nix", "git"} {
		if err = os.WriteFile(filepath.Join(tools, tool), []byte(launcher), 0700); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(argv)
	rb, _ := json.Marshal(rules)
	calls := filepath.Join(tools, "calls.jsonl")
	cmd := exec.Command(os.Args[0], "-test.run=^TestInputsSetMainSubprocess$")
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	env := os.Environ()
	replaceAuthority(&env, "CONFCTL_EXTENSION_ROOT", nil)
	replaceAuthority(&env, "CONFCTL_EXTENSION_REGISTRY", nil)
	cmd.Env = append(env, "CONFCTL_SET_MAIN=1", "CONFCTL_SET_TOOL=1", "CONFCTL_SET_RULES="+string(rb), "CONFCTL_STAGE1_ARGV="+string(b), "CONFCTL_INPUTS_CALLS="+calls, "PATH="+tools+":"+os.Getenv("PATH"), "PWD="+root)
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unlisted tool", err)
	}
	cb, err := os.ReadFile(calls)
	if os.IsNotExist(err) && len(rules) == 0 {
		return code, out.String(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(cb)), "\n")
	if len(rows) != len(rules) {
		t.Fatal("request chronology", rows, rules)
	}
	return code, out.String(), stderr.String()
}
func inputsSetLockRule(name, url string, text *string) inputsSetRule {
	return inputsSetRule{Argv: []string{"flake", "lock", "--update-input", name, "--override-input", name, url, "--output-lock-file", "${TEMP}"}, Text: text}
}
func inputsSetEvalRules(channels string) []inputsSetRule {
	settings := strings.Replace(inputsSettings, `"impureEval":true`, `"impureEval":false`, 1)
	return []inputsSetRule{{Argv: inputsEvalArgv(".#confctl.settings", false, "auto"), Stdout: settings}, {Argv: inputsEvalArgv(".#confctl.settings", false, "7"), Stdout: settings}, {Argv: inputsEvalArgv(".#confctl.channels", false, "7"), Stdout: channels}}
}

func TestInputsSetMainHelpArityLogAndPP(t *testing.T) {
	for _, path := range [][]string{{"inputs", "set"}, {"inputs", "channel", "set"}, {"inputs", "machine", "set"}} {
		help, _ := cli.BuiltinRegistry().Help(path, 80)
		for _, argv := range [][]string{append(append([]string{}, path...), "--help", "--unknown"), append([]string{"help"}, path...)} {
			code, out, stderr, root := actualCLI(t, argv, "")
			if code != 0 || out != help || stderr != "" {
				t.Fatal(argv, code, out, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, ".confctl")); !os.IsNotExist(err) {
				t.Fatal("help created state", err)
			}
		}
		argv := append(append([]string{}, path...), "--no-editor", "-d", "--no-downgrade", "a")
		root := t.TempDir()
		code, out, stderr := inputsSetCLI(t, root, argv, nil)
		if code != 1 || out != "" || !strings.Contains(stderr, "has no flake.nix") {
			t.Fatal("flake precedence", code, out, stderr)
		}
		root = inputsSetRoot(t)
		before := configurationRead(t, root, "flake.lock")
		code, out, stderr = inputsSetCLI(t, root, argv, nil)
		missing := "rev"
		if len(path) == 3 {
			missing = "role"
		}
		if code != 64 || out != help || !strings.HasSuffix(stderr, "error: missing argument <"+missing+">\n\n") {
			t.Fatal(code, out, stderr)
		}
		paths, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*-confctl-"+strings.Join(path, "-")+".log"))
		if len(paths) != 1 {
			t.Fatal(paths)
		}
		log := configurationRead(t, "/", paths[0])
		expected := []string{`"editor" => false`, `"d" => false`, `"downgrade" => false`, `"commit" => false`, `"changelog" => true`}
		previous := -1
		for _, row := range expected {
			pos := strings.Index(log, row)
			if pos <= previous {
				t.Fatal("PP insertion/alias/defaults", log)
			}
			previous = pos
		}
		if strings.Count(log, "#<GLI::Command::ParentKey:0x0000000000000000>") != len(path)-1 || strings.Contains(log, "Running ") || configurationRead(t, root, "flake.lock") != before {
			t.Fatal("arity context/effects", log)
		}
	}
	root := inputsSetRoot(t)
	code, out, stderr := inputsSetCLI(t, root, []string{"inputs", "set", "a", "rev", "--commit"}, nil)
	if code != 64 || !strings.Contains(out, "<input-name> <rev>") || !strings.HasSuffix(stderr, "error: unknown argument: --commit (note that options must come before arguments)\n\n") {
		t.Fatal(code, out, stderr)
	}
}

func TestInputsSetMainProcessGuardMissingMalformedAndPartialFailures(t *testing.T) {
	next := inputsSetChanged("abcdef987654321")
	for _, kind := range []string{"success", "unchanged", "exit", "retry", "guard", "missing", "malformed", "partial"} {
		t.Run(kind, func(t *testing.T) {
			root := inputsSetRoot(t)
			argv := []string{"inputs", "set", "a", "rev"}
			rules := []inputsSetRule{inputsSetLockRule("a", "git+https://repo?rev=rev", &next)}
			want := ""
			wantCode := 1
			switch kind {
			case "success":
				wantCode = 0
				want = "Updated inputs (1):\n  a: 12345678 -> abcdef98\n"
			case "unchanged":
				rules[0].Text = inputsStringPtr(inputsSetLock)
				wantCode = 0
				want = "No changes.\n"
			case "exit":
				rules[0].Exit = 73
				rules[0].Stderr = "finite refusal"
			case "retry":
				rules[0].Exit = 1
				rules[0].Stderr = "experimental feature nix-command is disabled"
				rules[0].Text = nil
				retry := inputsSetLockRule("a", "git+https://repo?rev=rev", &next)
				retry.Argv = append([]string{"--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes"}, retry.Argv...)
				retry.Retry = true
				retry.Exit = 73
				retry.Stderr = "second refusal"
				rules = append(rules, retry)
			case "guard":
				rules[0].Stderr = "unable to download 'https://api.github.com/repos/x': offline; using cached version"
			case "missing":
				rules[0].Text = nil
			case "malformed":
				rules[0].Text = inputsStringPtr("{not-json")
			case "partial":
				argv = []string{"inputs", "channel", "set", "--allow-shared", "{one,two}", "os", "rev"}
				eval := inputsSetEvalRules(`{"one":{"os":"a","other":"a"},"two":{"os":"b"}}`)
				rules = append(eval, rules...)
				failed := inputsSetLockRule("b", "github:org/b/rev", nil)
				failed.Exit = 73
				failed.Stderr = "second refused"
				rules = append(rules, failed)
				want = "Warning: selected input(s) are shared outside the requested channel/role: a (also used by one/other)\n"
			}
			code, out, stderr := inputsSetCLI(t, root, argv, rules)
			if code != wantCode || out != want || code == 0 && stderr != "" {
				t.Fatal(kind, code, out, stderr)
			}
			logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
			if code != 0 {
				if len(logs) != 1 {
					t.Fatal(logs)
				}
				if !strings.HasPrefix(stderr, "\nLog file: "+logs[0]+"\nerror: ") || !strings.HasSuffix(stderr, "\n") || strings.HasSuffix(stderr, "\n\n") {
					t.Fatal("single final LF", stderr)
				}
				log := configurationRead(t, "/", logs[0])
				if !strings.Contains(log, "Running nix flake lock --update-input a") || strings.Contains(log, "&& nix flake lock ") {
					t.Fatal("captured lock log", log)
				}
				switch kind {
				case "missing":
					if !strings.Contains(stderr, "No such file or directory @ rb_sysopen - "+root+"/flake.lock.tmp-") {
						t.Fatal(stderr)
					}
				case "malformed":
					if !strings.HasSuffix(stderr, "error: expected object key, got 'not-json' at line 1 column 2\n") {
						t.Fatal(stderr)
					}
				case "guard":
					if !strings.Contains(stderr, "Nix could not refresh GitHub input metadata and used a cached version instead.") {
						t.Fatal(stderr)
					}
				case "exit", "retry", "partial":
					last := rules[len(rules)-1]
					commands := regexp.MustCompile(`Running (nix [^\n]+)\n`).FindAllStringSubmatch(log, -1)
					if len(commands) == 0 {
						t.Fatal("missing captured command", log)
					}
					command := commands[len(commands)-1][1]
					wantError := fmt.Sprintf("\nLog file: %s\nerror: Running `%s` failed with\n  exit status: %d\n  stdout: Nothing written\n  stderr: %s\n", logs[0], command, last.Exit, last.Stderr)
					if stderr != wantError {
						t.Fatal("complete captured error presentation", stderr, wantError)
					}
				}
			} else if len(logs) != 0 {
				t.Fatal("success log retained", logs)
			}
			if kind == "success" || kind == "partial" {
				if !strings.Contains(configurationRead(t, root, "flake.lock"), "abcdef987654321") {
					t.Fatal("completed replacement lost")
				}
			} else if kind != "unchanged" && configurationRead(t, root, "flake.lock") != inputsSetLock {
				t.Fatal("failed TEMP replaced lock")
			}
			inputsSetNoTemps(t, root)
		})
	}
}

func TestInputsSetMainNoCttyFakeCommitForwardingAndRefusal(t *testing.T) {
	next := inputsSetChanged("abcdef987654321")
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			root := inputsSetRoot(t)
			commit := inputsSetRule{Argv: []string{"commit", "-m", "inputs: set a to abcdef98\n\na: 12345678 -> abcdef98", "flake.lock"}, Stdout: "commit output\n", Stderr: "commit diagnostic\n"}
			if failed {
				commit.Exit = 9
			}
			code, out, stderr := inputsSetCLI(t, root, []string{"inputs", "set", "--commit", "--no-changelog", "a", "rev"}, []inputsSetRule{inputsSetLockRule("a", "git+https://repo?rev=rev", &next), commit})
			if failed {
				if code != 1 || out != "commit output\n" || !strings.HasSuffix(stderr, "error: git commit failed\n") {
					t.Fatal(code, out, stderr)
				}
			} else if code != 0 || out != "commit output\nUpdated inputs (1):\n  a: 12345678 -> abcdef98\n" || stderr != "commit diagnostic\n" {
				t.Fatal(code, out, stderr)
			}
			if !strings.Contains(configurationRead(t, root, "flake.lock"), "abcdef987654321") {
				t.Fatal("commit rolled back lock")
			}
			logs, _ := filepath.Glob(filepath.Join(root, ".confctl/logs/*.log"))
			for _, p := range logs {
				log := configurationRead(t, "/", p)
				if strings.Contains(log, "Running git") || strings.Contains(log, "commit output") || strings.Contains(log, "commit diagnostic") {
					t.Fatal("inherited commit captured", log)
				}
			}
		})
	}
}

func TestInputsSetSelectionRefusalsBeforeActualSet(t *testing.T) {
	for _, tc := range []struct{ selector, role, channels, want string }{
		{"missing", "os", `{"a":{"os":"a"}}`, "no channels matched 'missing'"},
		{"*", "os", `{"a":{"os":false},"b":{"os":null}}`, "no inputs selected (check role name?)"},
		{"*", "os", `[]`, "no channels matched '*'"},
		{"{selected}", "os", `{"selected":{"os":"b"},"z":{"role":"b"},"a":{"role":"b"}}`, "selected input(s) are shared outside the requested channel/role: b (also used by a/role, z/role) (use --allow-shared to proceed)"},
		{"{one,two}", "os", `{"one":{"os":"b","other":"b"},"two":{"os":"a","other":"a"}}`, "selected input(s) are shared outside the requested channel/role: a (also used by two/other); b (also used by one/other) (use --allow-shared to proceed)"},
	} {
		t.Run(tc.selector+tc.channels, func(t *testing.T) {
			root := inputsSetRoot(t)
			e := inputsSetChannelEngine(t, root, tc.channels, false)
			deps := inputsSetDeps(func(context.Context, string, []string) (inputs.Output, error) {
				t.Fatal("selection refusal reached Set runner")
				return inputs.Output{}, nil
			})
			err := e.setInputs(inputsSetInvocation(t, "inputs", "channel", "set", tc.selector, tc.role, "rev"), deps, func(string) { t.Fatal("refused selection emitted output") })
			if err == nil || err.Error() != tc.want || configurationRead(t, root, "flake.lock") != inputsSetLock {
				t.Fatal(err)
			}
		})
	}
}

func TestInputsSetGroupedAliasCommitAndRootNoSummaryReread(t *testing.T) {
	root := inputsSetRoot(t)
	e := inputsSetChannelEngine(t, root, `{"z":{"os":"alias"},"a":{"os":"a"}}`, false)
	var calls [][]string
	var out strings.Builder
	lookups := 0
	commits := 0
	next := inputsSetChanged("abcdef987654321")
	deps := inputsSetDeps(inputsSetWriter(t, []string{next, next}, &calls))
	deps.Changelog = func(ctx context.Context, url, from, to string) (string, error) {
		lookups++
		if url != "https://repo" || from != "123456789" || to != "abcdef987654321" {
			t.Fatal(url, from, to)
		}
		return "finite log", nil
	}
	deps.Commit = func(ctx context.Context, root, message string, editor bool) error {
		commits++
		want := "inputs: set a, alias to abcdef98\n\nalias: 12345678 -> abcdef98\na: 12345678 -> abcdef98\nfinite log"
		if message != want || !editor {
			t.Fatal(message, editor)
		}
		return nil
	}
	if err := e.setInputs(inputsSetInvocation(t, "inputs", "channel", "set", "--commit", "{z,a}", "os", "rev"), deps, func(s string) { out.WriteString(s) }); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 || commits != 1 || len(calls) != 2 || calls[0][4] != "alias" || calls[1][4] != "a" || out.String() != "Configuring os in z -> abcdef98\nConfiguring os in a -> abcdef98\n" {
		t.Fatal(lookups, commits, calls, out.String())
	}
	root = inputsSetRoot(t)
	calls = nil
	out.Reset()
	deps = inputsSetDeps(inputsSetWriter(t, []string{next}, &calls))
	// A completed root Set must render Change rows, not load a lock after Commit.
	deps.Commit = func(ctx context.Context, root, message string, editor bool) error {
		return os.Remove(filepath.Join(root, "flake.lock"))
	}
	if err := (&Engine{Root: root, Context: context.Background()}).setInputs(inputsSetInvocation(t, "inputs", "set", "--commit", "--no-changelog", "a", "rev"), deps, func(s string) { out.WriteString(s) }); err != nil || out.String() != "Updated inputs (1):\n  a: 12345678 -> abcdef98\n" {
		t.Fatal(err, out.String())
	}
}
