package ui

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

type LiveState string

const (
	LivePending    LiveState = "pending"
	LiveRunning    LiveState = "running"
	LiveSucceeded  LiveState = "succeeded"
	LiveFailed     LiveState = "failed"
	LiveSkipped    LiveState = "skipped"
	LiveIncomplete LiveState = "incomplete"
)

type LiveHeader struct {
	Command, Operation, Detail, Target, Action, Generation, LogPath string
}

type LiveRow struct {
	ID, Label, Detail string
	Current, Total    *big.Int
	State             LiveState
	Target            bool
	DisplayOrder      int
}

const liveLineLimit = 8192
const liveHistoryLimit = 128

// LiveModel is a single-owner display model, not an execution or input owner.
// Its caller serializes access. Task snapshots are copied, and log history is
// bounded independently of the durable log and the terminal's dimensions.
type LiveModel struct {
	header  LiveHeader
	color   bool
	rows    []LiveRow
	index   map[string]int
	lines   []string
	partial []byte
	trimmed bool
}

func NewLiveModel(header LiveHeader, rows []LiveRow, color bool) (*LiveModel, error) {
	m := &LiveModel{index: make(map[string]int), color: color, header: LiveHeader{
		Command: liveBound(liveSafe(header.Command)), Operation: liveBound(liveSafe(header.Operation)), Detail: liveBound(liveSafe(header.Detail)),
		Target: liveBound(liveSafe(header.Target)), Action: liveBound(liveSafe(header.Action)), Generation: liveBound(liveSafe(header.Generation)), LogPath: liveBound(liveSafe(header.LogPath)),
	}}
	if err := m.SetRows(rows...); err != nil {
		return nil, err
	}
	return m, nil
}

func liveCopyRow(row LiveRow) LiveRow {
	row.Label, row.Detail = liveBound(row.Label), liveBound(row.Detail)
	if row.Current != nil {
		row.Current = new(big.Int).Set(row.Current)
	}
	if row.Total != nil {
		row.Total = new(big.Int).Set(row.Total)
	}
	return row
}

// Validate the whole update before applying any part. First registration owns
// rank and stable tie order; replacing a snapshot never infers completion.
func (m *LiveModel) SetRows(rows ...LiveRow) error {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == "" || seen[row.ID] {
			return fmt.Errorf("live row IDs must be nonempty and distinct: %q", row.ID)
		}
		seen[row.ID] = true
		if i, ok := m.index[row.ID]; ok && m.rows[i].DisplayOrder != row.DisplayOrder {
			return fmt.Errorf("live row %q cannot change display order", row.ID)
		}
	}
	inserted := false
	for _, row := range rows {
		row = liveCopyRow(row)
		if i, ok := m.index[row.ID]; ok {
			m.rows[i] = row
		} else {
			inserted = true
			m.index[row.ID] = len(m.rows)
			m.rows = append(m.rows, row)
		}
	}
	if inserted {
		sort.SliceStable(m.rows, func(i, j int) bool { return m.rows[i].DisplayOrder < m.rows[j].DisplayOrder })
		for i, row := range m.rows {
			m.index[row.ID] = i
		}
	}
	return nil
}

func (m *LiveModel) Advance(ids ...string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		i, ok := m.index[id]
		if !ok || m.rows[i].Current == nil {
			return fmt.Errorf("live row %q has no counter", id)
		}
		seen[id] = true
	}
	for id := range seen {
		i := m.index[id]
		m.rows[i].Current.Add(m.rows[i].Current, big.NewInt(1))
	}
	return nil
}

func (m *LiveModel) Rows() []LiveRow {
	rows := make([]LiveRow, len(m.rows))
	for i, row := range m.rows {
		rows[i] = liveCopyRow(row)
	}
	return rows
}

func (m *LiveModel) Summary(cols int) []string {
	lines := make([]string, 0, len(m.rows))
	for _, row := range m.rows {
		lines = append(lines, liveRowText(row, cols, 0, m.color))
	}
	return lines
}

func (m *LiveModel) Header() []string { return m.headerLines(0) }

