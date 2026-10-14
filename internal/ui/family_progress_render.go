package ui

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

func (p *FamilyProgress) measure(text string) (int, error) {
	if !utf8.ValidString(text) {
		return 0, fmt.Errorf("family progress rendering requires UTF-8")
	}
	if p.ops.Measure == nil {
		return 0, fmt.Errorf("family progress measurement is unavailable")
	}
	w, err := p.ops.Measure(text)
	if err != nil {
		return 0, err
	}
	if w < 0 {
		return 0, fmt.Errorf("family progress measurement is negative")
	}
	return w, nil
}

func (b *FamilyBar) format(inset int) (string, error) {
	if !utf8.ValidString(b.label) {
		return "", fmt.Errorf("family progress rendering requires UTF-8")
	}
	if b.mode == FamilyLiteral {
		return b.label, nil
	}
	before, after := b.label+" [", "] "+b.current.String()
	if b.mode == FamilyCountsPercent {
		total, percent := "-", "-"
		if b.total != nil {
			total = b.total.String()
			ratio, err := b.ratio()
			if err != nil {
				return "", err
			}
			percent = strconv.Itoa(int(ratio * 100))
		}
		after += "/" + total + " (" + percent + "%)"
	}
	if b.owner.ops.Columns == nil {
		return "", fmt.Errorf("family progress columns are unavailable")
	}
	columns, err := b.owner.ops.Columns()
	if err != nil {
		return "", err
	}
	if columns == nil {
		return "", fmt.Errorf("family progress columns are nil")
	}
	available := new(big.Int).Set(columns)
	textWidth, err := b.owner.measure(before + after)
	if err != nil {
		return "", err
	}
	available.Sub(available, new(big.Int).SetUint64(uint64(textWidth)))
	available.Sub(available, new(big.Int).SetUint64(uint64(inset)))
	width := 0
	if available.Sign() > 0 {
		width = 80
		if available.Cmp(big.NewInt(80)) < 0 {
			width = int(available.Int64())
		}
	}
	complete := 0
	if b.total != nil {
		ratio, e := b.ratio()
		if e != nil {
			return "", e
		}
		complete = int(math.Round(float64(width) * ratio))
	}
	// Preserve each pinned formatter measurement, including the repeated head.
	glyphs := []string{"=", " ", "="}
	if b.total == nil {
		glyphs = append([]string{"<=>"}, glyphs...)
	}
	for _, glyph := range glyphs {
		w, e := b.owner.measure(glyph)
		if e != nil {
			return "", e
		}
		if w != len(glyph) {
			return "", fmt.Errorf("family progress glyph measurement is incompatible")
		}
	}
	bar := ""
	if b.total == nil {
		ratio, e := b.ratio()
		if e != nil {
			return "", e
		}
		possible := width - 3
		complete = int(math.Round(float64(possible) * ratio))
		if complete < 0 || possible-complete < 0 {
			return "", fmt.Errorf("family progress indeterminate bar repetition is negative")
		}
		bar = strings.Repeat(" ", complete) + "<=>" + strings.Repeat(" ", possible-complete)
	} else {
		bar = strings.Repeat("=", complete) + strings.Repeat(" ", width-complete)
	}
	return before + bar + after, nil
}

func (b *FamilyBar) render(scope *LiveConsoleScope) error {
	inset := 0
	if b.multi != nil {
		w, err := b.owner.measure(b.multi.inset(b))
		if err != nil {
			return err
		}
		inset = w
	}
	text, err := b.format(inset)
	if err != nil {
		return err
	}
	w, err := b.owner.measure(text)
	if err != nil {
		return err
	}
	padding := ""
	if b.lastRenderWidth > w {
		padding = strings.Repeat(" ", b.lastRenderWidth-w)
	}
	if err := b.write(scope, text+padding, true); err != nil {
		return err
	}
	now, err := b.owner.now()
	if err != nil {
		return err
	}
	b.lastRender = now
	w, err = b.owner.measure(text)
	if err != nil {
		return err
	}
	b.lastRenderWidth = w
	return nil
}

func (b *FamilyBar) write(scope *LiveConsoleScope, text string, clearFirst bool) error {
	if b.owner.ops.IsTTY == nil {
		return fmt.Errorf("family progress TTY binding is unavailable")
	}
	tty, err := b.owner.ops.IsTTY()
	if err != nil || !tty {
		return err
	}
	print := func(text string) error { return scope.Print(b.owner.ops.Output, text) }
	first := false
	if b.multi != nil {
		first = b.firstRender
		if first {
			b.multi.rows.Add(&b.multi.rows, big.NewInt(1))
			b.row = new(big.Int).Set(&b.multi.rows)
		} else {
			up := new(big.Int).Add(&b.multi.rows, big.NewInt(1))
			up.Sub(up, b.row)
			if err := print("\x1b7"); err != nil {
				return err
			}
			if err := print("\x1b[" + up.String() + "A"); err != nil {
				return err
			}
		}
	}
	if clearFirst {
		if err := print("\x1b[1G"); err != nil {
			return err
		}
	}
	inset := ""
	if b.multi != nil {
		inset = b.multi.inset(b)
	}
	if err := print(inset + text); err != nil {
		return err
	}
	if b.owner.ops.Flush == nil {
		return fmt.Errorf("family progress flush is unavailable")
	}
	if err := b.owner.ops.Flush(); err != nil {
		return err
	}
	if b.multi != nil {
		if first {
			if err := print("\n"); err != nil {
				return err
			}
			b.firstRender = false
		} else {
			return print("\x1b8")
		}
	}
	return nil
}
