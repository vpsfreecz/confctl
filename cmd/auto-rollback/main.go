// auto-rollback retains the check-file handshake used by stored generations.
// Normal child exit statuses are deliberately ignored, as in the Ruby helper;
// confirmation establishes connectivity, not activation or system health.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type options struct {
	timeout  *big.Int
	toplevel string
	action   string
	check    string
}

// These dependencies are private test seams, not public path or clock overrides.
type dependencies struct {
	readlink func(string) (string, error)
	read     func(string) ([]byte, error)
	write    func(string, []byte) error
	unlink   func(string) error
	now      func() time.Time
	sleep    func(time.Duration)
	start    func([]string) (func() error, error)
	out      io.Writer
}

type presentation struct {
	stdout string
	stderr string
	status int
}

var errArity = errors.New("Invalid arguments")

func help(argv0 string) string {
	return "Usage: " + argv0 + " [options] <toplevel> <action> <check file>\n" +
		"    -t, --timeout TIMEOUT            Timeout in seconds\n" +
		"    -h, --help                       Print help message and exit\n"
}

func programName(argv0 string) string {
	name := filepath.Base(argv0)
	if dot := strings.LastIndexByte(name, '.'); dot > 0 && name != ".." {
		name = name[:dot]
	}
	return name
}

var completionWords = regexp.MustCompile(`[A-Za-z0-9_]+\b`)

// The pinned OptionParser expands quoted word segments with \w*. Only long
// spellings fold case; this helper's finite names remain local.
func completionPattern(key string, foldCase bool) *regexp.Regexp {
	pattern := "^" + completionWords.ReplaceAllString(regexp.QuoteMeta(key), "${0}[A-Za-z0-9_]*")
	if foldCase {
		pattern = "(?i)" + pattern
	}
	compiled, _ := regexp.Compile(pattern)
	return compiled
}

func bashCompletion(word string) string {
	long := strings.HasPrefix(word, "--")
	short := strings.HasPrefix(word, "-") && !long
	if word == "-" {
		long, short = true, true
	}
	word, _, _ = strings.Cut(word, "=")
	pattern := completionPattern(word, long)
	var out strings.Builder
	for _, name := range []string{"--timeout", "-t", "--help", "-h"} {
		if pattern != nil && ((long && strings.HasPrefix(name, "--")) || (short && !strings.HasPrefix(name, "--"))) && pattern.MatchString(name) {
			out.WriteString(name + "\n")
		}
	}
	return out.String()
}

func zshCompletion(name string) string {
	return "#compdef " + name + "\n\n" +
		"typeset -A opt_args\nlocal context state line\n\n_arguments -s -S \\\n" +
		"  \"-t[Timeout in seconds]\" \\\n" +
		"  \"--timeout[Timeout in seconds]\" \\\n" +
		"  \"-h[Print help message and exit]\" \\\n" +
		"  \"--help[Print help message and exit]\" \\\n" +
		"  '*:file:_files' && return 0\n"
}

var integerPattern = regexp.MustCompile(`^[-+]?(?:0[bB][01]+(?:_[01]+)*|0[xX][0-9a-fA-F]+(?:_[0-9a-fA-F]+)*|[0-9]+(?:_[0-9]+)*)$`)

// OptionParser's Integer accepts signs, octal, binary, hex and digit separators.
// Keep arbitrary precision; neither parsing nor expiry narrows to Duration.
func parseInteger(s string) (*big.Int, bool) {
	if !integerPattern.MatchString(s) {
		return nil, false
	}
	s = strings.ReplaceAll(s, "_", "")
	sign := ""
	if s[0] == '+' || s[0] == '-' {
		sign, s = s[:1], s[1:]
	}
	base := 10
	if len(s) > 1 && s[0] == '0' {
		switch s[1] {
		case 'b', 'B':
			base, s = 2, s[2:]
		case 'x', 'X':
			base, s = 16, s[2:]
		default:
			base = 8
		}
	}
	return new(big.Int).SetString(sign+s, base)
}

// Parsing permutes options unless POSIXLY_CORRECT is present, even if empty.
// The finite helper option set stays local, without the core CLI registry.
func parse(argv0 string, args []string, ordered bool) (options, *presentation, error) {
	o := options{timeout: big.NewInt(60)}
	var positional []string
	terminated := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if terminated || arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			terminated = terminated || ordered
			continue
		}
		if arg == "--" {
			terminated = true
			continue
		}
		name, value, attached := "", "", false
		if strings.HasPrefix(arg, "--") {
			raw, v, has := strings.Cut(arg[2:], "=")
			raw = strings.ReplaceAll(raw, "_", "-")
			pattern := completionPattern(raw, true)
			matches := 0
			for _, candidate := range []string{"timeout", "help", "version", "*-completion-bash", "*-completion-zsh"} {
				if candidate == raw {
					name, matches = candidate, 1
					break
				}
				if pattern != nil && pattern.MatchString(candidate) {
					name = candidate
					matches++
				}
			}
			if matches > 1 {
				return o, nil, fmt.Errorf("ambiguous option: %s", arg)
			}
			value, attached = v, has
		} else {
			switch arg[1] {
			case 't':
				name, value, attached = "timeout", arg[2:], len(arg) > 2
			case 'h':
				name = "help"
				if strings.HasPrefix(arg[2:], "=") {
					return o, nil, fmt.Errorf("needless argument: %s", arg)
				}
			case 'v':
				name = "version"
			}
		}
		switch name {
		case "help":
			if attached {
				return o, nil, fmt.Errorf("needless argument: %s", arg)
			}
			return o, &presentation{stdout: help(argv0)}, nil
		case "timeout":
			if !attached {
				if i+1 == len(args) {
					return o, nil, fmt.Errorf("missing argument: %s", arg)
				}
				i++
				value = args[i]
			}
			var ok bool
			o.timeout, ok = parseInteger(value)
			if !ok {
				return o, nil, fmt.Errorf("invalid timeout: %s", value)
			}
		case "version":
			// Package queries deliberately have no Ruby resolver. This policy
			// retains execute's startup banner, including for an empty query.
			return o, &presentation{stderr: programName(argv0) + ": version unknown\n", status: 1}, nil
		case "*-completion-bash":
			if !attached {
				if i+1 == len(args) {
					return o, nil, fmt.Errorf("missing argument: %s", arg)
				}
				i++
				value = args[i]
			}
			return o, &presentation{stdout: bashCompletion(value)}, nil
		case "*-completion-zsh":
			// An omitted OptionalArgument is nil, interpolated as an empty name.
			return o, &presentation{stdout: zshCompletion(value)}, nil
		default:
			return o, nil, fmt.Errorf("invalid option: %s", arg)
		}
	}
	if len(positional) != 3 {
		return o, nil, errArity
	}
	o.toplevel, o.action, o.check = positional[0], positional[1], positional[2]
	return o, nil, nil
}

