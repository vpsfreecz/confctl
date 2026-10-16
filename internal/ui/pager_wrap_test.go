package ui

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestWrapBasicIndentationAndLF(t *testing.T) {
	for _, test := range []struct {
		text  string
		width int64
		want  string
	}{
		{"", 1, ""},
		{"abcdef", 3, "abc\ndef"},
		{"  abcd\n\nxy\n", 4, "  ab\ncd\n\nxy\n"},
		{" a b ", 2, " a\n b\n "},
		{"a\nb", 1, "a\nb"},
	} {
		got, err := WrapBasic(test.text, big.NewInt(test.width))
		if err != nil || got != test.want {
			t.Fatalf("wrap %q width %d: %q, %v; want %q", test.text, test.width, got, err, test.want)
		}
	}
}

func TestWrapBasicKeepsGraphemesAndContent(t *testing.T) {
	// Each literal is one display cluster, including selectors and sequences
	// whose historical Ruby segmentation differed from current Unicode rules.
	for _, cluster := range []string{"e\u0301", "界", "🇨🇿", "👩‍👩‍👧‍👦", "✈\ufe0e", "✈\ufe0f", "👍🏽", "क\u093e"} {
		for _, width := range []int64{1, 2, 3} {
			text := "x" + cluster + "y" + cluster + "z"
			got, err := WrapBasic(text, big.NewInt(width))
			if err != nil || !utf8.ValidString(got) || strings.Count(got, cluster) != 2 || strings.ReplaceAll(got, "\n", "") != text {
				t.Fatalf("cluster %q width %d: %q, %v", cluster, width, got, err)
			}
			for _, line := range strings.Split(got, "\n") {
				if ansi.StringWidth(line) > int(width) && line != cluster {
					t.Fatalf("line exceeds width without an intact wide cluster: %q width %d", line, width)
				}
			}
		}
	}
}

func TestWrapBasicPreservesANSISequences(t *testing.T) {
	for _, text := range []string{
		"\x1b[31m  e\u0301界abc\x1b[0m",
		"\x1b]8;;https://example.invalid/\x1b\\link界\x1b]8;;\x1b\\",
		"\x1b]0;fixture title\aabc界xyz",
	} {
		got, err := WrapBasic(text, big.NewInt(3))
		if err != nil || strings.ReplaceAll(got, "\n", "") != text {
			t.Fatalf("escape or content changed: %q, %v", got, err)
		}
		for _, line := range strings.Split(got, "\n") {
			if ansi.StringWidth(line) > 3 {
				t.Fatalf("ANSI counted as display columns: %q", line)
			}
		}
	}
}

func TestWrapBasicZeroNegativeHugeAndInvalidInput(t *testing.T) {
	for _, text := range []string{"\xff", "\xc3", "x\x00\xff"} {
		if got, err := WrapBasic(text, big.NewInt(0)); got != text || err != nil {
			t.Fatalf("zero width: %q, %v", got, err)
		}
		for _, width := range []*big.Int{big.NewInt(-1), big.NewInt(1), new(big.Int).Lsh(big.NewInt(1), 500)} {
			if got, err := WrapBasic(text, width); got != "" || !errors.Is(err, ErrBasicEncoding) {
				t.Fatalf("invalid input width %s: %q, %v", width, got, err)
			}
		}
	}
	for _, width := range []*big.Int{big.NewInt(-1), new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 500)), new(big.Int).Lsh(big.NewInt(1), 500)} {
		before := new(big.Int).Set(width)
		text := "  e\u0301界\x1b[31mx\x1b[0m\n"
		if got, err := WrapBasic(text, width); got != text || err != nil || width.Cmp(before) != 0 {
			t.Fatalf("nonwrapping width %s: %q, %v", width, got, err)
		}
	}
	if _, err := WrapBasic("text", nil); err == nil {
		t.Fatal("missing width accepted")
	}
}