func (m *LiveModel) headerLines(cols int) []string {
	var lines []string
	if m.header.Command != "" {
		var b strings.Builder
		left := cols
		if cols <= 0 {
			left = -1
		}
		liveField(&b, "Command:", "bright", &left, m.color)
		liveField(&b, " "+m.header.Command, "", &left, m.color)
		lines = append(lines, b.String())
	}
	if line := m.operationLine(cols); line != "" {
		lines = append(lines, line)
	}
	if m.header.LogPath != "" {
		var b strings.Builder
		left := cols
		if cols <= 0 {
			left = -1
		}
		liveField(&b, "Full log:", "bright", &left, m.color)
		liveField(&b, " "+m.header.LogPath, "", &left, m.color)
		lines = append(lines, b.String())
	}
	return lines
}

func (m *LiveModel) operationLine(cols int) string {
	var b strings.Builder
	left := cols
	if cols <= 0 {
		left = -1
	}
	present := false
	add := func(text, style string) {
		if text == "" {
			return
		}
		if present {
			liveField(&b, " ", "", &left, m.color)
		}
		liveField(&b, text, style, &left, m.color)
		present = true
	}
	if m.header.Operation != "" {
		add(m.header.Operation+":", "bright")
	}
	add(m.header.Detail, "")
	add(m.header.Target, "yellow")
	if m.header.Action != "" || m.header.Generation != "" {
		if present {
			liveField(&b, " ", "", &left, m.color)
		}
		liveField(&b, "(", "", &left, m.color)
		liveField(&b, m.header.Action, "yellow", &left, m.color)
		if m.header.Action != "" && m.header.Generation != "" {
			liveField(&b, "; ", "", &left, m.color)
		}
		liveField(&b, m.header.Generation, "cyan", &left, m.color)
		liveField(&b, ")", "", &left, m.color)
	}
	return b.String()
}

func liveWidth(s string) int { return ansi.StringWidth(s) }

func liveBound(s string) string {
	if len(s) <= liveLineLimit {
		return strings.Clone(s)
	}
	return strings.Clone(s[:liveLineLimit]) + "…"
}

func (m *LiveModel) appendLine(line string) {
	if len(m.lines) == liveHistoryLimit {
		copy(m.lines, m.lines[1:])
		m.lines = m.lines[:len(m.lines)-1]
	}
	m.lines = append(m.lines, liveBound(line))
}

func (m *LiveModel) Line(line string) {
	for _, part := range strings.Split(line, "\n") {
		m.appendLine(part)
	}
}

// Feed retains an incomplete UTF-8/line suffix across reads. An enormous line
// keeps a bounded prefix; its LF still separates the following record. Invalid
// process bytes are normalized only at display time, never rejected or logged.
func (m *LiveModel) Feed(b []byte) {
	for len(b) != 0 {
		i := 0
		for i < len(b) && b[i] != '\n' {
			i++
		}
		n := min(i, liveLineLimit-len(m.partial))
		m.partial = append(m.partial, b[:n]...)
		if n < i {
			m.trimmed = true
		}
		b = b[i:]
		if len(b) == 0 {
			break
		}
		line := string(m.partial)
		if m.trimmed {
			line += "…"
		}
		m.appendLine(line)
		m.partial, m.trimmed = m.partial[:0], false
		b = b[1:]
	}
}

func (m *LiveModel) Finish() {
	if len(m.partial) == 0 && !m.trimmed {
		return
	}
	line := string(m.partial)
	if m.trimmed {
		line += "…"
	}
	m.appendLine(line)
	m.partial, m.trimmed = nil, false
}

// Display text is deliberately plain. Child ANSI/OSC and control sequences
// cannot move the cursor, set terminal titles or hide the operator's prompt.
func liveSafe(s string) string {
	s = ansi.Strip(strings.ToValidUTF8(s, "�"))
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
}

func liveClip(s string, cols int) string {
	return ansi.Truncate(liveSafe(s), max(cols, 0), "…")
}

func liveRowState(row LiveRow, tick uint64) string {
	state := string(row.State)
	if state == "" {
		state = string(LivePending)
	}
	if row.State == LiveRunning {
		state += string("|/-\\"[tick%4])
	}
	return liveSafe(state)
}

