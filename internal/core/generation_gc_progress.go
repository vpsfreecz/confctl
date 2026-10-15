package core

import (
	"bytes"
	"strings"
)

type gcProgress struct {
	line, path  string
	pathMatched bool
}

func gcParseProgress(raw string) gcProgress {
	p := gcProgress{line: raw}
	line := strings.TrimSuffix(raw, "\n") // Ruby $ permits one final LF, not CRLF.
	if strings.HasPrefix(line, "deleting '/nix/store/") && strings.HasSuffix(line, "'") {
		path := line[len("deleting '") : len(line)-1]
		if len(path) > len("/nix/store/") && !strings.ContainsRune(path, '\'') {
			p.path = path
			p.pathMatched = true
		}
	}
	return p
}

func gcSummaryLine(raw string) bool {
	i := 0
	for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
		i++
	}
	return i > 0 && strings.HasPrefix(raw[i:], " store paths deleted")
}

type gcTaskProgress struct {
	index          int
	host           string
	view           gcView
	tty            bool
	stdout, stderr []byte
	summary        string
}

func (p *gcTaskProgress) line(raw string) error {
	progress := gcParseProgress(raw)
	if err := p.view.Line(p.index, p.host+"> "+progress.line); err != nil {
		return err
	}
	if p.tty && progress.pathMatched {
		return p.view.Advance(p.index)
	}
	if gcSummaryLine(progress.line) {
		p.summary = progress.line
	}
	return nil
}

func (p *gcTaskProgress) feed(stdout, stderr []byte) error {
	p.stdout = append(p.stdout, stdout...)
	p.stderr = append(p.stderr, stderr...)
	for {
		outLF, errLF := bytes.IndexByte(p.stdout, '\n'), bytes.IndexByte(p.stderr, '\n')
		if outLF < 0 && errLF < 0 {
			return nil
		}
		if outLF >= 0 {
			line := string(p.stdout[:outLF+1])
			p.stdout = p.stdout[outLF+1:]
			if err := p.line(line); err != nil {
				return err
			}
		}
		if errLF >= 0 {
			line := string(p.stderr[:errLF+1])
			p.stderr = p.stderr[errLF+1:]
			if err := p.line(line); err != nil {
				return err
			}
		}
	}
}

func (p *gcTaskProgress) finish() error {
	// Both strings are delivered unconditionally, including empty tails. This is
	// source preservation, distinct from the accepted raw-line/path corrections.
	stdout, stderr := string(p.stdout), string(p.stderr)
	p.stdout, p.stderr = nil, nil
	if err := p.line(stdout); err != nil {
		return err
	}
	return p.line(stderr)
}
