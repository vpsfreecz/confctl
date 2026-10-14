package ui

import (
	"fmt"
	"math/big"
)

type FamilyMulti struct {
	owner    *FamilyProgress
	top      *FamilyBar
	children []*FamilyBar
	mode     FamilyProgressMode
	rows     big.Int
}

func (p *FamilyProgress) NewMulti(scope *LiveConsoleScope, spec FamilyProgressSpec) (multi *FamilyMulti, err error) {
	err = scope.Sync(func(s *LiveConsoleScope) error {
		if spec.Mode != FamilyCountsPercent && spec.Mode != FamilyCount {
			return fmt.Errorf("family progress multi requires a counts mode")
		}
		top, e := p.NewBar(s, spec)
		if e != nil {
			return e
		}
		multi = &FamilyMulti{owner: p, top: top, mode: spec.Mode}
		top.multi = multi
		return nil
	})
	return
}

func (m *FamilyMulti) Top() *FamilyBar { return m.top }

func (m *FamilyMulti) Register(scope *LiveConsoleScope, label string) (bar *FamilyBar, err error) {
	err = scope.Sync(func(s *LiveConsoleScope) error {
		child, e := m.owner.NewBar(s, FamilyProgressSpec{Mode: m.mode, Label: label})
		if e != nil {
			return e
		}
		child.multi = m
		m.children = append(m.children, child)
		bar = child
		total, e := m.Total(s)
		if e != nil {
			return e
		}
		if e := m.top.SetTotal(s, total); e != nil {
			return e
		}
		if m.top.done {
			return m.top.Resume(s)
		}
		return nil
	})
	return
}

func (m *FamilyMulti) Total(scope *LiveConsoleScope) (total *big.Int, err error) {
	err = scope.Sync(func(*LiveConsoleScope) error {
		for _, bar := range m.children {
			if bar.total != nil {
				if total == nil {
					total = new(big.Int)
				}
				total.Add(total, bar.total)
			}
		}
		return nil
	})
	return
}

func familyRowsEqual(a, b *big.Int) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Cmp(b) == 0
}

func (m *FamilyMulti) inset(bar *FamilyBar) string {
	if familyRowsEqual(bar.row, m.top.row) {
		return "┌ "
	}
	if bar.row != nil && bar.row.Cmp(&m.rows) == 0 {
		return "└── "
	}
	return "├── "
}
