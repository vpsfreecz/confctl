package ui

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFamilyHandDerivedChecksBytesAndCallbackOrder(t *testing.T) {
	f := newFamilyFixture()
	b := f.bar(t, FamilyProgressSpec{Label: "Checks", Total: big.NewInt(2)})
	f.trace = nil
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
		t.Fatal(err)
	}
	// Empty bar text is19 columns, leaving11. Round(11*1/2)=6, then5 spaces.
	first := "Checks [======     ] 1/2 (50%)"
	want := []string{"now", "columns", `measure:"Checks [] 1/2 (50%)"`, `measure:"="`, `measure:" "`, `measure:"="`, fmt.Sprintf("measure:%q", first), "tty", `write:"\x1b[1G"`, fmt.Sprintf("write:%q", first), "flush", "now", fmt.Sprintf("measure:%q", first)}
	if len(first) != 30 || !reflect.DeepEqual(f.trace, want) {
		t.Fatal(f.trace, want)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
		t.Fatal(err)
	}
	// At100%, empty text is20 columns, leaving10 complete cells.
	second := "Checks [==========] 2/2 (100%)"
	if len(second) != 30 || f.data.String() != "\x1b[1G"+first+"\x1b[1G"+second+"\n" {
		t.Fatalf("%q", f.data.String())
	}
	familyState(t, f, b, "2", "2", true)
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.Resume(s); err != nil {
			return err
		}
		if err := b.SetLiteral(s, "ok"); err != nil {
			return err
		}
		return b.Finish(s)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.data.String(), "\x1b[1Gok"+strings.Repeat(" ", 28)+"\n") || b.lastRenderWidth != 2 {
		t.Fatal(f.data.String(), b.lastRenderWidth)
	}
}

func TestFamilyWidthsRoundingAndTinyIndeterminate(t *testing.T) {
	for _, columns := range []*big.Int{big.NewInt(0), big.NewInt(6), big.NewInt(7), big.NewInt(8), big.NewInt(9), big.NewInt(86), new(big.Int).Lsh(big.NewInt(1), 150)} {
		for _, known := range []bool{false, true} {
			f := newFamilyFixture()
			f.columns = columns
			before := columns.String()
			var total *big.Int
			if known {
				total = big.NewInt(3)
			}
			b := f.bar(t, FamilyProgressSpec{Mode: FamilyCount, Label: "C", Total: total})
			err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) })
			if !known && columns.Cmp(big.NewInt(9)) < 0 {
				if err == nil || !strings.Contains(err.Error(), "repetition") || len(f.writes) != 0 {
					t.Fatal(columns, err)
				}
				continue
			}
			if err != nil || columns.String() != before {
				t.Fatal(columns, err)
			}
			width := 80
			if columns.Cmp(big.NewInt(86)) < 0 {
				width = int(columns.Int64()) - 6
				if width < 0 {
					width = 0
				}
			}
			if len(f.writes[1]) != 6+width {
				t.Fatal(columns, f.writes)
			}
		}
	}
	for _, tc := range []struct {
		current, total int64
		want           string
	}{{49, 100, "C [=  ] 49"}, {50, 100, "C [== ] 50"}, {51, 100, "C [== ] 51"}} {
		f := newFamilyFixture()
		b := f.bar(t, FamilyProgressSpec{Mode: FamilyCount, Label: "C", Total: big.NewInt(tc.total)})
		b.current.SetInt64(tc.current)
		f.columns = big.NewInt(10) // 7 text cells +3 bar cells.
		text, err := b.format(0)
		if err != nil || text != tc.want {
			t.Fatal(text, tc.want, err)
		}
	}
	f := newFamilyFixture()
	f.columns.SetInt64(0)
	b := f.bar(t, FamilyProgressSpec{Label: "C", Total: big.NewInt(2)})
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil || f.writes[1] != "C [] 1/2 (50%)" {
		t.Fatal(f.writes, err)
	}
	for _, tc := range []struct {
		mode        FamilyProgressMode
		wantColumns bool
	}{{FamilyCountsPercent, false}, {FamilyCount, true}} {
		f := newFamilyFixture()
		huge := new(big.Int).Lsh(big.NewInt(1), 2000)
		b := f.bar(t, FamilyProgressSpec{Mode: tc.mode, Label: "C", Total: new(big.Int).Add(huge, big.NewInt(5))})
		b.current.Set(huge)
		f.trace = nil
		err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) })
		if err == nil || !strings.Contains(err.Error(), "NaN") || strings.Contains(strings.Join(f.trace, "|"), "columns") != tc.wantColumns {
			t.Fatal(tc, err, f.trace)
		}
	}
}

func TestFamilyLiteralPinnedMeasurementExamples(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
	}{{"\x1b[31mred\x1b[0m", 3}, {"a\tb", 3}, {"a\bb", 1}, {"e\u0301", 1}, {"界", 2}, {"🙂", 2}, {"\x00\n\r", 0}, {"👩\u200d💻", 4}} {
		f := newFamilyFixture()
		calls := 0
		f.measure = func(s string) (int, error) {
			calls++
			if s != tc.text {
				t.Fatalf("unexpected measurement %q", s)
			}
			return tc.width, nil
		}
		f.owner.ops.Columns = nil
		b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: tc.text})
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != nil || f.data.String() != "\x1b[1G"+tc.text+"\n" || b.lastRenderWidth != tc.width || calls != 2 {
			t.Fatal(tc, f.trace, err)
		}
	}
}

