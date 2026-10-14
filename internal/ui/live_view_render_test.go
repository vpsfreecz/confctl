package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testBareLive(t *testing.T, header, title string, cols, size int, out io.Writer) *LiveView {
	t.Helper()
	v, err := OpenLiveView(LiveViewOptions{Header: header, Title: &title, Output: out, Console: &LiveConsole{}}, LiveViewOps{})
	if err != nil {
		t.Fatal(err)
	}
	v.rows, v.cols, v.size = 2, cols, size
	return v
}

func TestLiveHeaderTitleAndInclusiveFitSourceBytes(t *testing.T) {
	for _, test := range []struct {
		header string
		lines  []string
	}{
		{"", nil}, {"a\n", []string{"a\n"}}, {"\n\n", []string{"\n", "\n"}}, {"a\nb", []string{"a\n", "b"}}, {"\xff\n", []string{"\xff\n"}},
	} {
		if got := liveHeaderLines(test.header); !reflect.DeepEqual(got, test.lines) {
			t.Fatalf("%q ->%q", test.header, got)
		}
	}
	for _, test := range []struct {
		title string
		cols  int
		want  string
	}{
		{"Log", 1, "Log ->"}, {"Log", 4, "Log ->"}, {"Log", 8, "Log --->"},
		{"", 8, " ------>"}, {"long title", 8, "long title ->"},
		{"\x1b[mT\x1b[0m", 8, "\x1b[mT\x1b[0m ----->"},
		{"e\u0301", 8, "e\u0301 ---->"}, {"界", 8, "界 ----->"}, {"🙂", 8, "🙂 ----->"},
		{"\x1b[?mT", 8, "\x1b[?mT ->"}, {"\x1b]x", 8, "\x1b]x --->"},
		{"Log", 80, "Log " + strings.Repeat("-", 75) + ">"},
	} {
		got, err := liveTitleBar(test.title, test.cols)
		if err != nil || got != test.want {
			t.Fatalf("title%q cols%d: %q/%v want%q", test.title, test.cols, got, err, test.want)
		}
	}
	if _, err := liveTitleBar("\xff", 8); err == nil {
		t.Fatal("invalid title admitted")
	}
	for _, test := range []struct {
		line string
		cols int
		want string
	}{
		{"", 1, ""}, {"a", 1, "a..."}, {"abc", 4, "ab..."},
		{"abc", 8, "abc"}, {"abcd", 8, "abcd..."}, {"abcde", 8, "abcde..."}, {"abcdef", 8, "abcde..."},
		{"界🙂e\u0301x!", 8, "界🙂e\u0301x..."}, {"\x1b[31mred", 8, "\x1b[31m..."}, {"abc", 80, "abc"},
	} {
		if got := liveFitLine(test.line, test.cols); got != test.want {
			t.Fatalf("fit%q cols%d ->%q want%q", test.line, test.cols, got, test.want)
		}
	}
}

func TestLiveFrameOrderedWritesAndReachedInvalidTitle(t *testing.T) {
	w := &liveWriteRecorder{}
	v := testBareLive(t, "a\nb", "T", 8, 2, w)
	v.lines = []string{"abcdef", "\u00a0"}
	initial := v.snapshot()
	err := v.SyncConsole(func(scope *LiveConsoleScope) error { return v.renderFrame(scope, initial) })
	want := []string{"\x1b[2K\x1b[1G", "a\n", "\x1b[2K\x1b[1G", "b", "\x1b[2K\x1b[1G", "T ----->\n", "\x1b[2K\x1b[1G", "abcde...\n", "\x1b[2K\x1b[1G", "\u00a0\n", "\x1b[2K\x1b[1G", "<-------\n", "\n"}
	if err != nil || !reflect.DeepEqual(w.trace(), want) {
		t.Fatal(w.trace(), err)
	}
	for _, header := range []string{"", "\n\n", "tail", "\xff\n"} {
		var out bytes.Buffer
		v := testBareLive(t, header, "", 1, 1, &out)
		s := v.snapshot()
		if err := v.SyncConsole(func(scope *LiveConsoleScope) error { return v.renderFrame(scope, s) }); err != nil {
			t.Fatal(err)
		}
		prefix := ""
		for _, line := range strings.SplitAfter(header, "\n") {
			if line != "" {
				prefix += "\x1b[2K\x1b[1G" + line
			}
		}
		if out.String() != prefix+"\x1b[2K\x1b[1G ->\n\x1b[2K\x1b[1G\n\x1b[2K\x1b[1G<-\n\n" {
			t.Fatalf("raw header%q got%q", header, out.String())
		}
	}
	w = &liveWriteRecorder{}
	v = testBareLive(t, "\xff\n", "\xff", 8, 2, w)
	s := v.snapshot()
	err = v.SyncConsole(func(scope *LiveConsoleScope) error { return v.renderFrame(scope, s) })
	if err == nil || !reflect.DeepEqual(w.trace(), []string{"\x1b[2K\x1b[1G", "\xff\n", "\x1b[2K\x1b[1G"}) {
		t.Fatal(w.trace(), err)
	}
}