func writeText(w io.Writer, text string) error {
	n, err := io.WriteString(w, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

// File.join retains internal dots and slashes. Cleaning a symlink-containing
// path would change which executable is selected by the original helper.
func switchPath(toplevel string) string {
	return strings.TrimRight(toplevel, "/") + "/bin/switch-to-configuration"
}

func waitChild(d dependencies, argv []string) error {
	wait, err := d.start(argv)
	if err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	err = wait() // exactly one Wait after a successful Start
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// Normal nonzero or signaled termination is not a wait-system error.
		return nil
	}
	if err != nil {
		return fmt.Errorf("wait %s: %w", argv[0], err)
	}
	return nil
}

func wallNanos(t time.Time) *big.Int {
	n := new(big.Int).Mul(big.NewInt(t.Unix()), big.NewInt(1_000_000_000))
	return n.Add(n, big.NewInt(int64(t.Nanosecond())))
}

func expired(start, now time.Time, seconds *big.Int) bool {
	elapsed := new(big.Int).Sub(wallNanos(now), wallNanos(start))
	limit := new(big.Int).Mul(seconds, big.NewInt(1_000_000_000))
	return elapsed.Cmp(limit) > 0
}

func run(o options, d dependencies) (int, error) {
	current, err := d.readlink("/run/current-system")
	if err != nil {
		return 1, err
	}
	text := fmt.Sprintf("  current system = %s\n  new system     = %s\n  action         = %s\n  check file     = %s\n  timeout        = %s seconds\n\nSwitching to new configuration\n", current, o.toplevel, o.action, o.check, o.timeout)
	if err := writeText(d.out, text); err != nil {
		return 1, err
	}
	if err := d.write(o.check, []byte("switching")); err != nil {
		return 1, err
	}
	if err := waitChild(d, []string{switchPath(o.toplevel), o.action}); err != nil {
		return 1, err
	}
	if err := d.write(o.check, []byte("switched")); err != nil {
		return 1, err
	}
	if err := writeText(d.out, "Switch complete, waiting for confirmation\n"); err != nil {
		return 1, err
	}
	start := d.now()
	for {
		d.sleep(500 * time.Millisecond)
		data, err := d.read(o.check)
		if err != nil {
			return 1, err
		}
		// Ruby String#strip removes these ASCII bytes, including NUL, rather
		// than Go's broader Unicode whitespace set.
		if bytes.Equal(bytes.Trim(data, "\x00\t\n\v\f\r "), []byte("confirmed")) {
			if err := writeText(d.out, "Configuration confirmed\n"); err != nil {
				return 1, err
			}
			if err := d.unlink(o.check); err != nil {
				return 1, err
			}
			return 0, nil
		}
		if expired(start, d.now(), o.timeout) {
			break
		}
	}
	if err := writeText(d.out, "Timeout occurred, rolling back\n"); err != nil {
		return 1, err
	}
	if err := waitChild(d, []string{switchPath(current), o.action}); err != nil {
		return 1, err
	}
	return 1, writeText(d.out, "Rollback complete\n")
}

func inheritedStart(argv []string) (func() error, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Nil Env and empty Dir retain the caller's environment and CWD.
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

func productionDependencies() dependencies {
	return dependencies{
		readlink: os.Readlink, read: os.ReadFile,
		write: func(path string, data []byte) error { return os.WriteFile(path, data, 0666) },
		unlink: func(path string) error {
			if err := syscall.Unlink(path); err != nil {
				return &os.PathError{Op: "unlink", Path: path, Err: err}
			}
			return nil
		},
		now: time.Now, sleep: time.Sleep, start: inheritedStart, out: os.Stdout,
	}
}

func execute(argv0 string, args []string, ordered bool, d dependencies, stderr io.Writer) int {
	if err := writeText(d.out, "Deploying with auto-rollback\n"); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", argv0, err)
		return 1
	}
	o, p, err := parse(argv0, args, ordered)
	if errors.Is(err, errArity) {
		writeText(stderr, "Invalid arguments\n"+help(argv0))
		return 1
	}
	if err == nil && p != nil {
		if err = writeText(d.out, p.stdout); err == nil {
			err = writeText(stderr, p.stderr)
		}
		if err == nil {
			return p.status
		}
	}
	status := 1
	if err == nil {
		status, err = run(o, d)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", argv0, err)
		return 1
	}
	return status
}

func main() {
	_, ordered := os.LookupEnv("POSIXLY_CORRECT")
	os.Exit(execute(os.Args[0], os.Args[1:], ordered, productionDependencies(), os.Stderr))
}
