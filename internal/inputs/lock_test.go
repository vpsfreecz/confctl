package inputs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Source-derived boundaries from pinned cc40679 lib/confctl/flake_lock.rb.
// These assertions are not a replacement for accepted original observations.
const readerLock = `{"root":"other","unknown":{"large":9007199254740993},"nodes":{
"root":{"inputs":{"z":"git","a":["github","ignored"],"missing":"absent","empty":[],"nonstring":[5,"git"],"path":"path","unicode":"unicode"}},
"other":{"inputs":{"wrong":"git"}},
"git":{"locked":{"type":"git","url":"git+ssh://locked/repo","ref":"locked-ref","rev":"0123456789abcdef"},"original":{"ref":"main","url":"git+https://original/repo"}},
"github":{"locked":{"type":"github","owner":"locked","repo":"repository","rev":"abcdef0123456789"},"original":{"owner":"original","repo":"old","ref":"stable","url":"wrong"}},
"path":{"original":{"type":"path","path":"./local"}},
"unicode":{"locked":{"rev":"áéíóúαβγδε"}},
"ignored":{"locked":{"rev":"must-not-follow"}}}}
`

func TestLockRootResolutionAndInfo(t *testing.T) {
	l, err := DecodeLock([]byte(readerLock))
	if err != nil {
		t.Fatal(err)
	}
	names, err := l.RootInputs()
	if err != nil || !reflect.DeepEqual(names, []string{"a", "empty", "missing", "nonstring", "path", "unicode", "z"}) {
		t.Fatal(names, err)
	}
	for _, tc := range []struct {
		name any
		want Info
	}{
		{"z", Info{Type: "git", Ref: "main", Rev: "0123456789abcdef", ShortRev: "01234567", URL: "ssh://locked/repo"}},
		{"a", Info{Type: "github", Ref: "stable", Rev: "abcdef0123456789", ShortRev: "abcdef01", URL: "https://github.com/locked/repository"}},
		{"path", Info{Type: "path"}},
		{"unicode", Info{Type: "-", Rev: "áéíóúαβγδε", ShortRev: "áéíóúαβγ"}},
		{"missing", Info{Type: "-"}},
		{"empty", Info{Type: "-"}},
		{"nonstring", Info{Type: "-"}},
		{nil, Info{Type: "-"}},
	} {
		info, err := l.InputInfo(tc.name)
		if err != nil || !reflect.DeepEqual(info, tc.want) {
			t.Fatal(tc.name, info, tc.want, err)
		}
	}
	if l.data["unknown"].(map[string]any)["large"] != json.Number("9007199254740993") {
		t.Fatal("reader rounded unknown metadata", l.data)
	}
}

func TestLockMissingEmptyAndCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flake.lock")
	if _, err := LoadLock(path); !os.IsNotExist(err) {
		t.Fatal("missing lock lost filesystem error", err)
	}
	for _, b := range []string{`{}`, `{"nodes":{}}`, `{"nodes":{"root":{}}}`, `{"nodes":{"root":{"inputs":null}}}`} {
		l, err := DecodeLock([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		names, err := l.RootInputs()
		if err != nil || len(names) != 0 {
			t.Fatal(names, err)
		}
	}
	for _, b := range []string{`bad json`, `{"nodes":`, `{} {}`, `[]`} {
		if _, err := DecodeLock([]byte(b)); err == nil {
			t.Fatal("accepted invalid lock", b)
		}
	}
	if err := os.WriteFile(path, []byte(readerLock), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.InputInfo("a"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != readerLock {
		t.Fatal("read changed bytes", err)
	}
}

func TestLockURLAndRefPrecedence(t *testing.T) {
	for _, tc := range []struct {
		locked, original string
		want             Info
	}{
		{`{"url":"git+git+https://repo","ref":"locked","rev":""}`, `{"ref":"","type":"git"}`, Info{Type: "git", Ref: "", Rev: "", ShortRev: "", URL: "git+https://repo"}},
		{`{"type":false,"ref":false,"url":false}`, `{"type":"path","ref":"main","url":"git+file:///local"}`, Info{Type: "path", Ref: "main", URL: "file:///local"}},
		{`{"type":"github","owner":"new"}`, `{"repo":"repo","url":"git+https://fallback"}`, Info{Type: "github", URL: "https://github.com/new/repo"}},
		{`{"type":"github"}`, `{"owner":"only","url":"git+https://fallback"}`, Info{Type: "github", URL: "https://fallback"}},
	} {
		b := `{"nodes":{"root":{"inputs":{"input":"node"}},"node":{"locked":` + tc.locked + `,"original":` + tc.original + `}}}`
		l, err := DecodeLock([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		got, err := l.InputInfo("input")
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatal(got, tc.want, err)
		}
	}
	l, err := DecodeLock([]byte(strings.Replace(readerLock, `"root":"other"`, `"root":"root"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if names, err := l.RootInputs(); err != nil || names[0] != "a" {
		t.Fatal(names, err)
	}
}

func TestLockInitialValueDiagnosticIsBounded(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"bad json\n", "unexpected character: 'bad' at line 1 column 1"},
		{"\n  ?token next", "unexpected character: '?token' at line 2 column 3"},
	} {
		if _, err := DecodeLock([]byte(tc.input)); err == nil || err.Error() != tc.want {
			t.Fatal(tc.input, err)
		}
	}
	for _, input := range []string{`{"x":`, `nullx`, `"unterminated`} {
		_, err := DecodeLock([]byte(input))
		if err == nil || strings.Contains(err.Error(), "unexpected character:") {
			t.Fatal("unobserved syntax class was silently translated", input, err)
		}
	}
}
