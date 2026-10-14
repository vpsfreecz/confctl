package ui

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

type familyFixture struct {
	console LiveConsole
	owner   *FamilyProgress
	trace   []string
	writes  []string
	data    bytes.Buffer
	clock   []time.Time
	tty     bool
	columns *big.Int
	measure func(string) (int, error)
	write   func(string) (int, error)
}

// Test-only finite source widths: printable ASCII and the three selected
// insets. Special control/Unicode examples supply exact whole-string cases.
func familyFixtureWidth(text string) (int, error) {
	switch text {
	case "┌ ":
		return 2, nil
	case "├── ", "└── ":
		return 4, nil
	}
	for _, b := range []byte(text) {
		if b < 32 || b > 126 {
			return 0, fmt.Errorf("unbound fixture width %q", text)
		}
	}
	return len(text), nil
}

func newFamilyFixture() *familyFixture {
	f := &familyFixture{tty: true, columns: big.NewInt(30), measure: familyFixtureWidth}
	f.owner = NewFamilyProgress(FamilyProgressOps{
		Output: f, Flush: func() error { f.trace = append(f.trace, "flush"); return nil },
		IsTTY:   func() (bool, error) { f.trace = append(f.trace, "tty"); return f.tty, nil },
		Columns: func() (*big.Int, error) { f.trace = append(f.trace, "columns"); return f.columns, nil },
		Measure: func(text string) (int, error) {
			f.trace = append(f.trace, fmt.Sprintf("measure:%q", text))
			return f.measure(text)
		},
		Now: func() time.Time {
			f.trace = append(f.trace, "now")
			if len(f.clock) > 0 {
				value := f.clock[0]
				f.clock = f.clock[1:]
				return value
			}
			return time.Unix(100, 0)
		},
	})
	return f
}

