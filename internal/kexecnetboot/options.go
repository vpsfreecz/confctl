// Package kexecnetboot is the standalone helper's private implementation.
package kexecnetboot

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type Options struct {
	Server, Machine, Generation, Variant *string
	Interactive, Exec, Unload            bool
	Append                               string
}
type Presentation struct {
	Stdout, Stderr string
	Status         int
}
type option struct {
	short                       byte
	long, argument, description string
}

var declared = []option{
	{'s', "server-url", "URL", "Specify URL of the netboot server"},
	{'m', "machine", "FQDN", "Select machine by FQDN"},
	{'g', "generation", "N", "Select generation by number or negative offset"},
	{'v', "variant", "NAME", "Select a specific variant by name"},
	{'i', "interactive", "", "Enable interactive mode"},
	{'a', "append", "PARAMS", "Append parameters to kernel command line"},
	{'e', "exec", "", "Run the currently loaded kernel"},
	{'u', "unload", "", "Unload the current kexec target kernel and exit"},
	{'y', "no-sync", "", "Skip syncing filesystems"},
}
var words = regexp.MustCompile(`[A-Za-z0-9_]+\b`)

func completionPattern(key string, fold bool) *regexp.Regexp {
	p := "^" + words.ReplaceAllString(regexp.QuoteMeta(key), "${0}[A-Za-z0-9_]*")
	if fold {
		p = "(?i)" + p
	}
	r, _ := regexp.Compile(p)
	return r
}
func matchLong(key string) []string {
	p := completionPattern(key, true)
	if p == nil {
		return nil
	}
	var out []string
	// Declared switches form the first OptionParser table; implicit switches
	// are considered only when that table has no match (e.g. --v is variant).
	for _, table := range [][]string{longNames(), {"help", "version", "*-completion-bash", "*-completion-zsh"}} {
		for _, name := range table {
			if name == key {
				return []string{name}
			}
			if p.MatchString(name) {
				out = append(out, name)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}
func longNames() []string {
	out := make([]string, len(declared))
	for i, o := range declared {
		out[i] = o.long
	}
	return out
}

func Help(program string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: %s [options]\n", program)
	for _, o := range declared {
		s := fmt.Sprintf("    -%c, --%s", o.short, o.long)
		if o.argument != "" {
			s += " " + o.argument
		}
		fmt.Fprintf(&b, "%-36s %s\n", s, o.description)
	}
	return b.String()
}
func bashCompletion(word string) string {
	word, _, _ = strings.Cut(word, "=")
	long := strings.HasPrefix(word, "--")
	short := strings.HasPrefix(word, "-") && !long
	if word == "-" {
		long, short = true, true
	}
	p := completionPattern(word, long)
	var b strings.Builder
	for _, o := range declared {
		for _, name := range []string{"--" + o.long, "-" + string(o.short)} {
			isLong := strings.HasPrefix(name, "--")
			if p != nil && ((long && isLong) || (short && !isLong)) && p.MatchString(name) {
				b.WriteString(name + "\n")
			}
		}
	}
	return b.String()
}
func zshCompletion(name string) string {
	var b strings.Builder
	b.WriteString("#compdef " + name + "\n\ntypeset -A opt_args\nlocal context state line\n\n_arguments -s -S \\\n")
	for _, o := range declared {
		fmt.Fprintf(&b, "  \"-%c[%s]\" \\\n  \"--%s[%s]\" \\\n", o.short, o.description, o.long, o.description)
	}
	b.WriteString("  '*:file:_files' && return 0\n")
	return b.String()
}

// The finite helper options retain OptionParser permutation, word-segment
// expansion and opaque values. Leftover operands are intentionally ignored.
func Parse(program string, argv []string, ordered bool) (Options, *Presentation) {
	var out Options
	args := append([]string(nil), argv...)
	fail := func(message string) (Options, *Presentation) {
		return out, &Presentation{Stderr: message + "\n", Status: 1}
	}
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			break
		}
		if token == "-" || !strings.HasPrefix(token, "-") {
			if ordered {
				break
			}
			continue
		}
		name, value, label := "", "", token
		attached := false
		isLong := strings.HasPrefix(token, "--")
		if isLong {
			raw, v, eq := strings.Cut(token[2:], "=")
			label = "--" + raw
			matches := matchLong(strings.ReplaceAll(raw, "_", "-"))
			if len(matches) == 0 {
				return fail("invalid option: " + label)
			}
			if len(matches) > 1 {
				message := "ambiguous option: " + label
				if strings.ReplaceAll(raw, "_", "-") == "*-completion" {
					message += "\nDid you mean?  *-completion-zsh"
				}
				return fail(message)
			}
			name, value, attached = matches[0], v, eq
		} else {
			label = "-" + string(token[1])
			if token[1] == 'h' {
				name = "help"
			} else {
				for _, o := range declared {
					if o.short == token[1] {
						name = o.long
						break
					}
				}
			}
			if name == "" {
				return fail("invalid option: " + label)
			}
			value = token[2:]
			attached = len(value) > 0
		}
		required := name == "*-completion-bash"
		for _, o := range declared {
			if o.long == name {
				required = o.argument != ""
			}
		}
		if required && !attached {
			i++
			if i == len(args) {
				return fail("missing argument: " + label)
			}
			value = args[i]
		}
		if !required && attached && isLong && name != "version" && name != "*-completion-zsh" {
			return fail("needless argument: " + token)
		}
		if !required && !isLong && attached {
			args = append(args[:i+1], append([]string{"-" + value}, args[i+1:]...)...)
			value = ""
		}
		switch name {
		case "server-url":
			out.Server = &value
		case "machine":
			out.Machine = &value
		case "generation":
			out.Generation = &value
		case "variant":
			out.Variant = &value
		case "append":
			out.Append = value
		case "interactive":
			out.Interactive = true
		case "exec":
			out.Exec = true
		case "unload":
			out.Unload = true
		case "no-sync": // false is already the source default
		case "help":
			return out, &Presentation{Stdout: Help(program)}
		case "version":
			return out, &Presentation{Stderr: strings.TrimSuffix(filepath.Base(program), filepath.Ext(program)) + ": version unknown\n", Status: 1}
		case "*-completion-bash":
			return out, &Presentation{Stdout: bashCompletion(value)}
		case "*-completion-zsh":
			return out, &Presentation{Stdout: zshCompletion(value)}
		}
	}
	return out, nil
}
