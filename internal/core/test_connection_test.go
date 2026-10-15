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
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
)

type connectionRead struct {
	data string
	err  error
}

type connectionReader struct {
	t     *testing.T
	steps []connectionRead
	reads int
}

func (r *connectionReader) Read(b []byte) (int, error) {
	r.t.Helper()
	if len(b) > 16384 {
		r.t.Fatalf("real read request exceeded bound: %d", len(b))
	}
	r.reads++
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	s := r.steps[0]
	r.steps = r.steps[1:]
	if len(s.data) > len(b) {
		r.t.Fatal("scripted read not representable")
	}
	return copy(b, s.data), s.err
}

func TestTestConnectionCaptureReads(t *testing.T) {
	// Independent literal P/M/S payload and read pattern from the accepted
	// original retained log, SHA28a6da6a...; not replayed by production.
	parts := []string{strings.Repeat("P", 16384), strings.Repeat("P", 16384), "M" + strings.Repeat("S", 16383), strings.Repeat("S", 16384), "S"}
	steps := []connectionRead{}
	for _, p := range parts {
		steps = append(steps, connectionRead{data: p})
	}
	r := &connectionReader{t: t, steps: steps}
	var aggregate bytes.Buffer
	logged := []string{}
	c := &testConnectionCapture{buffer: &aggregate, chunk: func(b []byte) { logged = append(logged, string(b)) }}
	n, err := c.ReadFrom(r)
	if err != nil || n != 65537 || !reflect.DeepEqual(logged, parts) || aggregate.String() != strings.Repeat("P", 32768)+"M"+strings.Repeat("S", 32768) || r.reads != 6 {
		t.Fatal(n, err, r.reads, len(logged))
	}
	want := strings.Repeat("P", 32768) + "\n... omitting 1 bytes ...\n" + strings.Repeat("S", 32768)
	if string(testConnectionStderr(aggregate.Bytes())) != want || !strings.Contains(aggregate.String(), "M") {
		t.Fatal("diagnostic truncation changed full capture")
	}
	for _, terminal := range []error{io.EOF, errors.New("synthetic read error")} {
		t.Run(terminal.Error(), func(t *testing.T) {
			parts := []string{"a", "in\nside", strings.Repeat("q", 16383), "last\n"}
			steps := []connectionRead{{data: parts[0]}, {data: parts[1]}, {data: parts[2]}, {data: parts[3], err: terminal}}
			reader := &connectionReader{t: t, steps: steps}
			var buf bytes.Buffer
			var saved [][]byte
			sink := &testConnectionCapture{buffer: &buf, chunk: func(b []byte) { saved = append(saved, bytes.Clone(b)) }}
			n, err := sink.ReadFrom(reader)
			wantErr := terminal
			if terminal == io.EOF {
				wantErr = nil
			}
			if err != wantErr || n != int64(len(strings.Join(parts, ""))) || buf.String() != strings.Join(parts, "") || reader.reads != 4 {
				t.Fatal("positive bytes not processed before terminal error", n, err)
			}
			for i, b := range saved {
				if string(b) != parts[i] {
					t.Fatal("callback alias or coalesced short read", i)
				}
			}
		})
	}
	var empty bytes.Buffer
	c = &testConnectionCapture{buffer: &empty, chunk: func([]byte) { t.Fatal("empty EOF callback") }}
	if n, err := c.ReadFrom(strings.NewReader("")); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestTestConnectionCaptureFileCopyDispatch(t *testing.T) {
	var b bytes.Buffer
	parts := [][]byte{}
	sink := &testConnectionCapture{buffer: &b, chunk: func(p []byte) {
		if len(p) == 0 || len(p) > 16384 {
			t.Errorf("ReaderFrom dispatch lost: positive read length%d", len(p))
		}
		parts = append(parts, bytes.Clone(p))
	}}
	if _, ok := any(sink).(io.ReaderFrom); !ok {
		t.Fatal("missing ReaderFrom")
	}
	if _, ok := any(sink).(syscall.Conn); ok {
		t.Fatal("sink exposes syscall.Conn")
	}
	if _, ok := any(sink).(io.WriterTo); ok {
		t.Fatal("sink exposes WriterTo")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	payload := bytes.Repeat([]byte("different\nstream\x00"), 7000)
	done := make(chan error, 1)
	go func() {
		_, err := w.Write(payload)
		closeErr := w.Close()
		if err == nil {
			err = closeErr
		}
		done <- err
	}()
	n, err := io.Copy(sink, r) // Actual os.File.WriterTo/genericWriteTo dispatch.
	if producerErr := <-done; producerErr != nil {
		t.Fatal(producerErr)
	}
	if err != nil || n != int64(len(payload)) || !bytes.Equal(b.Bytes(), payload) || !bytes.Equal(bytes.Join(parts, nil), payload) || len(parts) < 2 {
		t.Fatal("pipe conservation/order", n, err, len(parts))
	}
	// Deliberately do not assert scheduler-dependent pipe partitions.
}

func connectionInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(append([]string{"test-connection"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func connectionMachine(name, key, host string, managed bool) Machine {
	return Machine{Name: name, Key: key, Spin: "nixos", Managed: managed, Target: Target{Host: &host, Port: 22, Localhost: host == "localhost"}, Attributes: map[string]any{"managed": managed, "spin": "nixos", "host": map[string]any{"target": host}, "tags": []any{"node"}}}
}

func connectionEngine(t *testing.T, ms ...Machine) *Engine {
	t.Helper()
	return &Engine{Context: context.Background(), Root: t.TempDir(), Inventory: ms, SettingsCache: map[string]any{"list": map[string]any{"columns": []any{"name", "spin", "managed", "host.target"}}}}
}

func TestTestConnectionSelectionAndLiteralArguments(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	all := []Machine{connectionMachine("zeta", "node*", "zeta.example", true), connectionMachine("alpha/site", "alpha", "alpha.example", true), connectionMachine("beta", "beta", "beta.example", true), connectionMachine(".hidden", "hidden", "hidden.example", false), connectionMachine("carried", "carried", "own.example", true), connectionMachine("nil", "nil", "", true), connectionMachine("empty-target", "empty", "", true)}
	carrier := "carrier"
	all[4].CarrierName, all[5].Target.Host = &carrier, nil
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"-y"}, []string{"zeta.example", "alpha.example", "beta.example", ""}},
		{[]string{"-y", "--managed", "y"}, []string{"zeta.example", "alpha.example", "beta.example", ""}},
		{[]string{"-y", "--managed", "yes"}, []string{"zeta.example", "alpha.example", "beta.example", ""}},
		{[]string{"-y", "--managed", "n"}, []string{"hidden.example"}},
		{[]string{"-y", "--managed", "no"}, []string{"hidden.example"}},
		{[]string{"-y", "--managed", "a"}, []string{"zeta.example", "alpha.example", "beta.example", "hidden.example", ""}},
		{[]string{"-y", "--managed", "all"}, []string{"zeta.example", "alpha.example", "beta.example", "hidden.example", ""}},
		{[]string{"-y", "node*"}, []string{"zeta.example"}},
		{[]string{"-y", "{alpha/site,zeta}"}, []string{"zeta.example", "alpha.example"}},
		{[]string{"-y", "*/site"}, []string{"alpha.example"}},
		{[]string{"-y", "--managed", "all", ".*"}, []string{"hidden.example"}},
		{[]string{"-y", "-a", "managed=true", "--attr", "name!=beta", "-t", "node", "--tag", "^absent"}, []string{"zeta.example", "alpha.example", ""}},
		{[]string{"-y", "zeta", "ignored", "--help", "--no-yes"}, []string{"zeta.example"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			e := connectionEngine(t, all...)
			var hosts []string
			var out bytes.Buffer
			err := e.runTestConnection(connectionInvocation(t, tc.args...), strings.NewReader(""), &out, testConnectionDeps{Run: func(ctx context.Context, root string, argv []string, input []byte) (Result, error) {
				if ctx != e.Context || root != e.Root || input != nil {
					t.Fatal("runner context/CWD/empty-open input changed")
				}
				hosts = append(hosts, argv[len(argv)-2])
				return Result{ExitCode: 0, Stdout: []byte("not-numeric"), Stderr: []byte("successful stderr")}, nil
			}})
			if err != nil || !reflect.DeepEqual(hosts, tc.want) || out.String() != fmt.Sprintf("\nResult: %d successful, 0 failed\n\nFailed machines:\n", len(tc.want)) {
				t.Fatal(hosts, err, out.String())
			}
		})
	}
	// Filtered exact name takes precedence over another machine's matching key.
	all = []Machine{connectionMachine("collision", "other", "excluded", true), connectionMachine("other", "collision", "survivor", true)}
	all[0].Attributes["tags"] = []any{}
	for _, ms := range [][]Machine{all, {}, {all[0]}} {
		e := connectionEngine(t, ms...)
		if ms == nil {
			e.Inventory = []Machine{}
		}
		var out bytes.Buffer
		err := e.runTestConnection(connectionInvocation(t, "-t", "node", "collision"), strings.NewReader("y\n"), &out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
			t.Fatal("empty selection ran target")
			return Result{}, nil
		}})
		if err == nil || err.Error() != "No machines to test" || out.Len() != 0 {
			t.Fatal(err, out.String())
		}
	}
	t.Setenv("CONFCTL_SSH_CONFIG", "private ssh%quote'/config")
	m := connectionMachine("quoted", "quoted", "target%with'quote", true)
	m.Target.Port = 2222
	a, err := testConnectionArgs(m)
	want := []string{"ssh", "-F", "private ssh%quote'/config", "-l", "root", "-p", "2222", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1", "target%with'quote", "cat /proc/uptime"}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Fatal(a, err)
	}
	const display = `ssh -F private\ ssh\%quote\'/config -l root -p 2222 -o ConnectTimeout\=3 -o ServerAliveInterval\=3 -o ServerAliveCountMax\=1 target\%with\'quote cat\ /proc/uptime`
	if testConnectionShellJoin(a) != display {
		t.Fatal(testConnectionShellJoin(a))
	}
	a, err = testConnectionArgs(connectionMachine("local", "local", "localhost", true))
	if err != nil || !reflect.DeepEqual(a, []string{"cat", "/proc/uptime"}) {
		t.Fatal(a, err)
	}
}