func (f *familyFixture) Write(data []byte) (int, error) {
	text := string(data)
	f.trace = append(f.trace, fmt.Sprintf("write:%q", text))
	f.writes = append(f.writes, text)
	if f.write != nil {
		n, err := f.write(text)
		f.data.Write(data[:n])
		return n, err
	}
	return f.data.Write(data)
}
func (f *familyFixture) bar(t *testing.T, spec FamilyProgressSpec) *FamilyBar {
	t.Helper()
	var bar *FamilyBar
	err := f.console.Sync(func(s *LiveConsoleScope) error { var err error; bar, err = f.owner.NewBar(s, spec); return err })
	if err != nil {
		t.Fatal(err)
	}
	return bar
}
func (f *familyFixture) multi(t *testing.T, spec FamilyProgressSpec) *FamilyMulti {
	t.Helper()
	var multi *FamilyMulti
	err := f.console.Sync(func(s *LiveConsoleScope) error { var err error; multi, err = f.owner.NewMulti(s, spec); return err })
	if err != nil {
		t.Fatal(err)
	}
	return multi
}
func (f *familyFixture) child(t *testing.T, m *FamilyMulti, label string) *FamilyBar {
	t.Helper()
	var child *FamilyBar
	err := f.console.Sync(func(s *LiveConsoleScope) error { var err error; child, err = m.Register(s, label); return err })
	if err != nil {
		t.Fatal(err)
	}
	return child
}
func familyState(t *testing.T, f *familyFixture, b *FamilyBar, current, total string, done bool) {
	t.Helper()
	err := f.console.Sync(func(s *LiveConsoleScope) error {
		state, err := b.State(s)
		if err != nil {
			return err
		}
		gotTotal := "nil"
		if state.Total != nil {
			gotTotal = state.Total.String()
		}
		if state.Current.String() != current || gotTotal != total || state.Done != done {
			t.Fatalf("state %s/%s/%t want %s/%s/%t", state.Current, gotTotal, state.Done, current, total, done)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFamilyConstructionAndReachedBindings(t *testing.T) {
	p := NewFamilyProgress(FamilyProgressOps{})
	var console LiveConsole
	if bar, err := p.NewBar(nil, FamilyProgressSpec{}); err == nil || bar != nil {
		t.Fatal(bar, err)
	}
	if err := console.Sync(func(s *LiveConsoleScope) error { _, err := p.NewBar(s, FamilyProgressSpec{}); return err }); err == nil || !strings.Contains(err.Error(), "clock") {
		t.Fatal(err)
	}
	f := newFamilyFixture()
	for _, total := range []*big.Int{nil, big.NewInt(0), big.NewInt(-3), new(big.Int).Lsh(big.NewInt(1), 130)} {
		b := f.bar(t, FamilyProgressSpec{Label: "\xff", Total: total})
		if b.current.Sign() != 0 || b.done || !b.firstRender || b.row != nil || b.phase != 0 {
			t.Fatal(b)
		}
		if total != nil {
			old := total.String()
			total.Add(total, big.NewInt(1))
			if b.total.String() != old {
				t.Fatal("borrowed total")
			}
		}
	}
	if len(f.trace) != 4 || !reflect.DeepEqual(f.trace, []string{"now", "now", "now", "now"}) {
		t.Fatal(f.trace)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { _, err := f.owner.NewBar(s, FamilyProgressSpec{Mode: 99}); return err }); err == nil {
		t.Fatal("mode")
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		_, err := f.owner.NewMulti(s, FamilyProgressSpec{Mode: FamilyLiteral})
		return err
	}); err == nil {
		t.Fatal("multi mode")
	}
}

func TestFamilyTotalResumeAndBorrowedState(t *testing.T) {
	f := newFamilyFixture()
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "done", Total: big.NewInt(1)})
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
		t.Fatal(err)
	}
	familyState(t, f, b, "1", "1", true)
	before := append([]string(nil), f.trace...)
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetTotal(s, big.NewInt(10)); err != nil {
			return err
		}
		if err := b.Advance(s); err != nil {
			return err
		}
		state, err := b.State(s)
		state.Current.SetInt64(999)
		state.Total.SetInt64(999)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.trace) {
		t.Fatal(f.trace)
	}
	familyState(t, f, b, "1", "10", true)
	oldTime, oldWidth, oldPhase := b.lastRender, b.lastRenderWidth, b.phase
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Resume(s) }); err != nil {
		t.Fatal(err)
	}
	if b.done || b.lastRender != oldTime || b.lastRenderWidth != oldWidth || b.phase != oldPhase || !b.firstRender {
		t.Fatal(b)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetLiteral(s, "short"); err != nil {
			return err
		}
		return b.Advance(s)
	}); err != nil {
		t.Fatal(err)
	}
	familyState(t, f, b, "2", "10", false)
}

func TestFamilyWallClockGateAndFinish(t *testing.T) {
	f := newFamilyFixture()
	f.clock = []time.Time{time.Unix(10, 0), time.Unix(9, 0), time.Unix(10, 0), time.Unix(11, 0), time.Unix(10, 0), time.Unix(8, 0)}
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	advance := func() error { return f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }) }
	if err := advance(); err != nil || len(f.writes) != 0 || b.current.Int64() != 1 || b.phase != 2 {
		t.Fatal(err, f.trace)
	}
	if err := advance(); err != nil || b.lastRender.Unix() != 11 || len(f.writes) != 2 {
		t.Fatal(err, f.trace)
	}
	if err := advance(); err != nil || len(f.writes) != 2 || b.current.Int64() != 3 {
		t.Fatal(err, f.trace)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != nil || !b.done || b.lastRender.Unix() != 8 || len(f.writes) != 5 {
		t.Fatal(err, f.trace)
	}
	trace := append([]string(nil), f.trace...)
	if err := advance(); err != nil || !reflect.DeepEqual(trace, f.trace) {
		t.Fatal(err, f.trace)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != nil || !reflect.DeepEqual(trace, f.trace) {
		t.Fatal(err, f.trace)
	}
}

func TestFamilyExactIntegersPhasesAndRatios(t *testing.T) {
	f := newFamilyFixture()
	f.tty = false
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	for _, tc := range []struct{ phase, want int }{{0, 2}, {98, 100}, {100, 102}, {196, 198}, {198, 0}} {
		b.phase = tc.phase
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil || b.phase != tc.want {
			t.Fatal(tc, b.phase, err)
		}
	}
	for _, total := range []int64{-3, 0, 1} {
		b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x", Total: big.NewInt(total)})
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
			t.Fatal(err)
		}
		familyState(t, f, b, fmt.Sprint(total), fmt.Sprint(total), true)
	}
	for _, tc := range []struct {
		cur, total string
		want       float64
	}{{"1", "2", 0.5}, {"1", "3", 1.0 / 3}, {"8", "2", 1}, {"-1", "2", 0}, {"1", "0", 0}, {"9223372036854775808", "18446744073709551616", 0.5}} {
		b.current.SetString(tc.cur, 10)
		b.total, _ = new(big.Int).SetString(tc.total, 10)
		got, err := b.ratio()
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	huge := new(big.Int).Lsh(big.NewInt(1), 2000)
	b.current.Set(huge)
	b.total = new(big.Int).Add(huge, big.NewInt(2))
	if _, err := b.ratio(); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Fatal(err)
	}
	b.total = big.NewInt(1)
	if ratio, err := b.ratio(); err != nil || ratio != 1 {
		t.Fatal(ratio, err)
	}
	b.current.SetInt64(1)
	b.total = huge
	if ratio, err := b.ratio(); err != nil || ratio != 0 {
		t.Fatal(ratio, err)
	}
}

