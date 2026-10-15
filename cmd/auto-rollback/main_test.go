package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only the test binary takes this route. No test invokes the production helper
// entrypoint or reads the host's /run/current-system.
func TestMain(m *testing.M) {
	if os.Getenv("AUTO_ROLLBACK_TEST_CHILD") == "1" {
		os.Exit(fakeSwitch())
	}
	os.Exit(m.Run())
}

type model struct {
	deps                            dependencies
	out, stderr                     bytes.Buffer
	state                           []byte
	writes                          []string
	events                          []string
	argv                            [][]string
	reads                           [][]byte
	times                           []time.Time
	readlinkErr, readErr, unlinkErr error
	writeErrAt                      int
	startErrors, waitErrors         []error
	early                           bool
}

func newModel(t *testing.T) *model {
	t.Helper()
	m := &model{reads: [][]byte{[]byte("switched"), []byte("confirmed\n")}, times: []time.Time{time.Unix(100, 0), time.Unix(160, 0)}}
	m.deps = dependencies{
		out: &m.out,
		readlink: func(path string) (string, error) {
			if path != "/run/current-system" {
				t.Fatalf("readlink path %q", path)
			}
			m.events = append(m.events, "readlink")
			return "../fixture/store/old", m.readlinkErr
		},
		write: func(path string, data []byte) error {
			m.events = append(m.events, "write:"+string(data))
			m.writes = append(m.writes, string(data))
			if m.writeErrAt == len(m.writes) {
				return syscall.EACCES
			}
			m.state = append([]byte(nil), data...)
			return nil
		},
		read: func(path string) ([]byte, error) {
			m.events = append(m.events, "read")
			if m.readErr != nil {
				return nil, m.readErr
			}
			if len(m.reads) == 0 {
				t.Fatal("unbounded confirmation read")
				return nil, io.EOF
			}
			data := m.reads[0]
			m.reads = m.reads[1:]
			m.state = append([]byte(nil), data...)
			return data, nil
		},
		unlink: func(path string) error {
			m.events = append(m.events, "unlink")
			if m.unlinkErr == nil {
				m.state = nil
			}
			return m.unlinkErr
		},
		now: func() time.Time {
			m.events = append(m.events, "now")
			if len(m.times) == 0 {
				t.Fatal("unbounded wall-clock read")
				return time.Time{}
			}
			now := m.times[0]
			m.times = m.times[1:]
			return now
		},
		sleep: func(d time.Duration) {
			if d != 500*time.Millisecond {
				t.Fatalf("poll sleep %s", d)
			}
			m.events = append(m.events, "sleep")
		},
		start: func(argv []string) (func() error, error) {
			m.events = append(m.events, "start")
			i := len(m.argv)
			m.argv = append(m.argv, append([]string(nil), argv...))
			if i < len(m.startErrors) && m.startErrors[i] != nil {
				return nil, m.startErrors[i]
			}
			if m.early {
				m.state = []byte("confirmed\n")
			}
			return func() error {
				m.events = append(m.events, "wait")
				if i < len(m.waitErrors) {
					return m.waitErrors[i]
				}
				return nil
			}, nil
		},
	}
	return m
}

