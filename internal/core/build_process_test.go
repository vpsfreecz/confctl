package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildStreamChild(t *testing.T) {
	if len(os.Args) > 2 && os.Args[len(os.Args)-2] == "--" && os.Args[len(os.Args)-1] == "environment" {
		value, present := os.LookupEnv("CONFCTL_BUILD_ENV")
		fmt.Fprintf(os.Stdout, "%t:%s", present, value)
		os.Exit(0)
	}
	if os.Getenv("CONFCTL_BUILD_STREAM_CHILD") != "yes" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "streams":
		var group sync.WaitGroup
		for _, stream := range []*os.File{os.Stdout, os.Stderr} {
			group.Add(1)
			go func(stream *os.File) {
				defer group.Done()
				for i := 0; i < 32; i++ {
					fmt.Fprintln(stream, "synthetic-stream-line")
				}
			}(stream)
		}
		group.Wait()
		os.Exit(7)
	case "block":
		fmt.Fprintln(os.Stdout, "ready")
		<-time.After(30 * time.Second)
		os.Exit(99)
	}
}

func TestBuildProcessExplicitEnvironment(t *testing.T) {
	t.Setenv("CONFCTL_BUILD_ENV", "inherited")
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"inherit", nil, "true:inherited"},
		{"empty", []string{}, "false:"},
		{"replacement", []string{"CONFCTL_BUILD_ENV=command-local"}, "true:command-local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := processWithStreamsEnv(context.Background(), t.TempDir(), buildChildArgs("environment"), []byte{}, tc.env, nil)
			if err != nil || result.ExitCode != 0 || string(result.Stdout) != tc.want {
				t.Fatal("command-local environment lost", result.ExitCode, err)
			}
			if os.Getenv("CONFCTL_BUILD_ENV") != "inherited" {
				t.Fatal("command environment mutated parent")
			}
		})
	}
}

func buildChildArgs(mode string) []string {
	return []string{os.Args[0], "-test.run=^TestBuildStreamChild$", "--", mode}
}

func TestBuildProcessTaggedStreamsAndSerialization(t *testing.T) {
	t.Setenv("CONFCTL_BUILD_STREAM_CHILD", "yes")
	var stdout, stderr bytes.Buffer
	var active atomic.Int32
	result, err := processWithStreams(context.Background(), t.TempDir(), buildChildArgs("streams"), []byte{}, func(out, diagnostic []byte) error {
		if active.Add(1) != 1 {
			t.Error("tagged callback overlap")
		}
		defer active.Add(-1)
		if (out == nil) == (diagnostic == nil) {
			t.Error("callback lost stream identity")
		}
		stdout.Write(out)
		stderr.Write(diagnostic)
		return nil
	})
	want := strings.Repeat("synthetic-stream-line\n", 32)
	if err != nil || result.ExitCode != 7 || stdout.String() != want || stderr.String() != want || !bytes.Equal(result.Stdout, stdout.Bytes()) || !bytes.Equal(result.Stderr, stderr.Bytes()) {
		t.Fatal("raw streams or exit data lost", result.ExitCode, err)
	}
}

func TestBuildProcessCallbackLogFailureAndCancellation(t *testing.T) {
	t.Setenv("CONFCTL_BUILD_STREAM_CHILD", "yes")
	t.Run("callback-error", func(t *testing.T) {
		sentinel := errors.New("synthetic tagged callback failure")
		result, err := processWithStreams(context.Background(), t.TempDir(), buildChildArgs("block"), nil, func([]byte, []byte) error { return sentinel })
		if err != sentinel || result.capturePrimary() != sentinel || !bytes.Contains(result.Stdout, []byte("ready")) || result.waitErr == nil {
			t.Fatal("callback did not cancel/join with primary intact", err)
		}
	})
	t.Run("external-cancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := processWithStreams(ctx, t.TempDir(), buildChildArgs("block"), nil, func([]byte, []byte) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) || result.waitErr == nil {
			t.Fatal("cancellation failed to join child", err)
		}
	})
	t.Run("log-error", func(t *testing.T) {
		sentinel := errors.New("synthetic log failure")
		file, err := os.Create(filepath.Join(t.TempDir(), "log"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		var logged strings.Builder
		owner := newEngineLog(file, engineLogOps{Write: func(b []byte) (int, error) {
			logged.Write(b)
			if bytes.Contains(b, []byte("ready")) {
				return 0, sentinel
			}
			return len(b), nil
		}, Flush: func() error { return nil }})
		e := &Engine{Root: t.TempDir(), Context: context.Background(), Log: file, logOwner: owner}
		_, err = e.buildProcess(buildChildArgs("block"), nil, nil)
		if !errors.Is(err, sentinel) || strings.Contains(logged.String(), "Finished") {
			t.Fatal("failed stream log emitted successful finish", err, logged.String())
		}
	})
}
