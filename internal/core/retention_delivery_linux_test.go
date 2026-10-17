package core

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type retentionDeliveryFixture struct{ comparisonDeliveryFixture }

type retentionDeliveryResult struct {
	code                                 int
	out, stderr, rawStderr, timingFooter string
}

func (r retentionDeliveryResult) requireTiming(t *testing.T, admitted bool) {
	t.Helper()
	if r.rawStderr != r.stderr+r.timingFooter {
		t.Fatal("timing split changed raw stderr", r)
	}
	if !admitted {
		if r.timingFooter != "" {
			t.Fatal("excluded invocation emitted timing", r)
		}
		return
	}
	state := "completed"
	if r.code != 0 {
		state = "failed"
	}
	if !strings.HasPrefix(r.timingFooter, fmt.Sprintf("\nTiming: %s (exit %d)\n", state, r.code)) {
		t.Fatal("missing timing or exit status mismatch", r)
	}
}

func retentionDelivery(t *testing.T, hosts ...string) retentionDeliveryFixture {
	t.Helper()
	ms := make([]Machine, len(hosts))
	for i, host := range hosts {
		// Retention and standalone GC do not add a managed-machine filter.
		ms[i] = connectionMachine(host, host, host+".invalid", false)
	}
	f := retentionDeliveryFixture{comparisonDelivery(t, ms...)}
	for _, host := range hosts {
		mutationSeed(t, f.root, host, "old")
		newDir, _ := mutationSeed(t, f.root, host, "new")
		if err := os.Symlink("new", filepath.Join(filepath.Dir(newDir), "current")); err != nil {
			t.Fatal(err)
		}
	}
	f.tool(t, "nix", `key="${@: -1}"
printf 'nix:%s\n' "$key" >> "$TRACE"
case "$key" in
 .#confctl.settings) printf '%s\n' '{"list":{"columns":["name","managed"]},"nix":{"maxJobs":"7"},"buildGenerations":{"min":0,"max":0,"maxAge":0,"collectGarbage":false},"hostGenerations":{"min":0,"max":0,"maxAge":0,"collectGarbage":true}}';;
 .#confctl.machinesJson) printf '[{"outputs":{"out":"%s/machines.json"}}]\n' "$PWD";;
 *) printf 'unexpected evaluation: %s\n' "$key" >&2; exit 98;;
esac
`)
	f.tool(t, "ssh", `request="${@: -1}"
host="${@: -2:1}"
name="${host%.invalid}"
printf 'ssh:%s:%s\n' "$host" "$request" >> "$TRACE"
case "$request" in
 'cat /proc/uptime') printf '600 0\n';;
 'bash --norc')
  while IFS= read -r line; do :; done
  printf '%s/store-%s-new\n' "$PWD" "$name"
  printf '/nix/var/nix/profiles/system-1-link;%s/store-%s-old;0\n' "$PWD" "$name"
  printf '/nix/var/nix/profiles/system-2-link;%s/store-%s-new;1\n' "$PWD" "$name";;
 'nix-env -p /nix/var/nix/profiles/system --delete-generations '*)
  id="${request##* }"
  [[ "$id" == "${FAIL_DELETE:-}" ]] && { printf 'synthetic deletion failure\n' >&2; exit 23; }
  printf '%s:%s\n' "$name" "$id" >> "$HOME/remote-deletions";;
 'nix-collect-garbage')
  printf '1 store paths deleted\n'
  printf 'gc-finished:%s\n' "$host" >> "$TRACE"
  [[ "$host" == "${FAIL_GC_HOST:-}" ]] && exit 23
  exit 0;;
 *) printf 'unexpected remote request: %s\n' "$request" >&2; exit 98;;
esac
`)
	return f
}

func (f retentionDeliveryFixture) run(t *testing.T, args []string, input string, env ...string) retentionDeliveryResult {
	t.Helper()
	cmd := f.command(t, args, env...)
	cmd.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	code := comparisonDeliveryExit(t, cmd.Run())
	body, footer, err := splitTimingFooter(stderr.String())
	if err != nil {
		t.Fatal("invalid timing footer", err, stderr.String())
	}
	return retentionDeliveryResult{code: code, out: out.String(), stderr: body, rawStderr: stderr.String(), timingFooter: footer}
}