func TestAutoRollbackParser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		ordered bool
		value   string
		err     string
	}{
		{"default", []string{"new", "switch", "check"}, false, "60", ""},
		{"repeat-last", []string{"-t", "7", "new", "--timeout=-1", "switch", "check"}, false, "-1", ""},
		{"long-abbreviation", []string{"--ti", "0xF_F", "new", "switch", "check"}, false, "255", ""},
		{"long-case-fold", []string{"--TIMEOUT", "-1", "new", "switch", "check"}, false, "-1", ""},
		{"short-attached", []string{"-t-1", "new", "switch", "check"}, false, "-1", ""},
		{"octal", []string{"-t", "0_10", "new", "switch", "check"}, false, "8", ""},
		{"binary", []string{"-t+0b1_01", "new", "switch", "check"}, false, "5", ""},
		{"huge", []string{"-t", "999999999999999999999999999999999999999999", "new", "switch", "check"}, false, "999999999999999999999999999999999999999999", ""},
		{"ordered", []string{"-t", "0", "new", "switch", "check"}, true, "0", ""},
		{"order-stops", []string{"new", "-t", "1", "switch", "check"}, true, "", "Invalid arguments"},
		{"termination", []string{"-t", "+2", "--", "-new", "-action", "-check"}, false, "2", ""},
		{"empty-opaque-operands", []string{"", "fixture action '$()'", ""}, false, "60", ""},
		{"minus-operand", []string{"-", "switch", "check"}, false, "60", ""},
		{"bad-octal", []string{"-t", "09"}, false, "", "invalid timeout: 09"},
		{"short-equals", []string{"-t=2"}, false, "", "invalid timeout: =2"},
		{"float", []string{"--timeout=0.5"}, false, "", "invalid timeout: 0.5"},
		{"empty", []string{"--timeout="}, false, "", "invalid timeout: "},
		{"missing", []string{"-t"}, false, "", "missing argument: -t"},
		{"required-consumes-option", []string{"-t", "--help"}, false, "", "invalid timeout: --help"},
		{"unknown", []string{"--unknown"}, false, "", "invalid option: --unknown"},
		{"help-equals", []string{"--help=x"}, false, "", "needless argument: --help=x"},
		{"short-help-equals", []string{"-h=x"}, false, "", "needless argument: -h=x"},
		{"ambiguous", []string{"--*-completion"}, false, "", "ambiguous option: --*-completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, p, err := parse("/fixture/auto-rollback", tc.args, tc.ordered)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("error=%v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || p != nil || o.timeout.String() != tc.value {
				t.Fatalf("options=%+v presentation=%+v error=%v", o, p, err)
			}
			if tc.name == "termination" && (o.toplevel != "-new" || o.action != "-action" || o.check != "-check") {
				t.Fatalf("opaque operands: %+v", o)
			}
		})
	}
}

const literalHelp = "Usage: /fixture/auto-rollback.rb [options] <toplevel> <action> <check file>\n" +
	"    -t, --timeout TIMEOUT            Timeout in seconds\n" +
	"    -h, --help                       Print help message and exit\n"

