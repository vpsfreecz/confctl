package ui

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	liveClearScreen = "\x1b[2J"
	liveHome        = "\x1b[H"
	liveClearLine   = "\x1b[2K\x1b[1G"
	liveSave        = "\x1b7"
	liveRestore     = "\x1b8"
)

var liveSGR = regexp.MustCompile("\x1b\\[[0-9;]*m") // only Rainbow's pattern

type liveSnapshot struct {
	lines                   []string
	rows, cols, size        int
	generation, resizeEpoch big.Int
	dirty, resized          bool
}

func (v *LiveView) snapshot() liveSnapshot {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := liveSnapshot{lines: append([]string(nil), v.lines...), rows: v.rows, cols: v.cols, size: v.size}
	s.generation.Set(&v.generation)
	s.resizeEpoch.Set(&v.resizeEpoch)
	s.resized = v.resizeEpoch.Cmp(&v.renderedResize) != 0
	s.dirty = s.resized || v.generation.Cmp(&v.renderedGeneration) != 0
	return s
}

func (v *LiveView) dimensions() (rows, cols, size int, err error) {
	if v.ops.Dimensions == nil {
		return 0, 0, 0, fmt.Errorf("live view dimensions callback is unavailable")
	}
	rows, cols, available, err := v.ops.Dimensions()
	if err != nil {
		return 0, 0, 0, err
	}
	if !available {
		rows, cols = 24, 80
	}
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	size = 10
	if v.options.Auto {
		// Compare before subtracting to avoid signed overflow, without a cap.
		header := len(liveHeaderLines(v.options.Header))
		if rows > header && rows-header > v.options.ReservedLines {
			size = max(rows-header-v.options.ReservedLines, 10)
		}
	}
	return rows, cols, size, nil
}

func liveHeaderLines(header string) []string {
	if header == "" {
		return nil
	}
	lines := strings.SplitAfter(header, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func liveTitleBar(title string, cols int) (string, error) {
	if !utf8.ValidString(title) {
		return "", fmt.Errorf("live view title is not valid UTF-8")
	}
	uncolored := liveSGR.ReplaceAllString(title, "")
	count := utf8.RuneCountInString(uncolored)
	dashes := 1
	if cols > count && cols-count > 2 {
		dashes = cols - count - 2
	}
	return title + " " + strings.Repeat("-", dashes) + ">", nil
}

func liveFitLine(line string, cols int) string {
	width := 1
	if cols > 4 {
		width = cols - 4
	}
	if utf8.RuneCountInString(line) < width {
		return line
	}
	// The source's 0..width slice includes width+1 codepoints, not width.
	count := 0
	for index := range line {
		if count > width {
			return line[:index] + "..."
		}
		count++
	}
	return line + "..."
}

func (v *LiveView) initializeScreen() error {
	return v.SyncConsole(func(scope *LiveConsoleScope) error {
		for i := 0; i < v.rows; i++ {
			if err := scope.Puts(v.options.Output, ""); err != nil {
				return err
			}
		}
		if err := scope.Print(v.options.Output, liveClearScreen); err != nil {
			return err
		}
		return scope.Print(v.options.Output, liveHome)
	})
}

func (v *LiveView) renderFrame(scope *LiveConsoleScope, s liveSnapshot) error {
	return scope.Sync(func(scope *LiveConsoleScope) error {
		for _, line := range liveHeaderLines(v.options.Header) {
			if err := scope.Print(v.options.Output, liveClearLine); err != nil {
				return err
			}
			if err := scope.Print(v.options.Output, line); err != nil {
				return err
			}
		}
		if err := scope.Print(v.options.Output, liveClearLine); err != nil {
			return err
		}
		title, err := liveTitleBar(v.title, s.cols) // after prior header/clear writes
		if err != nil {
			return err
		}
		if err := scope.Puts(v.options.Output, title); err != nil {
			return err
		}
		for i := 0; i < s.size; i++ {
			if err := scope.Print(v.options.Output, liveClearLine); err != nil {
				return err
			}
			line := ""
			if i < len(s.lines) {
				line = liveFitLine(s.lines[i], s.cols)
			}
			if err := scope.Puts(v.options.Output, line); err != nil {
				return err
			}
		}
		if err := scope.Print(v.options.Output, liveClearLine); err != nil {
			return err
		}
		if err := scope.Puts(v.options.Output, "<"+strings.Repeat("-", max(s.cols-1, 1))); err != nil {
			return err
		}
		return scope.Puts(v.options.Output, "")
	})
}

func (v *LiveView) renderIteration() error {
	s := v.snapshot()
	if !s.dirty {
		return nil
	}
	err := v.SyncConsole(func(scope *LiveConsoleScope) error {
		if s.resized {
			if err := scope.Print(v.options.Output, liveClearScreen); err != nil {
				return err
			}
		}
		if err := scope.Print(v.options.Output, liveSave); err != nil {
			return err
		}
		if err := scope.Print(v.options.Output, liveHome); err != nil {
			return err
		}
		if err := v.renderFrame(scope, s); err != nil {
			return err
		}
		return scope.Print(v.options.Output, liveRestore) // ordinary write, not ensure
	})
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.renderedGeneration.Set(&s.generation)
	v.renderedResize.Set(&s.resizeEpoch)
	v.mu.Unlock()
	return nil // feed/resize during output stays dirty for the next iteration
}