func TestRetentionDeliveryMainRefusalPreservesGenerations(t *testing.T) {
	for _, args := range [][]string{{"generation", "rm"}, {"generation", "rotate"}, {"collect-garbage"}} {
		for _, input := range []string{"n\n", ""} {
			t.Run(strings.Join(args, "/")+"/"+input, func(t *testing.T) {
				f := retentionDelivery(t, "node")
				before := testConfigurationState(t, filepath.Join(f.root, ".confctl/generations"))
				r := f.run(t, args, input)
				r.requireTiming(t, true)
				code, out, stderr := r.code, r.out, r.stderr
				trace := f.text("trace")
				diagnostic := "Aborted"
				if input == "" {
					diagnostic = "end of file reached"
				}
				if code != 1 || !strings.Contains(stderr, diagnostic) || strings.Count(out, "Continue?") != 1 || strings.Contains(trace, "ssh:") || len(f.logs(t)) != 1 {
					t.Fatal(code, out, stderr, trace)
				}
				if !reflect.DeepEqual(before, testConfigurationState(t, filepath.Join(f.root, ".confctl/generations"))) {
					t.Fatal("refusal changed local generations")
				}
				logPath := f.logs(t)[0]
				if !strings.Contains(filepath.Base(logPath), strings.Join(args, "-")) {
					t.Fatal("lost multi-leaf log name", logPath)
				}
			})
		}
	}
}

func TestRetentionDeliveryMainRemoteRemovalAndRotation(t *testing.T) {
	for _, command := range []string{"rm", "rotate"} {
		for _, yes := range []bool{false, true} {
			t.Run(command+"/"+map[bool]string{false: "confirmed", true: "yes"}[yes], func(t *testing.T) {
				f := retentionDelivery(t, "node")
				before := testConfigurationState(t, filepath.Join(f.root, ".confctl/generations"))
				args := []string{"generation", command, "--remote", "--no-gc"}
				if yes {
					args = append(args, "--yes")
				}
				r := f.run(t, args, "y\n")
				r.requireTiming(t, true)
				code, out, stderr := r.code, r.out, r.stderr
				want := "node:1\nnode:2\n"
				if command == "rotate" {
					want = "node:1\n" // Current is excluded from rotation only.
				}
				if code != 0 || stderr != "" || f.text("remote-deletions") != want || strings.Contains(f.text("trace"), "nix-collect-garbage") || len(f.logs(t)) != 0 || strings.Count(out, "Continue?") != map[bool]int{false: 1, true: 0}[yes] {
					t.Fatal(code, out, stderr, f.text("trace"), f.text("remote-deletions"))
				}
				if !reflect.DeepEqual(before, testConfigurationState(t, filepath.Join(f.root, ".confctl/generations"))) {
					t.Fatal("remote-only operation changed local generations/current")
				}
			})
		}
	}
}

func TestRetentionDeliveryMainPartialRemoteFailure(t *testing.T) {
	f := retentionDelivery(t, "node")
	r := f.run(t, []string{"generation", "rm", "-ry", "--max-concurrent-gc=0x2"}, "", "FAIL_DELETE=2")
	r.requireTiming(t, true)
	code, out, stderr := r.code, r.out, r.stderr
	if code != 1 || !strings.Contains(stderr, "exit status 23") || f.text("remote-deletions") != "node:1\n" || strings.Contains(f.text("trace"), "nix-collect-garbage") || len(f.logs(t)) != 1 {
		t.Fatal(code, out, stderr, f.text("trace"))
	}
	log, err := os.ReadFile(f.logs(t)[0])
	if err != nil || !strings.Contains(string(log), `command: ["generation", "rm"]`) || !strings.Contains(string(log), `"max-concurrent-gc" => 2`) || !strings.Contains(string(log), `"r" => true`) || !strings.Contains(string(log), "synthetic deletion failure") {
		t.Fatal("lost typed invocation/process log", string(log), err)
	}
}

func TestRetentionDeliveryMainPostGCPolicies(t *testing.T) {
	for _, command := range []string{"rm", "rotate"} {
		f := retentionDelivery(t, "node")
		r := f.run(t, []string{"generation", command, "-ry"}, "")
		r.requireTiming(t, true)
		code, out, stderr := r.code, r.out, r.stderr
		trace := f.text("trace")
		if code != 0 || stderr != "" || !strings.Contains(trace, "--delete-generations 1") || strings.Count(trace, "ssh:node.invalid:nix-collect-garbage\n") != 1 || len(f.logs(t)) != 0 || !strings.Contains(out, "1 store paths deleted") {
			t.Fatal(command, code, out, stderr, trace)
		}
		if strings.Index(trace, "--delete-generations") > strings.Index(trace, "nix-collect-garbage") {
			t.Fatal("GC ran before deletions", trace)
		}
	}
}

