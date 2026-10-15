package hookcheck

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMessageLiteralFileAndSourceCommentsDiffTail(t *testing.T) {
	root := t.TempDir()
	name := " message\n'$(inert) "
	put(t, root, name, ";comment\nSubject\r\n\r\nBody\n;comment after body\ndiff --git a/x b/x\nignored long "+strings.Repeat("x", 90))
	before := fixtureState(t, root)
	f, err := CheckMessage(filepath.Join(root, name), ";")
	if err != nil || len(f) != 0 {
		t.Fatalf("literal/comment/diff message: %#v %v", f, err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("message check rewrote bytes/modes")
	}
}

func TestMessageEmptyRulesAndReadErrors(t *testing.T) {
	for _, data := range []string{"", " \t\r\n\x00", "# comment\ndiff --git a/x b/x\nsubject", "#only comment"} {
		root := t.TempDir()
		put(t, root, "message", data)
		f, err := CheckMessage(filepath.Join(root, "message"), "#")
		if err != nil || len(f) != 1 || f[0].Check != "EmptyMessage" || !HasErrors(f) {
			t.Fatalf("empty message %q: %#v %v", data, f, err)
		}
	}
	root := t.TempDir()
	put(t, root, "message", "\u00a0") // Ruby strip does not remove NBSP.
	if f, err := CheckMessage(filepath.Join(root, "message"), "#"); err != nil || HasErrors(f) {
		t.Fatalf("Unicode strip rule widened: %#v %v", f, err)
	}
	if _, err := CheckMessage(filepath.Join(root, "missing"), "#"); !os.IsNotExist(err) {
		t.Fatalf("missing supplied file accepted: %v", err)
	}
	if _, err := CheckMessage(filepath.Join(root, "message"), ""); err == nil {
		t.Fatal("unresolved comment prefix accepted")
	}
}

func TestMessageAdvisorySubjectBodyWidthsAndPrefixes(t *testing.T) {
	for _, test := range []struct {
		name, data string
		checks     []string
	}{
		{"ordinary", "Subject\n\nBody\n", nil},
		{"second line", "Subject\nnot blank\n", []string{"SingleLineSubject"}},
		{"subject width", strings.Repeat("s", 61), []string{"TextWidth"}},
		{"fixup width allowance", "fixup! " + strings.Repeat("s", 60), nil},
		{"squash width allowance", "squash! " + strings.Repeat("s", 60), nil},
		{"prefix still bounded", "fixup! " + strings.Repeat("s", 61), []string{"TextWidth"}},
		{"body width", "Subject\n\n" + strings.Repeat("b", 73) + "\r\n", []string{"TextWidth"}},
		{"period", "Subject.  \t\r\n\nBody", []string{"TrailingPeriod"}},
		{"Unicode character widths", strings.Repeat("東京", 30) + "\n\n" + strings.Repeat("字", 72), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "message", test.data)
			before := fixtureState(t, root)
			f, err := CheckMessage(filepath.Join(root, "message"), "#")
			if err != nil || HasErrors(f) {
				t.Fatalf("advisory blocked commit: %#v %v", f, err)
			}
			var checks []string
			for _, finding := range f {
				checks = append(checks, finding.Check)
			}
			if !reflect.DeepEqual(checks, test.checks) {
				t.Fatalf("warnings %q != %q", checks, test.checks)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("advisory altered supplied message")
			}
		})
	}
}