func liveRowCount(row LiveRow) string {
	if row.Current == nil {
		return ""
	}
	count := row.Current.String()
	if row.Total == nil {
		return count + "/?"
	}
	count += "/" + row.Total.String()
	if liveBarCapable(row) {
		count += " (" + new(big.Int).Quo(new(big.Int).Mul(row.Current, big.NewInt(100)), row.Total).String() + "%)"
	}
	return count
}

func liveBarCapable(row LiveRow) bool {
	return row.Current != nil && row.Total != nil && row.Total.Sign() > 0 && row.Current.Sign() >= 0
}

func liveRowText(row LiveRow, cols int, tick uint64, color bool) string {
	state := liveRowState(row, tick)
	count := ""
	if row.Current != nil {
		count = " " + row.Current.String()
		if row.Total == nil {
			count += "/?"
		} else {
			count += "/" + row.Total.String()
			if row.Total.Sign() > 0 && row.Current.Sign() >= 0 {
				percent := new(big.Int).Quo(new(big.Int).Mul(row.Current, big.NewInt(100)), row.Total)
				count += " (" + percent.String() + "%)"
				width := min(20, max(cols-liveWidth(liveSafe(row.Label+" ["+state+"]"+count))-3, 0))
				filled := new(big.Int).Quo(new(big.Int).Mul(row.Current, big.NewInt(int64(width))), row.Total)
				if filled.Cmp(big.NewInt(int64(width))) > 0 {
					filled.SetInt64(int64(width))
				}
				n := int(filled.Int64())
				if width > 0 {
					count += " [" + strings.Repeat("=", n) + strings.Repeat(" ", width-n) + "]"
				}
			}
		}
	}
	var b strings.Builder
	left := cols
	if cols <= 0 {
		left = -1
	}
	labelStyle := ""
	if row.Target {
		labelStyle = "yellow"
	}
	stateStyle := ""
	switch row.State {
	case LiveSucceeded:
		stateStyle = "green"
	case LiveFailed:
		stateStyle = "red"
	case LiveSkipped, LiveIncomplete:
		stateStyle = "yellow"
	}
	liveField(&b, row.Label, labelStyle, &left, color)
	liveField(&b, " [", "", &left, color)
	liveField(&b, state, stateStyle, &left, color)
	liveField(&b, "]"+count, "", &left, color)
	if row.Detail != "" {
		liveField(&b, " — "+row.Detail, "", &left, color)
	}
	return b.String()
}

// Frame keeps a compact header and bordered raw/semantic output above task
// progress. Geometry allocates blank log slots too, so the bars never jump up
// into the log area before the first record arrives.
func (m *LiveModel) Frame(rows, cols int, tick uint64) []string {
	frame, _ := m.FrameWithRemainder(rows, cols, tick)
	return frame
}

// FrameWithRemainder uses exactly the visible selection for task layout,
// overflow and omitted rows. Width-clipping a selected task never appends it
// again; only a clipped/missing log locator gets the full-header exception.
func (m *LiveModel) FrameWithRemainder(rows, cols int, tick uint64) (frame, remainder []string) {
	budget := max(rows-1, 1)
	cols = max(cols, 1)
	var selected []int
	locatorShown := false
	if budget < 7 {
		frame, selected = m.tinyFrame(budget, cols, tick)
	} else {
		header := m.headerLines(cols)
		h := min(len(header), budget-4)
		frame = append(frame, header[:h]...)
		if m.header.LogPath != "" {
			locatorShown = h == len(header) && liveWidth(m.Header()[len(header)-1]) <= cols
		}
		r := budget - h - 2
		p := 0
		if len(m.rows) > 0 {
			if r >= 11 {
				p = min(len(m.rows), 6, r-10)
			} else {
				p = min(len(m.rows), 6, max(1, r/3))
			}
		}
		selected = m.selectTaskRows(p)
		title := "Live view " + strings.Repeat("-", max(cols-liveWidth("Live view ")-1, 0)) + ">"
		frame = append(frame, liveStyle(liveClip(title, cols), "bright", m.color))
		frame = append(frame, m.logLines(r-p, cols)...)
		frame = append(frame, liveClip("<"+strings.Repeat("-", max(cols-1, 0)), cols))
		frame = append(frame, m.taskLines(selected, p, cols, tick)...)
	}
	chosen := liveChosen(selected)
	for i, row := range m.rows {
		if !chosen[i] {
			remainder = append(remainder, liveRowText(row, 0, tick, m.color))
		}
	}
	if m.header.LogPath != "" && !locatorShown {
		header := m.Header()
		remainder = append(remainder, header[len(header)-1])
	}
	return frame, remainder
}

