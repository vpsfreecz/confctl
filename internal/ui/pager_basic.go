package ui

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"strings"
	"unicode/utf8"
)

type BasicSize struct{ Rows, Columns *big.Int }
type BasicIO struct {
	Size func(context.Context) (BasicSize, error)
	Wrap func(string, *big.Int) (string, error)
	Key  func(context.Context) (string, error)
}

type BasicPager struct {
	ctx             context.Context
	output          io.Writer
	ops             BasicIO
	width, height   *big.Int
	page, linesLeft *big.Int
	leftover        []string
}

func basicPrompt(page *big.Int) string {
	return "\n--- Page -" + page.String() + "- Press enter/return to continue (or q to quit) ---"
}

// NewBasicPager samples rows before a separate columns sample. Stored integers
// and Wrap arguments are copied; the callback never receives cursor aliases.
func NewBasicPager(ctx context.Context, output io.Writer, ops BasicIO) (*BasicPager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ops.Size == nil {
		return nil, fmt.Errorf("basic pager size callback is unavailable")
	}
	size, err := ops.Size(ctx)
	if err != nil {
		return nil, err
	}
	if size.Rows == nil {
		return nil, fmt.Errorf("basic pager rows are unavailable")
	}
	rows := new(big.Int).Set(size.Rows)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size, err = ops.Size(ctx)
	if err != nil {
		return nil, err
	}
	if size.Columns == nil {
		return nil, fmt.Errorf("basic pager columns are unavailable")
	}
	p := &BasicPager{ctx: ctx, output: output, ops: ops, width: new(big.Int).Set(size.Columns)}
	prompt, err := p.wrap(basicPrompt(big.NewInt(100)))
	if err != nil {
		return nil, err
	}
	p.height = new(big.Int).Sub(rows, big.NewInt(int64(len(pagerLines(prompt)))))
	p.reset()
	return p, nil
}

func (p *BasicPager) reset() {
	p.page = big.NewInt(1)
	p.linesLeft = new(big.Int).Set(p.height)
	p.leftover = nil
}
func (p *BasicPager) Close() error            { p.reset(); return nil }
func (p *BasicPager) Puts(text string) error  { return p.send(text, true) }
func (p *BasicPager) Write(text string) error { return p.send(text, false) }

func (p *BasicPager) wrap(text string) (string, error) {
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	if p.ops.Wrap == nil {
		return "", fmt.Errorf("basic pager wrap callback is unavailable")
	}
	return p.ops.Wrap(text, new(big.Int).Set(p.width))
}

func (p *BasicPager) emit(text string, puts bool) error {
	if puts {
		text = putsText(text)
	}
	return writePagerText(p.output, text)
}

// This keeps the source's leftover quirk: end-of-call output does not clear or
// debit it. A later input line consumes that already-emitted leftover again.
func (p *BasicPager) send(text string, puts bool) error {
	for _, line := range pagerLines(text) {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		chunk := append([]string(nil), p.leftover...)
		p.leftover = nil // clearing precedes the reached Wrap and its errors
		wrapped, err := p.wrap(line)
		if err != nil {
			return err
		}
		for _, part := range pagerLines(wrapped) {
			if p.linesLeft.Sign() != 0 {
				chunk = append(chunk, part)
				p.linesLeft.Sub(p.linesLeft, big.NewInt(1))
			} else {
				p.leftover = append(p.leftover, part)
			}
		}
		if err := p.emit(strings.Join(chunk, ""), puts); err != nil {
			return err
		}
		if p.linesLeft.Sign() != 0 {
			continue
		}
		prompt, err := p.wrap(basicPrompt(p.page))
		if err != nil {
			return err
		}
		if err := p.emit(prompt, true); err != nil {
			return err
		}
		if p.ops.Key == nil {
			return fmt.Errorf("basic pager key callback is unavailable")
		}
		if err := p.ctx.Err(); err != nil {
			return err
		}
		key, err := p.ops.Key(p.ctx)
		if err != nil {
			return err
		}
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if !utf8.ValidString(key) {
			return fmt.Errorf("%w: key is not valid UTF-8", ErrBasicEncoding)
		}
		if strings.ContainsAny(key, "qQ") {
			return &pagerClosed{message: "The pager tool was closed"}
		}
		p.page.Add(p.page, big.NewInt(1))
		p.linesLeft.Set(p.height)
		p.linesLeft.Sub(p.linesLeft, big.NewInt(int64(len(p.leftover))))
	}
	if len(p.leftover) != 0 {
		return p.emit(strings.Join(p.leftover, ""), puts)
	}
	return nil
}

func pagerLines(text string) []string {
	var lines []string
	for text != "" {
		end := strings.IndexByte(text, '\n')
		if end < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:end+1])
		text = text[end+1:]
	}
	return lines
}
