package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vpsfreecz/confctl/internal/kexecnetboot"
)

func TestPresentationDoesNotEnterProcNetworkOrChild(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--unknown"}, {"--server-url"}, {"--version"}, {"--version="}, {"--version=opaque"}} {
		var out, err bytes.Buffer
		d := kexecnetboot.Dependencies{Out: &out, Err: &err,
			BeginActions: func(context.Context) (context.Context, context.CancelFunc) {
				t.Fatal("action context installed for parser presentation", args)
				return nil, nil
			},
			ReadCmdline: func() ([]byte, error) { t.Fatal("proc entered", args); return nil, nil }}
		status := execute(context.Background(), "/actual/kexec-netboot", args, false, kexecnetboot.Config{}, d)
		want := 1
		if args[0] == "--help" || args[0] == "-h" {
			want = 0
		}
		if status != want {
			t.Fatal(args, status, out.String(), err.String())
		}
	}
}
func TestConflictingActionNoEffects(t *testing.T) {
	var out, err bytes.Buffer
	d := kexecnetboot.Dependencies{Out: &out, Err: &err, ReadCmdline: func() ([]byte, error) { t.Fatal("proc entered"); return nil, nil }}
	if status := execute(context.Background(), "kexec-netboot", []string{"--exec", "--unload"}, false, kexecnetboot.Config{}, d); status != 1 || out.Len() != 0 || err.String() != "ERROR: use either --unload or --exec, not both\n" {
		t.Fatal(status, out.String(), err.String())
	}
}

const promptIndex = `{"machines":[{"fqdn":"node.test","spin":"nixos","generations":[{"generation":7,"time_s":"time","shortrev":"rev","kernel_version":"kernel","current":true,"kernel_params":[],"variants":[{"name":"default","label":"Default","kernel_params":[]}],"boot_files":{}}]}]}`

// This private test route calls execute/Run with literal index data and an owned
// pipe. It never enters the installed main or any production dependency.
func TestPromptSignalChild(t *testing.T) {
	stage := os.Getenv("CONFCTL_KEXEC_PROMPT_CHILD")
	if stage == "" {
		return
	}
	args := []string{"--interactive"}
	if stage != "machine" {
		args = append(args, "--machine=node.test")
	}
	if stage == "variant" || stage == "confirmation" {
		args = append(args, "--generation=7")
	}
	if stage == "confirmation" {
		args = append(args, "--variant=default")
	}
	requests := 0
	d := kexecnetboot.Dependencies{
		BeginActions: func(parent context.Context) (context.Context, context.CancelFunc) {
			fmt.Fprintln(os.Stdout, "UNEXPECTED action context")
			return actionContext(parent)
		},
		ReadCmdline: func() ([]byte, error) { return []byte("httproot=http://fixture.test/node/7/initrd"), nil },
		Request: func(context.Context, string) (*http.Response, error) {
			requests++
			if requests != 1 {
				fmt.Fprintln(os.Stdout, "UNEXPECTED download")
				return nil, errors.New("unexpected download")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(promptIndex))}, nil
		},
		CreateTemp: func(string) (kexecnetboot.Temp, error) {
			fmt.Fprintln(os.Stdout, "UNEXPECTED temp")
			return kexecnetboot.Temp{}, errors.New("unexpected temp")
		},
		Child: func(context.Context, []string) error {
			fmt.Fprintln(os.Stdout, "UNEXPECTED child")
			return errors.New("unexpected child")
		},
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
	}
	status := execute(context.Background(), "private-kexec-fixture", args, false,
		kexecnetboot.Config{MachineFQDN: "node.test", Kexec: "/never/kexec"}, d)
	os.Exit(status)
}

func TestBlockedPromptsTerminateOnFirstSignal(t *testing.T) {
	for _, tc := range []struct{ stage, want string }{
		{"machine", "       FQDN                            SPIN\n [1]*  node.test                       nixos\nSelect a machine by number: "},
		{"generation", "Available generations for node.test: (default is 7 - current)\n       TIME                 REVISION    KERNEL\n [7]*  time                 rev         kernel\nEnter generation number (or negative offset) [ENTER for default]: "},
		{"variant", "Variants available for generation 7:\n       LABEL\n  [1]  Default\nChoose variant by number [ENTER for 'default']: "},
		{"confirmation", "\nSelected configuration:\n  Machine:       node.test (spin=nixos)\n  Generation:    7 (time, rev=rev, kernel=kernel)\n  Variant:       default (Default)\n  Kernel params: \n\nContinue? [y/N]: "},
	} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(tc.stage+"/"+sig.String(), func(t *testing.T) {
				bin, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				inR, inW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer inR.Close()
				defer inW.Close() // Remains open without input until the child exits.
				outR, outW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer outR.Close()
				defer outW.Close()
				var stderr bytes.Buffer
				cmd := exec.Command(bin, "-test.run=^TestPromptSignalChild$")
				cmd.Env = append(os.Environ(), "CONFCTL_KEXEC_PROMPT_CHILD="+tc.stage)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				_ = inR.Close()
				_ = outW.Close()
				// Failure containment applies only to this owned fake child. It
				// cannot make a hung or SIGKILLed child satisfy the signal assertion.
				watchdog := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
				got := make([]byte, len(tc.want))
				_, readErr := io.ReadFull(outR, got)
				var signalErr error
				if readErr == nil && string(got) == tc.want {
					signalErr = cmd.Process.Signal(sig)
				} else {
					_ = cmd.Process.Kill()
				}
				waitErr := cmd.Wait() // Exactly one wait, including watchdog failures.
				notExpired := watchdog.Stop()
				tail, tailErr := io.ReadAll(outR)
				var exit *exec.ExitError
				if !notExpired || readErr != nil || string(got) != tc.want || signalErr != nil ||
					tailErr != nil || len(tail) != 0 || stderr.Len() != 0 || !errors.As(waitErr, &exit) {
					t.Fatalf("ready%q read%v signal%v wait%v watchdog%v tail%q stderr%q", got, readErr, signalErr, waitErr, notExpired, tail, stderr.String())
				}
				state, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !state.Signaled() || state.Signal() != sig {
					t.Fatalf("first signal %s: actual state %v", sig, exit.ProcessState)
				}
			})
		}
	}
}