func TestFamilyDynamicTTYAndPostOutputMeasurement(t *testing.T) {
	f := newFamilyFixture()
	f.tty = false
	f.owner.ops.Output = nil
	f.owner.ops.Flush = nil
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	f.trace = nil
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil || !reflect.DeepEqual(f.trace, []string{"now", `measure:"x"`, "tty", "now", `measure:"x"`}) {
		t.Fatal(err, f.trace)
	}
	f = newFamilyFixture()
	ttyCalls := 0
	f.owner.ops.IsTTY = func() (bool, error) { ttyCalls++; return ttyCalls == 1, nil }
	b = f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != nil || ttyCalls != 2 || f.data.String() != "\x1b[1Gx" {
		t.Fatal(err, f.trace)
	}
	f = newFamilyFixture()
	b = f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	sentinel := errors.New("post-measure")
	calls := 0
	f.clock = []time.Time{time.Unix(100, 0), time.Unix(123, 0)}
	f.measure = func(string) (int, error) {
		calls++
		if calls == 2 {
			return 0, sentinel
		}
		return 1, nil
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != sentinel || b.lastRender.Unix() != 123 || b.lastRenderWidth != 0 || f.data.String() != "\x1b[1Gx" || b.done {
		t.Fatal(err, f.trace, b)
	}
}

func TestFamilyWritePrefixesAndFinishErrorState(t *testing.T) {
	sentinel := errors.New("write")
	for _, at := range []int{1, 2, 3} {
		f := newFamilyFixture()
		calls := 0
		f.write = func(text string) (int, error) {
			calls++
			if calls == at {
				return len(text) / 2, sentinel
			}
			return len(text), nil
		}
		b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "abcd", Total: big.NewInt(0)})
		err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) })
		want := []string{"\x1b[1G", "abcd", "\n"}
		prefix := ""
		for i := 0; i < at-1; i++ {
			prefix += want[i]
		}
		prefix += want[at-1][:len(want[at-1])/2]
		if err != sentinel || !b.done || f.data.String() != prefix || len(f.writes) != at {
			t.Fatal(at, err, f.writes, f.data.String())
		}
		old := len(f.trace)
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != nil || len(f.trace) != old {
			t.Fatal(err)
		}
	}
	f := newFamilyFixture()
	f.write = func(text string) (int, error) { return len(text) / 2, nil }
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != io.ErrShortWrite || b.done || f.data.String() != "\x1b[" {
		t.Fatal(err, f.data.String())
	}
	f = newFamilyFixture()
	b = f.bar(t, FamilyProgressSpec{Mode: FamilyCount, Label: "C", Total: big.NewInt(3)})
	f.owner.ops.Columns = func() (*big.Int, error) { return nil, nil }
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err == nil || len(f.writes) != 0 {
		t.Fatal(err)
	}
	f = newFamilyFixture()
	b = f.bar(t, FamilyProgressSpec{Mode: FamilyCount, Label: "C", Total: big.NewInt(3)})
	f.measure = func(s string) (int, error) {
		if s == "=" {
			return 0, nil
		}
		return familyFixtureWidth(s)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err == nil || len(f.writes) != 0 {
		t.Fatal(err)
	}
	f = newFamilyFixture()
	b = f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	f.measure = func(string) (int, error) { return -1, nil }
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err == nil {
		t.Fatal(err)
	}
}

func TestFamilyMeasureFailureStagesAndBorrowedColumns(t *testing.T) {
	sentinel := errors.New("measure stage")
	for _, known := range []bool{false, true} {
		last := 7
		if known {
			last = 6
		}
		for at := 1; at <= last; at++ {
			f := newFamilyFixture()
			var total *big.Int
			if known {
				total = big.NewInt(3)
			}
			b := f.bar(t, FamilyProgressSpec{Mode: FamilyCount, Label: "C", Total: total})
			f.clock = []time.Time{time.Unix(100, 0), time.Unix(123, 0)}
			calls := 0
			f.measure = func(text string) (int, error) {
				calls++
				if calls == at {
					return 0, sentinel
				}
				return familyFixtureWidth(text)
			}
			err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) })
			if err != sentinel || b.current.Int64() != 1 || b.done || b.lastRenderWidth != 0 {
				t.Fatal(known, at, err, b)
			}
			if at < last && (len(f.writes) != 0 || b.lastRender.Unix() != 100) {
				t.Fatal(known, at, f.trace)
			}
			if at == last && (len(f.writes) != 2 || b.lastRender.Unix() != 123) {
				t.Fatal(known, at, f.trace)
			}
		}
	}
	f := newFamilyFixture()
	b := f.bar(t, FamilyProgressSpec{Label: "C", Total: big.NewInt(2)})
	f.measure = func(text string) (int, error) { f.columns.SetInt64(0); return familyFixtureWidth(text) }
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil || len(f.writes[1]) != 30 {
		t.Fatal(f.writes, err)
	}
}
