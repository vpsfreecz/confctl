package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

func TestBasicTwoSamplesCopiesAndPrompt100Order(t *testing.T) {
	var events []string
	rows, columns := big.NewInt(8), big.NewInt(10)
	calls := 0
	ops := BasicIO{
		Size: func(context.Context) (BasicSize, error) {
			calls++
			events = append(events, "size")
			if calls == 2 {
				rows.SetInt64(900)
			}
			return BasicSize{rows, columns}, nil
		},
		Wrap: func(text string, width *big.Int) (string, error) {
			events = append(events, "wrap:"+text+":"+width.String())
			width.SetInt64(1)
			return "\nprompt100", nil
		},
	}
	p, err := NewBasicPager(context.Background(), io.Discard, ops)
	if err != nil {
		t.Fatal(err)
	}
	columns.SetInt64(-400)
	if p.width.String() != "10" || p.height.String() != "6" || p.linesLeft.String() != "6" || p.page.String() != "1" {
		t.Fatalf("width%s height%s left%s page%s", p.width, p.height, p.linesLeft, p.page)
	}
	want := []string{"size", "size", "wrap:\n--- Page -100- Press enter/return to continue (or q to quit) ---:10"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("%q", events)
	}
}

func scriptedBasic(t *testing.T, output io.Writer, rows int64, wrap func(string, *big.Int) (string, error), key func(context.Context) (string, error)) *BasicPager {
	t.Helper()
	p, err := NewBasicPager(context.Background(), output, BasicIO{Size: func(context.Context) (BasicSize, error) { return BasicSize{big.NewInt(rows), big.NewInt(80)}, nil }, Wrap: wrap, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBasicPutsVersusWriteEmptyAndExactPrompts(t *testing.T) {
	for _, puts := range []bool{true, false} {
		var output bytes.Buffer
		keys := 0
		p := scriptedBasic(t, &output, 3, func(text string, _ *big.Int) (string, error) { return text, nil }, func(context.Context) (string, error) { keys++; return "x", nil })
		if err := p.Puts(""); err != nil {
			t.Fatal(err)
		}
		if output.Len() != 0 {
			t.Fatal("Basic empty Puts must have no String#lines")
		}
		var err error
		if puts {
			err = p.Puts("a\nb")
		} else {
			err = p.Write("a\nb")
		}
		want := "a\n\n--- Page -1- Press enter/return to continue (or q to quit) ---\nb"
		if puts {
			want += "\n"
		}
		// Height is3 minus the prompt's two source lines, so each input
		// line reaches a page break; the final line has a second prompt.
		want += "\n--- Page -2- Press enter/return to continue (or q to quit) ---\n"
		if err != nil || output.String() != want || keys != 2 {
			t.Fatalf("puts%v got%q want%q keys%d err%v", puts, output.String(), want, keys, err)
		}
		p.Close()
		if p.page.String() != "1" || p.linesLeft.String() != "1" || len(p.leftover) != 0 {
			t.Fatal("close reset")
		}
	}
}

func TestBasicLeftoverIsEmittedAgainAndDebitedWithoutReflow(t *testing.T) {
	var output bytes.Buffer
	keys := 0
	p := scriptedBasic(t, &output, 3, func(text string, _ *big.Int) (string, error) {
		if strings.HasPrefix(text, "\n--- Page") {
			return text, nil
		}
		if text == "long\n" {
			return "a\nb\nc\n", nil
		}
		return text, nil
	}, func(context.Context) (string, error) { keys++; return "\r", nil })
	if err := p.Puts("long\n"); err != nil {
		t.Fatal(err)
	}
	want := "a\n\n--- Page -1- Press enter/return to continue (or q to quit) ---\nb\nc\n"
	if output.String() != want || p.linesLeft.String() != "-1" || !reflect.DeepEqual(p.leftover, []string{"b\n", "c\n"}) {
		t.Fatalf("%q left%s leftover%q", output.String(), p.linesLeft, p.leftover)
	}
	if err := p.Puts("d\n"); err != nil {
		t.Fatal(err)
	}
	if output.String() != want+"b\nc\nd\n" || keys != 1 || p.linesLeft.String() != "-2" || len(p.leftover) != 0 {
		t.Fatalf("%q keys%d left%s", output.String(), keys, p.linesLeft)
	}
}

func TestBasicZeroNegativeAndHugeRowsAreNotClamped(t *testing.T) {
	for _, rows := range []string{"2", "0", "-3", "99999999999999999999999999999999999999999999999999999999"} {
		var output bytes.Buffer
		row, _ := new(big.Int).SetString(rows, 10)
		keys := 0
		p, err := NewBasicPager(context.Background(), &output, BasicIO{Size: func(context.Context) (BasicSize, error) {
			return BasicSize{row, new(big.Int).Lsh(big.NewInt(1), 500)}, nil
		}, Wrap: func(text string, _ *big.Int) (string, error) { return text, nil }, Key: func(context.Context) (string, error) { keys++; return "", nil }})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Puts("one\n"); err != nil {
			t.Fatal(err)
		}
		if rows == "2" {
			want := "\n\n--- Page -1- Press enter/return to continue (or q to quit) ---\none\n"
			if output.String() != want || keys != 1 || p.linesLeft.String() != "-1" {
				t.Fatalf("zero height %q %d %s", output.String(), keys, p.linesLeft)
			}
		} else if output.String() != "one\n" || keys != 0 {
			t.Fatalf("rows%s got%q keys%d", rows, output.String(), keys)
		}
	}
}

func TestBasicQuitEOFEncodingAndKeyErrorsRetainReachedOutput(t *testing.T) {
	failure := errors.New("key failed")
	for _, test := range []struct {
		key       string
		err, want error
	}{{"q\n", nil, ErrPagerClosed}, {"Q", nil, ErrPagerClosed}, {"\xff", nil, ErrBasicEncoding}, {"", io.EOF, io.EOF}, {"", failure, failure}} {
		var output bytes.Buffer
		p := scriptedBasic(t, &output, 3, func(text string, _ *big.Int) (string, error) { return text, nil }, func(context.Context) (string, error) { return test.key, test.err })
		err := p.Puts("a\nb\n")
		want := "a\n\n--- Page -1- Press enter/return to continue (or q to quit) ---\n"
		if !errors.Is(err, test.want) || output.String() != want || p.page.String() != "1" {
			t.Fatalf("key%q out%q page%s err%v", test.key, output.String(), p.page, err)
		}
	}
}

func TestBasicWrapFailureClearsLeftoverBeforeOutput(t *testing.T) {
	var output bytes.Buffer
	failure := errors.New("wrap")
	p := scriptedBasic(t, &output, 8, func(text string, _ *big.Int) (string, error) {
		if text == "bad\n" {
			return "", failure
		}
		return text, nil
	}, nil)
	p.leftover = []string{"old\n"}
	if err := p.Puts("bad\n"); err != failure || len(p.leftover) != 0 || output.Len() != 0 {
		t.Fatalf("leftover%q out%q err%v", p.leftover, output.String(), err)
	}
}

func TestBasicConstructorAndPromptFailurePriority(t *testing.T) {
	failure := errors.New("selected failure")
	for _, phase := range []int{1, 2, 3} {
		calls := 0
		_, err := NewBasicPager(context.Background(), io.Discard, BasicIO{Size: func(context.Context) (BasicSize, error) {
			calls++
			if phase == calls {
				return BasicSize{}, failure
			}
			return BasicSize{big.NewInt(8), big.NewInt(80)}, nil
		}, Wrap: func(string, *big.Int) (string, error) { calls++; return "", failure }})
		if err != failure || calls != phase {
			t.Fatalf("phase%d calls%d err%v", phase, calls, err)
		}
	}
}

func TestBasicRealWrapReachedEncodingAndWidthZero(t *testing.T) {
	for _, columns := range []int64{0, 80} {
		var output bytes.Buffer
		p, err := NewBasicPager(context.Background(), &output, BasicIO{Size: func(context.Context) (BasicSize, error) { return BasicSize{big.NewInt(20), big.NewInt(columns)}, nil }, Wrap: WrapBasic})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Puts("earlier\n"); err != nil {
			t.Fatal(err)
		}
		err = p.Puts("\xff\n")
		if columns == 0 {
			if err != nil || output.String() != "earlier\n\xff\n" {
				t.Fatalf("%q %v", output.String(), err)
			}
		} else if !errors.Is(err, ErrBasicEncoding) || output.String() != "earlier\n" {
			t.Fatalf("%q %v", output.String(), err)
		}
	}
}

func TestBasicKeyCancellationAndCloseReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	p, err := NewBasicPager(ctx, &out, BasicIO{Size: func(context.Context) (BasicSize, error) { return BasicSize{big.NewInt(3), big.NewInt(80)}, nil }, Wrap: func(text string, _ *big.Int) (string, error) { return text, nil }, Key: func(context.Context) (string, error) { cancel(); return "x", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Puts("a\nb\n"); err != context.Canceled || strings.Contains(out.String(), "b\n") {
		t.Fatalf("%q %v", out.String(), err)
	}
	p.Close()
	if p.page.String() != "1" || p.linesLeft.String() != "1" {
		t.Fatal("failed body close did not reset")
	}
}

func TestBasicOtherCompleteKeysContinueAndDoNotConsumeAnotherKey(t *testing.T) {
	for _, key := range []string{"", "é", "\r", "\n", "\x03", "\x1a"} {
		var out bytes.Buffer
		calls := 0
		p := scriptedBasic(t, &out, 3, func(text string, _ *big.Int) (string, error) { return text, nil }, func(context.Context) (string, error) { calls++; return key, nil })
		if err := p.Puts("one\n"); err != nil || calls != 1 || p.page.String() != "2" {
			t.Fatalf("key%q calls%d page%s err%v", key, calls, p.page, err)
		}
	}
}

func TestBasicANSIAndWideTextPreservesEarlierChunk(t *testing.T) {
	var out bytes.Buffer
	p, err := NewBasicPager(context.Background(), &out, BasicIO{Size: func(context.Context) (BasicSize, error) { return BasicSize{big.NewInt(20), big.NewInt(80)}, nil }, Wrap: WrapBasic})
	if err != nil {
		t.Fatal(err)
	}
	const text = "earlier\n界界界界界界\x1b[31mx"
	if err = p.Puts(text); err != nil || out.String() != text+"\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}
