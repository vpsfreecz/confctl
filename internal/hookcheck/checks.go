package hookcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Finding struct {
	Check, Path, Message string
	Line                 int
	Warning              bool
}

func HasErrors(findings []Finding) bool {
	for _, f := range findings {
		if !f.Warning {
			return true
		}
	}
	return false
}

type CheckOptions struct {
	Nixfmt, Gofmt, RuboCop string
	ServiceRuby            bool
	Invoke                 Invoke
}

// Ruby's ^/$ anchors are line anchors, including without an explicit /m flag.
var authorEmail = regexp.MustCompile(`(?m)^[^@]+@.*$`)
var mergeMarker = regexp.MustCompile(`(?m)^<<<<<<<[ \t]`)
var rubyMessage = regexp.MustCompile(`^((?:\w:)?[^:]+):(\d+):[^ ]+ ([^ ]+)`)
var genericRubyMessage = regexp.MustCompile(`^[a-zA-Z]+`)

func envValue(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], key+"=") {
			return strings.TrimPrefix(env[i], key+"="), true
		}
	}
	return "", false
}

// Tool calls run in the staged tree, without inherited real Git-directory/index
// bindings. The discovery ceiling also prevents a nested temp parent finding Git.
func (s *Snapshot) toolEnv() []string {
	var out []string
	for _, pair := range s.env {
		key, _, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(key, "GIT_") && key != "GIT_AUTHOR_NAME" && key != "GIT_AUTHOR_EMAIL" &&
			key != "GIT_COMMITTER_NAME" && key != "GIT_COMMITTER_EMAIL" {
			continue
		}
		out = append(out, pair)
	}
	return envSet(out, "GIT_CEILING_DIRECTORIES", filepath.Dir(s.dir))
}

// Ruby's default Unicode downcase uses the full dotted-I mapping; its remaining
// simple mappings are Unicode lowercase (not case folding: sharp-s is not "ss").
func lowerPath(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, "İ", "i\u0307"))
}

func caseFindings(s *Snapshot) []Finding {
	dirs := make(map[string]bool)
	names := make(map[string]bool)
	for _, name := range s.selected {
		dirs[filepath.Dir(name)] = true
		names[name] = true
	}
	for _, name := range s.headPaths {
		if dirs[filepath.Dir(name)] {
			names[name] = true
		}
	}
	groups := make(map[string][]string)
	for name := range names {
		key := lowerPath(name)
		groups[key] = append(groups[key], name)
	}
	var findings []Finding
	for _, name := range s.selected {
		conflicts := groups[lowerPath(name)]
		if len(conflicts) > 1 {
			sort.Strings(conflicts)
			findings = append(findings, Finding{Check: "CaseConflicts", Path: name,
				Message: fmt.Sprintf("case-insensitive path conflict: %q", conflicts)})
		}
	}
	return findings
}

func rubyPath(path string) bool {
	switch filepath.Ext(path) {
	case ".gemspec", ".rake", ".rb", ".ru":
		return true
	}
	return filepath.Base(path) == "Gemfile" || filepath.Base(path) == "Rakefile"
}