func TestTestConnectionConfirmation(t *testing.T) {
	// Literal table/prompt bytes from accepted original no-yes-uppercase and
	// prompt-* records. Injected Run outcomes remain synthetic.
	const intro = "Test SSH connection to the following machines:\nNAME   SPIN    MANAGED   HOST.TARGET\none    nixos   true      one.example\n"
	const prompt = "\nContinue? [y/N]: "
	const summary = "\nResult: 1 successful, 0 failed\n\nFailed machines:\n"
	for _, tc := range []struct {
		in      string
		err     string
		prompts int
		newline bool
	}{
		{" \x00\tY\r\n", "", 1, true}, {"\ninvalid\ny\n", "", 3, true}, {"n\n", "Aborted", 1, true}, {"", "end of file reached", 1, false}, {"y", "", 1, true}, {"N", "Aborted", 1, true},
	} {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			e := connectionEngine(t, connectionMachine("one", "one", "one.example", true))
			var out bytes.Buffer
			calls := 0
			err := e.runTestConnection(connectionInvocation(t, "--no-yes", "one"), strings.NewReader(tc.in), &out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
				calls++
				return Result{ExitCode: 0}, nil
			}})
			want := intro + strings.Repeat(prompt, tc.prompts)
			if tc.newline {
				want += "\n"
			}
			if tc.err == "" {
				want += summary
			}
			if out.String() != want || (err == nil) != (tc.err == "") || err != nil && err.Error() != tc.err || calls != map[bool]int{true: 1, false: 0}[tc.err == ""] {
				t.Fatal(err, calls, out.String(), want)
			}
		})
	}
	readErr := errors.New("synthetic reader failure")
	var out bytes.Buffer
	err := testConnectionConfirm(&connectionReader{t: t, steps: []connectionRead{{data: "y", err: readErr}}}, &out)
	if err != readErr || out.String() != prompt {
		t.Fatal("read failure was accepted", err)
	}
}

