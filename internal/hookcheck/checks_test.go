package hookcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAuthorPresenceAndRetainedPredicates(t *testing.T) {
	for _, test := range []struct {
		name string
		env  []string
		bad  string
	}{
		{"config fallback", nil, ""},
		{"present empty email", []string{"GIT_AUTHOR_EMAIL="}, "AuthorEmail"},
		{"present empty name", []string{"GIT_AUTHOR_NAME="}, "AuthorName"},
		{"empty domain allowed", []string{"GIT_AUTHOR_EMAIL=foo@"}, ""},
		{"Ruby line anchors retained", []string{"GIT_AUTHOR_EMAIL=foo@\nother text"}, ""},
		{"missing at", []string{"GIT_AUTHOR_EMAIL=example"}, "AuthorEmail"},
		{"single name allowed", []string{"GIT_AUTHOR_NAME=One"}, ""},
		{"whitespace name retained", []string{"GIT_AUTHOR_NAME= "}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Snapshot{dir: t.TempDir(), env: test.env, config: map[string]string{"user.email": "configured@", "user.name": "Configured"}}
			f, err := CheckFiles(context.Background(), s, CheckOptions{})
			if err != nil || HasErrors(f) != (test.bad != "") {
				t.Fatalf("author: %#v %v", f, err)
			}
			if test.bad != "" && (len(f) != 1 || f[0].Check != test.bad) {
				t.Fatalf("presence did not win: %#v", f)
			}
		})
	}
}

func TestFormatToolsUseLiteralStagedPathsAndDetachedGitEnvironment(t *testing.T) {
	root, env := newFixture(t)
	names := []string{" edge\nname.nix", "quote'\"$(inert).nix", "sample.go"}
	for _, name := range names {
		put(t, root, name, "staged "+name)
	}
	put(t, root, "config/import.nix", "staged import")
	stage(t, root, env, ".")
	put(t, root, "config/import.nix", "unstaged import")
	put(t, root, "sample.go", "unstaged Go")
	put(t, root, "untracked", "retained")
	before := fixtureState(t, root)
	env = append(env, "GIT_DIR="+filepath.Join(root, ".git"), "GIT_WORK_TREE="+root)
	s := captureFixture(t, root, env)
	var calls []Invocation
	f, err := CheckFiles(context.Background(), s, CheckOptions{Nixfmt: "/declared/nixfmt", Gofmt: "/declared/gofmt",
		Invoke: func(ctx context.Context, req Invocation) (ToolResult, error) {
			calls = append(calls, req)
			if req.Dir != s.Directory() {
				t.Fatal("tool CWD is not staged tree")
			}
			for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
				if _, present := envValue(req.Env, key); present {
					t.Fatalf("real index context leaked to tool: %s", key)
				}
			}
			if value, ok := envValue(req.Env, "GIT_CEILING_DIRECTORIES"); !ok || value != filepath.Dir(s.Directory()) {
				t.Fatal("Git discovery not bounded")
			}
			b, err := os.ReadFile(filepath.Join(req.Dir, "config/import.nix"))
			if err != nil || string(b) != "staged import" {
				t.Fatal("tool sees unstaged import/config")
			}
			return ToolResult{}, nil
		}})
	if err != nil || HasErrors(f) || len(calls) != 2 {
		t.Fatalf("format calls: %#v %#v %v", calls, f, err)
	}
	wantNix := []string{"--check", filepath.Join(s.Directory(), " edge\nname.nix"), filepath.Join(s.Directory(), "config/import.nix"), filepath.Join(s.Directory(), "quote'\"$(inert).nix")}
	if !reflect.DeepEqual(calls[0].Args, wantNix) || !reflect.DeepEqual(calls[1].Args, []string{"-l", filepath.Join(s.Directory(), "sample.go")}) {
		t.Fatalf("literal argv/order: %#v", calls)
	}
	if err := s.Close(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("tool boundary changed real state")
	}
}