func liveChosen(indices []int) map[int]bool {
	chosen := make(map[int]bool, len(indices))
	for _, i := range indices {
		chosen[i] = true
	}
	return chosen
}

func (m *LiveModel) logLines(count, cols int) []string {
	lines := m.lines
	partial := m.partial
	for i := 0; i < len(partial); {
		if !utf8.FullRune(partial[i:]) {
			partial = partial[:i]
			break
		}
		_, n := utf8.DecodeRune(partial[i:])
		i += n
	}
	if len(m.partial) > 0 {
		lines = append(append([]string(nil), lines...), string(partial))
	}
	lines = lines[max(0, len(lines)-count):]
	out := make([]string, count)
	for i, line := range lines {
		out[i] = liveClip(line, cols)
	}
	return out
}

func (m *LiveModel) selectTaskRows(slots int) []int {
	if slots <= 0 || len(m.rows) == 0 {
		return nil
	}
	if len(m.rows) <= slots {
		selected := make([]int, len(m.rows))
		for i := range selected {
			selected[i] = i
		}
		return selected
	}
	limit := min(slots, len(m.rows))
	if len(m.rows) > limit {
		limit--
	}
	chosen := make(map[int]bool, limit)
	order := []int{0}
	for _, state := range []LiveState{LiveFailed, LiveIncomplete, LiveRunning} {
		for i, row := range m.rows {
			if i != 0 && row.State == state {
				order = append(order, i)
			}
		}
	}
	for i := range m.rows {
		order = append(order, i)
	}
	selected := make([]int, 0, limit)
	for _, i := range order {
		if len(selected) == limit {
			break
		}
		if chosen[i] {
			continue
		}
		chosen[i] = true
		selected = append(selected, i)
	}
	// Priority chooses membership only; painting always follows canonical rank.
	sort.Ints(selected)
	return selected
}

func (m *LiveModel) taskLines(selected []int, slots, cols int, tick uint64) []string {
	layout := m.rowLayout(selected, cols, tick)
	var lines []string
	for _, i := range selected {
		lines = append(lines, m.alignedRow(m.rows[i], layout, cols, tick))
	}
	if slots > 0 && len(selected) < len(m.rows) {
		lines = append(lines, liveClip(m.hiddenTaskText(liveChosen(selected)), cols))
	}
	return lines
}

func (m *LiveModel) tinyFrame(budget, cols int, tick uint64) ([]string, []int) {
	operation := m.operationLine(cols)
	hiddenHeader := 0
	if m.header.Command != "" {
		hiddenHeader++
	}
	if m.header.LogPath != "" {
		hiddenHeader++
	}
	if budget == 1 {
		note := ""
		if hiddenHeader > 0 {
			note = fmt.Sprintf("%d header lines omitted", hiddenHeader)
		}
		if len(m.rows) > 0 {
			if note != "" {
				note += "; "
			}
			note += strings.TrimPrefix(m.hiddenTaskText(nil), "… ")
		}
		if note != "" {
			if operation != "" {
				note += " | " + liveSafe(operation)
			}
			return []string{liveClip("… "+note, cols)}, nil
		}
		if operation != "" {
			return []string{operation}, nil
		}
		return []string{liveStyle(liveClip("Live view", cols), "bright", m.color)}, nil
	}
	var frame []string
	if operation != "" {
		frame = append(frame, operation)
	}
	// In tiny screens the title doubles as context-omission accounting.
	title := "Live view"
	if hiddenHeader > 0 {
		title += fmt.Sprintf(" (%d header lines omitted)", hiddenHeader)
	}
	remaining := budget - len(frame)
	if remaining >= 2 {
		frame = append(frame, liveStyle(liveClip(title, cols), "bright", m.color))
		remaining--
	}
	p := 0
	if len(m.rows) > 0 {
		p = 1
	}
	logs := remaining - p
	if logs < 1 && p > 0 {
		// Share the title/operation row with truthful hidden-task accounting, leaving
		// the last actual row for output rather than inventing more vertical space.
		note := m.hiddenTaskText(nil)
		if len(frame) > 0 {
			frame[len(frame)-1] = liveClip(note+" | "+liveSafe(frame[len(frame)-1]), cols)
		} else {
			frame = append(frame, liveClip(note, cols))
			remaining--
		}
		p = 0
		logs = remaining
	}
	if logs > 0 {
		frame = append(frame, m.logLines(logs, cols)...)
	}
	selected := m.selectTaskRows(p)
	frame = append(frame, m.taskLines(selected, p, cols, tick)...)
	return frame, selected
}

