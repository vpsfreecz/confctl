package ui

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestLiveFrameRemainderUsesActualSelection(t *testing.T) {
	var rows []LiveRow
	for i := range 10 {
		state := LiveSucceeded
		if i == 2 || i == 8 {
			state = LiveFailed
		}
		if i == 7 || i == 9 {
			state = LiveIncomplete
		}
		rows = append(rows, LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprintf("task-%02d", i), State: state})
	}
	rows[9].Current = new(big.Int).Lsh(big.NewInt(1), 400)
	for _, color := range []bool{false, true} {
		m, _ := NewLiveModel(LiveHeader{Command: "confctl copy", Operation: "Copy", LogPath: "/logs/full"}, rows, color)
		frame, remainder := m.FrameWithRemainder(24, 100, 0)
		// Aggregate first, then failed and incomplete rows. One of six slots is
		// overflow, so these five tasks are selected, regardless of field clipping.
		chosen := map[int]bool{0: true, 2: true, 8: true, 7: true, 9: true}
		text := ansi.Strip(strings.Join(frame, "\n"))
		if !strings.HasSuffix(text, "… 5 more tasks (0 failed, 0 incomplete)") || len(remainder) != 5 {
			t.Fatal(frame, remainder)
		}
		position := 0
		for i, row := range rows {
			if chosen[i] {
				if !strings.Contains(text, row.Label) || strings.Contains(strings.Join(remainder, "\n"), row.Label) {
					t.Fatal("selected task replayed/lost", i, frame, remainder)
				}
				continue
			}
			if remainder[position] != liveRowText(row, 0, 0, color) {
				t.Fatal("omitted order/value changed", remainder)
			}
			liveAssertStyles(t, remainder[position], color)
			position++
		}
		// Resize to a tiny viewport: every task is omitted, including exact huge
		// values, and only the missing full locator is recovered after them.
		_, tiny := m.FrameWithRemainder(3, 12, 0)
		if len(tiny) != len(rows)+1 || ansi.Strip(tiny[len(rows)]) != "Full log: /logs/full" || !strings.Contains(tiny[9], rows[9].Current.String()) {
			t.Fatal(tiny)
		}
		if strings.Contains(strings.Join(tiny, "\n"), "Command:") || strings.Contains(strings.Join(tiny, "\n"), "Copy:") {
			t.Fatal("header replay", tiny)
		}
	}
}

func TestLiveRemainderEmptyAndLocatorRecovery(t *testing.T) {
	for _, height := range []int{0, 1, 2, 6, 24} {
		m, _ := NewLiveModel(LiveHeader{}, nil, false)
		frame, rest := m.FrameWithRemainder(height, 80, 0)
		if len(rest) != 0 || len(frame) > max(height-1, 1) {
			t.Fatal(frame, rest)
		}
	}
	const locator = "Full log: /logs/long-command-file.log"
	for _, cols := range []int{len(locator) - 1, len(locator), len(locator) + 1} {
		m, _ := NewLiveModel(LiveHeader{Command: "confctl build", Operation: "Build", LogPath: strings.TrimPrefix(locator, "Full log: ")}, []LiveRow{{ID: "a", Label: strings.Repeat("long", 30), State: LiveSucceeded}}, true)
		frame, rest := m.FrameWithRemainder(24, cols, 0)
		if cols < len(locator) {
			if len(rest) != 1 || ansi.Strip(rest[0]) != locator {
				t.Fatal("missing clipped locator", frame, rest)
			}
		} else if len(rest) != 0 {
			t.Fatal("complete locator/selected row replayed", rest)
		}
	}
}

var liveTrackPattern = regexp.MustCompile(`\[[= ]{4,20}\]`)