func TestFormatFailureMissingToolAndCancellationPreserveState(t *testing.T) {
	for _, mode := range []string{"status", "spawn", "missing", "cancel", "gofmt-output"} {
		t.Run(mode, func(t *testing.T) {
			root, env := newFixture(t)
			name := "file.nix"
			if mode == "gofmt-output" {
				name = "file\n.go"
			}
			put(t, root, name, "staged")
			stage(t, root, env, name)
			put(t, root, name, "unstaged")
			put(t, root, "untracked", "untouched")
			before := fixtureState(t, root)
			s := captureFixture(t, root, env)
			boom := errors.New("spawn failure")
			o := CheckOptions{Nixfmt: "nixfmt", Gofmt: "gofmt", Invoke: func(context.Context, Invocation) (ToolResult, error) {
				switch mode {
				case "spawn":
					return ToolResult{}, boom
				case "cancel":
					return ToolResult{}, context.Canceled
				case "gofmt-output":
					return ToolResult{Stdout: []byte("literal filename with\n.go\n")}, nil
				default:
					return ToolResult{ExitCode: 1, Stderr: []byte("formatter refusal")}, nil
				}
			}}
			if mode == "missing" {
				o.Nixfmt = ""
			}
			f, err := CheckFiles(context.Background(), s, o)
			if mode == "status" || mode == "gofmt-output" {
				if err != nil || !HasErrors(f) {
					t.Fatalf("tool failure accepted: %#v %v", f, err)
				}
			} else if err == nil {
				t.Fatal("infrastructure failure accepted")
			}
			if mode == "spawn" && !errors.Is(err, boom) {
				t.Fatal("spawn error masked")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancel error masked")
			}
			if err := s.Close(nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("failed tool changed index/worktree/untracked state")
			}
		})
	}
}