func (m *LiveModel) hiddenTaskText(chosen map[int]bool) string {
	failed, incomplete := 0, 0
	for i, row := range m.rows {
		if chosen[i] {
			continue
		}
		switch row.State {
		case LiveFailed:
			failed++
		case LiveIncomplete:
			incomplete++
		}
	}
	return fmt.Sprintf("… %d more tasks (%d failed, %d incomplete)", len(m.rows)-len(chosen), failed, incomplete)
}

// liveRowLayout is frame-local display geometry. Only selected tasks contribute
// natural widths; overflow text and offscreen labels/counts never widen it.
type liveRowLayout struct {
	prefix, count, track int
	tiny                 bool
}

func (m *LiveModel) rowLayout(selected []int, width int, tick uint64) liveRowLayout {
	naturalPrefix, naturalCount, stateWidth := 0, 0, 0
	detail, bar := false, false
	for _, i := range selected {
		row := m.rows[i]
		token := liveWidth("[" + liveRowState(row, tick) + "]")
		stateWidth = max(stateWidth, token)
		naturalPrefix = max(naturalPrefix, liveWidth(liveSafe(row.Label))+1+token)
		naturalCount = max(naturalCount, liveWidth(liveRowCount(row)))
		detail = detail || liveSafe(row.Detail) != ""
		bar = bar || liveBarCapable(row)
	}
	if len(selected) == 0 {
		return liveRowLayout{}
	}
	minPrefix := min(naturalPrefix, stateWidth+2)
	layout := liveRowLayout{prefix: min(naturalPrefix, max(stateWidth+2, width/3)), count: min(naturalCount, max(8, width/4))}
	reserved := 0
	if detail {
		reserved = 3 + min(16, width/4)
	}
	fields := func() int {
		n := layout.prefix
		if layout.count > 0 {
			n += 1 + layout.count
		}
		return n
	}
	shrinkPrefix := func(need int) { layout.prefix -= min(max(need, 0), layout.prefix-minPrefix) }
	shrinkCount := func(need, floor int) { layout.count -= min(max(need, 0), max(layout.count-floor, 0)) }
	if bar {
		// One gutter, two brackets and at least four track cells.
		shrinkPrefix(fields() + 7 + reserved - width)
		shrinkCount(fields()+7+reserved-width, min(naturalCount, 8))
		if free := width - fields() - reserved - 3; free >= 4 {
			layout.track = min(20, free)
			return layout
		}
	}
	shrinkPrefix(fields() + reserved - width)
	minCount := min(naturalCount, 1)
	shrinkCount(fields()+reserved-width, minCount)
	if fields()+reserved > width {
		layout.tiny = true
	}
	return layout
}