func TestFamilyReachedUTF8AndLiteralLabels(t *testing.T) {
	f := newFamilyFixture()
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "\xff"})
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err == nil || b.current.Int64() != 1 || len(f.writes) != 0 {
		t.Fatal(err, b.current.String())
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err == nil || !b.done {
		t.Fatal(err)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetLiteral(s, "\xfe"); err != nil {
			return err
		}
		return b.Advance(s)
	}); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"node:current", "node:elapsed :bar $ {%}"} {
		b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: label})
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.data.String(), label) {
			t.Fatal(f.data.String())
		}
		counts := newFamilyFixture()
		counts.columns = big.NewInt(200)
		bar := counts.bar(t, FamilyProgressSpec{Label: label, Total: big.NewInt(3)})
		if err := counts.console.Sync(func(s *LiveConsoleScope) error { return bar.Advance(s) }); err != nil || !strings.HasPrefix(counts.writes[1], label+" [") || !strings.HasSuffix(counts.writes[1], " 1/3 (33%)") {
			t.Fatal(counts.writes, err)
		}
	}
}

func TestFamilyMissingBindingsAreLazyAndSentinelsUnchanged(t *testing.T) {
	for _, name := range []string{"columns", "measure", "tty", "writer", "flush"} {
		t.Run(name, func(t *testing.T) {
			f := newFamilyFixture()
			switch name {
			case "columns":
				f.owner.ops.Columns = nil
			case "measure":
				f.owner.ops.Measure = nil
			case "tty":
				f.owner.ops.IsTTY = nil
			case "writer":
				f.owner.ops.Output = nil
			case "flush":
				f.owner.ops.Flush = nil
			}
			b := f.bar(t, FamilyProgressSpec{Mode: FamilyCount, Label: "C", Total: big.NewInt(3)})
			if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err == nil || b.current.Int64() != 1 || b.done {
				t.Fatal(err)
			}
		})
	}
	sentinel := errors.New("provider")
	for _, name := range []string{"columns", "measure", "tty", "flush"} {
		f := newFamilyFixture()
		switch name {
		case "columns":
			f.owner.ops.Columns = func() (*big.Int, error) { return nil, sentinel }
		case "measure":
			f.measure = func(string) (int, error) { return 0, sentinel }
		case "tty":
			f.owner.ops.IsTTY = func() (bool, error) { return false, sentinel }
		case "flush":
			f.owner.ops.Flush = func() error { return sentinel }
		}
		b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "C"})
		if name == "columns" {
			b.mode = FamilyCount
		}
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Finish(s) }); err != sentinel || !b.done {
			t.Fatal(name, err)
		}
	}
}