func TestAutoRollbackPresentation(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		status   int
		out, err string
	}{
		{[]string{"--help"}, 0, literalHelp, ""},
		{[]string{"--he"}, 0, literalHelp, ""},
		{[]string{"-hignored", "--unknown"}, 0, literalHelp, ""},
		{[]string{"new", "switch"}, 1, "", "Invalid arguments\n" + literalHelp},
		{[]string{"new", "switch", "check", "extra"}, 1, "", "Invalid arguments\n" + literalHelp},
		{[]string{"-t", "oops"}, 1, "", "/fixture/auto-rollback.rb: invalid timeout: oops\n"},
		{[]string{"--unknown"}, 1, "", "/fixture/auto-rollback.rb: invalid option: --unknown\n"},
		{[]string{"--version"}, 1, "", "auto-rollback: version unknown\n"},
		{[]string{"-v"}, 1, "", "auto-rollback: version unknown\n"},
		{[]string{"--version=PKG"}, 1, "", "auto-rollback: version unknown\n"},
		{[]string{"--version="}, 1, "", "auto-rollback: version unknown\n"},
		{[]string{"--version=opaque, '$()'"}, 1, "", "auto-rollback: version unknown\n"},
		{[]string{"--*-completion-bash", "-"}, 0, "--timeout\n-t\n--help\n-h\n", ""},
		{[]string{"--*-completion-bash=--ti"}, 0, "--timeout\n", ""},
		{[]string{"--*-completion-bash=--TI"}, 0, "--timeout\n", ""},
		{[]string{"--*-completion-bash=-h"}, 0, "-h\n", ""},
		{[]string{"--*-completion-bash=-H"}, 0, "", ""},
		{[]string{"--*-completion-bash="}, 0, "", ""},
		{[]string{"--*-completion-bash"}, 1, "", "/fixture/auto-rollback.rb: missing argument: --*-completion-bash\n"},
		{[]string{"--*-completion-zsh=NAME:FILE"}, 0,
			"#compdef NAME:FILE\n\ntypeset -A opt_args\nlocal context state line\n\n_arguments -s -S \\\n" +
				"  \"-t[Timeout in seconds]\" \\\n  \"--timeout[Timeout in seconds]\" \\\n" +
				"  \"-h[Print help message and exit]\" \\\n  \"--help[Print help message and exit]\" \\\n" +
				"  '*:file:_files' && return 0\n", ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			m := newModel(t)
			status := execute("/fixture/auto-rollback.rb", tc.args, false, m.deps, &m.stderr)
			if status != tc.status || m.out.String() != "Deploying with auto-rollback\n"+tc.out || m.stderr.String() != tc.err {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, m.out.String(), m.stderr.String())
			}
			if len(m.events) != 0 {
				t.Fatalf("presentation had effects: %v", m.events)
			}
		})
	}
	for _, args := range [][]string{{"--*-completion-zsh"}, {"--*-completion-zsh="}, {"--*-completion-zsh=provided"}} {
		_, p, err := parse("/fixture/auto-rollback.rb", args, false)
		name := ""
		if args[0] == "--*-completion-zsh=provided" {
			name = "provided"
		}
		if err != nil || p == nil || p.stdout != zshCompletion(name) || !strings.HasPrefix(p.stdout, "#compdef "+name+"\n") {
			t.Fatalf("source-derived Zsh name args=%q p=%+v err=%v", args, p, err)
		}
	}
}

// These are the accepted inventory's twenty source nominations (two distinct
// AR17 rows, ending at AR19), not old captures or normalized parity evidence.
func TestAutoRollbackSourceNominations(t *testing.T) {
	for _, tc := range []struct {
		name             string
		args             []string
		tune             func(*model)
		status, children int
		state            string
	}{
		{"AR01-confirmed", nil, nil, 0, 1, ""},
		{"AR02-help", []string{"--help"}, nil, 0, 0, ""},
		{"AR03-short-arity", []string{"new", "switch"}, nil, 1, 0, ""},
		{"AR04-extra-arity", []string{"-t", "60", "new", "switch", "check", "extra"}, nil, 1, 0, ""},
		{"AR05-invalid-timeout", []string{"-t", "oops", "new", "switch", "check"}, nil, 1, 0, ""},
		{"AR06-unknown-option", []string{"--unknown", "new", "switch", "check"}, nil, 1, 0, ""},
		{"AR07-switch-child-nonzero", nil, func(m *model) { m.waitErrors = []error{&exec.ExitError{}} }, 0, 1, ""},
		{"AR08-no-confirmation", nil, timeoutModel, 1, 2, "switched"},
		{"AR09-rollback-child-nonzero", nil, func(m *model) { timeoutModel(m); m.waitErrors = []error{nil, &exec.ExitError{}} }, 1, 2, "switched"},
		{"AR10-confirm-file-missing", nil, func(m *model) { m.readErr = syscall.ENOENT }, 1, 1, "switched"},
		{"AR11-confirm-unlink-error", nil, func(m *model) { m.unlinkErr = syscall.EACCES }, 1, 1, "confirmed\n"},
		{"AR12-current-system-not-symlink", nil, func(m *model) { m.readlinkErr = syscall.EINVAL }, 1, 0, ""},
		{"AR13-current-system-relative", nil, timeoutModel, 1, 2, "switched"},
		{"AR14-early-confirmation", nil, func(m *model) { m.early = true }, 0, 1, ""},
		{"AR15-spawn-error", nil, func(m *model) { m.startErrors = []error{syscall.ENOENT} }, 1, 1, "switching"},
		{"AR16-check-write-error", nil, func(m *model) { m.writeErrAt = 1 }, 1, 0, ""},
		{"AR17-timeout-0", []string{"-t", "0", "new", "switch", "check"}, timeoutModel, 1, 2, "switched"},
		{"AR17-timeout--1", []string{"-t", "-1", "new", "switch", "check"}, timeoutModel, 1, 2, "switched"},
		{"AR18-unvalidated-action", []string{"new", "fixture-action", "check"}, timeoutModel, 1, 2, "switched"},
		{"AR19-parser-env", nil, nil, 0, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(t)
			if tc.tune != nil {
				tc.tune(m)
			}
			args := tc.args
			if args == nil {
				args = []string{"-t", "60", "new", "switch", "check"}
			}
			status := execute("auto-rollback", args, tc.name == "AR19-parser-env", m.deps, &m.stderr)
			if status != tc.status || len(m.argv) != tc.children || string(m.state) != tc.state {
				t.Fatalf("status=%d children=%v state=%q stderr=%q", status, m.argv, m.state, m.stderr.String())
			}
			if tc.children > 0 {
				if m.argv[0][0] != "new/bin/switch-to-configuration" {
					t.Fatalf("activation argv %q", m.argv[0])
				}
				if !reflect.DeepEqual(m.writes[:1], []string{"switching"}) {
					t.Fatalf("writes %q", m.writes)
				}
			}
			if tc.children == 2 {
				if m.argv[1][0] != "../fixture/store/old/bin/switch-to-configuration" || m.argv[1][1] != m.argv[0][1] {
					t.Fatalf("rollback argv %q", m.argv)
				}
				if !strings.HasSuffix(m.out.String(), "Timeout occurred, rolling back\nRollback complete\n") {
					t.Fatalf("rollback output %q", m.out.String())
				}
			}
		})
	}
}

