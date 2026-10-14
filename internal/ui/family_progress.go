package ui

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"time"
)

// Family progress is invocation-local synchronous presentation. Its callers own
// semantic events, selection, processes and viewport lifetime. Dynamic labels
// are literal; unused gem meter/rate/elapsed work is deliberately omitted.
type FamilyProgressMode uint8

const (
	FamilyCountsPercent FamilyProgressMode = iota
	FamilyCount
	FamilyLiteral
)

type FamilyProgressSpec struct {
	Mode  FamilyProgressMode
	Label string
	Total *big.Int
}

// Measure must bind the pinned Strings::ANSI/display_width rule; Columns must
// query the selected source screen rule per render. Neither has a fallback here.
type FamilyProgressOps struct {
	Output  io.Writer
	Flush   func() error
	IsTTY   func() (bool, error)
	Columns func() (*big.Int, error)
	Measure func(string) (int, error)
	Now     func() time.Time
}

type FamilyProgressState struct {
	Current *big.Int
	Total   *big.Int
	Done    bool
}

type FamilyProgress struct{ ops FamilyProgressOps }

type FamilyBar struct {
	owner           *FamilyProgress
	mode            FamilyProgressMode
	label           string
	current         big.Int
	total           *big.Int
	done            bool
	phase           int
	firstRender     bool
	row             *big.Int
	lastRender      time.Time
	lastRenderWidth int
	multi           *FamilyMulti
}

func NewFamilyProgress(ops FamilyProgressOps) *FamilyProgress { return &FamilyProgress{ops: ops} }

func familyCopyInt(n *big.Int) *big.Int {
	if n == nil {
		return nil
	}
	return new(big.Int).Set(n)
}

func (p *FamilyProgress) now() (time.Time, error) {
	if p.ops.Now == nil {
		return time.Time{}, fmt.Errorf("family progress clock is unavailable")
	}
	return p.ops.Now().Round(0), nil // Source gate is wall time, never monotonic elapsed time.
}

func (p *FamilyProgress) NewBar(scope *LiveConsoleScope, spec FamilyProgressSpec) (bar *FamilyBar, err error) {
	err = scope.Sync(func(*LiveConsoleScope) error {
		if spec.Mode > FamilyLiteral {
			return fmt.Errorf("family progress mode is unavailable")
		}
		now, e := p.now()
		if e != nil {
			return e
		}
		bar = &FamilyBar{owner: p, mode: spec.Mode, label: spec.Label, total: familyCopyInt(spec.Total), firstRender: true, lastRender: now}
		return nil
	})
	return
}

func (b *FamilyBar) State(scope *LiveConsoleScope) (state FamilyProgressState, err error) {
	err = scope.Sync(func(*LiveConsoleScope) error {
		state = FamilyProgressState{Current: new(big.Int).Set(&b.current), Total: familyCopyInt(b.total), Done: b.done}
		return nil
	})
	return
}

func (b *FamilyBar) SetTotal(scope *LiveConsoleScope, total *big.Int) error {
	return scope.Sync(func(*LiveConsoleScope) error { b.total = familyCopyInt(total); return nil })
}

func (b *FamilyBar) SetLiteral(scope *LiveConsoleScope, text string) error {
	return scope.Sync(func(*LiveConsoleScope) error { b.mode, b.label = FamilyLiteral, text; return nil })
}

func (b *FamilyBar) Resume(scope *LiveConsoleScope) error {
	return scope.Sync(func(*LiveConsoleScope) error { b.done = false; return nil })
}

func (b *FamilyBar) Advance(scope *LiveConsoleScope) error {
	return scope.Sync(func(s *LiveConsoleScope) error {
		if b.done {
			return nil
		}
		if b.multi != nil && b != b.multi.top {
			if err := b.multi.top.Advance(s); err != nil {
				return err
			}
		}
		b.current.Add(&b.current, big.NewInt(1))
		if b.total == nil {
			b.phase += 2
			if b.phase > 199 {
				b.phase = 0
			}
		}
		if b.total != nil && b.current.Cmp(b.total) >= 0 {
			return b.Finish(s)
		}
		now, err := b.owner.now()
		if err != nil {
			return err
		}
		if now.Before(b.lastRender) {
			return nil
		}
		return b.render(s)
	})
}

func (b *FamilyBar) Finish(scope *LiveConsoleScope) error {
	return scope.Sync(func(s *LiveConsoleScope) error {
		defer func() { b.done = true }()
		if b.done {
			return nil
		}
		if b.total != nil {
			b.current.Set(b.total)
		}
		if err := b.render(s); err != nil {
			return err
		}
		// Finish's LF is a separate ordinary write, including its own TTY/row work.
		return b.write(s, "\n", false)
	})
}

func (b *FamilyBar) ratio() (float64, error) {
	var ratio float64
	if b.total == nil {
		phase := b.phase
		if phase > 100 {
			phase = 200 - phase
		}
		ratio = float64(phase) / 100
	} else if b.total.Sign() > 0 {
		current, _ := b.current.Float64()
		total, _ := b.total.Float64()
		ratio = current / total
	}
	if math.IsNaN(ratio) {
		return 0, fmt.Errorf("family progress ratio is NaN")
	}
	return math.Max(0, math.Min(1, ratio)), nil
}
