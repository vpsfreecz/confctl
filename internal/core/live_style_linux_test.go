package core

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestLiveTextColorPolicyUsesActualDestination(t *testing.T) {
	_, tty := openCommitTestPTY(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for _, tc := range []struct {
		mode, noColor string
		out           io.Writer
		want          bool
	}{
		{"always", "1", io.Discard, true}, {"never", "", tty, false}, {"auto", "", tty, true}, {"auto", "1", tty, false}, {"auto", "", w, false}, {"", "", tty, false},
	} {
		t.Setenv("NO_COLOR", tc.noColor)
		e := &Engine{Color: tc.mode}
		text := e.liveText(tc.out, "node\x1b[35mhidden\x1b[0m", "yellow")
		if strings.Contains(text, "\x1b[") != tc.want || strings.Contains(text, "35m") {
			t.Fatal(tc.mode, tc.noColor, text)
		}
		if tc.want && text != "\x1b[33mnodehidden\x1b[0m" {
			t.Fatal(text)
		}
	}
}

func TestLiveHeaderKeepsInvocationThroughEngineCopies(t *testing.T) {
	e := &Engine{liveCommand: "confctl status -y '*'"}
	copy := *e
	header := copy.liveHeader("Build group", "2/3 (8 machines)")
	if header.Command != e.liveCommand || header.Operation != "Build group" || header.Detail != "2/3 (8 machines)" || header.LogPath != "" {
		t.Fatal(header)
	}
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	copy.Log = f
	if h := copy.liveHeader("SSH", "cat"); h.Command != e.liveCommand || h.LogPath != f.Name() {
		t.Fatal(h)
	}
	if h := (&Engine{}).liveHeader("Copy", ""); h.Command != "" {
		t.Fatal("invented command", h)
	}
}
