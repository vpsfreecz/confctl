//go:build linux && logprocessnative

package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/ui"
)

// These cells require a separately selected native route and finite containment.
// Source-only verification compiles/vets this file and never runs its binary.
// No init/TestMain, parent environment switch, external fixture or global log
// factory is involved. The helper is reached only by the literal child argv.
func TestLogProcessNativeFixture(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "logprocess-write-until-refusal":
		p := bytes.Repeat([]byte("native-out\n"), 16384)
		for i := 0; i < 128; i++ {
			if _, err := os.Stdout.Write(p); err != nil {
				os.Exit(0)
			}
		}
		os.Exit(9) // bounded fixture exhausted without the selected refusal
	case "logprocess-split":
		_, _ = os.Stdout.Write([]byte("out-token\n"))
		_, _ = os.Stderr.Write([]byte("err-token\n"))
		os.Exit(7)
	}
}

func logProcessNativeArgv(mode string) []string {
	return []string{os.Args[0], "-test.run=^TestLogProcessNativeFixture$", "--", mode}
}

func TestLogProcessNativeCallbackBeforeBindAndJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	failure := errors.New("native callback refused")
	calls := 0
	r, err := processWithTestConnectionLog(ctx, t.TempDir(), logProcessNativeArgv("logprocess-write-until-refusal"), []byte{}, func([]byte) error { calls++; return failure })
	if err != failure || ctx.Err() != nil || calls != 1 || len(r.Stdout) == 0 || r.capturePrimary() != failure || len(r.capture.streams) != 2 || r.capture.streams[0].count != int64(len(r.Stdout)) {
		t.Fatal(err, calls, r)
	}
	// Actual Wait returned with copy results. This does not certify descendant
	// cleanup or join the existing cancellation timer worker.
}

func TestLogProcessNativeSplitAndMergedStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	argv := logProcessNativeArgv("logprocess-split")
	r, err := Process(ctx, t.TempDir(), argv, []byte{})
	if err != nil || r.ExitCode != 7 || string(r.Stdout) != "out-token\n" || string(r.Stderr) != "err-token\n" || len(r.capture.streams) != 2 {
		t.Fatal(r, err)
	}
	empty := ""
	e := &Engine{Context: ctx}
	r, err = e.sshProcess(ctx, t.TempDir(), argv, sshInput{String: &empty}, true, nil)
	if err != nil || r.ExitCode != 7 || len(r.Stderr) != 0 || len(r.capture.streams) != 1 || r.capture.streams[0].stream != "combined" || r.capture.streams[0].count != int64(len(r.Stdout)) || strings.Count(string(r.Stdout), "out-token\n") != 1 || strings.Count(string(r.Stdout), "err-token\n") != 1 {
		t.Fatal(r, err)
	}
	// Merged output token presence is bound; no arbitrary split-stream order.
}

func TestLogProcessNativeRawFileAndCleanup(t *testing.T) {
	for _, success := range []bool{false, true} {
		e := &Engine{Root: t.TempDir()}
		if err := e.OpenLog("native-private"); err != nil {
			t.Fatal(err)
		}
		name := e.Log.Name()
		if err := e.logText("raw\x00\xff\n"); err != nil {
			t.Fatal(err)
		}
		if err := e.CloseLog(success); err != nil {
			t.Fatal(err)
		}
		if err := e.CloseLog(!success); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(name)
		if success {
			if !os.IsNotExist(err) {
				t.Fatal("successful log retained", err)
			}
		} else if err != nil || string(b) != "raw\x00\xff\n" {
			t.Fatal(err, b)
		}
	}
	e := &Engine{Root: t.TempDir()}
	if err := e.OpenLog("renamed-private"); err != nil {
		t.Fatal(err)
	}
	name := e.Log.Name()
	if err := os.Rename(name, name+".moved"); err != nil {
		t.Fatal(err)
	}
	first := e.CloseLog(true)
	if first == nil || e.CloseLog(false) != first {
		t.Fatal("remove failure not cached", first)
	}
	if _, err := os.Stat(name + ".moved"); err != nil {
		t.Fatal("renamed path was repaired", err)
	}
}

func TestLogProcessNativeStatusMetadataSuppression(t *testing.T) {
	// Refuse before the evaluator's child acquisition. With no selected hosts,
	// this actual Status call demonstrates its metadata suppression only. Host
	// downstream admission through a real fixture evaluator remains unproved.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("metadata start refused")
	e := logProcessEngine(engineLogOps{Write: func([]byte) (int, error) { return 0, failure }})
	e.Root, e.SettingsCache = root, map[string]any{"nix": map[string]any{"maxJobs": "1"}}
	if _, err := e.nix("eval", []string{".#confctl.machineKeys"}, false, false, "1"); err != failure {
		t.Fatal("primitive did not return error", err)
	}
	if _, err := e.Status(nil, nil); err != nil {
		t.Fatal("metadata suppression changed", err)
	}
}

// N5 actual Main/hook-driver metadata/cleanup fault injection and RPC transport
// are not represented by a fake runner or skipped parity test. Current private
// projector/LogCleanupStatus/supervisor scripts cover their owning seams; an
// exact later entrypoint/fixture mechanism must be selected before claiming N5.

func TestLogProcessNativeViewDetach(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	if err := e.OpenLog("view-private"); err != nil {
		t.Fatal(err)
	}
	v, err := ui.OpenLiveView(ui.LiveViewOptions{}, ui.LiveViewOps{AttachLogger: e.attachLog})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.logText("split \xc3"); err != nil {
		t.Fatal(err)
	}
	if err := e.scopedEngine(context.Background()).logText("\xa9\nunterminated"); err != nil {
		t.Fatal(err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	if len(e.logOwner.live.readers) != 0 {
		t.Fatal("reader still attached")
	}
	if err := e.CloseLog(false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.Log.Name())
	if err != nil || string(b) != "split é\nunterminated" {
		t.Fatal(err, b)
	}
	// This file/view cell does not certify a native descriptor/PTY renderer or
	// arbitrary blocked-callback termination. Those routes remain separate.
}
