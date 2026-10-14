package ui

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

func TestFamilyMultiRegistrationNullableSumAndResume(t *testing.T) {
	f := newFamilyFixture()
	m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	a := f.child(t, m, "a")
	b := f.child(t, m, "b")
	if len(f.writes) != 0 || m.rows.Sign() != 0 || a.row != nil || b.row != nil || m.Top() != m.top {
		t.Fatal(m, f.trace)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		total, err := m.Total(s)
		if err != nil || total != nil {
			t.Fatal(total, err)
		}
		if err := a.SetTotal(s, big.NewInt(0)); err != nil {
			return err
		}
		total, err = m.Total(s)
		if err != nil || total == nil || total.Sign() != 0 {
			t.Fatal(total, err)
		}
		total.SetInt64(99)
		if err := b.SetTotal(s, big.NewInt(-7)); err != nil {
			return err
		}
		total, err = m.Total(s)
		if err != nil || total.String() != "-7" {
			t.Fatal(total, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := m.top.SetLiteral(s, "top"); err != nil {
			return err
		}
		return m.top.Finish(s)
	}); err != nil {
		t.Fatal(err)
	}
	oldCurrent := m.top.current.String()
	before := len(f.writes)
	c := f.child(t, m, "c")
	if c.mode != FamilyCount || c.total != nil || m.top.done || m.top.total.String() != "-7" || m.top.current.String() != oldCurrent || len(f.writes) != before {
		t.Fatal(m, c)
	}
	count := len(m.children)
	f.owner.ops.Now = nil
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		bar, err := m.Register(s, "unreached")
		if bar != nil {
			t.Fatal(bar)
		}
		return err
	}); err == nil || len(m.children) != count {
		t.Fatal(err)
	}
}

func TestFamilyMultiHandDerivedGCRowAndFinishBytes(t *testing.T) {
	f := newFamilyFixture()
	f.columns = big.NewInt(20)
	m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	a := f.child(t, m, "a")
	b := f.child(t, m, "b")
	if m.inset(a) != "┌ " {
		t.Fatal("both nil must use top inset")
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := a.SetLiteral(s, "a: done"); err != nil {
			return err
		}
		return a.Finish(s)
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"\x1b[1G", "└── a: done", "\n", "\x1b7", "\x1b[1A", "└── \n", "\x1b8"}
	if !reflect.DeepEqual(f.writes, want) || m.top.row != nil || m.top.current.Sign() != 0 || m.top.done {
		t.Fatal(f.writes, m.top)
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
		t.Fatal(err)
	}
	// Top:20-7-2=11 slots. Child:20-6-4=10. Both phase2 round to0.
	want = append(want, "\x1b[1G", "┌ GC [<=>        ] 1", "\n", "\x1b[1G", "└── b [<=>       ] 1", "\n")
	if !reflect.DeepEqual(f.writes, want) || m.rows.Int64() != 3 || m.top.row.Int64() != 2 || a.row.Int64() != 1 || b.row.Int64() != 3 || m.inset(a) != "├── " {
		t.Fatal(f.writes, m.rows.String())
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetLiteral(s, "b: done"); err != nil {
			return err
		}
		return b.Finish(s)
	}); err != nil {
		t.Fatal(err)
	}
	want = append(want, "\x1b7", "\x1b[1A", "\x1b[1G", "└── b: done"+strings.Repeat(" ", 9), "\x1b8", "\x1b7", "\x1b[1A", "└── \n", "\x1b8")
	if !reflect.DeepEqual(f.writes, want) || !a.done || !b.done || m.top.done || m.top.current.Int64() != 1 || m.top.total != nil {
		t.Fatal(f.writes, m.top)
	}
}

func TestFamilyMultiTopBeforeChildAndReachedUTF8(t *testing.T) {
	sentinel := errors.New("top")
	f := newFamilyFixture()
	m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	child := f.child(t, m, "child")
	f.measure = func(string) (int, error) { return 0, sentinel }
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return child.Advance(s) }); err != sentinel || m.top.current.Int64() != 1 || child.current.Sign() != 0 || child.phase != 0 {
		t.Fatal(err, m.top.current.String(), child.current.String())
	}
	f = newFamilyFixture()
	m = f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	child = f.child(t, m, "\xff")
	if err := f.console.Sync(func(s *LiveConsoleScope) error { return child.Advance(s) }); err == nil || m.top.current.Int64() != 1 || child.current.Int64() != 1 || len(f.writes) != 3 || m.rows.Int64() != 1 || child.row != nil {
		t.Fatal(err, f.writes)
	}
	if m.top.done || child.done {
		t.Fatal("Advance render failure must not mark done")
	}
	if f.trace[len(f.trace)-1] != `measure:"├── "` || child.phase != 2 {
		t.Fatal(f.trace, child.phase)
	}
}