func TestLivePartialScopedFrameDoesNotRestoreOrConsumeEpochs(t *testing.T) {
	sentinel := errors.New("write")
	// Fail after save/home and at different reached header/title/body operations.
	for _, at := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9} {
		w := &liveWriteRecorder{failAt: at, err: sentinel}
		v := testBareLive(t, "h\n", "T", 8, 1, w)
		v.lines = []string{"line"}
		v.generation.SetInt64(1)
		if err := v.renderIteration(); err != sentinel || len(w.trace()) != at || strings.Contains(w.text(), "\x1b8") {
			t.Fatal(at, w.trace(), err)
		}
		if v.renderedGeneration.Sign() != 0 {
			t.Fatal("failed frame consumed generation")
		}
	}
	w := &liveWriteRecorder{shortAt: 2}
	v := testBareLive(t, "", "T", 8, 1, w)
	v.generation.SetInt64(1)
	if err := v.renderIteration(); err != io.ErrShortWrite || w.text() != "\x1b7\x1b" {
		t.Fatal(w.text(), err)
	}
	// A clean iteration writes nothing; arbitrarily large counters stay exact.
	w = &liveWriteRecorder{}
	v = testBareLive(t, "", "T", 8, 1, w)
	if err := v.renderIteration(); err != nil || len(w.trace()) != 0 {
		t.Fatal(w.trace(), err)
	}
	v.generation.Lsh(big.NewInt(1), 130)
	v.resizeEpoch.Lsh(big.NewInt(1), 150)
	if err := v.renderIteration(); err != nil {
		t.Fatal(err)
	}
	if v.generation.Cmp(&v.renderedGeneration) != 0 || v.resizeEpoch.Cmp(&v.renderedResize) != 0 || !strings.HasPrefix(w.text(), "\x1b[2J\x1b7\x1b[H") || !strings.HasSuffix(w.text(), "\x1b8") {
		t.Fatal(w.text())
	}
	before := w.text()
	if err := v.renderIteration(); err != nil || w.text() != before {
		t.Fatal("clean frame repeated", err)
	}
}

func TestLiveDimensionsHeaderReserveAndIndependentFallback(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, test := range []struct {
		rows, cols      int
		available, auto bool
		header          string
		reserve         int
		want            [3]int
	}{
		{1, 1, false, true, "", 0, [3]int{24, 80, 24}},
		{0, 8, true, true, "a\nb", 10, [3]int{24, 8, 12}},
		{30, -1, true, true, "a\n\n", 10, [3]int{30, 80, 18}},
		{-1, -1, true, false, "", 0, [3]int{24, 80, 10}},
		{3, 8, true, true, "tail", 10, [3]int{3, 8, 10}},
		{maxInt, 80, true, true, "a\nb", maxInt, [3]int{maxInt, 80, 10}},
		{maxInt, 80, true, true, "a\nb", 0, [3]int{maxInt, 80, maxInt - 2}},
	} {
		v, err := OpenLiveView(LiveViewOptions{Auto: test.auto, Header: test.header, ReservedLines: test.reserve}, LiveViewOps{Dimensions: func() (int, int, bool, error) { return test.rows, test.cols, test.available, nil }})
		if err != nil {
			t.Fatal(err)
		}
		rows, cols, size, err := v.dimensions()
		if err != nil || [3]int{rows, cols, size} != test.want {
			t.Fatal(test, rows, cols, size, err)
		}
	}
	failure := errors.New("dimension")
	v, _ := OpenLiveView(LiveViewOptions{}, LiveViewOps{Dimensions: func() (int, int, bool, error) { return 0, 0, false, failure }})
	if _, _, _, err := v.dimensions(); err != failure {
		t.Fatal(err)
	}
}

func TestLiveResizeAndFeedDuringWriteRemainDirty(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	w := &liveGateWriter{entered: entered, release: release}
	v := testBareLive(t, "", "T", 8, 1, w)
	v.lines, v.cols = []string{"old"}, 8
	v.generation.SetInt64(1)
	v.ops.Dimensions = func() (int, int, bool, error) { return 10, 9, true, nil }
	done := make(chan error, 1)
	go func() { done <- v.renderIteration() }()
	<-entered
	if err := v.resize(); err != nil {
		t.Fatal(err)
	}
	// A feeder update commits without acquiring the held console/writer.
	v.mu.Lock()
	v.lines = []string{"new"}
	v.generation.Add(&v.generation, big.NewInt(1))
	v.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	first := w.text()
	if !strings.Contains(first, "old\n") || strings.Contains(first, "new\n") || v.renderedGeneration.Int64() != 1 || v.renderedResize.Sign() != 0 {
		t.Fatal(first)
	}
	if err := v.renderIteration(); err != nil {
		t.Fatal(err)
	}
	second := strings.TrimPrefix(w.text(), first)
	if !strings.HasPrefix(second, "\x1b[2J\x1b7\x1b[H") || !strings.Contains(second, "new\n") || v.snapshot().dirty {
		t.Fatal(second)
	}
}

type liveGateWriter struct {
	liveWriteRecorder
	once             bool
	entered, release chan struct{}
}

func (w *liveGateWriter) Write(data []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.entered)
		<-w.release
	}
	return w.liveWriteRecorder.Write(data)
}

func TestLiveSynchronousResizeErrorLeavesStateAndEpochs(t *testing.T) {
	w := &liveWriteRecorder{}
	v := testBareLive(t, "", "T", 8, 2, w)
	before := v.snapshot()
	failure := errors.New("resize")
	v.ops.Dimensions = func() (int, int, bool, error) { return 99, 99, true, failure }
	if err := v.resize(); err != failure {
		t.Fatal(err)
	}
	after := v.snapshot()
	if !reflect.DeepEqual(before, after) || v.rendererErr != nil {
		t.Fatal(before, after)
	}
	if err := v.FeedLine("still admitted"); err != nil {
		t.Fatal(err)
	}
	v.ops.Wait = func(context.Context, time.Duration) error { return nil }
	if err := v.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
}
