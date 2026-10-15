package hookcheck

import (
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

func rubyStrip(s string) string {
	return strings.Trim(s, "\x00\t\n\v\f\r ")
}

func chomp(s string) string {
	if strings.HasSuffix(s, "\n") {
		return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	}
	return strings.TrimSuffix(s, "\r")
}

// CheckMessage reads the literal Git-supplied path. It never rewrites it.
// The caller supplies the selected literal Git comment prefix. This checker
// neither resolves "auto" nor infers a prefix from message content.
func CheckMessage(path, commentPrefix string) ([]Finding, error) {
	if commentPrefix == "" {
		return nil, errors.New("a resolved Git comment prefix is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "diff --git") {
			break
		}
		if !strings.HasPrefix(line, commentPrefix) {
			lines = append(lines, line)
		}
	}
	if rubyStrip(strings.Join(lines, "")) == "" {
		return []Finding{{Check: "EmptyMessage", Message: "commit message should not be empty"}}, nil
	}
	var out []Finding
	if len(lines) > 1 && rubyStrip(lines[1]) != "" {
		out = append(out, Finding{Check: "SingleLineSubject", Line: 2, Warning: true,
			Message: "subject should be followed by a blank line"})
	}
	subject := chomp(lines[0])
	limit := 60
	for _, prefix := range []string{"fixup! ", "squash! "} {
		if strings.HasPrefix(subject, prefix) {
			limit += len(prefix)
		}
	}
	if utf8.RuneCountInString(subject) > limit {
		out = append(out, Finding{Check: "TextWidth", Line: 1, Warning: true,
			Message: "subject exceeds retained character width"})
	}
	for i := 2; i < len(lines); i++ {
		if utf8.RuneCountInString(chomp(lines[i])) > 72 {
			out = append(out, Finding{Check: "TextWidth", Line: i + 1, Warning: true,
				Message: "body line exceeds 72 characters"})
		}
	}
	if strings.HasSuffix(strings.TrimRight(lines[0], "\x00\t\n\v\f\r "), ".") {
		out = append(out, Finding{Check: "TrailingPeriod", Line: 1, Warning: true,
			Message: "omit trailing subject period"})
	}
	return out, nil
}