// CheckFiles retains the finite predecessor predicates and report-on-unmodified
// Ruby messages. Infrastructure failures are errors, not empty successful reports.
func CheckFiles(ctx context.Context, s *Snapshot, o CheckOptions) ([]Finding, error) {
	if s == nil || s.dir == "" {
		return nil, errors.New("an open staged snapshot is required")
	}
	var findings []Finding
	for _, rule := range []struct{ env, config, check string }{
		{"GIT_AUTHOR_EMAIL", "user.email", "AuthorEmail"},
		{"GIT_AUTHOR_NAME", "user.name", "AuthorName"},
	} {
		value, present := envValue(s.env, rule.env)
		if !present {
			value = s.config[rule.config]
		}
		valid := value != ""
		if rule.check == "AuthorEmail" {
			valid = authorEmail.MatchString(value)
		}
		if !valid {
			findings = append(findings, Finding{Check: rule.check, Message: "invalid author value"})
		}
	}
	findings = append(findings, caseFindings(s)...)
	var nixFiles, goFiles, rubyFiles []string
	for _, name := range s.selected {
		if err := ctx.Err(); err != nil {
			return findings, err
		}
		literal := filepath.Join(s.dir, name)
		info, err := os.Lstat(literal)
		if err != nil {
			return findings, fmt.Errorf("read selected %q: %w", name, err)
		}
		resolved, err := s.confinedPath(name)
		if err != nil {
			if info.Mode()&os.ModeSymlink != 0 && errors.Is(err, fs.ErrNotExist) {
				findings = append(findings, Finding{Check: "BrokenSymlinks", Path: name, Message: "dangling staged symlink"})
				continue
			}
			return findings, fmt.Errorf("resolve selected %q: %w", name, err)
		}
		target, err := os.Stat(resolved)
		if err != nil {
			return findings, err
		}
		if info.Mode()&os.ModeSymlink != 0 && target.Size() == 0 {
			findings = append(findings, Finding{Check: "BrokenSymlinks", Path: name, Message: "staged symlink target has zero size"})
		}
		if target.IsDir() {
			continue
		}
		if !target.Mode().IsRegular() {
			return findings, fmt.Errorf("selected target is not a regular file: %q", name)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return findings, fmt.Errorf("read selected %q: %w", name, err)
		}
		// Source grep -I under the declared UTF-8 locale excludes NUL and invalid
		// encoding; marker input errors above remain refusals rather than passes.
		if !bytes.ContainsRune(data, '\x00') && utf8.Valid(data) && mergeMarker.Match(data) {
			findings = append(findings, Finding{Check: "MergeConflicts", Path: name, Message: "line-start merge conflict marker"})
		}
		// Keep literal selected filenames in argv, including their symlink spelling;
		// containment was checked before any executable sees them.
		if strings.HasSuffix(name, ".nix") {
			nixFiles = append(nixFiles, literal)
		}
		if strings.HasSuffix(name, ".go") {
			goFiles = append(goFiles, literal)
		}
		if rubyPath(name) {
			rubyFiles = append(rubyFiles, literal)
		}
	}
	if len(rubyFiles) > 0 && !o.ServiceRuby {
		findings = append(findings, Finding{Check: "RuboCop", Message: "staged Ruby requires the explicit service-checks lane"})
	}
	if HasErrors(findings) {
		return findings, nil
	}
	invoke := func(program string, args []string) (ToolResult, error) {
		if program == "" || o.Invoke == nil {
			return ToolResult{}, errors.New("selected check requires its declared executable and invocation adapter")
		}
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		return o.Invoke(ctx, Invocation{Program: program, Args: args, Dir: s.dir, Env: s.toolEnv()})
	}
	for _, tool := range []struct {
		name, program string
		flags, files  []string
	}{
		{"Nixfmt", o.Nixfmt, []string{"--check"}, nixFiles},
		{"Gofmt", o.Gofmt, []string{"-l"}, goFiles},
	} {
		if len(tool.files) == 0 {
			continue
		}
		sort.Strings(tool.files)
		result, err := invoke(tool.program, append(tool.flags, tool.files...))
		if err != nil {
			return findings, fmt.Errorf("%s invocation: %w", tool.name, err)
		}
		if result.ExitCode != 0 || (tool.name == "Gofmt" && len(result.Stdout) > 0) {
			findings = append(findings, Finding{Check: tool.name,
				Message: fmt.Sprintf("check status %d: %s%s", result.ExitCode, result.Stdout, result.Stderr)})
		}
	}
	if len(rubyFiles) > 0 {
		sort.Strings(rubyFiles)
		args := append([]string{"--format=emacs", "--force-exclusion", "--display-cop-names"}, rubyFiles...)
		r, err := invoke(o.RuboCop, args)
		if err != nil {
			return findings, fmt.Errorf("RuboCop invocation: %w", err)
		}
		if r.ExitCode != 0 {
			messages := parseRubyMessages(r)
			if len(messages) == 0 {
				return findings, fmt.Errorf("unparseable RuboCop failure (status %d): %s%s", r.ExitCode, r.Stdout, r.Stderr)
			}
			findings = append(findings, messages...)
		}
	}
	if err := ctx.Err(); err != nil {
		return findings, err
	}
	return findings, nil
}

func parseRubyMessages(r ToolResult) []Finding {
	var out []Finding
	for _, line := range strings.Split(string(r.Stderr), "\n") {
		if kind := genericRubyMessage.FindString(line); kind != "" {
			out = append(out, Finding{Check: "RuboCop", Message: line,
				Warning: strings.HasPrefix(kind, "warn")})
		}
	}
	for _, line := range strings.Split(string(r.Stdout), "\n") {
		if match := rubyMessage.FindStringSubmatch(line); match != nil {
			number, _ := strconv.Atoi(match[2])
			out = append(out, Finding{Check: "RuboCop", Path: match[1], Line: number,
				Message: line, Warning: strings.Contains(match[3], "W")})
		}
	}
	return out
}