func TestFamilyMultiEveryWriteFailureAndRowsRemainPartial(t *testing.T) {
	sentinel := errors.New("partial")
	want := []string{"\x1b[1G", "└── ok", "\n", "\x1b7", "\x1b[1A", "└── \n", "\x1b8"}
	for at := 1; at <= len(want); at++ {
		f := newFamilyFixture()
		m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
		b := f.child(t, m, "b")
		calls := 0
		f.write = func(s string) (int, error) {
			calls++
			if calls == at {
				return len(s) / 2, sentinel
			}
			return len(s), nil
		}
		err := f.console.Sync(func(s *LiveConsoleScope) error {
			if err := b.SetLiteral(s, "ok"); err != nil {
				return err
			}
			return b.Finish(s)
		})
		prefix := strings.Join(want[:at-1], "") + want[at-1][:len(want[at-1])/2]
		if err != sentinel || !b.done || m.rows.Int64() != 1 || b.row.Int64() != 1 || b.firstRender != (at <= 3) || f.data.String() != prefix || len(f.writes) != at {
			t.Fatal(at, err, f.writes, b)
		}
	}
	for _, flushAt := range []int{1, 2} {
		f := newFamilyFixture()
		m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
		b := f.child(t, m, "b")
		calls := 0
		f.owner.ops.Flush = func() error {
			calls++
			if calls == flushAt {
				return sentinel
			}
			return nil
		}
		err := f.console.Sync(func(s *LiveConsoleScope) error {
			if err := b.SetLiteral(s, "ok"); err != nil {
				return err
			}
			return b.Finish(s)
		})
		if err != sentinel || !b.done || m.rows.Int64() != 1 || b.firstRender != (flushAt == 1) {
			t.Fatal(err, b)
		}
	}
	f := newFamilyFixture()
	m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	b := f.child(t, m, "b")
	f.write = func(s string) (int, error) {
		if s == "\n" {
			return 0, sentinel
		}
		return len(s), nil
	}
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetLiteral(s, "ok"); err != nil {
			return err
		}
		return b.Finish(s)
	}); err != sentinel || !b.firstRender {
		t.Fatal(err)
	}
	f.write = nil
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.Resume(s); err != nil {
			return err
		}
		return b.Finish(s)
	}); err != nil || m.rows.Int64() != 2 || b.row.Int64() != 2 {
		t.Fatal(err, m.rows.String())
	}
	// Huge exact row arithmetic without creating that many rows or callbacks.
	f = newFamilyFixture()
	m = f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	b = f.child(t, m, "b")
	m.rows.Lsh(big.NewInt(1), 130)
	b.row = big.NewInt(3)
	b.firstRender = false
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetLiteral(s, "x"); err != nil {
			return err
		}
		return b.Finish(s)
	}); err != nil {
		t.Fatal(err)
	}
	up := new(big.Int).Sub(new(big.Int).Set(&m.rows), big.NewInt(2))
	if f.writes[1] != "\x1b["+up.String()+"A" {
		t.Fatal(f.writes)
	}
}

func TestFamilyWholeScopeExclusionAndOwnedCallbackReturn(t *testing.T) {
	f := newFamilyFixture()
	b := f.bar(t, FamilyProgressSpec{Mode: FamilyLiteral, Label: "x"})
	entered, release := make(chan struct{}), make(chan struct{})
	returned := make(chan struct{})
	once := true
	f.write = func(text string) (int, error) {
		if once {
			once = false
			close(entered)
			<-release
			close(returned)
			return len(text), context.Canceled
		}
		return len(text), nil
	}
	done := make(chan error, 1)
	go func() { done <- f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }) }()
	<-entered
	v, err := OpenLiveView(LiveViewOptions{Output: f, Console: &f.console}, LiveViewOps{})
	if err != nil {
		t.Fatal(err)
	}
	v.cols, v.size = 8, 1
	attempted, other := make(chan struct{}), make(chan error, 1)
	go func() {
		close(attempted)
		other <- v.SyncConsole(func(s *LiveConsoleScope) error {
			if err := v.renderFrame(s, v.snapshot()); err != nil {
				return err
			}
			return s.Print(f, "message")
		})
	}()
	<-attempted
	select {
	case err := <-other:
		t.Fatal("frame entered held bar scope", err)
	default:
	}
	select {
	case <-returned:
		t.Fatal("callback detached")
	default:
	}
	close(release)
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	<-returned
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.data.String(), "\x1b[1G\x1b[2K\x1b[1G") || !strings.HasSuffix(f.data.String(), "message") || b.current.Int64() != 1 || b.done {
		t.Fatal(f.data.String())
	}
}

