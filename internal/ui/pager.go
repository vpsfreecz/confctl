// Package ui owns the private comparison pager's selection and text effects.
// Children, terminal descriptors and production discovery belong to core.
package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
)

var ErrPagerClosed = errors.New("pager closed")

type pagerClosed struct{ message string }

func (e *pagerClosed) Error() string { return e.message }
func (e *pagerClosed) Unwrap() error { return ErrPagerClosed }

type PagerSink interface{ Puts(string) error }
type PagerPipe interface {
	Write([]byte) (int, error)
	Close() error
}
type PagerSinkCloser interface {
	PagerSink
	Close() error
}
type PagerOptions struct {
	Enabled   bool
	Output    io.Writer
	OutputTTY func() bool
}
type PagerOps struct {
	Env    func(string) (string, bool)
	Exists func(string) bool
	// Ordinary completed Git-config status is ignored by the adapter; only
	// construction/infrastructure/cancellation failures are errors here.
	GitConfig func(context.Context) (string, error)
	// Clean means exit0 and exactly empty stderr; captured stdout is ignored.
	Probe func(context.Context, string) (clean bool, err error)
	// The adapter owns process startup and maps ordinary completed Close
	// statuses (including nonzero/signal) to nil, retaining its result itself.
	Start func(context.Context, string) (PagerPipe, error)
	Basic func(context.Context, io.Writer) (PagerSinkCloser, error)
}

// Page closes an acquired pager even after a failed body. The source ensure
// gives close errors precedence; invocation cancellation remains distinguishable.
func Page(ctx context.Context, options PagerOptions, ops PagerOps, body func(PagerSink) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var pager PagerSinkCloser
	if !options.Enabled {
		pager = &nullPager{output: options.Output, tty: options.OutputTTY}
	} else {
		command, err := findPager(ctx, ops)
		if err != nil {
			return err
		}
		if command == "" {
			if ops.Basic == nil {
				return fmt.Errorf("basic pager callback is unavailable")
			}
			pager, err = ops.Basic(ctx, options.Output)
			if err != nil {
				return err
			}
		} else {
			// Class selection and SystemPager construction each discover eagerly.
			command, err = findPager(ctx, ops)
			if err != nil {
				return err
			}
			if command == "" {
				return fmt.Errorf("TTY::Pager::SystemPager cannot be used on your system due to lack of appropriate pager executable. Install `less` like pager or try using `BasicPager` instead.")
			}
			pager = &systemPager{ctx: ctx, command: command, ops: ops}
		}
	}
	if pager == nil {
		return fmt.Errorf("basic pager callback returned no pager")
	}
	var bodyErr error
	if body == nil {
		bodyErr = fmt.Errorf("pager body is unavailable")
	} else {
		bodyErr = body(pager)
	}
	closeErr := pager.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if errors.Is(bodyErr, ErrPagerClosed) {
		return nil
	}
	return bodyErr
}

type nullPager struct {
	output io.Writer
	tty    func() bool
}

func (p *nullPager) Puts(text string) error {
	if p.tty == nil {
		return fmt.Errorf("pager output TTY callback is unavailable")
	}
	if !p.tty() {
		return nil
	}
	return writePagerText(p.output, putsText(text))
}
func (*nullPager) Close() error { return nil }

func putsText(text string) string {
	if !strings.HasSuffix(text, "\n") {
		return text + "\n"
	}
	return text
}

func writePagerText(output io.Writer, text string) error {
	if output == nil {
		return fmt.Errorf("pager output is unavailable")
	}
	n, err := io.WriteString(output, text)
	if err == nil && n != len(text) {
		return io.ErrShortWrite
	}
	return err
}

type systemPager struct {
	ctx     context.Context
	command string
	live    string
	ops     PagerOps
	pipe    PagerPipe
}

func (p *systemPager) Puts(text string) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.pipe == nil {
		if p.ops.Probe == nil || p.ops.Start == nil {
			return fmt.Errorf("system pager process callbacks are unavailable")
		}
		clean, err := p.ops.Probe(p.ctx, p.command)
		if err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
		p.live = p.command
		if !clean || err != nil {
			p.live = firstWord(p.command)
		}
		if err := p.ctx.Err(); err != nil {
			return err
		}
		p.pipe, err = p.ops.Start(p.ctx, p.live)
		if err != nil {
			return err
		}
		if p.pipe == nil {
			return fmt.Errorf("system pager start returned no pipe")
		}
	}
	data := []byte(putsText(text))
	n, err := p.pipe.Write(data)
	if errors.Is(err, syscall.EPIPE) {
		// PagerIO stores spawn_pager's final live string, including fallback.
		return &pagerClosed{message: "The pager process (`" + p.live + "`) was closed"}
	}
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (p *systemPager) Close() error {
	if p.pipe == nil {
		return nil
	}
	pipe := p.pipe
	p.pipe = nil // exactly one resource close; adapters own their sole Wait
	return pipe.Close()
}

func rubySpace(b byte) bool { return b == ' ' || b >= '\t' && b <= '\r' }
func rubyStrip(text string) string {
	return strings.TrimFunc(text, func(r rune) bool { return r == 0 || r == ' ' || r >= '\t' && r <= '\r' })
}
func firstWord(text string) string {
	start := 0
	for start < len(text) && rubySpace(text[start]) {
		start++
	}
	end := start
	for end < len(text) && !rubySpace(text[end]) {
		end++
	}
	return text[start:end]
}

func rubyPathParts(text string) []string {
	parts := strings.Split(text, ":")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func pagerEnv(ops PagerOps, name string) string {
	value, present := ops.Env(name)
	if !present {
		return ""
	}
	return value
}

func commandExists(ctx context.Context, ops PagerOps, command string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if ops.Exists == nil {
		return false, fmt.Errorf("pager existence callback is unavailable")
	}
	ext := pagerEnv(ops, "PATHEXT")
	exts := rubyPathParts(ext)
	check := func(path string) bool {
		if ops.Exists(path) {
			return true
		}
		for _, suffix := range exts {
			if ops.Exists(path + suffix) {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(command, "/") {
		return check(command), nil
	}
	path := pagerEnv(ops, "PATH")
	for _, dir := range rubyPathParts(path) {
		// Ruby File.join retains trailing slashes for a relative command.
		// An empty PATH component joins to "/command", not a cwd path.
		file := dir
		if !strings.HasSuffix(file, "/") {
			file += "/"
		}
		file += command
		if check(file) {
			return true, nil
		}
	}
	return false, nil
}

func findPager(ctx context.Context, ops PagerOps) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ops.Env == nil {
		return "", fmt.Errorf("pager environment callback is unavailable")
	}
	gitPager := pagerEnv(ops, "GIT_PAGER")
	pager := pagerEnv(ops, "PAGER")
	gitExists, err := commandExists(ctx, ops, "git")
	if err != nil {
		return "", err
	}
	configured := ""
	if gitExists {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if ops.GitConfig == nil {
			return "", fmt.Errorf("pager Git configuration callback is unavailable")
		}
		configured, err = ops.GitConfig(ctx)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	seen := make(map[string]bool)
	for _, candidate := range []string{gitPager, pager, configured, "less -r", "more -r", "most", "pg", "cat", "pager"} {
		candidate = rubyStrip(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		exists, err := commandExists(ctx, ops, firstWord(candidate))
		if err != nil {
			return "", err
		}
		if exists {
			return candidate, nil
		}
	}
	return "", nil
}
