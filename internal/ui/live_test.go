package ui

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestLiveSnapshotsAndAtomicUpdates(t *testing.T) {
	n := big.NewInt(1)
	m, err := NewLiveModel(LiveHeader{Operation: "operation"}, []LiveRow{{ID: "all", Label: "All", Current: n, Total: big.NewInt(4), State: LiveRunning}, {ID: "host", Label: "host"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	n.SetInt64(99)
	if err := m.Advance("all", "all"); err != nil {
		t.Fatal(err)
	}
	if err := m.Advance("all", "missing"); err == nil {
		t.Fatal("partial invalid advance accepted")
	}
	if err := m.SetRows(LiveRow{ID: "all", Label: "bad"}, LiveRow{ID: "all"}); err == nil {
		t.Fatal("duplicate update accepted")
	}
	rows := m.Rows()
	if rows[0].Current.Int64() != 2 || rows[0].Total.Int64() != 4 || rows[0].Label != "All" {
		t.Fatal(rows)
	}
	rows[0].Current.SetInt64(80)
	if err := m.SetRows(LiveRow{ID: "host", Label: "changed", State: LiveFailed}, LiveRow{ID: "last", State: LiveSkipped}); err != nil {
		t.Fatal(err)
	}
	rows = m.Rows()
	if rows[0].ID != "all" || rows[1].ID != "host" || rows[2].ID != "last" || rows[0].Current.Int64() != 2 {
		t.Fatal(rows)
	}
	if err := m.Advance("host"); err == nil {
		t.Fatal("uncounted row advanced")
	}
}

func TestLiveCountsAndStates(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 400)
	m, _ := NewLiveModel(LiveHeader{}, nil, false)
	for _, row := range []LiveRow{
		{ID: "unknown", Label: "Unknown", Current: big.NewInt(3), State: LiveRunning},
		{ID: "zero", Label: "Empty", Current: big.NewInt(0), Total: big.NewInt(0), State: LiveSucceeded},
		{ID: "huge", Label: "Huge", Current: huge, Total: new(big.Int).Mul(huge, big.NewInt(2)), State: LiveIncomplete},
		{ID: "skipped", Label: "No-op", State: LiveSkipped},
	} {
		if err := m.SetRows(row); err != nil {
			t.Fatal(err)
		}
	}
	text := strings.Join(m.Summary(1000), "\n")
	for _, want := range []string{"3/?", "0/0", "(50%)", "incomplete", "skipped"} {
		if !strings.Contains(text, want) {
			t.Fatal(want, text)
		}
	}
	if strings.Contains(text, "0/0 (") {
		t.Fatal("zero total acquired a percentage", text)
	}
	if err := m.SetRows(LiveRow{ID: "unknown", Label: "Known later", Current: big.NewInt(3), Total: big.NewInt(8), State: LiveRunning}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(m.Summary(1000), "\n"), "3/8 (37%)") {
		t.Fatal(m.Rows())
	}
}

func TestLivePartialUTF8ControlsAndBoundedHistory(t *testing.T) {
	m, _ := NewLiveModel(LiveHeader{}, nil, false)
	m.Feed([]byte("first\npart e\xcc"))
	text := strings.Join(m.Frame(20, 80, 0), "\n")
	if strings.Contains(text, "�") || !strings.Contains(text, "part e") {
		t.Fatal(text)
	}
	m.Feed([]byte("\x81界\x1b]0;hostile\a\x1b[2Jdone\r\x00\xff"))
	text = strings.Join(m.Frame(20, 80, 0), "\n")
	if !strings.Contains(text, "é界done�") || strings.ContainsAny(text, "\x1b\a\r\x00") || strings.Contains(text, "hostile") {
		t.Fatal(text)
	}
	m.Feed([]byte(strings.Repeat("x", liveLineLimit*4)))
	if len(m.partial) > liveLineLimit {
		t.Fatal("unbounded partial", len(m.partial))
	}
	m.Feed([]byte("\nafter\n"))
	for i := 0; i < 1000; i++ {
		m.Line("recent")
	}
	if len(m.lines) != liveHistoryLimit {
		t.Fatal("unbounded history", len(m.lines))
	}
	m.Feed([]byte("final partial"))
	m.Finish()
	if m.lines[len(m.lines)-1] != "final partial" || len(m.partial) != 0 {
		t.Fatal(m.lines)
	}
}

func TestLiveTinyResizeUnicodeAndHiddenFailures(t *testing.T) {
	m, _ := NewLiveModel(LiveHeader{Operation: "Copy", Target: "nodes", LogPath: "fixture"}, nil, false)
	for i, state := range []LiveState{LiveRunning, LiveSucceeded, LiveFailed, LiveIncomplete, LiveSkipped} {
		id := string(rune('a' + i))
		_ = m.SetRows(LiveRow{ID: id, Label: id + " 👩‍👩‍👧‍👦 é界", State: state})
	}
	m.Line("stream 👩‍👩‍👧‍👦 é界")
	for _, size := range [][2]int{{1, 1}, {2, 2}, {5, 12}, {8, 30}, {40, 120}, {0, 0}} {
		frame := m.Frame(size[0], size[1], 1)
		if len(frame) > max(size[0]-1, 1) {
			t.Fatal(size, frame)
		}
		for _, line := range frame {
			if !utf8.ValidString(line) || ansi.StringWidth(line) > max(size[1], 1) {
				t.Fatal(size, line)
			}
			if strings.Contains(line, "👩") && !strings.Contains(line, "👩‍👩‍👧‍👦") {
				t.Fatal("split grapheme", line)
			}
		}
	}
	text := strings.Join(m.Frame(8, 80, 0), "\n")
	if !strings.Contains(text, "more tasks") || !strings.Contains(text, "failed") {
		t.Fatal(text)
	}
	if len(m.Summary(80)) != 5 {
		t.Fatal("lost offscreen tasks")
	}
}

func TestLiveCompactHeaderAndOwnedStyles(t *testing.T) {
	header := LiveHeader{Command: "confctl build -y '*'", Operation: "Build group", Detail: "1/1 (8 machines)", Target: "node-a", Action: "switch", Generation: "gen-42", LogPath: "/logs/build.log"}
	want := []string{"Command: confctl build -y '*'", "Build group: 1/1 (8 machines) node-a (switch; gen-42)", "Full log: /logs/build.log"}
	for _, color := range []bool{false, true} {
		m, err := NewLiveModel(header, nil, color)
		if err != nil {
			t.Fatal(err)
		}
		got := m.Header()
		if len(got) != 3 {
			t.Fatal(got)
		}
		for i, line := range got {
			if ansi.Strip(line) != want[i] {
				t.Fatal(line, want[i])
			}
			liveAssertStyles(t, line, color)
		}
		if color {
			for _, styled := range []string{"\x1b[1mCommand:\x1b[0m", "\x1b[1mBuild group:\x1b[0m", "\x1b[33mnode-a\x1b[0m", "\x1b[33mswitch\x1b[0m", "\x1b[36mgen-42\x1b[0m"} {
				if !strings.Contains(strings.Join(got, "\n"), styled) {
					t.Fatal(styled, got)
				}
			}
		}
	}
	for _, header := range []LiveHeader{{Operation: "Copy"}, {Operation: "Activate", Action: "switch"}, {Operation: "Activate", Generation: "gen"}, {Operation: "Activate", Action: "\x1b[31m\x1b[0m", Generation: "\x00"}} {
		m, _ := NewLiveModel(header, nil, true)
		line := ansi.Strip(m.Header()[0])
		if strings.Contains(line, "()") || strings.Contains(line, "(; ") || strings.Contains(line, ";)") {
			t.Fatal("empty header separator", line)
		}
	}
}

func liveAssertStyles(t *testing.T, line string, color bool) {
	t.Helper()
	if !utf8.ValidString(line) {
		t.Fatal("invalid styled UTF-8", line)
	}
	plain := ansi.Strip(line)
	if !color && plain != line {
		t.Fatal("style while disabled", line)
	}
	rest := line
	opened, resets := 0, 0
	for _, code := range []string{"1", "31", "32", "33", "36"} {
		token := "\x1b[" + code + "m"
		opened += strings.Count(rest, token)
		rest = strings.ReplaceAll(rest, token, "")
	}
	resets = strings.Count(rest, "\x1b[0m")
	rest = strings.ReplaceAll(rest, "\x1b[0m", "")
	if opened != resets || strings.ContainsRune(rest, '\x1b') {
		t.Fatal("unbalanced or foreign controls", line)
	}
}

func TestLiveBorderedLogRegionAboveBoundedProgress(t *testing.T) {
	rows := []LiveRow{{ID: "all", Label: "All machines", Current: big.NewInt(1), Total: big.NewInt(20), State: LiveRunning}}
	for i := 0; i < 20; i++ {
		state := LiveRunning
		if i == 17 {
			state = LiveFailed
		}
		if i == 18 {
			state = LiveIncomplete
		}
		rows = append(rows, LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprintf("node-%02d", i), State: state, Target: true})
	}
	header := LiveHeader{Command: "confctl build -y '*'", Operation: "Build group", Detail: "1/1 (20 machines)", LogPath: "/logs/build.log"}
	for _, color := range []bool{false, true} {
		m, _ := NewLiveModel(header, rows, color)
		for i := 0; i < 18; i++ {
			m.Line(fmt.Sprintf("child-line-%02d", i))
		}
		frame := m.Frame(24, 80, 1)
		if len(frame) != 23 {
			t.Fatal("wrong frame budget", len(frame))
		}
		plain := make([]string, len(frame))
		for i, line := range frame {
			plain[i] = ansi.Strip(line)
			if ansi.StringWidth(line) > 80 {
				t.Fatal("overwide", line)
			}
			liveAssertStyles(t, line, color)
		}
		if plain[0] != "Command: confctl build -y '*'" || plain[1] != "Build group: 1/1 (20 machines)" || plain[2] != "Full log: /logs/build.log" {
			t.Fatal(plain[:3])
		}
		if !strings.HasPrefix(plain[3], "Live view ---") || !strings.HasSuffix(plain[3], ">") || !strings.HasPrefix(plain[16], "<---") {
			t.Fatal("missing bordered log region", plain)
		}
		for i := 0; i < 12; i++ {
			if plain[i+4] != fmt.Sprintf("child-line-%02d", i+6) {
				t.Fatal("log not above progress", plain)
			}
		}
		if !strings.HasPrefix(plain[17], "All machines") || !strings.HasPrefix(plain[18], "node-17 [failed]") || !strings.HasPrefix(plain[19], "node-18 [incomplete]") || plain[22] != "… 16 more tasks (0 failed, 0 incomplete)" {
			t.Fatal("task priority/count", plain[17:])
		}
		if len(m.Header()) != 3 || len(m.Summary(0)) != 21 {
			t.Fatal("viewport dropped final summary")
		}
		if color && !strings.Contains(frame[3], "\x1b[1mLive view") {
			t.Fatal("unbold title", frame[3])
		}
		// With no output the same twelve blank slots keep progress below the border.
		empty, _ := NewLiveModel(header, rows, color)
		initial := empty.Frame(24, 80, 0)
		for _, line := range initial[4:16] {
			if line != "" {
				t.Fatal("log geometry jumps", initial)
			}
		}
	}
}

func TestLivePlainChildAndBalancedNarrowStyling(t *testing.T) {
	m, _ := NewLiveModel(LiveHeader{Command: "confctl \x1b[31munsafe\x1b[0m", Operation: "Activate", Target: "node👩‍👩‍👧‍👦", Action: "switch", Generation: "gen界", LogPath: "/log"}, []LiveRow{{ID: "a", Label: "node👩‍👩‍👧‍👦", Detail: "child\x1b[31mred\x1b[0m", State: LiveFailed, Target: true}}, true)
	m.Feed([]byte("\x1b[31mchild red\x1b[0m\x1b]0;title\a\x00\r\n"))
	frame := m.Frame(24, 80, 0)
	if frame[4] != "child red" {
		t.Fatal("child acquired styling/controls", frame[4])
	}
	for _, size := range [][2]int{{1, 1}, {2, 2}, {3, 10}, {4, 10}, {6, 12}, {7, 1}, {8, 10}, {24, 80}, {40, 120}} {
		frame := m.Frame(size[0], size[1], 0)
		if len(frame) > max(size[0]-1, 1) {
			t.Fatal("too many physical rows", size, frame)
		}
		for _, line := range frame {
			liveAssertStyles(t, line, true)
			if ansi.StringWidth(line) > size[1] {
				t.Fatal("narrow SGR overflow", size, line)
			}
			if strings.Contains(line, "👩") && !strings.Contains(line, "👩‍👩‍👧‍👦") {
				t.Fatal("partial grapheme", line)
			}
		}
	}
	summary := strings.Join(m.Summary(0), "\n")
	if !strings.Contains(summary, "\x1b[33mnode👩‍👩‍👧‍👦\x1b[0m") || !strings.Contains(summary, "\x1b[31mfailed\x1b[0m") || strings.Contains(summary, "\x1b[31mred") {
		t.Fatal(summary)
	}
	for _, style := range []string{"bright", "yellow", "cyan", "green", "red", "", "31mBAD"} {
		text := LiveText("x\x1b[35mchild\x1b[0m\x1b]2;hostile\a", style, true)
		if ansi.Strip(text) != "xchild" {
			t.Fatal(text)
		}
		liveAssertStyles(t, text, true)
		if style == "31mBAD" && text != "xchild" {
			t.Fatal("unknown palette style accepted", text)
		}
	}
}

func TestLiveTinyOverflowAndStateColors(t *testing.T) {
	tasks := []LiveRow{{ID: "all", Label: "All", State: LiveFailed}, {ID: "a", Label: "A", State: LiveIncomplete}, {ID: "b", Label: "B", State: LiveSkipped}}
	for _, height := range []int{1, 2, 3, 4, 5, 6, 7, 8, 10} {
		m, _ := NewLiveModel(LiveHeader{Command: "confctl", Operation: "Copy", LogPath: "/log"}, tasks, true)
		frame := m.Frame(height, 160, 0)
		text := ansi.Strip(strings.Join(frame, "\n"))
		// Tiny task allocation is one overflow slot: all three are actually hidden.
		if height <= 8 && !strings.Contains(text, "3 more tasks (1 failed, 1 incomplete)") {
			t.Fatal(height, frame)
		}
		if len(frame) > max(height-1, 1) {
			t.Fatal("unbounded tiny layout", frame)
		}
		for _, line := range frame {
			liveAssertStyles(t, line, true)
		}
	}
	m, _ := NewLiveModel(LiveHeader{}, []LiveRow{{ID: "a", Label: "A", State: LiveSucceeded}, {ID: "b", Label: "B", State: LiveFailed}, {ID: "c", Label: "C", State: LiveSkipped}, {ID: "d", Label: "D", State: LiveIncomplete}}, true)
	summary := strings.Join(m.Summary(0), "\n")
	for _, want := range []string{"\x1b[32msucceeded\x1b[0m", "\x1b[31mfailed\x1b[0m", "\x1b[33mskipped\x1b[0m", "\x1b[33mincomplete\x1b[0m"} {
		if !strings.Contains(summary, want) {
			t.Fatal(want, summary)
		}
	}
}

func TestLiveOverflowSeparatesHiddenFailedAndIncomplete(t *testing.T) {
	rows := []LiveRow{{ID: "all", Label: "All", State: LiveRunning}}
	for i := 1; i <= 7; i++ {
		rows = append(rows, LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprint(i), State: LiveFailed})
	}
	for i := 8; i <= 10; i++ {
		rows = append(rows, LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprint(i), State: LiveIncomplete})
	}
	m, _ := NewLiveModel(LiveHeader{Operation: "Copy"}, rows, false)
	// Six progress slots show aggregate plus four failed rows, then overflow.
	frame := m.Frame(24, 100, 0)
	if frame[len(frame)-1] != "… 6 more tasks (3 failed, 3 incomplete)" {
		t.Fatal(frame)
	}
	// Tiny one-slot progress shows no task, including no hidden aggregate count.
	tiny := strings.Join(m.Frame(7, 100, 0), "\n")
	if !strings.Contains(tiny, "… 11 more tasks (7 failed, 3 incomplete)") {
		t.Fatal(tiny)
	}
}
