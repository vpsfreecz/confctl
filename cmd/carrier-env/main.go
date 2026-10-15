// carrier-env keeps the profile update and its callback sequential. Diagnostics
// intentionally omit Ruby exception wrappers; a failed callback never reverses
// the first effect. Old generations keep their original helper and presentation.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// The package binds this private executable path. It is not a CLI/env override.
var onChangeCommands string

type action uint8

const (
	noAction action = iota
	setProfile
	deleteGenerations
)

type options struct {
	action         action
	profile        string
	profilePresent bool
	generation     string
}

type presentation struct {
	stdout string
	stderr string
	status int
}

func help(argv0 string) string {
	return "Usage: " + argv0 + " <options>\n" +
		"    -p, --profile PROFILE            Profile path\n" +
		"        --set GENERATION             Set profile to generation\n" +
		"        --delete-generations GENERATIONS\n" +
		"                                     Delete generations\n"
}

func leftover(argv0 string) presentation {
	return presentation{stdout: "Usage: " + argv0 + " <options>\n" +
		"  -p, --profile PROFILE: Profile path\n" +
		"  --set GENERATION: Set profile to generation\n" +
		"  --delete-generations GENERATIONS: Delete generations\n", stderr: "Too many arguments\n", status: 1}
}

func programName(argv0 string) string {
	name := filepath.Base(argv0)
	if dot := strings.LastIndexByte(name, '.'); dot > 0 && name != ".." {
		name = name[:dot]
	}
	return name
}

var completionWords = regexp.MustCompile(`[A-Za-z0-9_]+\b`)

// The pinned OptionParser expands each quoted word segment with \w*, anchored
// at the beginning. Long spellings fold case; short completion does not.
func completionPattern(key string, foldCase bool) *regexp.Regexp {
	pattern := "^" + completionWords.ReplaceAllString(regexp.QuoteMeta(key), "${0}[A-Za-z0-9_]*")
	if foldCase {
		pattern = "(?i)" + pattern
	}
	// A malformed UTF-8 spelling cannot form a Go regexp. Keep the prior
	// helper's ordinary invalid-option path instead of introducing a panic.
	compiled, _ := regexp.Compile(pattern)
	return compiled
}

func longMatches(key string) []string {
	pattern := completionPattern(key, true)
	if pattern == nil {
		return nil
	}
	var matches []string
	for _, candidate := range longOptions {
		if candidate == key { // exact spelling precedes expanded candidates
			return []string{candidate}
		}
		if pattern.MatchString(candidate) {
			matches = append(matches, candidate)
		}
	}
	return matches
}

func bashCompletion(word string) string {
	// Only explicit helper options are offered, in declaration order. The
	// dependency's finite candidate surface distinguishes short/long prefixes.
	long, short := strings.HasPrefix(word, "--"), strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--")
	if word == "-" {
		long, short = true, true
	}
	word, _, _ = strings.Cut(word, "=")
	pattern := completionPattern(word, long)
	var b strings.Builder
	for _, name := range []string{"--profile", "-p", "--set", "--delete-generations"} {
		isLong := strings.HasPrefix(name, "--")
		if pattern != nil && ((long && isLong) || (short && !isLong)) && pattern.MatchString(name) {
			b.WriteString(name + "\n")
		}
	}
	return b.String()
}

func zshCompletion(name string) string {
	return "#compdef " + name + "\n\n" +
		"typeset -A opt_args\nlocal context state line\n\n_arguments -s -S \\\n" +
		"  \"-p[Profile path]\" \\\n" +
		"  \"--profile[Profile path]\" \\\n" +
		"  \"--set[Set profile to generation]\" \\\n" +
		"  \"--delete-generations[Delete generations]\" \\\n" +
		"  '*:file:_files' && return 0\n"
}

// This is the helper's finite option set, not another general CLI registry.
var longOptions = [...]string{"profile", "set", "delete-generations", "help", "version", "*-completion-bash", "*-completion-zsh"}