func TestTestConnectionMalformedAttrPreservesSharedSelector(t *testing.T) {
	one := connectionMachine("one", "one", "one.example", true)
	_, err := Select([]Machine{one}, nil, []string{"malformed"}, nil, "yes")
	if err == nil || err.Error() != "Invalid filter 'false'" {
		t.Fatal("shared default selector changed", err)
	}
	e := connectionEngine(t, one)
	var out bytes.Buffer
	err = e.runTestConnection(connectionInvocation(t, "-a", "malformed", "-y"), nil, &out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
		t.Fatal("malformed filter ran tool")
		return Result{}, nil
	}})
	var argument *testConnectionArgumentError
	if !errors.As(err, &argument) || err.Error() != "Invalid filter ''" || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
}

func TestTestConnectionCompletedStatusesAndPriority(t *testing.T) {
	// Finite original signal diagnostics are15/2/9 for both paths. Ordinary
	// 143/130/137 here are synthetic counterexamples, never converted to signals.
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	for _, local := range []bool{false, true} {
		for _, status := range []int{7, 255, 15, 2, 9, 143, 130, 137} {
			t.Run(fmt.Sprint(local, status), func(t *testing.T) {
				host := "signal.example"
				command := `ssh -l root -o ConnectTimeout\=3 -o ServerAliveInterval\=3 -o ServerAliveCountMax\=1 signal.example cat\ /proc/uptime`
				if local {
					host, command = "localhost", "cat /proc/uptime"
				}
				e := connectionEngine(t, connectionMachine("signalled", "first", host, true), connectionMachine("later", "later", "later.example", true))
				var out bytes.Buffer
				calls := 0
				err := e.runTestConnection(connectionInvocation(t, "-y"), nil, &out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
					calls++
					if calls == 2 {
						return Result{ExitCode: 0}, nil
					}
					r := Result{ExitCode: status, Stdout: []byte("signal stdout\n"), Stderr: []byte("signal stderr\n")}
					if status == 15 || status == 2 || status == 9 {
						r.ExitCode, r.signal, r.Signal, r.waitErr = -1, syscall.Signal(status), "synthetic signal text", &exec.ExitError{}
					}
					return r, nil
				}})
				want := fmt.Sprintf("Unable to connect to signalled: Running `%s` failed with\n  exit status: %d\n  stdout: signal stdout\n  stderr: signal stderr\n\n\nResult: 1 successful, 1 failed\n\nFailed machines:\n  signalled\n", command, status)
				if err != nil || calls != 2 || out.String() != want {
					t.Fatal(err, calls, out.String())
				}
			})
		}
	}
	primary := errors.New("synthetic start failure")
	copyErr := errors.New("synthetic copy failure")
	for _, tc := range []struct {
		name      string
		canceled  bool
		r         Result
		err, want error
	}{
		{"cancel-signal-exit", true, Result{ExitCode: -1, signal: 15, waitErr: &exec.ExitError{}}, primary, context.Canceled},
		{"cancel-nonzero", true, Result{ExitCode: 7}, nil, context.Canceled},
		{"start", false, Result{ExitCode: -1, signal: 9}, primary, primary},
		{"copy", false, Result{ExitCode: 7, waitErr: copyErr}, nil, copyErr},
		{"WaitDelay", false, Result{ExitCode: 0, waitErr: exec.ErrWaitDelay}, nil, exec.ErrWaitDelay},
		{"absent-status", false, Result{ExitCode: -1}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := connectionEngine(t, connectionMachine("first", "first", "one", true), connectionMachine("later", "later", "two", true))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.Context = ctx
			var out bytes.Buffer
			calls := 0
			err := e.runTestConnection(connectionInvocation(t, "-y"), nil, &out, testConnectionDeps{Run: func(context.Context, string, []string, []byte) (Result, error) {
				calls++
				if tc.canceled {
					cancel()
				}
				return tc.r, tc.err
			}})
			if err == nil || tc.want != nil && err != tc.want || calls != 1 || out.Len() != 0 {
				t.Fatal(err, calls, out.String())
			}
		})
	}
}

