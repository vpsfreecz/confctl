package hooks_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

// This is a native build-side template check. It never executes the target
// checker, so the policy can also be generated for another target architecture.
func TestNixPolicyMatchesFixedConfig(t *testing.T) {
	nix, err := exec.LookPath("nix-instantiate")
	if err != nil {
		t.Skip("Nix template check requires declared nix-instantiate")
	}
	template, err := filepath.Abs("../../nix/lefthook-config.nix")
	if err != nil {
		t.Fatal(err)
	}
	checker := "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-confctl-hook-checker/bin/confctl-hooks"
	expr := "import (builtins.toPath " + strconv.Quote(template) + ") { checker = " + strconv.Quote(checker) + "; }"
	cmd := exec.Command(nix, "--eval", "--strict", "--json", "--expr", expr)
	// Nix initializes profile state even for this local template evaluation.
	// Keep it writable and private inside package build sandboxes.
	cmd.Env = append(os.Environ(), "NIX_STATE_DIR="+t.TempDir())
	actual, err := nixPolicyOutput(cmd)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := hookcheck.FixedConfig(checker)
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(expected) {
		t.Fatalf("Nix policy differs from checker template:\n%s\nwant:\n%s", actual, expected)
	}
}

func nixPolicyOutput(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("Nix policy: %w\nstdout:\n%s\nstderr:\n%s", err, output, stderr.Bytes())
	}
	var actual string
	if err := json.Unmarshal(output, &actual); err != nil {
		return "", fmt.Errorf("Nix policy JSON: %w\nstdout:\n%s\nstderr:\n%s", err, output, stderr.Bytes())
	}
	return actual, nil
}

func TestNixPolicyOutputSeparatesDiagnostics(t *testing.T) {
	if mode := os.Getenv("CONFCTL_POLICY_TEST_PROCESS"); mode != "" {
		fmt.Fprintln(os.Stderr, "warning: fixture diagnostic")
		if mode == "bad-json" {
			fmt.Fprint(os.Stdout, "invalid policy JSON")
		} else {
			fmt.Fprint(os.Stdout, `"fixture policy"`)
		}
		if mode == "nonzero" {
			os.Exit(7)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"warning", "nonzero", "bad-json"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNixPolicyOutputSeparatesDiagnostics$")
			cmd.Env = append(os.Environ(), "CONFCTL_POLICY_TEST_PROCESS="+mode)
			actual, err := nixPolicyOutput(cmd)
			if mode == "warning" {
				if err != nil || actual != "fixture policy" {
					t.Fatal(actual, err)
				}
			} else {
				want := `"fixture policy"`
				if mode == "bad-json" {
					want = "invalid policy JSON"
				}
				if err == nil || !strings.Contains(err.Error(), "warning: fixture diagnostic") || !strings.Contains(err.Error(), want) {
					t.Fatal("lost failure diagnostics", actual, err)
				}
				if mode == "nonzero" {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 7 {
						t.Fatal("lost native failure status", err)
					}
				}
			}
		})
	}
}
