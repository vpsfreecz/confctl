package core

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Corrected raw-line/match-only expectations are separate source assertions;
// they neither rewrite the missing-accessor Ruby failure nor its path? defect.
func TestGenerationGCProgressRawPathAndSummaryRules(t *testing.T) {
	for _, tc := range []struct {
		raw, path string
		matched   bool
	}{
		{"deleting '/nix/store/a'", "/nix/store/a", true},
		{"deleting '/nix/store/a'\n", "/nix/store/a", true},
		{"deleting '/nix/store/a b'\n", "/nix/store/a b", true},
		{"deleting '/nix/store/a\x00\xff'\n", "/nix/store/a\x00\xff", true},
		{"deleting '/nix/store/a'\r\n", "", false},
		{"deleting '/nix/store/'\n", "", false},
		{" deleting '/nix/store/a'\n", "", false},
		{"deleting '/other/a'\n", "", false},
		{"deleting '/nix/store/a'b'\n", "", false},
		{"deleting '/nix/store/a' extra\n", "", false},
		{"ordinary chatter\n", "", false}, {"", "", false},
	} {
		p := gcParseProgress(tc.raw)
		if p.line != tc.raw || p.path != tc.path || p.pathMatched != tc.matched {
			t.Fatal(tc, p)
		}
	}
	for _, tc := range []struct {
		raw  string
		want bool
	}{{"0 store paths deleted", true}, {"99999999999999999999999999999999999 store paths deleted, suffix\n", true}, {"1 store paths deletedXYZ", true}, {"01 store paths deleted\r\n", true}, {" 1 store paths deleted", false}, {"-1 store paths deleted", false}, {"١ store paths deleted", false}, {"1.0 store paths deleted", false}, {"1 store path deleted", false}, {"", false}} {
		if got := gcSummaryLine(tc.raw); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}

func TestGenerationGCProgressPairedOrderAndNormalTails(t *testing.T) {
	v := &gcTestView{}
	p := gcTaskProgress{index: 2, host: "A", view: v, tty: true}
	if err := p.feed([]byte("O1\nO2\ntailO"), []byte("E1\nE2\ntailE")); err != nil {
		t.Fatal(err)
	}
	if err := p.finish(); err != nil {
		t.Fatal(err)
	}
	gcExpect(t, v, gcTestEvent{"line", 2, "A> O1\n"}, gcTestEvent{"line", 2, "A> E1\n"}, gcTestEvent{"line", 2, "A> O2\n"}, gcTestEvent{"line", 2, "A> E2\n"}, gcTestEvent{"line", 2, "A> tailO"}, gcTestEvent{"line", 2, "A> tailE"})
	long := strings.Repeat("x", 70000) + "\x00\xff\n"
	for _, tc := range []struct {
		out, err string
		want     []string
	}{
		{"", "", []string{"", ""}},
		{"O\n", "", []string{"O\n", "", ""}},
		{"", "E", []string{"", "E"}},
		{"O\r\n", "E\x00\xfe", []string{"O\r\n", "", "E\x00\xfe"}},
		{long, "", []string{long, "", ""}},
	} {
		v := &gcTestView{}
		p := gcTaskProgress{view: v, host: "raw"}
		if err := p.feed([]byte(tc.out), []byte(tc.err)); err != nil {
			t.Fatal(err)
		}
		if err := p.finish(); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range v.snapshot() {
			got = append(got, strings.TrimPrefix(e.text, "raw> "))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("raw lines differ (%d output bytes)", len(tc.out))
		}
	}
}

func TestGenerationGCProgressPartitionsAndBorrowedChunks(t *testing.T) {
	source := []byte("O1\nO2\r\nO\x00\xfftail")
	for cut := 0; cut <= len(source); cut++ {
		v := &gcTestView{}
		p := gcTaskProgress{host: "A", view: v}
		first, second := append([]byte{}, source[:cut]...), append([]byte{}, source[cut:]...)
		if err := p.feed(first, nil); err != nil {
			t.Fatal(err)
		}
		for i := range first {
			first[i] = '!'
		}
		if err := p.feed(second, nil); err != nil {
			t.Fatal(err)
		}
		for i := range second {
			second[i] = '!'
		}
		if err := p.finish(); err != nil {
			t.Fatal(err)
		}
		gcExpect(t, v, gcTestEvent{"line", 0, "A> O1\n"}, gcTestEvent{"line", 0, "A> O2\r\n"}, gcTestEvent{"line", 0, "A> O\x00\xfftail"}, gcTestEvent{"line", 0, "A> "})
	}
	// Independent stream partitions preserve each side's own order and bytes.
	for cut := 0; cut <= 6; cut++ {
		v := &gcTestView{}
		p := gcTaskProgress{host: "A", view: v}
		out, errout := []byte("O1\nO2\n"), []byte("E1\nE2\n")
		if err := p.feed(out[:cut], errout[:6-cut]); err != nil {
			t.Fatal(err)
		}
		if err := p.feed(out[cut:], errout[6-cut:]); err != nil {
			t.Fatal(err)
		}
		if err := p.finish(); err != nil {
			t.Fatal(err)
		}
		var outs, errs []string
		for _, e := range v.snapshot() {
			raw := strings.TrimPrefix(e.text, "A> ")
			if strings.HasPrefix(raw, "O") {
				outs = append(outs, raw)
			}
			if strings.HasPrefix(raw, "E") {
				errs = append(errs, raw)
			}
		}
		if !reflect.DeepEqual(outs, []string{"O1\n", "O2\n"}) || !reflect.DeepEqual(errs, []string{"E1\n", "E2\n"}) {
			t.Fatal(outs, errs)
		}
	}
}

func TestGenerationGCProgressLastSummaryAndPutsBytes(t *testing.T) {
	for _, tty := range []bool{false, true} {
		v := &gcTestView{}
		execute := func(_ context.Context, _ Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
			feed([]byte("1 store paths deleted\n2 store paths deleted suffix\n"), []byte("3 store paths deleted\n4 store paths deleted\n"))
			feed(nil, []byte("999999999999999999999999999999 store paths deleted tail"))
			return Result{}, nil
		}
		if err := runGenerationGC(context.Background(), gcTasks("A"), gcInt(1), gcFixture(v, tty, execute)); err != nil {
			t.Fatal(err)
		}
		events := v.snapshot()
		var summary string
		for _, e := range events {
			if e.kind == "finish" {
				summary = e.text
			}
		}
		if summary != "A: 999999999999999999999999999999 store paths deleted tail" || strings.Contains(v.output(), "tail\n\n") {
			t.Fatal(summary, v.output())
		}
	}
	for _, lf := range []bool{false, true} {
		v := &gcTestView{}
		raw := "5 store paths deleted"
		if lf {
			raw += "\n"
		}
		execute := func(_ context.Context, _ Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
			feed([]byte(raw), nil)
			return Result{}, nil
		}
		if err := runGenerationGC(context.Background(), gcTasks("A"), gcInt(1), gcFixture(v, false, execute)); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(v.output(), "A: 5 store paths deleted\n") || strings.HasSuffix(v.output(), "\n\n") {
			t.Fatal(v.output())
		}
	}
	v := &gcTestView{}
	execute := func(_ context.Context, m Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
		if m.Name == "effective-A" {
			feed([]byte("10 store paths deleted\n"), nil)
		}
		return Result{}, nil
	}
	if err := runGenerationGC(context.Background(), gcTasks("A", "B"), gcInt(1), gcFixture(v, false, execute)); err != nil || !strings.HasSuffix(v.output(), "B: done\n") {
		t.Fatal(err, v.output())
	}
}

func TestGenerationGCProgressOnlyMatchedPathsAdvance(t *testing.T) {
	for _, tty := range []bool{false, true} {
		v := &gcTestView{}
		execute := func(_ context.Context, _ Machine, _ []string, feed func([]byte, []byte)) (Result, error) {
			feed([]byte("deleting '/nix/store/a'\nnoise\n"), []byte("deleting '/nix/store/b'\r\n"))
			return Result{}, nil
		}
		if err := runGenerationGC(context.Background(), gcTasks("A"), gcInt(1), gcFixture(v, tty, execute)); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range v.snapshot() {
			if e.kind == "advance" {
				n++
			}
		}
		want := 0
		if tty {
			want = 1
		}
		if n != want {
			t.Fatal(v.snapshot())
		}
	}
}
