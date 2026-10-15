package build

import (
	"reflect"
	"testing"
)

func eventText(e Event) string {
	return e.Type + " " + e.Progress.String() + "/" + e.Total.String() + " " + e.Path
}

// Literal line/chunk scenarios derive from NixBuildFlake/StdLineBuffer at
// e813d3d. No Nix process or recorded native progress is used in these tests.
func TestProgressModernSeparateStreamsAndChunks(t *testing.T) {
	got := []string{}
	var first Event
	p := NewProgress(func(e Event) {
		if len(got) == 0 {
			first = e
		}
		got = append(got, eventText(e))
	})
	p.Feed([]byte("these 999 derivations will be built:\nbuilding '/stdout'\n"), []byte("building '/early'\nthese 2 deriv"))
	p.Feed([]byte("copying path '/stdout-tail'"), []byte("ations will be built:\nthis path will be fetched (1 MiB)\ncopying path '/fetch'\nbuild"))
	p.Feed(nil, []byte("ing '/second'\nthis derivation will be built:\nbuilding '/third'\nthese 3 paths will be fetched (3 MiB)\ncopying path '/tail'"))
	if len(got) != 4 {
		t.Fatalf("tail flushed early: %v", got)
	}
	p.Finish()
	want := []string{"build 1/0 /early", "fetch 1/1 /fetch", "build 2/2 /second", "build 3/1 /third", "fetch 2/3 /tail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if eventText(first) != want[0] {
		t.Fatal("earlier counters mutated")
	}
}

func TestProgressOldListsAndSourceFlagTransitions(t *testing.T) {
	got := []string{}
	p := NewProgress(func(e Event) { got = append(got, eventText(e)) })
	p.Feed(nil, []byte("building '/early'\nthese derivations will be built:\n  /nix/store/a.drv\n\t/nix/store/b.drv\nbuilding '/late'\ncopying path '/early-fetch'\nthese paths will be fetched (1 MiB)\n  /nix/store/c\ncopying path '/late-fetch'\n"))
	p.Finish()
	want := []string{"build 1/0 /early", "build 2/3 /late", "fetch 1/0 /early-fetch", "fetch 2/2 /late-fetch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// A modern summary returns before clearing the old-list flag, matching
	// the source branch order rather than normalizing the parser's state.
	got = nil
	p = NewProgress(func(e Event) { got = append(got, eventText(e)) })
	p.Feed(nil, []byte("these derivations will be built:\nthis derivation will be built:\n  /nix/store/d\nbuilding '/d'\n"))
	if !reflect.DeepEqual(got, []string{"build 1/2 /d"}) {
		t.Fatal(got)
	}
}

func TestProgressLiteralPrefixesAndUnboundedTotals(t *testing.T) {
	got := []string{}
	p := NewProgress(func(e Event) { got = append(got, eventText(e)) })
	p.Feed(nil, []byte(" these 8 derivations will be built:\nthese 184467440737095516160 derivations will be built:\n building '/ignored'\nbuilding ''\nbuilding '/literal' more\nthese 0 paths will be fetched (0 MiB)\ncopying path '/zero'\n"))
	p.Finish()
	if !reflect.DeepEqual(got, []string{"build 1/184467440737095516160 /literal", "fetch 1/0 /zero"}) {
		t.Fatal(got)
	}
	// No callback means no progress parsing, as in the original run block.
	NewProgress(nil).Feed(nil, []byte("building '/x'\n"))
}

func TestLineFramingRetainsLFCRAndEmptyLines(t *testing.T) {
	buffer := "\nfirst\r\nlast"
	for _, want := range []string{"\n", "first\r\n"} {
		got, ok := readLine(&buffer)
		if !ok || got != want {
			t.Fatalf("%q %v want %q", got, ok, want)
		}
	}
	if got, ok := readLine(&buffer); ok || got != "" || buffer != "last" {
		t.Fatal(got, ok, buffer)
	}
	got := []string{}
	p := NewProgress(func(e Event) { got = append(got, eventText(e)) })
	p.Feed(nil, []byte("these derivations will be built:\n\v/nix/store/v.drv\nbuilding '/v'\r\n"))
	if !reflect.DeepEqual(got, []string{"build 1/1 /v"}) {
		t.Fatal(got)
	}
}
