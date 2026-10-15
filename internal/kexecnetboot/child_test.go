package kexecnetboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestFakeKexecChild(t *testing.T) {
	mode := os.Getenv("CONFCTL_KEXEC_CHILD")
	if mode == "" {
		return
	}
	if mode == "wait" {
		_, _ = os.Stdout.Write([]byte("ready\n"))
		for {
			var b [1]byte
			if _, err := os.Stdin.Read(b[:]); err != nil {
				os.Exit(30)
			}
		}
	}
	if mode == "signal" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		os.Exit(31)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(32)
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(33)
	}
	b, err := json.Marshal(map[string]any{"argv": os.Args[1:], "stdin": input, "cwd": cwd, "env": os.Getenv("CONFCTL_KEXEC_LITERAL")})
	if err != nil {
		os.Exit(34)
	}
	_, _ = os.Stdout.Write(b)
	_, _ = os.Stderr.Write([]byte("child stderr\x00\n"))
	if mode == "exit23" {
		os.Exit(23)
	}
	os.Exit(0)
}

func TestDirectFakeChildLiteralDescriptorsExitSignal(t *testing.T) {
	root := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	t.Setenv("CONFCTL_KEXEC_CHILD", "normal")
	t.Setenv("CONFCTL_KEXEC_LITERAL", "literal $() ; '\"")
	in, err := os.CreateTemp(root, "in")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_, _ = in.Write([]byte("input\x00\n"))
	_, _ = in.Seek(0, 0)
	out, err := os.CreateTemp(root, "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stderr, err := os.CreateTemp(root, "err")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = in, out, stderr
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()
	argv := []string{bin, "-test.run=^TestFakeKexecChild$", "--", "--load", "/private/kernel", "--initrd=/private/initrd", "--append=literal $() ; '\" ", "--no-sync"}
	if err := directChild(context.Background(), argv); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Argv  []string `json:"argv"`
		Stdin []byte   `json:"stdin"`
		CWD   string   `json:"cwd"`
		Env   string   `json:"env"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Argv, argv[1:]) || !bytes.Equal(got.Stdin, []byte("input\x00\n")) || got.CWD != root || got.Env != "literal $() ; '\"" {
		t.Fatal(got)
	}
	b, err = os.ReadFile(stderr.Name())
	if err != nil || string(b) != "child stderr\x00\n" {
		t.Fatal(string(b), err)
	}
	t.Setenv("CONFCTL_KEXEC_CHILD", "exit23")
	err = directChild(context.Background(), argv)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatal("nonzero state lost", err)
	}
	t.Setenv("CONFCTL_KEXEC_CHILD", "signal")
	err = directChild(context.Background(), argv)
	if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatal("signal state lost", err)
	}
	err = directChild(context.Background(), []string{filepath.Join(root, "absent")})
	if err == nil || !strings.HasPrefix(err.Error(), "start kexec:") {
		t.Fatal(err)
	}
}

func TestOwnedFakeChildCancellationJoinsWaiter(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFCTL_KEXEC_CHILD", "wait")
	readIn, writeIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readIn.Close()
	defer writeIn.Close()
	readOut, writeOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readOut.Close()
	defer writeOut.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = readIn, writeOut
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- directChild(ctx, []string{bin, "-test.run=^TestFakeKexecChild$"}) }()
	ready := make([]byte, 6)
	if _, err := io.ReadFull(readOut, ready); err != nil || string(ready) != "ready\n" {
		t.Fatal(string(ready), err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