func TestLiveAlignedMixedProgressFields(t *testing.T) {
	rows := []LiveRow{
		{ID: "build", Label: "Building", Current: big.NewInt(1), Total: big.NewInt(5), State: LiveRunning, Detail: "building path"},
		{ID: "fetch", Label: "Fetching long", Current: big.NewInt(75), Total: big.NewInt(100), State: LiveSucceeded, Detail: "fetching path"},
		{ID: "unknown", Label: "Unknown", Current: big.NewInt(3), State: LiveRunning, Detail: "unknown path"},
		{ID: "zero", Label: "Zero", Current: big.NewInt(0), Total: big.NewInt(0), State: LiveSkipped, Detail: "empty path"},
		{ID: "none", Label: "No count", State: LiveSkipped, Detail: "No steps observed"},
	}
	for _, color := range []bool{false, true} {
		m, _ := NewLiveModel(LiveHeader{}, rows, color)
		frame := m.Frame(24, 100, 0)
		tasks := frame[len(frame)-len(rows):]
		trackStart, trackWidth, countStart, detailStart := -1, -1, -1, -1
		for i, styled := range tasks {
			rowIndex := i // All-fit rows retain canonical registration order.
			line := ansi.Strip(styled)
			liveAssertStyles(t, styled, color)
			if liveWidth(line) > 100 {
				t.Fatal("overwide aligned row", line)
			}
			d := strings.Index(line, " — ")
			if d < 0 {
				t.Fatal("reserved detail lost", line)
			}
			column := liveWidth(line[:d])
			if detailStart >= 0 && detailStart != column {
				t.Fatal("unaligned detail", tasks)
			}
			detailStart = column
			match := liveTrackPattern.FindStringIndex(line)
			if rowIndex < 2 {
				if match == nil {
					t.Fatal("missing known-total track", line)
				}
				start, width := liveWidth(line[:match[0]]), liveWidth(line[match[0]:match[1]])
				if trackStart >= 0 && (trackStart != start || trackWidth != width) {
					t.Fatal("unequal track geometry", tasks)
				}
				trackStart, trackWidth = start, width
				if filled := strings.Count(line[match[0]:match[1]], "="); filled != []int{4, 15}[rowIndex] {
					t.Fatal("shared track lost distinct exact ratios", line)
				}
			} else if match != nil {
				t.Fatal("unobserved/unknown/zero row acquired a track", line)
			}
			if rowIndex < 4 {
				c := strings.Index(line, liveRowCount(rows[rowIndex]))
				if c < 0 || countStart >= 0 && countStart != liveWidth(line[:c]) {
					t.Fatal("unaligned exact counts", tasks)
				}
				countStart = liveWidth(line[:c])
			}
		}
		if strings.TrimSpace(tasks[4][strings.Index(tasks[4], "]")+1:strings.Index(tasks[4], " — ")]) != "" && !color {
			t.Fatal("counterless row acquired progress", tasks[4])
		}
	}
}

func TestLiveUnobservedFetchingDoesNotInventProgress(t *testing.T) {
	rows := []LiveRow{{ID: "build", Label: "Building", Current: big.NewInt(1), Total: big.NewInt(1), State: LiveRunning, Detail: "/nix/store/0123456789abcdfghijklmnpqrsvwxyz-build.drv"}, {ID: "fetch", Label: "Fetching", State: LivePending}}
	m, _ := NewLiveModel(LiveHeader{}, rows, false)
	frame := m.Frame(32, 160, 0)
	if got := frame[len(frame)-1]; got != "Fetching [pending]" {
		t.Fatal("pending row acquired padding/progress/detail", got)
	}
	rows[0].State = LiveSucceeded
	rows[1].State, rows[1].Detail = LiveSkipped, "No steps observed"
	_ = m.SetRows(rows...)
	frame = m.Frame(32, 160, 0)
	a, b := frame[len(frame)-2], frame[len(frame)-1]
	if strings.Index(a, " — ") != strings.Index(b, " — ") || liveTrackPattern.MatchString(b) || strings.Contains(b, "1/1") {
		t.Fatal("skipped Fetch detail/blank fields", a, b)
	}
}

func TestLiveAlignedRowsWithoutDetailsStopAtLastContent(t *testing.T) {
	rows := []LiveRow{{ID: "a", Label: "A", Current: big.NewInt(1), Total: big.NewInt(4), State: LiveRunning}, {ID: "b", Label: "Longer label", Current: big.NewInt(3), Total: big.NewInt(4), State: LiveSucceeded}, {ID: "c", Label: "Negative", Current: big.NewInt(-1), Total: big.NewInt(4), State: LiveIncomplete}}
	m, _ := NewLiveModel(LiveHeader{}, rows, false)
	for _, width := range []int{24, 40, 80} {
		frame := m.Frame(24, width, 0)
		tracks := 0
		for _, line := range frame[len(frame)-3:] {
			if line != strings.TrimRight(line, " ") || liveWidth(line) > width || strings.Contains(line, " — ") {
				t.Fatal("padding past last content", width, line)
			}
			if liveTrackPattern.MatchString(line) {
				tracks++
			}
			if strings.HasPrefix(line, "Negative") && (strings.Contains(line, "%") || liveTrackPattern.MatchString(line)) {
				t.Fatal("negative count acquired percentage/bar", line)
			}
		}
		if width == 24 && tracks != 0 || width == 80 && tracks != 2 {
			t.Fatal("common bar degradation", width, frame)
		}
	}
}