func timeoutModel(m *model) {
	m.reads = [][]byte{[]byte("switched"), []byte("switched")}
	m.times = []time.Time{time.Unix(100, 0), time.Unix(160, 0), time.Unix(160, 500_000_000)}
}

func TestAutoRollbackOrderAndOutput(t *testing.T) {
	m := newModel(t)
	status := execute("auto-rollback", []string{"new", "switch", "check"}, false, m.deps, &m.stderr)
	wantEvents := []string{"readlink", "write:switching", "start", "wait", "write:switched", "now", "sleep", "read", "now", "sleep", "read", "unlink"}
	if status != 0 || !reflect.DeepEqual(m.events, wantEvents) {
		t.Fatalf("status=%d events=%v", status, m.events)
	}
	want := "Deploying with auto-rollback\n  current system = ../fixture/store/old\n  new system     = new\n  action         = switch\n  check file     = check\n  timeout        = 60 seconds\n\nSwitching to new configuration\nSwitch complete, waiting for confirmation\nConfiguration confirmed\n"
	if m.out.String() != want || m.stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", m.out.String(), m.stderr.String())
	}
	if !reflect.DeepEqual(m.writes, []string{"switching", "switched"}) {
		t.Fatalf("state bytes %q", m.writes)
	}
}

func TestAutoRollbackClockAndConfirmation(t *testing.T) {
	start := time.Unix(100, 123)
	for _, tc := range []struct {
		now     time.Time
		seconds string
		want    bool
	}{
		{time.Unix(160, 123), "60", false}, {time.Unix(160, 124), "60", true},
		{time.Unix(100, 123), "0", false}, {time.Unix(100, 124), "0", true},
		{time.Unix(100, 123), "-1", true}, {time.Unix(99, 123), "-1", false},
		{time.Unix(90, 0), "0", false},
		{time.Unix(1<<62, 0), "999999999999999999999999999999999999", false},
		{time.Unix(-1<<62, 0), "-999999999999999999999999999999999999", true},
	} {
		n, _ := new(big.Int).SetString(tc.seconds, 10)
		before := n.String()
		if expired(start, tc.now, n) != tc.want || n.String() != before {
			t.Fatalf("expiry now=%v seconds=%s", tc.now, tc.seconds)
		}
	}
	for _, timeout := range []string{"60", "0", "-1"} {
		t.Run("confirmation-wins-"+timeout, func(t *testing.T) {
			m := newModel(t)
			m.reads = [][]byte{[]byte("\x00\t\n\v\f\r confirmed \r\n\x00")}
			m.times = []time.Time{start}
			if execute("auto-rollback", []string{"-t", timeout, "new", "switch", "check"}, false, m.deps, &m.stderr) != 0 {
				t.Fatal(m.stderr.String())
			}
			// No expiry-clock read is allowed after a confirmed read.
			if len(m.times) != 0 || len(m.argv) != 1 {
				t.Fatalf("unexpected expiry/rollback %v", m.events)
			}
		})
	}
	for _, invalid := range []string{"\u00a0confirmed\u00a0", "CONFIRMED", "confirmed\xff"} {
		m := newModel(t)
		m.reads = [][]byte{[]byte(invalid)}
		m.times = []time.Time{start, start.Add(time.Second)}
		if execute("auto-rollback", []string{"-t", "0", "new", "switch", "check"}, false, m.deps, &m.stderr) != 1 || len(m.argv) != 2 {
			t.Fatalf("incorrect strip acceptance %q", invalid)
		}
	}
}

