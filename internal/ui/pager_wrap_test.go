package ui

import (
	"errors"
	"math/big"
	"reflect"
	"testing"
)

func TestPinnedDisplayWidthControlsAndSignedTotal(t *testing.T) {
	for _, test := range []struct {
		text string
		want int
	}{{"", 0}, {"a\b", 0}, {"\b\b", 0}, {"\t", 1}, {"\x1b", 1}, {"\x00\x05\a\n\v\f\r\x0e\x0f", 0}, {"\x1b[31mabc\x1b[0m", 3}, {"¡", 1}, {"e\u0301", 1}, {"界", 2}, {"⸻", 3}, {"é\b", 0}} {
		if got := pagerDisplayWidth(test.text); got != test.want {
			t.Fatalf("width %q=%d want%d", test.text, got, test.want)
		}
	}
}

func TestPinnedGraphemeRulesDoNotUseModernPairOrZWJRules(t *testing.T) {
	for _, test := range []struct {
		text string
		want []string
	}{
		{"a\r\nb", []string{"a", "\r\n", "b"}},
		{"e\u0301x", []string{"e\u0301", "x"}},
		{"\u1100\u1161\u11a8", []string{"\u1100\u1161\u11a8"}},
		{"🇦🇧🇨", []string{"🇦🇧🇨"}},
		{"👩\u200d👩", []string{"👩\u200d", "👩"}},
		{"\u0600a", []string{"\u0600", "a"}},
		{"क\u093e", []string{"क\u093e"}},
	} {
		if got := pagerGraphemes(test.text); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%q: %q want%q", test.text, got, test.want)
		}
	}
}

func TestFiniteANSIMatcherAndRubyLineAnchors(t *testing.T) {
	for _, test := range []struct {
		text, clean string
		only        bool
	}{
		{"\x1b[31m", "", true}, {"[\x1b[31m]", "", true}, {"[\x1b[31m];", ";", false}, {"\x1bA", "", true},
		{"\x1b[?12;h", "", true}, {"\x1b[", "\x1b[", false},
		{"prefix\n\x1b[31m\nsuffix", "prefix\n\nsuffix", true},
		{"\x1b]title\a", "\x1b]title\a", false},
	} {
		if got := pagerSanitize(test.text); got != test.clean || pagerANSIOnly.MatchString(test.text) != test.only {
			t.Fatalf("%q clean%q only%v", test.text, got, pagerANSIOnly.MatchString(test.text))
		}
	}
}

func TestWrapKeepsSourceDisplayOffsetVersusCharacterIndexFailure(t *testing.T) {
	// Six wide characters place the ANSI state at display offset12. The
	// completed seven-character line gains a four-character RESET, so the
	// source insertion at12 is still outside its eleven character positions.
	if got, err := WrapBasic("界界界界界界\x1b[31mx", big.NewInt(80)); err == nil || got != "" {
		t.Fatalf("source insertion error was clamped or lost: %q %v", got, err)
	}
}

func TestWrapLiteralSourcePredictions(t *testing.T) {
	for _, test := range []struct {
		text  string
		width int64
		want  string
	}{
		{"", 4, ""}, {"Some longish text", 8, "Some \nlongish \ntext"},
		{"abcdef", 3, "abc\ndef"}, {"abc", 3, "abc"}, {"a b", 2, "a \nb"},
		{"abc\r\nxy", 3, "abc\r\nxy"}, {"a\nb", 1, "a\nb"},
		{"\x1b[31ma", 80, "\x1b[31ma"},
		{"\x1b[31mé", 80, "\x1b[31mé\x1b[0m"},
		{"e\u0301x", 1, "e\u0301\nx"}, {"界x", 2, "界\nx"},
	} {
		got, err := WrapBasic(test.text, big.NewInt(test.width))
		if err != nil || got != test.want {
			t.Fatalf("wrap%q width%d got%q want%q err%v", test.text, test.width, got, test.want, err)
		}
	}
}

func TestWrapRawZeroEncodingAndHugeWidth(t *testing.T) {
	for _, text := range []string{"\xff", "\xc3", "x\x00\xff"} {
		if got, err := WrapBasic(text, big.NewInt(0)); got != text || err != nil {
			t.Fatalf("width0 %q %v", got, err)
		}
		if got, err := WrapBasic(text, big.NewInt(80)); got != "" || !errors.Is(err, ErrBasicEncoding) {
			t.Fatalf("invalid %q %v", got, err)
		}
	}
	width := new(big.Int).Lsh(big.NewInt(1), 500)
	before := new(big.Int).Set(width)
	if got, err := WrapBasic("unchanged ASCII", width); got != "unchanged ASCII" || err != nil || width.Cmp(before) != 0 {
		t.Fatal(got, err, width)
	}
}

func TestANSIInsertionCharacterPositionsAndStackReplacement(t *testing.T) {
	for _, test := range []struct {
		position int
		want     string
	}{{0, "Xéz"}, {1, "éXz"}, {2, "ézX"}, {-1, "ézX"}, {-2, "éXz"}} {
		got, err := insertPagerText("éz", test.position, "X")
		if err != nil || got != test.want {
			t.Fatalf("%d %q %v", test.position, got, err)
		}
	}
	if _, err := insertPagerText("é", 2, "X"); err == nil {
		t.Fatal("source insertion must fail, not clamp a display-width position")
	}
	stack := []pagerANSIEntry{{"\x1b[31m", 0}, {"\x1b[32m", 1}}
	got, err := insertPagerANSI("ab\n", &stack)
	if err != nil || got != "\x1b[31ma\x1b[32mb\x1b[0m\x1b[0m\n" || !reflect.DeepEqual(stack, []pagerANSIEntry{{"\x1b[32m", 1}, {"\x1b[31m", 0}}) {
		t.Fatalf("%q stack%v err%v", got, stack, err)
	}
	stack = []pagerANSIEntry{{"\x1b[31m", 0}, {pagerANSIReset, 1}}
	got, err = insertPagerANSI("ab", &stack)
	if err != nil || got != "\x1b[31ma\x1b[0mb" || len(stack) != 0 {
		t.Fatalf("%q %v %v", got, stack, err)
	}
	stack = []pagerANSIEntry{{"\x1b[31m", 2}}
	got, err = insertPagerANSI("ab", &stack)
	if err != nil || got != "ab" || len(stack) != 1 {
		t.Fatal(got, stack, err)
	}
}