func TestLiveAlignmentBoundsAndHiddenHugeFields(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 500)
	rows := []LiveRow{{ID: "a", Label: "👩‍👩‍👧‍👦 é界" + strings.Repeat("long", 30), Current: huge, Total: new(big.Int).Mul(huge, big.NewInt(2)), Detail: "detail-still-visible", State: LiveIncomplete, Target: true}, {ID: "b", Label: "B", Current: big.NewInt(2), Total: big.NewInt(5), Detail: "small detail", State: LiveRunning}}
	for _, color := range []bool{false, true} {
		m, _ := NewLiveModel(LiveHeader{}, rows, color)
		for width := 1; width <= 160; width++ {
			frame := m.Frame(24, width, 0)
			tracks := 0
			for _, line := range frame[len(frame)-2:] {
				liveAssertStyles(t, line, color)
				if liveWidth(line) > width || strings.Contains(line, "👩") && !strings.Contains(line, "👩‍👩‍👧‍👦") {
					t.Fatal("invalid bounded grapheme row", width, line)
				}
				if liveTrackPattern.MatchString(ansi.Strip(line)) {
					tracks++
				}
			}
			if tracks == 1 {
				t.Fatal("only one capable row retained its bar", width, frame)
			}
			if width == 80 {
				line := ansi.Strip(frame[len(frame)-2])
				separator := strings.Index(line, " — ")
				if separator < 0 || liveWidth(line[separator+len(" — "):]) < 16 || !strings.Contains(line, "…") || !strings.Contains(line, "[incomplete]") {
					t.Fatal("huge fields stole reserved detail/state", line)
				}
			}
		}
		if m.Rows()[0].Current.Cmp(huge) != 0 || !strings.Contains(m.Summary(0)[0], huge.String()) {
			t.Fatal("layout modified precise counts")
		}
	}
	base := []LiveRow{{ID: "0", Label: "A", Current: big.NewInt(1), Total: big.NewInt(2), State: LiveRunning}}
	for i := 1; i <= 7; i++ {
		base = append(base, LiveRow{ID: fmt.Sprint(i), Label: fmt.Sprint(i), State: LiveSucceeded})
	}
	m, _ := NewLiveModel(LiveHeader{}, base, false)
	before := strings.Join(m.Frame(24, 80, 0), "\n")
	_ = m.SetRows(LiveRow{ID: "7", Label: strings.Repeat("HUGE", 100), Current: huge, Total: big.NewInt(1), State: LiveSucceeded})
	if after := strings.Join(m.Frame(24, 80, 0), "\n"); after != before {
		t.Fatal("omitted fields changed selected geometry", before, after)
	}
}

func TestLiveStoreDetailOnlyShortensWhenNeeded(t *testing.T) {
	const hash = "0123456789abcdfghijklmnpqrsvwxyz"
	path := "/nix/store/" + hash + "-name👩‍👩‍👧‍👦é/suffix"
	short := "/nix/store/" + hash[:7] + "…-name👩‍👩‍👧‍👦é/suffix"
	for _, tc := range []struct {
		text  string
		space int
		want  string
	}{
		{path, -1, path}, {path, liveWidth(path), path}, {path, liveWidth(path) - 1, short}, {path, 0, short},
		{"/nix/store/short-name", 1, "/nix/store/short-name"},
		{"/nix/store/" + strings.Repeat("e", 32) + "-name", 1, "/nix/store/" + strings.Repeat("e", 32) + "-name"},
		{"/nix/store/" + hash + "-", 1, "/nix/store/" + hash + "-"},
		{"/nix/store/" + hash + "-/suffix", 1, "/nix/store/" + hash + "-/suffix"},
		{"ordinary long detail", 1, "ordinary long detail"},
	} {
		if got := liveDetail(tc.text, tc.space); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	for _, color := range []bool{false, true} {
		row := LiveRow{ID: "copy", Label: "Copy", Detail: path, State: LiveSucceeded, Target: true}
		m, _ := NewLiveModel(LiveHeader{}, []LiveRow{row}, color)
		exact := liveWidth("Copy [succeeded] — ") + liveWidth(path)
		for _, width := range []int{exact - 1, exact, exact + 1} {
			frame := m.Frame(24, width, 0)
			line := frame[len(frame)-1]
			liveAssertStyles(t, line, color)
			want := path
			if width < exact {
				want = short
			}
			if !strings.HasSuffix(ansi.Strip(line), want) || liveWidth(line) > width {
				t.Fatal("actual remaining-space boundary", width, line, want)
			}
		}
		if m.Rows()[0].Detail != path || !strings.HasSuffix(m.Summary(0)[0], path) {
			t.Fatal("display shortening mutated path")
		}
	}
}
