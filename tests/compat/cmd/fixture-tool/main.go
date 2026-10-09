// fixture-tool is the only executable exposed for external requests in tier 2.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/vpsfreecz/confctl/compat/harness"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	code := run()
	os.Exit(code)
}
func run() int {
	tool := filepath.Base(os.Args[0])
	argv := os.Args[1:]
	host := ""
	if tool == "ssh" {
		for i := 0; i < len(argv)-1; i++ {
			if argv[i] == "-F" || argv[i] == "-l" || argv[i] == "-p" || argv[i] == "-o" {
				i++
				continue
			}
			host = argv[i]
			break
		}
	}
	start := time.Now().UnixNano()
	var input []byte
	plan, e := harness.LoadPlan(os.Getenv("COMPAT_PLAN"), tool, host, argv)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 126
	}
	root := os.Getenv("COMPAT_ROOT")
	expand := func(s string) string { return strings.ReplaceAll(s, "${ROOT}", root) }
	stdinMode := "not-read-empty-contract"
	for _, planned := range plan {
		r := planned.Rule
		if r.ReadStdin && harness.MatchRequest(r, tool, host, argv, root) {
			input, _ = io.ReadAll(os.Stdin)
			stdinMode = "read-explicit-input-to-eof"
			break
		}
	}
	if stdinMode == "not-read-empty-contract" {
		_ = syscall.SetNonblock(0, true)
		probe := make([]byte, 1)
		n, err := syscall.Read(0, probe)
		_ = syscall.SetNonblock(0, false)
		switch {
		case n == 0 && err == nil:
			stdinMode = "empty-eof"
		case err == syscall.EAGAIN:
			stdinMode = "empty-open"
		default:
			stdinMode = "unexpected-unread-input"
			input = probe[:max(n, 0)]
		}
	}
	event := harness.Event{StdinMode: stdinMode, Tool: tool, Host: host, Argv: argv, Stdin: string(input), CWD: actualCWD(), PID: os.Getpid(), Start: start, Rule: -1, Exit: 126, Env: map[string]string{}}
	for _, k := range []string{"PWD", "HOME", "TMPDIR", "COMPAT_CASE", "CONFCTL_SSH_CONFIG", "NIX_SSHOPTS", "CONFCTL_MAX_JOBS"} {
		event.Env[k] = os.Getenv(k)
	}
	n := 1
	needsCounter := os.Getenv("COMPAT_TRACE_MODE") != "off"
	for _, planned := range plan {
		if planned.Rule.Occurrence != 0 && harness.MatchRequest(planned.Rule, tool, host, argv, root) {
			needsCounter = true
		}
	}
	if needsCounter {
		identity, _ := json.Marshal([]any{tool, host, argv, string(input)})
		n, e = harness.NextOccurrence(os.Getenv("COMPAT_TRACE"), fmt.Sprintf("%x", sha256.Sum256(identity)))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 126
		}
	}
	event.Occurrence = n
	for _, planned := range plan {
		i, r := planned.Index, planned.Rule
		if !harness.MatchRequest(r, tool, host, argv, root) {
			continue
		}
		if r.Stdin != nil && expand(*r.Stdin) != string(input) {
			continue
		}
		// Occurrences belong to a semantic rule, never a global request FIFO. A
		// per-rule flock protects only test bookkeeping, not confctl operations.
		if r.Occurrence != 0 && r.Occurrence != n {
			continue
		}
		event.Rule = i
		event.Exit = r.Exit
		event.Signal = r.Signal
		event.Writes = r.Writes
		if r.DelayMS > 0 {
			time.Sleep(time.Duration(r.DelayMS) * time.Millisecond)
		}
		if e = harness.WriteFiles(root, r.Writes); e != nil {
			event.Exit = 126
			fmt.Fprintln(os.Stderr, e)
		}
		event.Stdout = expand(r.Stdout)
		event.Stderr = expand(r.Stderr)
		fmt.Fprint(os.Stdout, event.Stdout)
		fmt.Fprint(os.Stderr, event.Stderr)
		break
	}
	if event.Rule < 0 {
		fmt.Fprintf(os.Stderr, "unexpected fixture request: %s host=%q argv=%q stdin=%q\n", tool, host, argv, string(input))
	}
	event.End = time.Now().UnixNano()
	b, _ := json.MarshalIndent(event, "", "  ")
	p := filepath.Join(os.Getenv("COMPAT_TRACE"), fmt.Sprintf("%d.json", os.Getpid()))
	if os.Getenv("COMPAT_TRACE_MODE") != "off" || event.Rule < 0 {
		_ = os.WriteFile(p, b, 0644)
	}
	if event.Signal != "" {
		var sig syscall.Signal
		switch event.Signal {
		case "TERM":
			sig = syscall.SIGTERM
		case "INT":
			sig = syscall.SIGINT
		case "KILL":
			sig = syscall.SIGKILL
		}
		if sig != 0 {
			_ = syscall.Kill(os.Getpid(), sig)
		}
	}
	return event.Exit
}

func actualCWD() string { p, _ := os.Getwd(); return p }