func TestAutoRollbackFailureBoundaries(t *testing.T) {
	sentinel := errors.New("synthetic wait-system failure")
	for _, tc := range []struct {
		name  string
		tune  func(*model)
		state string
		waits int
	}{
		{"first-start", func(m *model) { m.startErrors = []error{sentinel} }, "switching", 0},
		{"first-wait", func(m *model) { m.waitErrors = []error{sentinel} }, "switching", 1},
		{"second-write", func(m *model) { m.writeErrAt = 2 }, "switching", 1},
		{"second-start", func(m *model) { timeoutModel(m); m.startErrors = []error{nil, sentinel} }, "switched", 1},
		{"second-wait", func(m *model) { timeoutModel(m); m.waitErrors = []error{nil, sentinel} }, "switched", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(t)
			tc.tune(m)
			o, _, err := parse("auto-rollback", []string{"new", "switch", "check"}, false)
			if err != nil {
				t.Fatal(err)
			}
			status, err := run(o, m.deps)
			count := 0
			for _, event := range m.events {
				if event == "wait" {
					count++
				}
				if event == "unlink" {
					t.Fatal("failure cleanup")
				}
			}
			if status != 1 || err == nil || string(m.state) != tc.state || count != tc.waits {
				t.Fatalf("status=%d error=%v state=%q events=%v", status, err, m.state, m.events)
			}
		})
	}
	// Synthetic ExitError records exercise the classifier without fabricating
	// ProcessState/WaitStatus or claiming a native signal observation.
	m := newModel(t)
	m.waitErrors = []error{&exec.ExitError{Stderr: []byte("synthetic signal nomination; raw status unavailable")}}
	if execute("auto-rollback", []string{"new", "switch", "check"}, false, m.deps, &m.stderr) != 0 {
		t.Fatal(m.stderr.String())
	}
	for _, tc := range []struct{ in, want string }{
		{"./new/../current/", "./new/../current/bin/switch-to-configuration"},
		{"../old//system", "../old//system/bin/switch-to-configuration"},
		{"/", "/bin/switch-to-configuration"}, {"", "/bin/switch-to-configuration"},
	} {
		if got := switchPath(tc.in); got != tc.want {
			t.Fatalf("literal join %q=%q", tc.in, got)
		}
	}
}