func TestMergeMarkerBinaryAndUnreadableRefusal(t *testing.T) {
	for _, test := range []struct {
		data string
		bad  bool
	}{
		{"<<<<<<< conflict\n", true}, {"text\n<<<<<<<\tconflict", true},
		{" <<<<<<< conflict", false}, {"<<<<<<<without-space", false},
		{"=======\n>>>>>>> end", false}, {"\x00<<<<<<< conflict\n", false},
		{"\xff<<<<<<< conflict\n", false},
	} {
		root, env := newFixture(t)
		put(t, root, "file", test.data)
		stage(t, root, env, "file")
		s := captureFixture(t, root, env)
		f, err := CheckFiles(context.Background(), s, CheckOptions{})
		if err != nil || HasErrors(f) != test.bad {
			t.Fatalf("marker/binary %q: %#v %v", test.data, f, err)
		}
	}
	root, env := newFixture(t)
	put(t, root, "file", "ordinary text")
	stage(t, root, env, "file")
	before := fixtureState(t, root)
	s := captureFixture(t, root, env)
	if err := os.Remove(filepath.Join(s.Directory(), "file")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFiles(context.Background(), s, CheckOptions{}); err == nil {
		t.Fatal("unreadable marker input silently accepted")
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("read refusal changed real state")
	}
}

func TestSourceCaseRulesAndHEADDirectoryScope(t *testing.T) {
	// Source uses Ruby String#downcase, not Unicode case folding. U+0130 has
	// the full lowercase i+combining-dot mapping; sharp-s is not expanded to ss.
	for _, pair := range [][2]string{{"FILE", "file"}, {"Ä", "ä"}, {"İ", "i\u0307"}, {"ẞ", "ß"}, {"Σ", "σ"}} {
		s := &Snapshot{selected: []string{pair[1]}, headPaths: []string{pair[0], "other/" + pair[0]}}
		f := caseFindings(s)
		if len(f) != 1 || f[0].Path != pair[1] {
			t.Fatalf("source lowercase %q: %#v", pair, f)
		}
	}
	for _, s := range []*Snapshot{
		{selected: []string{"ß"}, headPaths: []string{"SS"}},
		{selected: []string{"dir/file"}, headPaths: []string{"other/FILE"}},
		{selected: []string{"same"}, headPaths: []string{"same"}},
	} {
		if f := caseFindings(s); len(f) != 0 {
			t.Fatalf("case-fold/directory/duplicate scope widened: %#v", f)
		}
	}
	s := &Snapshot{selected: []string{"A", "a"}}
	if f := caseFindings(s); len(f) != 2 {
		t.Fatal("initial selected conflicts skipped")
	}
}

func TestSymlinkTargetSizeAndFormatterEscape(t *testing.T) {
	for _, target := range []string{"empty", "missing", "nonempty", "../outside", "/absolute/outside"} {
		t.Run(target, func(t *testing.T) {
			root, env := newFixture(t)
			put(t, root, "empty", "")
			put(t, root, "nonempty", "value")
			if err := os.Symlink(target, filepath.Join(root, "link.nix")); err != nil {
				t.Fatal(err)
			}
			stage(t, root, env, ".")
			before := fixtureState(t, root)
			s := captureFixture(t, root, env)
			calls := 0
			f, err := CheckFiles(context.Background(), s, CheckOptions{Nixfmt: "nixfmt", Invoke: func(context.Context, Invocation) (ToolResult, error) {
				calls++
				return ToolResult{}, nil
			}})
			if target == "nonempty" {
				if err != nil || HasErrors(f) || calls != 1 {
					t.Fatalf("valid symlink: %#v %v", f, err)
				}
			} else {
				if err == nil && !HasErrors(f) || calls != 0 {
					t.Fatalf("broken/unsafe formatter target accepted: %#v %v", f, err)
				}
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("symlink check changed source")
			}
		})
	}
}

func TestConfinedSymlinkDotDotKeepsPhysicalTraversal(t *testing.T) {
	root, env := newFixture(t)
	put(t, root, "physical.nix", "wrong lexical file")
	put(t, root, "deep/physical.nix", "correct physical file")
	put(t, root, "deep/sub/keep", "directory")
	if err := os.Symlink("deep/sub", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("link/../physical.nix", filepath.Join(root, "selected.nix")); err != nil {
		t.Fatal(err)
	}
	stage(t, root, env, ".")
	s := captureFixture(t, root, env)
	p, err := s.confinedPath("selected.nix")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "correct physical file" {
		t.Fatalf("symlink/.. was cleaned lexically: %q %v", b, err)
	}
}

func TestExplicitServiceRubyLaneMessagesAndUnparseableFailure(t *testing.T) {
	root, env := newFixture(t)
	put(t, root, "service/code.rb", "staged Ruby")
	put(t, root, ".rubocop.yml", "staged lint config")
	stage(t, root, env, ".")
	put(t, root, ".rubocop.yml", "unstaged lint config")
	before := fixtureState(t, root)
	s := captureFixture(t, root, env)
	f, err := CheckFiles(context.Background(), s, CheckOptions{})
	if err != nil || !HasErrors(f) || !strings.Contains(f[0].Message, "service-checks") {
		t.Fatalf("ordinary lane silently omitted Ruby: %#v %v", f, err)
	}
	for _, test := range []struct {
		name                        string
		result                      ToolResult
		wantError, wantFindingError bool
	}{
		{"success", ToolResult{}, false, false},
		{"generic warning", ToolResult{ExitCode: 1, Stderr: []byte("warning: retained\n")}, false, false},
		{"generic warn", ToolResult{ExitCode: 1, Stderr: []byte("warn: retained\n")}, false, false},
		{"generic capitalized Warning", ToolResult{ExitCode: 1, Stderr: []byte("Warning: retained\n")}, false, true},
		{"generic uppercase WARNING", ToolResult{ExitCode: 1, Stderr: []byte("WARNING: retained\n")}, false, true},
		{"cop warning", ToolResult{ExitCode: 1, Stdout: []byte("service/code.rb:999:2: W: warning on unchanged line\n")}, false, false},
		{"cop error unmodified", ToolResult{ExitCode: 1, Stdout: []byte("service/code.rb:999:2: C: error on unchanged line\n")}, false, true},
		{"generic error", ToolResult{ExitCode: 2, Stderr: []byte("Error: configuration failed\n")}, false, true},
		{"unparseable", ToolResult{ExitCode: 2, Stdout: []byte("??? refused\n")}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, err := CheckFiles(context.Background(), s, CheckOptions{ServiceRuby: true, RuboCop: "/declared/rubocop",
				Invoke: func(ctx context.Context, req Invocation) (ToolResult, error) {
					want := []string{"--format=emacs", "--force-exclusion", "--display-cop-names", filepath.Join(s.Directory(), "service/code.rb")}
					if !reflect.DeepEqual(req.Args, want) || req.Program != "/declared/rubocop" {
						t.Fatalf("Ruby flags/globs changed: %#v", req)
					}
					b, _ := os.ReadFile(filepath.Join(req.Dir, ".rubocop.yml"))
					if string(b) != "staged lint config" {
						t.Fatal("Ruby callback saw unstaged config")
					}
					return test.result, nil
				}})
			if (err != nil) != test.wantError || HasErrors(f) != test.wantFindingError {
				t.Fatalf("Ruby result: %#v %v", f, err)
			}
			if strings.HasPrefix(test.name, "generic") && (len(f) != 1 || f[0].Check != "RuboCop" ||
				f[0].Message != strings.TrimSuffix(string(test.result.Stderr), "\n") || f[0].Warning != !test.wantFindingError) {
				t.Fatalf("generic parsed classification: %#v", f)
			}
			if strings.Contains(test.name, "unmodified") && (len(f) != 1 || f[0].Line != 999) {
				t.Fatalf("unmodified-line error suppressed: %#v", f)
			}
		})
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("Ruby lane callback changed real state")
	}
	for _, path := range []string{"Gemfile", "nested/Gemfile", "Rakefile", "nested/file.rb", "file.ru", "file.rake", "file.gemspec"} {
		if !rubyPath(path) {
			t.Fatalf("source Ruby glob omitted: %s", path)
		}
	}
}