func TestFamilySubsequentWriteFailuresAndSkippedRows(t *testing.T) {
	sentinel := errors.New("subsequent")
	want := []string{"\x1b7", "\x1b[1A", "\x1b[1G", "└── z ", "\x1b8", "\x1b7", "\x1b[1A", "└── \n", "\x1b8"}
	for at := 1; at <= len(want); at++ {
		f := newFamilyFixture()
		m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
		b := f.child(t, m, "b")
		if err := f.console.Sync(func(s *LiveConsoleScope) error {
			if err := b.SetLiteral(s, "ok"); err != nil {
				return err
			}
			return b.Finish(s)
		}); err != nil {
			t.Fatal(err)
		}
		f.writes = nil
		f.data.Reset()
		calls := 0
		f.write = func(text string) (int, error) {
			calls++
			if calls == at {
				return len(text) / 2, sentinel
			}
			return len(text), nil
		}
		err := f.console.Sync(func(s *LiveConsoleScope) error {
			if err := b.Resume(s); err != nil {
				return err
			}
			if err := b.SetLiteral(s, "z"); err != nil {
				return err
			}
			return b.Finish(s)
		})
		prefix := strings.Join(want[:at-1], "") + want[at-1][:len(want[at-1])/2]
		if err != sentinel || len(f.writes) != at || f.data.String() != prefix || !b.done || b.firstRender || m.rows.Int64() != 1 {
			t.Fatal(at, err, f.writes)
		}
	}
	f := newFamilyFixture()
	f.tty = false
	f.owner.ops.Output = nil
	f.owner.ops.Flush = nil
	m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "GC"})
	b := f.child(t, m, "b")
	if err := f.console.Sync(func(s *LiveConsoleScope) error {
		if err := b.SetLiteral(s, "ok"); err != nil {
			return err
		}
		return b.Finish(s)
	}); err != nil || m.rows.Sign() != 0 || b.row != nil || !b.firstRender || !b.done || b.lastRenderWidth != 2 {
		t.Fatal(err, b, m.rows.String())
	}
}

