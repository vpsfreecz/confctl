// Package docgen renders declaration-backed Markdown without evaluating Nix.
package docgen

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Fixed context supplements declarative Help without another command catalogue.
//
//go:embed command-intro.md
var commandIntro string

//go:embed command-outro.md
var commandOutro string

// Metadata is supplied by the caller; generation never reads the clock.
type Metadata struct {
	Version string
	Date    string
}

func (m Metadata) validate() error {
	if m.Version == "" || strings.ContainsAny(m.Version, "\r\n") {
		return fmt.Errorf("version must be nonempty and fit on one line")
	}
	if _, err := time.Parse("2006-01-02", m.Date); err != nil {
		return fmt.Errorf("date must be YYYY-MM-DD: %w", err)
	}
	return nil
}

func (m Metadata) header(name string) string {
	// JSON string syntax is compatible with YAML double-quoted scalars. Go's
	// %q would introduce control escapes that YAML does not recognize.
	quote := func(text string) string { data, _ := json.Marshal(text); return string(data) }
	return fmt.Sprintf("---\ntitle: %s\nsection: \"8\"\ndate: %s\nfooter: %s\nheader: \"confctl\"\n---\n", quote(name), quote(m.Date), quote(m.Version))
}

// Commands uses Registry.Help for all synopsis, option and availability text.
// Neither an extension executable nor a configuration loader is invoked here.
func Commands(registry *cli.Registry, metadata Metadata) ([]byte, error) {
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, fmt.Errorf("command registry is nil")
	}
	commands := registry.Commands()
	sort.SliceStable(commands, func(i, j int) bool {
		return strings.Join(commands[i].Path, " ") < strings.Join(commands[j].Path, " ")
	})
	var b strings.Builder
	b.WriteString(metadata.header("confctl"))
	b.WriteByte('\n')
	b.WriteString(commandIntro)
	b.WriteString("\n# COMMANDS\n")
	for _, command := range commands {
		help, err := registry.Help(command.Path, 80)
		if err != nil {
			return nil, err
		}
		name := "confctl"
		if len(command.Path) > 0 {
			name += " " + strings.Join(command.Path, " ")
		}
		fmt.Fprintf(&b, "\n## %s\n\n", codeSpan(name))
		b.WriteString(codeBlock(help, "text"))
	}
	b.WriteByte('\n')
	b.WriteString(commandOutro)
	return []byte(b.String()), nil
}

func longestBackticks(s string) int {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// codeSpan supplies single-line terms and inline description literals. Padding
// protects edge backticks/spaces; multiline display literals use codeBlock.
func codeSpan(s string) string {
	delimiter := strings.Repeat("`", longestBackticks(s)+1)
	padding := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.Trim(s, " ") != "") {
		padding = " "
	}
	return delimiter + padding + s + padding + delimiter
}

// codeBlock retains complete literal bytes. Only a missing line separator
// before the closing fence is added; a zero-length literal needs none.
func codeBlock(text, language string) string {
	fence := strings.Repeat("`", max(3, longestBackticks(text)+1))
	separator := ""
	if text != "" && !strings.HasSuffix(text, "\n") {
		separator = "\n"
	}
	return fence + language + "\n" + text + separator + fence + "\n"
}
