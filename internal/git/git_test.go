package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCommitLiteralInheritedRunnerAndFailure(t *testing.T) {
	for _, editor := range []bool{false, true} {
		for _, tty := range []bool{false, true} {
			err := Commit(context.Background(), "root", "subject\n\nbody 'literal'", editor, tty, func(_ context.Context, root string, argv []string) (int, error) {
				want := []string{"git", "commit"}
				if editor && tty {
					want = append(want, "-e")
				}
				want = append(want, "-m", "subject\n\nbody 'literal'", "flake.lock")
				if root != "root" || !reflect.DeepEqual(argv, want) {
					t.Fatal(root, argv, want)
				}
				return 0, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, failure := range []error{errors.New("spawn"), context.Canceled, context.DeadlineExceeded} {
		err := Commit(context.Background(), "root", "message", true, true, func(context.Context, string, []string) (int, error) { return -1, failure })
		if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
		} else if err == nil || err.Error() != "git commit failed" {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := Commit(ctx, "root", "message", true, true, func(context.Context, string, []string) (int, error) { cancel(); return 0, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCommitAdapterDiagnosticAndCancellationPriority(t *testing.T) {
	underlying := errors.New("commit relay malformed startup")
	marked := &CommitAdapterError{Err: underlying}
	for _, failure := range []error{marked, fmt.Errorf("terminal setup: %w", marked), errors.Join(errors.New("cleanup diagnostic"), marked)} {
		err := Commit(context.Background(), "root", "message", false, false, func(context.Context, string, []string) (int, error) { return -1, failure })
		if err != failure || !errors.Is(err, underlying) {
			t.Fatal("adapter diagnostic replaced", err, failure)
		}
		var adapter *CommitAdapterError
		if !errors.As(err, &adapter) || adapter != marked {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := Commit(ctx, "root", "message", false, false, func(context.Context, string, []string) (int, error) { cancel(); return -1, marked })
	if err != context.Canceled {
		t.Fatal("adapter failure lost cancellation priority", err)
	}
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		failure := errors.Join(marked, fmt.Errorf("invocation: %w", cancellation))
		err = Commit(context.Background(), "root", "message", false, false, func(context.Context, string, []string) (int, error) { return -1, failure })
		if err != failure || !errors.Is(err, cancellation) {
			t.Fatal(err)
		}
	}
}
func TestMirrorActualURLCacheCloneFetchLogAndCancellation(t *testing.T) {
	root := t.TempDir()
	var calls [][]string
	m := Mirror{Root: root, Cache: filepath.Join(root, "cache/confctl"), URL: "https://repo/", Run: func(_ context.Context, dir string, argv []string) (string, error) {
		if dir != root {
			t.Fatal(dir)
		}
		calls = append(calls, append([]string{}, argv...))
		return "\n > change \n", nil
	}}
	wantPath := filepath.Join(root, "cache/confctl/git-mirrors", fmt.Sprintf("%x", sha256.Sum256([]byte("https://repo/"))))
	if m.Path() != wantPath {
		t.Fatal(m.Path())
	}
	if err := m.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, err := os.Stat(wantPath); err != nil || !s.IsDir() {
		t.Fatal(s, err)
	}
	if err := m.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	text, err := m.Log(context.Background(), "old", "new")
	if err != nil || text != "git log for old..new\n> change" {
		t.Fatal(text, err)
	}
	want := [][]string{{"git", "clone", "--quiet", "--mirror", "https://repo/", wantPath}, {"git", "-C", wantPath, "fetch", "--no-show-forced-updates", "--quiet"}, {"git", "-C", wantPath, "log", "--no-decorate", "--left-right", "--cherry-mark", "--oneline", "old..new"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal(calls, want)
	}
	if (Mirror{Cache: m.Cache, URL: "https://repo"}).Path() == m.Path() {
		t.Fatal("normalized cache identity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Run = func(context.Context, string, []string) (string, error) {
		cancel()
		return "", errors.New("ordinary failure")
	}
	if err = m.Setup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestMirrorRelativeCacheAndFailedCloneRetainsDirectory(t *testing.T) {
	root := t.TempDir()
	m := Mirror{Root: root, Cache: "relative//cache/confctl", URL: "literal", Run: func(_ context.Context, _ string, argv []string) (string, error) {
		if argv[5] != "relative//cache/confctl/git-mirrors/"+fmt.Sprintf("%x", sha256.Sum256([]byte("literal"))) {
			t.Fatal(argv)
		}
		return "", errors.New("refused")
	}}
	if err := m.Setup(context.Background()); err == nil {
		t.Fatal("clone failure hidden")
	}
	if s, err := os.Stat(filepath.Join(root, m.Path())); err != nil || !s.IsDir() {
		t.Fatal("clone partial state removed", s, err)
	}
}

func TestMirrorStatFailureDoesNotRunOrRepair(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	m := Mirror{Root: root, Cache: filepath.Join(blocked, "cache"), URL: "literal", Run: func(context.Context, string, []string) (string, error) {
		t.Fatal("tool ran after stat failure")
		return "", nil
	}}
	if err := m.Setup(context.Background()); err == nil {
		t.Fatal("invalid cache accepted")
	}
	b, err := os.ReadFile(blocked)
	if err != nil || string(b) != "retain" {
		t.Fatal("failed mirror repaired cache", string(b), err)
	}
}

func TestMirrorLogOptionsLiteralArgumentsStripAndBorrowedSlice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		legacy  bool
		options []string
		output  string
		body    string
	}{
		{"legacy", true, nil, "\x00\t\n\v\f\r > old\n= kept \t\x00", "> old\n= kept"},
		{"nil", false, nil, "", ""},
		{"empty", false, []string{}, " \t\r\n\v\f\x00", ""},
		{"oneline_max", false, []string{"--oneline", "--max-count=50"}, " > one \n", "> one"},
		{"patch", false, []string{"-p"}, "\tpatch\n\nbody\r\n", "patch\n\nbody"},
		{"ordered", false, []string{"--oneline", "-p", "--max-count=50", "-p"}, " \u00a0kept\u00a0 \n", "\u00a0kept\u00a0"},
		{"literal", false, []string{"", "--format=quoted 'value'\n;$(literal)*", "--", "--max-count=0"}, "\x00 body \x00", "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), struct{}{}, "same-context")
			from, to := "old ref;$(literal)\n--all", "new' ref*..end"
			span := from + ".." + to
			m := Mirror{Root: "literal root/../cwd", Cache: "relative//cache/", URL: "literal URL/"}
			wantOptions := tc.options
			if tc.legacy {
				wantOptions = []string{"--oneline"}
			}
			want := []string{"git", "-C", m.Path(), "log", "--no-decorate", "--left-right", "--cherry-mark"}
			want = append(want, wantOptions...)
			want = append(want, span)
			// Spare capacity catches implementations that append the range into
			// the caller's backing array. The fake also mutates its argv copy.
			var options, backing, before []string
			if tc.options != nil {
				backing = append(append([]string{}, tc.options...), "spare-one", "spare-two")
				options = backing[:len(tc.options)]
				before = append([]string{}, backing...)
			}
			calls := 0
			m.Run = func(actual context.Context, cwd string, argv []string) (string, error) {
				calls++
				if actual != ctx || cwd != m.Root || !reflect.DeepEqual(argv, want) {
					t.Fatalf("context/CWD/argv changed: %q %q; want %q", cwd, argv, want)
				}
				if len(wantOptions) > 0 {
					argv[7] = "runner-owned-change"
				}
				return tc.output, nil
			}
			var text string
			var err error
			if tc.legacy {
				text, err = m.Log(ctx, from, to)
			} else {
				text, err = m.LogWithOptions(ctx, from, to, options)
			}
			if err != nil || text != "git log for "+span+"\n"+tc.body || calls != 1 {
				t.Fatalf("text%q err%v calls%d", text, err, calls)
			}
			if !reflect.DeepEqual(backing, before) {
				t.Fatalf("modified borrowed options: %q; before %q", backing, before)
			}
		})
	}
}

func TestMirrorLogOptionsErrorsAndCancellationPriority(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		call := func(m Mirror, ctx context.Context) (string, error) {
			if legacy {
				return m.Log(ctx, "from", "to")
			}
			return m.LogWithOptions(ctx, "from", "to", []string{"-p", "--max-count=50"})
		}
		for _, failure := range []error{errors.New("ordinary log error"), context.Canceled, context.DeadlineExceeded} {
			m := Mirror{Run: func(context.Context, string, []string) (string, error) { return "partial stdout", failure }}
			text, err := call(m, context.Background())
			if text != "" || err != failure {
				t.Fatalf("legacy%v lost runner error/output boundary: text%q err%v", legacy, text, err)
			}
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		expired, stop := context.WithDeadline(context.Background(), time.Unix(1, 0))
		for _, ctx := range []context.Context{canceled, expired} {
			m := Mirror{Run: func(context.Context, string, []string) (string, error) {
				t.Fatal("ran after preexisting cancellation")
				return "", nil
			}}
			text, err := call(m, ctx)
			if text != "" || err != ctx.Err() {
				t.Fatalf("legacy%v pre-cancel text%q err%v", legacy, text, err)
			}
		}
		stop()
		for _, failure := range []error{nil, errors.New("ordinary callback failure")} {
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			m := Mirror{Run: func(actual context.Context, _ string, _ []string) (string, error) {
				if actual != ctx {
					t.Fatal("context replaced")
				}
				calls++
				cancel()
				return "successful or partial stdout", failure
			}}
			text, err := call(m, ctx)
			if text != "" || err != context.Canceled || calls != 1 {
				t.Fatalf("legacy%v callback cancellation priority text%q err%v calls%d", legacy, text, err, calls)
			}
			cancel()
		}
	}
}