func TestTestConnectionTypedSpawnAndProcessMetadata(t *testing.T) {
	bash, bashErr := exec.LookPath("bash")
	if bashErr != nil {
		t.Fatal("declared Bash required for confined connection tests", bashErr)
	}
	bash, bashErr = filepath.Abs(bash)
	if bashErr != nil {
		t.Fatal(bashErr)
	}
	cat := []string{"cat", "/proc/uptime"}
	missing := &exec.Error{Name: "cat", Err: exec.ErrNotFound}
	_, err := testConnectionStatus(context.Background(), cat, Result{ExitCode: -1}, missing)
	if err == nil || err.Error() != "No such file or directory - cat" {
		t.Fatal(err)
	}
	for _, e := range []error{errors.New(missing.Error()), &exec.Error{Name: "ssh", Err: exec.ErrNotFound}, &exec.Error{Name: "cat", Err: exec.ErrDot}, &os.PathError{Op: "fork/exec", Path: "cat", Err: syscall.EACCES}} {
		if _, got := testConnectionStatus(context.Background(), cat, Result{ExitCode: -1}, e); got != e {
			t.Fatal("uncharacterized error remapped", got)
		}
	}
	if _, got := testConnectionStatus(context.Background(), []string{"ssh", "/proc/uptime"}, Result{}, missing); got != missing {
		t.Fatal(got)
	}
	for _, nonexec := range []bool{false, true} {
		root := t.TempDir()
		t.Setenv("PATH", root)
		if nonexec {
			if err := os.WriteFile(filepath.Join(root, "cat"), []byte("not executable"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		r, e := Process(context.Background(), root, cat, nil)
		_, e = testConnectionStatus(context.Background(), cat, r, e)
		if e == nil || e.Error() != "No such file or directory - cat" || r.waitErr != nil {
			t.Fatal(e, r)
		}
	}
	// Disposable declared Bash only; never host cat or real SSH.
	for _, tc := range []struct {
		script string
		exit   int
		signal syscall.Signal
	}{
		{"printf out; printf err >&2; exit 7", 7, 0}, {"printf out; printf err >&2; kill -TERM $$", -1, 15}, {"exit 0", 0, 0},
	} {
		r, e := Process(context.Background(), t.TempDir(), []string{bash, "--noprofile", "--norc", "-c", tc.script}, nil)
		if e != nil || r.ExitCode != tc.exit || r.signal != tc.signal || tc.exit != 0 && r.waitErr == nil || tc.exit == 0 && r.waitErr != nil {
			t.Fatal(e, r)
		}
		if tc.exit != 0 && (string(r.Stdout) != "out" || string(r.Stderr) != "err") {
			t.Fatal(r)
		}
		public, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(public, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 5 || fields["exit_code"] != float64(tc.exit) || fields["command"] != r.Command {
			t.Fatal(string(public))
		}
		copy := r
		copy.signal, copy.waitErr = 0, nil
		plain, _ := json.Marshal(copy)
		if !bytes.Equal(public, plain) {
			t.Fatal("private fields leaked", string(public))
		}
	}
}

func TestTestConnectionAdapterLogAndFullCapture(t *testing.T) {
	e := connectionEngine(t)
	if err := e.OpenLog("test-connection"); err != nil {
		t.Fatal(err)
	}
	defer e.CloseLog(false)
	bash, bashErr := exec.LookPath("bash")
	if bashErr != nil {
		t.Fatal("declared Bash required for confined connection tests", bashErr)
	}
	bash, bashErr = filepath.Abs(bash)
	if bashErr != nil {
		t.Fatal(bashErr)
	}
	r, err := e.testConnectionProcess(e.Context, e.Root, []string{bash, "--noprofile", "--norc", "-c", "printf 'inside\nlast\n'; printf stderr >&2; exit 7"}, nil)
	if err != nil || string(r.Stdout) != "inside\nlast\n" || string(r.Stderr) != "stderr" || r.ExitCode != 7 || r.waitErr == nil {
		t.Fatal(r, err)
	}
	b, err := os.ReadFile(e.Log.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Count(text, "] \tinside\nlast\n") != 1 || strings.Count(text, "] \tstderr\n") != 1 || strings.Count(text, "] Finished") != 1 || !strings.Contains(text, "exit status 7 (failed)") || strings.Index(text, "] Running") > strings.Index(text, "] \t") || strings.Index(text, "] \t") > strings.Index(text, "] Finished") {
		t.Fatal(text)
	}
	t.Setenv("PATH", t.TempDir())
	_, err = e.testConnectionProcess(e.Context, e.Root, []string{"cat", "/proc/uptime"}, nil)
	if err == nil || err.Error() != "No such file or directory - cat" {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(e.Log.Name())
	if strings.Count(string(b), "] Finished") != 1 || !strings.HasSuffix(string(b), "Running cat /proc/uptime\n") {
		t.Fatal("spawn emitted Finished", string(b))
	}
}
