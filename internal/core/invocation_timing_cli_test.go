package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInvocationTimingFooterSplitterPreservesBodyAndRejectsMalformedSuffix(t *testing.T) {
	const footer = "\nTiming: failed (exit 7)\n  Started: 2026-10-18T09:00:00+02:00\n  Ended: 2026-10-18T09:00:10+02:00\n  Evaluation: 1.000s\n  Build: 2.000s\n  Prompt wait: 0.012s\n  Other: 6.988s\n  Total: 10.000s\n"
	const body = "error: previous bytes\nTiming: an unrelated diagnostic\nmore detail without final newline"
	gotBody, gotFooter, err := splitTimingFooter(body + footer)
	if err != nil || gotBody != body || gotFooter != footer {
		t.Fatal(gotBody, gotFooter, err)
	}
	for _, plain := range []string{"", "ordinary Timing: text\n", "error: previous bytes\n"} {
		if body, block, err := splitTimingFooter(plain); err != nil || body != plain || block != "" {
			t.Fatal(body, block, err)
		}
	}
	for _, bad := range []string{
		strings.TrimSuffix(footer, "\n"), footer + "\n", footer + footer,
		strings.Replace(footer, "failed (exit 7)", "failed (exit 0)", 1),
		strings.Replace(footer, "failed (exit 7)", "completed (exit 7)", 1),
		strings.Replace(footer, "+02:00", "not-a-timestamp", 1),
		strings.Replace(footer, "1.000s", "-1.000s", 1),
		strings.Replace(footer, "1.000s", "1s", 1),
		strings.Replace(footer, "  Evaluation: 1.000s\n  Build: 2.000s\n", "  Build: 2.000s\n  Evaluation: 1.000s\n", 1),
		strings.Replace(footer, "  Build: 2.000s\n", "  Evaluation: 2.000s\n", 1),
		strings.Replace(footer, "  Prompt wait: 0.012s\n", "", 1),
		strings.Replace(footer, "  Total: 10.000s\n", "", 1),
		strings.Replace(footer, "  Build:", "  Fetch:", 1),
	} {
		if _, _, err := splitTimingFooter(bad); err == nil {
			t.Fatal("malformed suffix accepted", bad)
		}
	}
}

func assertInvocationTimingFooter(t *testing.T, r connectionCLIResult, state string) {
	t.Helper()
	if r.rawStderr != r.stderr+r.timingFooter || !strings.HasPrefix(r.timingFooter, fmt.Sprintf("\nTiming: %s (exit %d)\n", state, r.code)) || strings.Count(r.timingFooter, "\nTiming: ") != 1 || strings.ContainsRune(r.timingFooter, '\x1b') {
		t.Fatal("missing/duplicate/mismatched raw footer", r)
	}
}

func TestInvocationTimingMainAdmittedFailuresAndExcludedCommands(t *testing.T) {
	for _, argv := range [][]string{{"build", "-y"}, {"status", "-y", "-gnone"}, {"deploy", "-y"}, {"health-check", "-y"}, {"ssh", "-y"}, {"test-connection", "-y"}, {"collect-garbage", "-y"}, {"generation", "rm", "-y"}, {"generation", "rotate", "-y"}} {
		r := actualCLIRawWithSetup(t, argv, "", nil)
		if r.code == 0 || r.out != "" || !strings.Contains(r.stderr, "has no flake.nix") {
			t.Fatal(argv, r)
		}
		assertInvocationTimingFooter(t, r, "failed")
	}
	for _, argv := range [][]string{{"--help"}, {"--version"}, {"build", "--help"}, {"build", "--unknown"}, {"ssh", "-y", "one"}, {"cssh", "-y", "one"}, {"ls"}, {"inputs", "ls"}, {"generation", "ls"}, {"diff"}, {"gen-data"}} {
		r := actualCLIRawWithSetup(t, argv, "", nil)
		if r.timingFooter != "" || r.rawStderr != r.stderr {
			t.Fatal("excluded command acquired timing", argv, r)
		}
	}
}

func TestInvocationTimingMainRedirectedSuccessAndPreservedErrorBody(t *testing.T) {
	m := connectionMachine("one", "one", "one.invalid", true)
	r := connectionCLI(t, []string{"test-connection", "-y"}, "", []Machine{m}, "exit 0\n", 0700, true, "")
	if r.code != 0 || r.stderr != "" || r.log != "" || r.out != "\nResult: 1 successful, 0 failed\n\nFailed machines:\n" {
		t.Fatal(r)
	}
	assertInvocationTimingFooter(t, r, "completed")
	if !strings.Contains(r.timingFooter, "  Evaluation:") || !strings.Contains(r.timingFooter, "  Build:") || !strings.Contains(r.timingFooter, "  Connection tests:") {
		t.Fatal("real work phases missing", r.timingFooter)
	}
	r = connectionCLI(t, []string{"test-connection", "one"}, "n\n", []Machine{m}, "exit 98\n", 0700, true, "")
	if r.code != 1 || !strings.HasSuffix(r.stderr, "error: Aborted\n") || strings.Contains(r.trace, "ssh\n") || !strings.Contains(r.timingFooter, "  Prompt wait:") {
		t.Fatal(r)
	}
	assertInvocationTimingFooter(t, r, "failed")
}

