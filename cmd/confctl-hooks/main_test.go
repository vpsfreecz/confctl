package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

func TestPreparedInputFiniteArgumentsAndEnvironment(t *testing.T) {
	env := map[string]string{"CONFCTL_HOOK_CONTEXT": "/private/context", "CONFCTL_HOOK_CONTEXT_SHA256": strings.Repeat("ab", 32), "CONFCTL_HOOK_MESSAGE": ""}
	lookup := func(key string) (string, bool) { value, ok := env[key]; return value, ok }
	got, err := preparedInput([]string{"pre-commit"}, lookup)
	if err != nil || got.ContextPath != "/private/context" || got.SHA256[0] != 0xab {
		t.Fatal(got, err)
	}
	env["CONFCTL_HOOK_MESSAGE"] = "/private/message"
	if _, err := preparedInput([]string{"commit-msg"}, lookup); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"wrong"}, {"pre-commit", "extra"}} {
		if _, err := preparedInput(args, lookup); err == nil {
			t.Fatal("accepted args", args)
		}
	}
	for _, digest := range []string{"", strings.Repeat("AB", 32), strings.Repeat("ab", 31), strings.Repeat("gg", 32)} {
		env["CONFCTL_HOOK_CONTEXT_SHA256"] = digest
		if _, err := preparedInput([]string{"commit-msg"}, lookup); err == nil {
			t.Fatal("accepted digest", digest)
		}
	}
	env["CONFCTL_HOOK_CONTEXT_SHA256"] = strings.Repeat("ab", 32)
	delete(env, "CONFCTL_HOOK_MESSAGE")
	if _, err := preparedInput([]string{"pre-commit"}, lookup); err == nil {
		t.Fatal("accepted missing environment")
	}
}

func TestCommentPrefixConfigOrderAndLiteralAuto(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings []string
		want     string
	}{
		{"absent", nil, "#"}, {"default", []string{"commentChar", "#"}, "#"},
		{"custom", []string{"commentChar", ";"}, ";"}, {"multiple", []string{"commentString", "//"}, "//"},
		{"char-last", []string{"commentString", "//", "commentChar", ";"}, ";"},
		{"string-last", []string{"commentChar", ";", "commentString", "//"}, "//"},
		{"auto", []string{"commentChar", "auto"}, "auto"}, {"empty", []string{"commentString", ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := installerFixture(t)
			for i := 0; i < len(tc.settings); i += 2 {
				runGit(t, q, "config", "--add", "core."+tc.settings[i], tc.settings[i+1])
			}
			got, err := commentPrefix(context.Background(), q)
			if tc.want == "" {
				if err == nil {
					t.Fatal("empty prefix accepted")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
			message := filepath.Join(t.TempDir(), "message")
			if err := os.WriteFile(message, []byte(got+" comment\n"), 0600); err != nil {
				t.Fatal(err)
			}
			findings, err := hookcheck.CheckMessage(message, got)
			if err != nil || !hookcheck.HasErrors(findings) {
				t.Fatal("configured comments were treated as message", findings, err)
			}
			if tc.name == "auto" {
				if err := os.WriteFile(message, []byte("# remains message\n"), 0600); err != nil {
					t.Fatal(err)
				}
				findings, err := hookcheck.CheckMessage(message, got)
				if err != nil || hookcheck.HasErrors(findings) {
					t.Fatal("auto guessed #", findings, err)
				}
			}
		})
	}
}

func TestCommentPrefixReadFailureAndInheritedOrder(t *testing.T) {
	q, _ := installerFixture(t)
	include := filepath.Join(t.TempDir(), "included config")
	if err := os.WriteFile(include, []byte("[core]\ncommentString = //\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, q, "config", "include.path", include)
	runGit(t, q, "config", "core.commentChar", ";")
	got, err := commentPrefix(context.Background(), q)
	if err != nil || got != "//" {
		t.Fatal(got, err)
	}
	// Git updates the earlier core section; the later include still wins.
	config := filepath.Join(q.Capture.Dir, ".git", "config")
	f, err := os.OpenFile(config, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[core]\ncommentChar = !\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err = commentPrefix(context.Background(), q)
	if err != nil || got != "!" {
		t.Fatal(got, err)
	}
	sentinel := errors.New("read failure")
	q.Capture.Invoke = func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
		return hookcheck.ToolResult{}, sentinel
	}
	if _, err := commentPrefix(context.Background(), q); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	q.Capture.Invoke = func(context.Context, hookcheck.Invocation) (hookcheck.ToolResult, error) {
		return hookcheck.ToolResult{ExitCode: 2}, nil
	}
	if _, err := commentPrefix(context.Background(), q); err == nil {
		t.Fatal("config status ignored")
	}
}

func TestCommandUsageAndPreparedFailureStatuses(t *testing.T) {
	for _, args := range [][]string{nil, {"--unknown"}, {"check-prepared"}, {"check-prepared", "pre-commit", "extra"}, {"--tools", "/missing", "run"}} {
		var out bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &out, &out); code == 0 {
			t.Fatal("invalid invocation succeeded", args, out.String())
		}
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &out); code != 0 {
		t.Fatal(code)
	}
}