func TestRetentionDeliveryMainGCFailureDrainsAndJoins(t *testing.T) {
	f := retentionDelivery(t, "first", "second")
	r := f.run(t, []string{"collect-garbage", "-y", "--max-concurrent-gc=1"}, "", "FAIL_GC_HOST=first.invalid")
	r.requireTiming(t, true)
	code, out, stderr := r.code, r.out, r.stderr
	trace := f.text("trace")
	if code != 1 || !strings.Contains(stderr, "failed on: first") || strings.Contains(stderr, "failed on: first, second") || len(f.logs(t)) != 1 || strings.Count(trace, "gc-finished:") != 2 || !strings.Contains(out, "second: 1 store paths deleted") {
		t.Fatal(code, out, stderr, trace)
	}
	for _, host := range []string{"first", "second"} {
		if !strings.Contains(trace, "gc-finished:"+host+".invalid\n") {
			t.Fatal("returned before owned fake GC completed", host, trace)
		}
	}
}

func TestRetentionDeliveryMainGCConfirmation(t *testing.T) {
	f := retentionDelivery(t, "node")
	r := f.run(t, []string{"collect-garbage"}, "y\n")
	r.requireTiming(t, true)
	code, out, stderr := r.code, r.out, r.stderr
	if code != 0 || stderr != "" || strings.Count(out, "Continue?") != 1 || strings.Count(f.text("trace"), "gc-finished:node.invalid\n") != 1 || len(f.logs(t)) != 0 {
		t.Fatal(code, out, stderr, f.text("trace"))
	}
}

func TestRetentionDeliveryMainGCWorkersAndEmptySelection(t *testing.T) {
	for _, workers := range []string{"0", "-1", "9999999999999999999999999999999999"} {
		f := retentionDelivery(t, "node")
		r := f.run(t, []string{"collect-garbage", "-y", "--max-concurrent-gc=" + workers}, "")
		r.requireTiming(t, true)
		code, out, stderr := r.code, r.out, r.stderr
		wantGC := workers[0] != '-' && workers != "0"
		if code != 0 || stderr != "" || strings.Contains(f.text("trace"), "gc-finished:") != wantGC || len(f.logs(t)) != 0 || !strings.Contains(out, "Collecting garbage on node") {
			t.Fatal(workers, code, out, stderr, f.text("trace"))
		}
	}
	for _, args := range [][]string{{"generation", "rm", "-y", "missing"}, {"generation", "rotate", "-y", "missing"}, {"collect-garbage", "-y", "missing"}} {
		f := retentionDelivery(t, "node")
		r := f.run(t, args, "")
		r.requireTiming(t, true)
		code, out, stderr := r.code, r.out, r.stderr
		want := 0
		if args[0] == "collect-garbage" {
			want = 1
		}
		if code != want || strings.Contains(f.text("trace"), "ssh:") || want == 0 && !strings.Contains(out, "No generations") || want == 1 && !strings.Contains(stderr, "No machines to collect garbage on") {
			t.Fatal(code, out, stderr, f.text("trace"))
		}
	}
}

func TestRetentionDeliveryMainUsageAndHelp(t *testing.T) {
	for _, path := range [][]string{{"generation", "rm"}, {"generation", "rotate"}, {"collect-garbage"}} {
		for _, flags := range [][]string{{"--help"}, {"--unknown"}, {"--max-concurrent-gc=auto"}, {"-a", "invalid"}} {
			f := retentionDelivery(t, "node")
			args := append(append([]string{}, path...), flags...)
			r := f.run(t, args, "")
			// Filter syntax is rejected by the admitted handler; help, unknown
			// options and invalid integer values return before timing admission.
			r.requireTiming(t, flags[0] == "-a")
			code, out, stderr := r.code, r.out, r.stderr
			want := 64
			if flags[0] == "--help" {
				want = 0
			}
			if code != want || !strings.Contains(out, "SYNOPSIS") || strings.Contains(f.text("trace"), "ssh:") || want == 64 && !strings.Contains(stderr, "error:") || flags[0] != "-a" && f.text("trace") != "" {
				t.Fatal(args, code, out, stderr, f.text("trace"))
			}
		}
	}
}