func (m *LiveModel) alignedRow(row LiveRow, layout liveRowLayout, cols int, tick uint64) string {
	if layout.tiny {
		return liveTinyRow(row, cols, tick, m.color)
	}
	state := liveRowState(row, tick)
	tokenWidth := liveWidth("[" + state + "]")
	label := liveClip(row.Label, max(layout.prefix-tokenWidth-1, 0))
	labelStyle, stateStyle := "", ""
	if row.Target {
		labelStyle = "yellow"
	}
	switch row.State {
	case LiveSucceeded:
		stateStyle = "green"
	case LiveFailed:
		stateStyle = "red"
	case LiveSkipped, LiveIncomplete:
		stateStyle = "yellow"
	}
	var out strings.Builder
	left := cols
	liveField(&out, label, labelStyle, &left, m.color)
	liveField(&out, " [", "", &left, m.color)
	liveField(&out, state, stateStyle, &left, m.color)
	liveField(&out, "]", "", &left, m.color)
	count := liveRowCount(row)
	detail := liveSafe(row.Detail)
	hasBar := layout.track > 0 && liveBarCapable(row)
	if count == "" && !hasBar && detail == "" {
		return out.String()
	}
	// Right-padding belongs to the prefix/count field, never the last actual
	// content field. Text-only rows leave shared count/bar space blank for detail.
	liveField(&out, strings.Repeat(" ", max(layout.prefix-(cols-left), 0)), "", &left, m.color)
	if layout.count > 0 {
		liveField(&out, " ", "", &left, m.color)
		clipped := liveClip(count, layout.count)
		liveField(&out, clipped, "", &left, m.color)
		if hasBar || detail != "" {
			liveField(&out, strings.Repeat(" ", layout.count-liveWidth(clipped)), "", &left, m.color)
		}
	}
	if layout.track > 0 && (hasBar || detail != "") {
		liveField(&out, " ", "", &left, m.color)
		field := strings.Repeat(" ", layout.track+2)
		if hasBar {
			filled := new(big.Int).Quo(new(big.Int).Mul(row.Current, big.NewInt(int64(layout.track))), row.Total)
			if filled.Cmp(big.NewInt(int64(layout.track))) > 0 {
				filled.SetInt64(int64(layout.track))
			}
			n := int(filled.Int64())
			field = "[" + strings.Repeat("=", n) + strings.Repeat(" ", layout.track-n) + "]"
		}
		liveField(&out, field, "", &left, m.color)
	}
	if detail != "" {
		liveField(&out, " — ", "", &left, m.color)
		liveField(&out, liveDetail(detail, left), "", &left, m.color)
	}
	return out.String()
}

// Tiny fallback preserves the existing unpadded field order, but never adds a
// bar that the common allocation could not admit.
func liveTinyRow(row LiveRow, cols int, tick uint64, color bool) string {
	state := liveRowState(row, tick)
	var out strings.Builder
	left := cols
	labelStyle, stateStyle := "", ""
	if row.Target {
		labelStyle = "yellow"
	}
	switch row.State {
	case LiveSucceeded:
		stateStyle = "green"
	case LiveFailed:
		stateStyle = "red"
	case LiveSkipped, LiveIncomplete:
		stateStyle = "yellow"
	}
	liveField(&out, row.Label, labelStyle, &left, color)
	liveField(&out, " [", "", &left, color)
	liveField(&out, state, stateStyle, &left, color)
	liveField(&out, "]", "", &left, color)
	if count := liveRowCount(row); count != "" {
		liveField(&out, " "+count, "", &left, color)
	}
	if row.Detail != "" {
		liveField(&out, " — ", "", &left, color)
		liveField(&out, liveDetail(row.Detail, left), "", &left, color)
	}
	return out.String()
}

func liveDetail(detail string, remaining int) string {
	detail = liveSafe(detail)
	if remaining < 0 || liveWidth(detail) <= remaining {
		return detail
	}
	const prefix = "/nix/store/"
	if !strings.HasPrefix(detail, prefix) {
		return detail
	}
	tail := detail[len(prefix):]
	if len(tail) < 34 || tail[32] != '-' || tail[33] == '/' {
		return detail
	}
	const alphabet = "0123456789abcdfghijklmnpqrsvwxyz"
	for _, c := range tail[:32] {
		if !strings.ContainsRune(alphabet, c) {
			return detail
		}
	}
	return prefix + tail[:7] + "…" + tail[32:]
}
