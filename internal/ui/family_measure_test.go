package ui_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/ui"
)

func TestMeasureFamilyProgressPinnedWholeStringWidths(t *testing.T) {
	// Literal expectations follow display_width.rb's ASCII signed sum and
	// pinned Unicode 16 defaults, including its >15/<161 shortcut. The ANSI
	// cases follow strings-ansi 0.2.0's broad matcher, not an SGR-only parser.
	for _, test := range []struct {
		name, text string
		want       int
	}{
		{"empty", "", 0},
		{"ASCII", "abc", 3},
		{"TAB", "\t", 1},
		{"backspace before text", "\bab", 1},
		{"negative total", "\b\b", 0},
		{"zero controls", "\x00\x05\a\n\v\f\r\x0e\x0f", 0},
		{"raw ESC", "\x1b", 1},
		{"incomplete ANSI", "\x1b[", 2},
		{"unrecognized OSC", "\x1b]title\a", 7},
		{"SGR", "\x1b[31mabc\x1b[0m", 3},
		{"bracketed ANSI", "[\x1b[31m]", 0},
		{"broad optional suffix", "[\x1b[31m];", 1},
		{"bare ANSI letter", "\x1bA", 0},
		{"private ANSI parameters", "\x1b[?12;h", 0},
		{"ANSI across lines", "prefix\n\x1b[31m\nsuffix", 12},
		{"combining mark", "e\u0301", 1},
		{"wide character", "界", 2},
		{"ambiguous character", "¡", 1},
		{"width three", "⸻", 3},
		{"nonASCII signed total", "é\b", 0},
		{"backspace before wide character", "\b界", 1},
		{"ZWJ without emoji correction", "👩\u200d👩", 4},
		{"family without emoji correction", "👩\u200d👩\u200d👧\u200d👦", 8},
		{"regional indicators", "🇦🇧", 2},
		{"source shortcut", "\x10\u00a0", 2},
		{"ASCII DEL", "\x7f", 1},
		{"mixed DEL shortcut", "\x7fé", 2},
		{"mixed C1 shortcut", "\u0080é", 2},
		{"nonASCII zero controls", "é\x00\x05\a\n\v\f\r\x0e\x0f", 1},
		{"colored wide text", "\x1b[31m界\x1b[0m", 2},
		{"absent trailing plane default", "\U0010ffff", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := strings.Clone(test.text)
			for range 2 {
				got, err := ui.MeasureFamilyProgress(test.text)
				if err != nil || got != test.want {
					t.Fatalf("measurement %q = %d, %v; want %d, nil", test.text, got, err, test.want)
				}
				if test.text != before {
					t.Fatal("measurement changed the supplied text")
				}
			}
		})
	}
}

func TestMeasureFamilyProgressRejectsWholeInvalidUTF8(t *testing.T) {
	for _, text := range []string{
		"\xff", "\xc3", "\x80text", "text\xff", "x\x00\xff",
		"\xc0\xaf", "\xed\xa0\x80", "\xf4\x90\x80\x80",
		"\x1b[31m\xff\x1b[0m", "\x1b[\xffm",
	} {
		before := strings.Clone(text)
		got, err := ui.MeasureFamilyProgress(text)
		if got != 0 || err != ui.ErrFamilyMeasureEncoding || !errors.Is(err, ui.ErrFamilyMeasureEncoding) {
			t.Fatalf("invalid input measurement = %d, %v", got, err)
		}
		if err.Error() != "family progress text is not valid UTF-8" {
			t.Fatalf("encoding error disclosed or changed input: %q", err.Error())
		}
		if text != before {
			t.Fatal("invalid input was rewritten")
		}
	}
}

func TestMeasureFamilyProgressAcceptsEncodedReplacementCharacter(t *testing.T) {
	// U+FFFD supplied as valid UTF-8 is an ambiguous-width character at the
	// selected width 1; malformed input above must never be replaced with it.
	got, err := ui.MeasureFamilyProgress("\ufffd")
	if got != 1 || err != nil {
		t.Fatalf("valid replacement character = %d, %v; want 1, nil", got, err)
	}
}
