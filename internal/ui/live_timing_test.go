package ui

import (
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

func TestInvocationTimingCanonicalRowsAndAtomicRankValidation(t *testing.T) {
	m, _ := NewLiveModel(LiveHeader{}, []LiveRow{
		{ID: "build", Label: "Building", State: LiveSucceeded, DisplayOrder: 10},
		{ID: "fetch", Label: "Fetching", State: LivePending, DisplayOrder: 20},
		{ID: "eval", Label: "nix eval", State: LiveRunning, DisplayOrder: 40},
	}, false)
	if err := m.SetRows(LiveRow{ID: "observed", Label: "nix build", State: LiveRunning, DisplayOrder: 30}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []LiveState{LiveRunning, LiveSucceeded, LiveFailed, LiveIncomplete} {
		if err := m.SetRows(LiveRow{ID: "build", Label: "Building", State: state, DisplayOrder: 10}); err != nil {
			t.Fatal(err)
		}
		frame := m.Frame(24, 120, 0)
		for i, label := range []string{"Building", "Fetching", "nix build", "nix eval"} {
			if !strings.HasPrefix(frame[len(frame)-4+i], label+" [") {
				t.Fatal("state/late admission reordered canonical rows", frame)
			}
		}
	}
	before := m.Rows()
	if err := m.SetRows(LiveRow{ID: "build", Label: "must not apply", DisplayOrder: 10}, LiveRow{ID: "fetch", DisplayOrder: 1}); err == nil || !reflect.DeepEqual(before, m.Rows()) {
		t.Fatal("rank validation was not atomic", err, m.Rows())
	}
	// Stable equal ranks and rebuilt indices survive a new earlier insertion.
	m, _ = NewLiveModel(LiveHeader{}, []LiveRow{{ID: "a", Label: "A", Current: big.NewInt(0)}, {ID: "b", Label: "B"}}, false)
	_ = m.SetRows(LiveRow{ID: "c", Label: "C", DisplayOrder: -1}, LiveRow{ID: "d", Label: "D"})
	if err := m.Advance("a"); err != nil {
		t.Fatal(err)
	}
	rows := m.Rows()
	if rows[0].ID != "c" || rows[1].ID != "a" || rows[2].ID != "b" || rows[3].ID != "d" || rows[1].Current.Int64() != 1 {
		t.Fatal("rank insertion damaged stable ties/index", rows)
	}
}

func TestInvocationTimingPriorityMembershipRendersCanonically(t *testing.T) {
	var rows []LiveRow
	for i := range 8 {
		state := LiveRunning
		if i == 6 {
			state = LiveFailed
		} else if i == 7 {
			state = LiveIncomplete
		}
		rows = append(rows, LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprintf("task-%d", i), DisplayOrder: i, State: state})
	}
	m, _ := NewLiveModel(LiveHeader{}, rows, false)
	frame, remainder := m.FrameWithRemainder(24, 100, 0)
	for i, want := range []int{0, 1, 2, 6, 7} {
		if !strings.HasPrefix(frame[len(frame)-6+i], fmt.Sprintf("task-%d [", want)) {
			t.Fatal("priority moved visible rows out of canonical order", frame)
		}
	}
	if len(remainder) != 3 || !strings.HasPrefix(remainder[0], "task-3 [") || !strings.HasPrefix(remainder[1], "task-4 [") || !strings.HasPrefix(remainder[2], "task-5 [") || frame[len(frame)-1] != "… 3 more tasks (0 failed, 0 incomplete)" {
		t.Fatal("selection/remainder diverged", frame, remainder)
	}
}