type switchRecord struct {
	Argv       []string
	CWD, Value string
	Input      []byte
	State      string
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write(data []byte) (int, error) {
	return 0, w.err
}

func TestAutoRollbackFilesystemAndOutput(t *testing.T) {
	for _, err := range []error{nil, io.ErrClosedPipe} {
		m := newModel(t)
		m.deps.out = failingWriter{err: err}
		if execute("auto-rollback", []string{"new", "switch", "check"}, false, m.deps, &m.stderr) != 1 || len(m.events) != 0 {
			t.Fatalf("banner failure had effects: %v", m.events)
		}
		want := io.ErrShortWrite
		if err != nil {
			want = err
		}
		if m.stderr.String() != "auto-rollback: "+want.Error()+"\n" {
			t.Fatalf("output error %q", m.stderr.String())
		}
	}
	root := t.TempDir()
	link := filepath.Join(root, "current-system")
	if err := os.Symlink("../fixture/store/old", link); err != nil {
		t.Fatal(err)
	}
	d := productionDependencies()
	if got, err := d.readlink(link); err != nil || got != "../fixture/store/old" {
		t.Fatalf("literal fixture readlink %q %v", got, err)
	}
	check := filepath.Join(root, "check")
	if err := os.WriteFile(check, []byte("old longer content"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := d.write(check, []byte("switching")); err != nil {
		t.Fatal(err)
	}
	data, err := d.read(check)
	if err != nil || string(data) != "switching" {
		t.Fatalf("truncated bytes %q %v", data, err)
	}
	info, err := os.Stat(check)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("existing mode %v %v", info, err)
	}
	if err := d.unlink(check); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(check); !os.IsNotExist(err) {
		t.Fatalf("unlink %v", err)
	}
	dir := filepath.Join(root, "directory")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.unlink(dir); err == nil {
		t.Fatal("File.unlink must not remove a directory")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("directory lost %v", err)
	}
}

func fakeSwitch() int {
	root := os.Getenv("AUTO_ROLLBACK_TEST_ROOT")
	name := filepath.Base(filepath.Dir(filepath.Dir(os.Args[0])))
	record := switchRecord{Argv: os.Args, CWD: "", Value: os.Getenv("AUTO_ROLLBACK_TEST_VALUE")}
	record.CWD, _ = os.Getwd()
	if name == "new" {
		record.Input, _ = io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(filepath.Join(root, "check"))
	if err != nil {
		return 90
	}
	record.State = string(data)
	encoded, _ := json.Marshal(record)
	if os.WriteFile(filepath.Join(root, name+".json"), encoded, 0600) != nil {
		return 91
	}
	effects, err := os.OpenFile(filepath.Join(root, "effects"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 92
	}
	_, err = effects.WriteString(name + "\n")
	closeErr := effects.Close()
	if err != nil || closeErr != nil {
		return 93
	}
	os.Stdout.Write([]byte(name + ":stdout\x00\xff\n"))
	os.Stderr.Write([]byte(name + ":stderr\x00\xfe\n"))
	if name == "new" {
		if os.WriteFile(filepath.Join(root, "check"), []byte("confirmed\n"), 0600) != nil {
			return 94
		}
		return 7
	}
	return 23
}

func TestAutoRollbackInheritedChildren(t *testing.T) {
	for _, scenario := range []string{"confirmed", "timeout", "missing-new", "nonexec-new", "missing-old"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"new", "old"} {
				bin := filepath.Join(root, name, "bin")
				if err := os.MkdirAll(bin, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(bin, "switch-to-configuration")
				if scenario == "missing-new" && name == "new" || scenario == "missing-old" && name == "old" {
					continue
				}
				if scenario == "nonexec-new" && name == "new" {
					err = os.WriteFile(path, []byte("not executable\n"), 0600)
				} else {
					err = os.Symlink(executable, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("AUTO_ROLLBACK_TEST_CHILD", "1")
			t.Setenv("AUTO_ROLLBACK_TEST_ROOT", root)
			t.Setenv("AUTO_ROLLBACK_TEST_VALUE", "literal env '$()'")
			input := []byte("literal stdin\x00\xff\n")
			if err := os.WriteFile(filepath.Join(root, "stdin"), input, 0600); err != nil {
				t.Fatal(err)
			}
			in, err := os.Open(filepath.Join(root, "stdin"))
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			out, err := os.Create(filepath.Join(root, "stdout"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			stderr, err := os.Create(filepath.Join(root, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
			os.Stdin, os.Stdout, os.Stderr = in, out, stderr
			defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			check := filepath.Join(root, "check")
			newTop := filepath.Join(root, "new")
			oldTop, err := filepath.Rel(cwd, filepath.Join(root, "old"))
			if err != nil {
				t.Fatal(err)
			}
			d := productionDependencies()
			d.readlink = func(path string) (string, error) {
				if path != "/run/current-system" {
					t.Fatalf("readlink=%s", path)
				}
				return oldTop, nil
			}
			clock := time.Unix(200, 0)
			d.now = func() time.Time { return clock }
			d.sleep = func(duration time.Duration) {
				if duration != 500*time.Millisecond {
					t.Fatalf("sleep %s", duration)
				}
				clock = clock.Add(duration)
				data, err := os.ReadFile(check)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "switched" {
					t.Fatalf("early confirmation not overwritten: %q", data)
				}
				if scenario == "confirmed" {
					if err := os.WriteFile(check, []byte("confirmed\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			starts, waits := 0, 0
			d.start = func(argv []string) (func() error, error) {
				starts++
				wait, err := inheritedStart(argv)
				if err != nil {
					return nil, err
				}
				return func() error { waits++; return wait() }, nil
			}
			action := "fixture action '$()'; --literal"
			status := execute("auto-rollback", []string{"-t", "0", newTop, action, check}, false, d, stderr)
			wantStatus := 1
			wantStarts, wantWaits := 2, 2
			wantEffects := "new\nold\n"
			wantState := "switched"
			switch scenario {
			case "confirmed":
				wantStatus = 0
				wantStarts, wantWaits = 1, 1
				wantEffects = "new\n"
				wantState = ""
			case "missing-new", "nonexec-new":
				wantStarts, wantWaits = 1, 0
				wantEffects = ""
				wantState = "switching"
			case "missing-old":
				wantWaits = 1
				wantEffects = "new\n"
			}
			if status != wantStatus || starts != wantStarts || waits != wantWaits {
				t.Fatalf("status=%d start=%d wait=%d", status, starts, waits)
			}
			data, err := os.ReadFile(check)
			if wantState == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("confirmed check retained err=%v", err)
				}
			} else if err != nil || string(data) != wantState {
				t.Fatalf("check=%q err=%v", data, err)
			}
			effects, _ := os.ReadFile(filepath.Join(root, "effects"))
			if string(effects) != wantEffects {
				t.Fatalf("effects=%q", effects)
			}
			for _, name := range strings.Fields(wantEffects) {
				var record switchRecord
				data, err := os.ReadFile(filepath.Join(root, name+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				path := switchPath(newTop)
				state := "switching"
				if name == "old" {
					path = switchPath(oldTop)
					state = "switched"
				}
				if !reflect.DeepEqual(record.Argv, []string{path, action}) || record.CWD != cwd || record.Value != "literal env '$()'" || record.State != state {
					t.Fatalf("record=%+v", record)
				}
				if name == "new" && !bytes.Equal(record.Input, input) {
					t.Fatalf("stdin=%q", record.Input)
				}
			}
			stdoutBytes, _ := os.ReadFile(filepath.Join(root, "stdout"))
			stderrBytes, _ := os.ReadFile(filepath.Join(root, "stderr"))
			for _, name := range strings.Fields(wantEffects) {
				if !bytes.Contains(stdoutBytes, []byte(name+":stdout\x00\xff\n")) || bytes.Contains(stdoutBytes, []byte(name+":stderr")) || !bytes.Contains(stderrBytes, []byte(name+":stderr\x00\xfe\n")) {
					t.Fatalf("stream inheritance stdout=%q stderr=%q", stdoutBytes, stderrBytes)
				}
			}
		})
	}
}