func TestInvocationTimingMainCanceledAndLocalTimestamp(t *testing.T) {
	t.Setenv("CONFCTL_STAGE1_CANCELED", "1")
	r := actualCLIRawWithSetup(t, []string{"test-connection", "-y"}, "", nil)
	assertInvocationTimingFooter(t, r, "canceled")
	var started, ended time.Time
	for _, line := range strings.Split(r.timingFooter, "\n") {
		if value, ok := strings.CutPrefix(line, "  Started: "); ok {
			started, _ = time.Parse(time.RFC3339, value)
		}
		if value, ok := strings.CutPrefix(line, "  Ended: "); ok {
			ended, _ = time.Parse(time.RFC3339, value)
		}
	}
	if started.IsZero() || ended.IsZero() || ended.Before(started) {
		t.Fatal(r.timingFooter)
	}
}

func TestInvocationTimingMainExitPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		argv                          []string
		prior, final                  int
		signaled, canceled, nilPolicy bool
		state                         string
		cleanup                       bool
	}{
		{name: "normal-signal", argv: []string{"test-connection", "-y"}, prior: 1, final: 1, signaled: true, state: "canceled"},
		{name: "prototype-SIGINT", argv: []string{"test-connection", "-y"}, prior: 1, final: 130, signaled: true, state: "canceled"},
		{name: "prototype-SIGTERM", argv: []string{"test-connection", "-y"}, prior: 1, final: 143, signaled: true, state: "canceled"},
		{name: "ordinary-130", argv: []string{"test-connection", "-y"}, prior: 1, final: 130, state: "failed"},
		{name: "ordinary-143", argv: []string{"test-connection", "-y"}, prior: 1, final: 143, state: "failed"},
		{name: "context-canceled", argv: []string{"test-connection", "-y"}, prior: 1, final: 7, canceled: true, state: "canceled"},
		{name: "nil-policy", argv: []string{"test-connection", "-y"}, prior: 1, final: 1, nilPolicy: true, state: "failed"},
		{name: "early-version", argv: []string{"--version"}, prior: 1, final: 143, signaled: true},
		{name: "excluded-parse-error", argv: []string{"build", "--unknown"}, prior: 64, final: 130, signaled: true},
		{name: "successful-cleanup", argv: []string{"init"}, prior: 0, final: 143, signaled: true, cleanup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "configuration")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			for _, key := range []string{"CONFCTL_EXTENSION_REGISTRY", "CONFCTL_EXTENSION_ROOT"} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			stdout, err := os.Create(filepath.Join(base, "stdout"))
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			stderr, err := os.Create(filepath.Join(base, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			lateStderr, err := os.Create(filepath.Join(base, "late-stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer lateStderr.Close()
			originalStdout, originalStderr := os.Stdout, os.Stderr
			os.Stdout, os.Stderr = stdout, stderr
			defer func() { os.Stdout, os.Stderr = originalStdout, originalStderr }()

			calls := 0
			resolve := func(code int) (int, bool) {
				calls++
				if code != tc.prior {
					t.Errorf("policy received %d, want post-cleanup status %d", code, tc.prior)
				}
				if tc.cleanup {
					if _, err := os.Stat(filepath.Join(root, "flake.nix")); err != nil {
						t.Error("init did not finish", err)
					}
					logs, err := os.ReadDir(filepath.Join(root, ".confctl", "logs"))
					if err != nil || len(logs) != 0 {
						t.Error("policy preceded successful log cleanup", logs, err)
					}
				}
				before, err := os.ReadFile(stderr.Name())
				if err != nil || strings.Contains(string(before), "\nTiming: ") {
					t.Error("timing report preceded status policy", string(before), err)
				}
				if _, err := stderr.WriteString("exit policy resolved\n"); err != nil {
					t.Error(err)
				}
				// The footer must still use the stderr captured at Main entry.
				os.Stderr = lateStderr
				return tc.final, tc.signaled
			}
			if tc.nilPolicy {
				resolve = nil
			}
			ctx := context.Background()
			if tc.canceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			code := MainWithExitPolicy(ctx, tc.argv, resolve)
			wantCalls := 1
			if tc.nilPolicy {
				wantCalls = 0
			}
			if code != tc.final || calls != wantCalls {
				t.Fatal("wrong final status/resolver count", code, calls, tc.final, wantCalls)
			}
			raw, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			body, footer, err := splitTimingFooter(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.nilPolicy && !strings.HasSuffix(body, "exit policy resolved\n") {
				t.Fatal("pre-report stderr body changed", body)
			}
			if tc.state == "" {
				if footer != "" {
					t.Fatal("excluded command gained a footer", footer)
				}
			} else {
				assertInvocationTimingFooter(t, connectionCLIResult{code: code, stderr: body, rawStderr: string(raw), timingFooter: footer}, tc.state)
			}
			if late, err := os.ReadFile(lateStderr.Name()); err != nil || len(late) != 0 {
				t.Fatal("footer used replacement stderr", string(late), err)
			}
			if out, err := os.ReadFile(stdout.Name()); err != nil || tc.state != "" && len(out) != 0 {
				t.Fatal("operational failure stdout changed", string(out), err)
			}
		})
	}
}