func TestFamilySevenSourceConsumerTraces(t *testing.T) {
	t.Run("single-copy", func(t *testing.T) {
		f := newFamilyFixture()
		f.columns = big.NewInt(200)
		b := f.bar(t, FamilyProgressSpec{Label: "Copying"})
		var feeds []string
		for i, total := range []int64{2, 3} {
			feeds = append(feeds, "[i/n] path")
			if err := f.console.Sync(func(s *LiveConsoleScope) error {
				if b.total == nil || b.total.Int64() != total {
					if err := b.SetTotal(s, big.NewInt(total)); err != nil {
						return err
					}
				}
				return b.Advance(s)
			}); err != nil {
				t.Fatal(i, err)
			}
		}
		familyState(t, f, b, "2", "3", false)
		if len(feeds) != 2 {
			t.Fatal(feeds)
		}
		idle := f.bar(t, FamilyProgressSpec{Label: "Copying"})
		if idle.done || idle.current.Sign() != 0 {
			t.Fatal(idle)
		}
	})
	t.Run("bulk-copy", func(t *testing.T) {
		f := newFamilyFixture()
		f.columns = big.NewInt(200)
		m := f.multi(t, FamilyProgressSpec{Label: "Copying"})
		a, b, c := f.child(t, m, "a"), f.child(t, m, "b"), f.child(t, m, "c")
		event := func(child *FamilyBar, total int64) {
			t.Helper()
			if err := f.console.Sync(func(s *LiveConsoleScope) error {
				if err := child.SetTotal(s, big.NewInt(total)); err != nil {
					return err
				}
				if m.top.done {
					if err := m.top.Resume(s); err != nil {
						return err
					}
				}
				sum, err := m.Total(s)
				if err != nil {
					return err
				}
				if err := m.top.SetTotal(s, sum); err != nil {
					return err
				}
				return child.Advance(s)
			}); err != nil {
				t.Fatal(err)
			}
		}
		event(a, 1)
		if !m.top.done {
			t.Fatal("top should finish at first total")
		}
		event(b, 2)
		if m.top.done || m.top.current.Int64() != 2 {
			t.Fatal(m.top)
		}
		for _, tc := range []struct {
			bar    *FamilyBar
			status string
		}{{b, "b: error occurred"}, {c, "c: nothing to do"}} {
			if err := f.console.Sync(func(s *LiveConsoleScope) error {
				if err := tc.bar.SetLiteral(s, tc.status); err != nil {
					return err
				}
				return tc.bar.Advance(s)
			}); err != nil {
				t.Fatal(err)
			}
		}
		if c.done || c.current.Int64() != 1 || !strings.Contains(f.data.String(), "c: nothing to do") {
			t.Fatal(c)
		}
	})
	t.Run("activation-no-bar", func(t *testing.T) {
		f := newFamilyFixture()
		if err := f.console.Sync(func(s *LiveConsoleScope) error { return s.Print(f, "dry|activation") }); err != nil || strings.Contains(strings.Join(f.trace, "|"), "now") {
			t.Fatal(err, f.trace)
		}
	})
	t.Run("health-completion-not-attempt", func(t *testing.T) {
		f := newFamilyFixture()
		f.columns = big.NewInt(200)
		b := f.bar(t, FamilyProgressSpec{Label: "Checks", Total: big.NewInt(2)})
		attempts := 0
		for _, n := range []int{2, 1} {
			for i := 0; i < n; i++ {
				attempts++
			}
			if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
				t.Fatal(err)
			}
		}
		familyState(t, f, b, "2", "2", true)
		if attempts != 3 {
			t.Fatal(attempts)
		}
	})
	t.Run("ssh-returned-results", func(t *testing.T) {
		f := newFamilyFixture()
		f.columns = big.NewInt(200)
		b := f.bar(t, FamilyProgressSpec{Label: "Command", Total: big.NewInt(3)})
		results := []bool{}
		for _, ok := range []bool{true, false} {
			results = append(results, ok)
			if err := f.console.Sync(func(s *LiveConsoleScope) error { return b.Advance(s) }); err != nil {
				t.Fatal(err)
			}
		}
		familyState(t, f, b, "2", "3", false)
		if len(results) != 2 {
			t.Fatal(results)
		}
	})
	t.Run("build-group-across-retry", func(t *testing.T) {
		f := newFamilyFixture()
		f.columns = big.NewInt(200)
		m := f.multi(t, FamilyProgressSpec{Label: "nix build"})
		build, fetch := f.child(t, m, "Building"), f.child(t, m, "Fetching")
		event := func(b *FamilyBar, total int64) {
			t.Helper()
			if err := f.console.Sync(func(s *LiveConsoleScope) error {
				if total > 0 && b.total == nil {
					if err := b.SetTotal(s, big.NewInt(total)); err != nil {
						return err
					}
				}
				return b.Advance(s)
			}); err != nil {
				t.Fatal(err)
			}
		}
		event(build, 2)
		event(fetch, 1)
		if m.top.total != nil || m.top.current.Int64() != 2 {
			t.Fatal("child events must precede separate top-total scope", m.top)
		}
		if err := f.console.Sync(func(s *LiveConsoleScope) error {
			sum, err := m.Total(s)
			if err != nil {
				return err
			}
			return m.top.SetTotal(s, sum)
		}); err != nil {
			t.Fatal(err)
		}
		event(build, 99)
		event(fetch, 9)
		if build.total.Int64() != 2 || fetch.total.Int64() != 1 || build.current.Int64() != 2 || fetch.current.Int64() != 1 || m.top.current.Int64() != 3 {
			t.Fatal(build, fetch, m.top)
		}
	})
	t.Run("gc-registration-and-explicit-finish", func(t *testing.T) {
		f := newFamilyFixture()
		f.columns = big.NewInt(200)
		m := f.multi(t, FamilyProgressSpec{Mode: FamilyCount, Label: "Collecting garbage"})
		a, b := f.child(t, m, "a"), f.child(t, m, "b")
		if len(f.writes) != 0 || a.done || b.done || m.top.done {
			t.Fatal("nonpositive workers must leave all bars unadvanced")
		}
		for _, matched := range []bool{false, true, false} {
			if matched {
				if err := f.console.Sync(func(s *LiveConsoleScope) error { return a.Advance(s) }); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, child := range []*FamilyBar{a, b} {
			if err := f.console.Sync(func(s *LiveConsoleScope) error {
				if err := child.SetLiteral(s, child.label+": done"); err != nil {
					return err
				}
				return child.Finish(s)
			}); err != nil {
				t.Fatal(err)
			}
		}
		if !a.done || !b.done || m.top.done || m.top.current.Int64() != 1 || m.top.total != nil || b.current.Sign() != 0 {
			t.Fatal(a, b, m.top)
		}
	})
	t.Run("family-disabled", func(t *testing.T) {
		f := newFamilyFixture()
		enabled := false
		if enabled {
			_ = f.bar(t, FamilyProgressSpec{})
		}
		if len(f.trace) != 0 {
			t.Fatal(f.trace)
		}
	})
}
