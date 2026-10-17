package ui

import (
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
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

func TestInvocationTimingHeaderAndLogGeometry(t *testing.T) {
	started := time.Date(2026, 10, 18, 9, 8, 7, 0, time.FixedZone("fixture", 2*3600))
	for _, color := range []bool{false, true} {
		m, _ := NewLiveModel(LiveHeader{Command: "confctl build", Operation: "Build group", Detail: "1/1 (8 machines)", LogPath: "/log"}, nil, color)
		for i := range 8 {
			_ = m.SetRows(LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprint(i), State: LiveRunning})
		}
		for i := range 12 {
			m.Line(fmt.Sprintf("log-%02d", i))
		}
		m.SetTiming(LiveTiming{StartedAt: started, Elapsed: time.Hour + time.Second + 999*time.Millisecond})
		header := m.Header()
		want := "Started: " + started.Local().Format(time.RFC3339) + "  Elapsed: 1h 0m 1s"
		if len(header) != 4 || ansi.Strip(header[1]) != want || !strings.HasPrefix(ansi.Strip(header[2]), "Build group:") || ansi.Strip(header[3]) != "Full log: /log" {
			t.Fatal(header)
		}
		if color && (!strings.Contains(header[1], "\x1b[1mStarted:\x1b[0m") || !strings.Contains(header[1], "\x1b[1mElapsed:\x1b[0m")) {
			t.Fatal("fixed timing headings lost policy style", header[1])
		}
		frame, rest := m.FrameWithRemainder(24, 80, 0)
		if len(frame) != 23 || !strings.HasPrefix(ansi.Strip(frame[4]), "Live view ") || !strings.HasPrefix(frame[17], "<---") || len(rest) != 4 {
			t.Fatal("four-header frame consumed log budget", frame, rest)
		}
		for i := range 12 {
			if frame[5+i] != fmt.Sprintf("log-%02d", i) {
				t.Fatal("less than twelve real log lines", frame)
			}
		}
		if strings.Contains(strings.Join(rest, "\n"), "Started:") || strings.Contains(strings.Join(rest, "\n"), "Elapsed:") {
			t.Fatal("remainder replayed timing/header", rest)
		}
		for _, size := range [][2]int{{1, 1}, {2, 10}, {5, 12}, {8, 30}, {24, 80}, {32, 160}} {
			for _, line := range m.Frame(size[0], size[1], 0) {
				liveAssertStyles(t, line, color)
				if liveWidth(line) > size[1] {
					t.Fatal("timing overflowed narrow viewport", size, line)
				}
			}
		}
		m.SetTiming(LiveTiming{})
		if len(m.Header()) != 3 {
			t.Fatal("zero timing did not disable header")
		}
	}
}

func TestInvocationTimingCompactElapsed(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"}, {0, "0s"}, {999 * time.Millisecond, "0s"},
		{time.Minute + time.Second + 999*time.Millisecond, "1m 1s"},
		{time.Hour + time.Second, "1h 0m 1s"}, {25*time.Hour + 2*time.Minute + 3*time.Second, "25h 2m 3s"},
	} {
		if got := liveElapsed(tc.d); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}