func parse(argv0 string, argv []string, ordered bool) (options, *presentation) {
	var opts options
	operands := []string{}
	for i := 0; i < len(argv); i++ {
		token := argv[i]
		if token == "--" {
			operands = append(operands, argv[i+1:]...)
			break
		}
		if token == "-" || !strings.HasPrefix(token, "-") {
			operands = append(operands, token)
			if ordered {
				operands = append(operands, argv[i+1:]...)
				break
			}
			continue
		}
		name, value, attached := "", "", false
		label := token
		if strings.HasPrefix(token, "--") {
			raw, v, equal := strings.Cut(token[2:], "=")
			normalized := strings.ReplaceAll(raw, "_", "-")
			label = "--" + raw
			matches := longMatches(normalized)
			if len(matches) == 0 {
				p := presentation{stderr: "invalid option: " + label + "\n", status: 1}
				return opts, &p
			}
			if len(matches) != 1 {
				message := "ambiguous option: " + label + "\n"
				if normalized == "*-completion" {
					message += "Did you mean?  *-completion-zsh\n"
				}
				p := presentation{stderr: message, status: 1}
				return opts, &p
			}
			name, value, attached = matches[0], v, equal
		} else {
			switch token[1] {
			case 'p':
				name, value, attached, label = "profile", token[2:], len(token) > 2, "-p"
			case 'h':
				name, label = "help", "-h"
			default:
				p := presentation{stderr: "invalid option: -" + string(token[1]) + "\n", status: 1}
				return opts, &p
			}
		}
		required := name == "profile" || name == "set" || name == "delete-generations" || name == "*-completion-bash"
		if required && !attached {
			i++
			if i == len(argv) {
				p := presentation{stderr: "missing argument: " + label + "\n", status: 1}
				return opts, &p
			}
			value = argv[i]
		}
		switch name {
		case "profile":
			opts.profile, opts.profilePresent = value, true
		case "set":
			opts.action, opts.generation = setProfile, value
		case "delete-generations":
			opts.action, opts.generation = deleteGenerations, value
		case "help":
			if attached {
				p := presentation{stderr: "needless argument: " + token + "\n", status: 1}
				return opts, &p
			}
			p := presentation{stdout: help(argv0)}
			return opts, &p
		case "version":
			// Package-query loading is intentionally unsupported: the selected Go
			// policy uses the same unknown-version result, without a resolver.
			p := presentation{stderr: programName(argv0) + ": version unknown\n", status: 1}
			return opts, &p
		case "*-completion-bash":
			p := presentation{stdout: bashCompletion(value)}
			return opts, &p
		case "*-completion-zsh":
			// OptionalArgument passes nil when omitted; interpolation is empty
			// rather than invoking compsys's default argument.
			p := presentation{stdout: zshCompletion(value)}
			return opts, &p
		}
	}
	if len(operands) != 0 {
		p := leftover(argv0)
		return opts, &p
	}
	if opts.action == noAction {
		p := presentation{stderr: "No action specified\n", status: 1}
		return opts, &p
	}
	if !opts.profilePresent {
		p := presentation{stderr: "Profile not set\n", status: 1}
		return opts, &p
	}
	return opts, nil
}

func run(argv0 string, argv []string, ordered bool, callback string, execute func(string, []string) error, stdout, stderr io.Writer) int {
	opts, result := parse(argv0, argv, ordered)
	if result != nil {
		fmt.Fprint(stdout, result.stdout)
		fmt.Fprint(stderr, result.stderr)
		return result.status
	}
	flag := "--set"
	if opts.action == deleteGenerations {
		flag = "--delete-generations"
	}
	args := []string{"-p", opts.profile, flag, opts.generation}
	if err := execute("nix-env", args); err != nil {
		fmt.Fprintln(stderr, strings.Join(append([]string{"nix-env"}, args...), " ")+" failed")
		return 1
	}
	// No preflight: callback failure must occur after the successful profile call.
	if err := execute(callback, []string{}); err != nil {
		fmt.Fprintln(stderr, callback+" failed")
		return 1
	}
	return 0
}

func runInherited(program string, args []string) error {
	cmd := exec.Command(program, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Dir and Env are unset: inherit the caller's CWD/environment and PATH lookup.
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

func main() {
	_, ordered := os.LookupEnv("POSIXLY_CORRECT")
	os.Exit(run(os.Args[0], os.Args[1:], ordered, onChangeCommands, runInherited, os.Stdout, os.Stderr))
}
