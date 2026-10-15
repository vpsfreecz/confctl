package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/vpsfreecz/confctl/internal/inputs"
)

func TestComparisonChangelogOptionsLiteralRangeAndMirrorResidue(t *testing.T) {
	for _, tc := range []struct{ args, want []string }{
		{nil, []string{"--oneline", "--max-count=50"}},
		{[]string{"-v"}, []string{}},
		{[]string{"-p"}, []string{"--oneline", "-p"}},
		{[]string{"-v", "-p"}, []string{"-p"}},
	} {
		for _, downgrade := range []bool{false, true} {
			t.Run(strings.Join(tc.args, "/")+fmt.Sprint(downgrade), func(t *testing.T) {
				root := t.TempDir()
				cache := root + "/cache//literal"
				t.Setenv("XDG_CACHE_HOME", cache)
				e := &Engine{Root: root, Context: context.Background()}
				url := "url 'quoted';$(literal)\nrepo"
				path := cache + "/confctl/git-mirrors/" + fmt.Sprintf("%x", sha256.Sum256([]byte(url)))
				calls := [][]string{}
				options := [][]string{}
				cloneError := errors.New("clone failed after mkdir")
				log := func(ctx context.Context, u, from, to string, opts []string) (string, error) {
					options = append(options, append([]string{}, opts...))
					return e.comparisonMirrorLog(ctx, u, from, to, opts, func(actual context.Context, cwd string, argv []string) (string, error) {
						if actual != e.Context || cwd != root {
							t.Fatal("context/CWD", actual, cwd)
						}
						calls = append(calls, append([]string{}, argv...))
						if argv[1] == "clone" {
							return "partial clone", cloneError
						}
						if argv[3] == "log" {
							return " \n< entry\t\n", nil
						}
						return "", nil
					})
				}
				args := append([]string{"changelog"}, tc.args...)
				if downgrade {
					args = append(args, "--downgrade")
				}
				st := Status{Machine: Machine{Name: "h"}, Target: map[string]map[string]any{"a": {"rev": "NEW;literal", "url": url}, "b": {"rev": "NEW;literal", "url": url}}, Deployed: map[string]map[string]any{"a": {"rev": "OLD..literal"}, "b": {"rev": "OLD..literal"}}}
				sink := &comparisonSink{}
				err := comparisonRender(e.Context, sink, comparisonInvocation(t, args...), nil, st, log)
				from, to := "OLD..literal", "NEW;literal"
				if downgrade {
					from, to = to, from
				}
				logArgv := append([]string{"git", "-C", path, "log", "--no-decorate", "--left-right", "--cherry-mark"}, tc.want...)
				logArgv = append(logArgv, from+".."+to)
				wantCalls := [][]string{{"git", "clone", "--quiet", "--mirror", url, path}, {"git", "-C", path, "fetch", "--no-show-forced-updates", "--quiet"}, logArgv}
				wantText := "h @ a:\nclone failed after mkdir\n\nh @ b:\ngit log for " + from + ".." + to + "\n< entry\n\n"
				if err != nil || !reflect.DeepEqual(calls, wantCalls) || !reflect.DeepEqual(options, [][]string{tc.want, tc.want}) || sink.String() != wantText {
					t.Fatal(err, calls, options, sink.String(), wantText)
				}
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatal("partial clone residue not retained", info, err)
				}
			})
		}
	}
}

func TestComparisonChangelogReachedWriteRescueAndOutsideWrites(t *testing.T) {
	st := Status{Machine: Machine{Name: "h"}, Target: comparisonInfo(t, `{"a":{"rev":"new","url":"repo"},"b":{"rev":"new","url":"repo"}}`), Deployed: comparisonInfo(t, `{"a":"old","b":"old"}`)}
	for _, tc := range []struct {
		name       string
		failedCall int
		retryFails bool
		wantCalls  []string
		wantLogs   int
		wantError  bool
	}{
		{"header", 1, false, []string{"h @ a:"}, 0, true},
		{"log-one-shot", 2, false, []string{"h @ a:", "log", "write failed", "", "h @ b:", "log", ""}, 2, false},
		{"retry", 2, true, []string{"h @ a:", "log", "write failed"}, 1, true},
		{"trailing-blank", 3, false, []string{"h @ a:", "log", ""}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := errors.New("write failed")
			sink := &comparisonSink{fail: func(n int, _ string) error {
				if n == tc.failedCall || tc.retryFails && n == tc.failedCall+1 {
					return failure
				}
				return nil
			}}
			logs := 0
			err := comparisonRender(context.Background(), sink, comparisonInvocation(t, "changelog"), nil, st, func(context.Context, string, string, string, []string) (string, error) { logs++; return "log", nil })
			if (err != nil) != tc.wantError || logs != tc.wantLogs || !reflect.DeepEqual(sink.texts, tc.wantCalls) {
				t.Fatal(err, logs, sink.texts)
			}
		})
	}
	// Mirror ordinary errors are rendered per role; cancellation errors are not.
	for _, failure := range []error{errors.New("bad ref"), context.Canceled, context.DeadlineExceeded} {
		sink := &comparisonSink{}
		logs := 0
		err := comparisonRender(context.Background(), sink, comparisonInvocation(t, "changelog"), nil, st, func(context.Context, string, string, string, []string) (string, error) {
			logs++
			return "partial output", failure
		})
		if failure.Error() == "bad ref" {
			if err != nil || logs != 2 || sink.String() != "h @ a:\nbad ref\n\nh @ b:\nbad ref\n\n" {
				t.Fatal(err, logs, sink.String())
			}
		} else {
			if err != failure || logs != 1 || !reflect.DeepEqual(sink.texts, []string{"h @ a:"}) {
				t.Fatal(err, logs, sink.texts)
			}
		}
	}
}

func TestComparisonActualPageOneShotClosedLogRetryAndFallbackDiagnostic(t *testing.T) {
	ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true), connectionMachine("a", "key-a", "unused.invalid", true)}
	e, d, events := comparisonFixture(t, ms)
	pipe := &comparisonPipe{events: events}
	pipe.failure = func(n int, p []byte) (int, error) {
		if n == 2 {
			return 0, syscall.EPIPE
		}
		return len(p), nil
	}
	comparisonSystemPager(&d, events, pipe)
	d.Pager.Probe = func(context.Context, string) (bool, error) {
		*events = append(*events, "probe-failed")
		return false, nil
	}
	d.Metadata = func() (*inputs.MachineResolver, error) {
		return &inputs.MachineResolver{Eval: func(s string) ([]byte, error) {
			if s == ".#confctl.machineKeys" {
				return []byte(`{"z":"key-z","a":"key-a"}`), nil
			}
			return []byte(`{"os":{"rev":"new","url":"repo"}}`), nil
		}}, nil
	}
	d.Log = func(context.Context, string, string, string, []string) (string, error) {
		*events = append(*events, "mirror")
		return "log", nil
	}
	_, err := runComparison(t, e, comparisonInvocation(t, "changelog", "-y"), "", d)
	if err != nil || pipe.out.String() != "z @ os:\nThe pager process (`pager`) was closed\n\na @ os:\nlog\n\n" {
		t.Fatal(err, pipe.out.String(), *events)
	}
	if !strings.Contains(strings.Join(*events, "|"), "start:pager") || (*events)[len(*events)-1] != "close" {
		t.Fatal(*events)
	}
}

func TestComparisonMirrorFilesystemErrorsAndCancellation(t *testing.T) {
	for _, kind := range []string{"mkdir", "stat", "fetch", "log", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			cache := root + "/cache"
			t.Setenv("XDG_CACHE_HOME", cache)
			e := &Engine{Root: root, Context: context.Background()}
			url := "repo"
			path := cache + "/confctl/git-mirrors/" + fmt.Sprintf("%x", sha256.Sum256([]byte(url)))
			{
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "mkdir" {
					// A dangling directory link passes Setup's ENOENT branch but
					// refuses MkdirAll at the existing final component.
					if err := os.Symlink(root+"/missing", path); err != nil {
						t.Fatal(err)
					}
				} else if kind == "stat" {
					if err := os.Symlink(path, path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			failure := errors.New("ordinary " + kind)
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := e.comparisonMirrorLog(ctx, url, "old", "new", []string{"--oneline"}, func(_ context.Context, _ string, argv []string) (string, error) {
				calls++
				if kind == "cancel" {
					cancel()
					return "partial", failure
				}
				if argv[3] == kind {
					return "partial", failure
				}
				return "", nil
			})
			switch kind {
			case "mkdir":
				if err == nil || !strings.Contains(err.Error(), "dir_s_mkdir") || calls != 0 {
					t.Fatal(err, calls)
				}
			case "stat":
				if err == nil || !strings.Contains(err.Error(), "rb_file_s_stat") || calls != 0 {
					t.Fatal(err, calls)
				}
			case "cancel":
				if err != context.Canceled || calls != 1 {
					t.Fatal(err, calls)
				}
			default:
				if err != failure {
					t.Fatal(err, calls)
				}
			}
		})
	}
}

func TestComparisonLogInvocationLiteralOptionsAndNoMutation(t *testing.T) {
	inv := comparisonInvocation(t, "diff", "-g", "", "-j", "-17", "--cores", "arbitrary", "-t", "node", "-a", "name!=other", "-y", "z", "", "ignored")
	before := fmt.Sprintf("%#v", inv)
	f, err := os.Create(filepath.Join(t.TempDir(), "comparison.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e := &Engine{Color: "auto", Log: f, logOwner: nativeEngineLog(f)}
	if err := e.LogComparisonCLI(inv); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{command: ["diff"],
 global_options: {"c" => "auto", "color" => "auto", "help" => false},
 command_options:
  {"g" => "",
   "generation" => "",
   "j" => "-17",
   "max-jobs" => "-17",
   "cores" => "arbitrary",
   "t" => ["node"],
   "tag" => ["node"],
   "a" => ["name!=other"],
   "attr" => ["name!=other"],
   "y" => true,
   "yes" => true,
   "d" => false,
   "downgrade" => false},
 arguments: ["z", "", "ignored"]}
`
	if string(data) != want || fmt.Sprintf("%#v", inv) != before {
		t.Fatalf("got%s want%s", data, want)
	}
	// Default generation/max/cores remain nil; verbose and patch are booleans
	// in declaration order under the unchanged owning logger.
	for _, command := range []string{"diff", "changelog"} {
		f, err := os.Create(filepath.Join(t.TempDir(), command+".log"))
		if err != nil {
			t.Fatal(err)
		}
		e.Log, e.logOwner = f, nativeEngineLog(f)
		if err := e.LogComparisonCLI(comparisonInvocation(t, command)); err != nil {
			t.Fatal(err)
		}
		f.Close()
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"generation" => nil`) || !strings.Contains(string(b), `"max-jobs" => nil`) || !strings.Contains(string(b), `"cores" => nil`) || !strings.HasSuffix(string(b), "arguments: []}\n") {
			t.Fatal(string(b))
		}
	}
}

func TestComparisonNonstringURLFailsAtReachedMirrorConstruction(t *testing.T) {
	st := Status{Machine: Machine{Name: "h"}, Target: comparisonInfo(t, `{"a":{"rev":"new","url":{"b":"two","a":"one"}},"b":{"rev":"new","url":"good"}}`), Deployed: comparisonInfo(t, `{"a":"old","b":"old"}`)}
	logs := 0
	sink := &comparisonSink{}
	err := comparisonRender(context.Background(), sink, comparisonInvocation(t, "changelog"), nil, st, func(_ context.Context, url, from, to string, _ []string) (string, error) {
		logs++
		if url != "good" {
			t.Fatal("nonstring URL reached mirror", url)
		}
		return "log", nil
	})
	if err != nil || logs != 1 || sink.String() != "h @ a:\nno implicit conversion of Hash into String\n\nh @ b:\nlog\n\n" {
		t.Fatal(err, logs, sink.String())
	}
	sink = &comparisonSink{}
	err = comparisonRender(context.Background(), sink, comparisonInvocation(t, "diff"), nil, st, func(context.Context, string, string, string, []string) (string, error) {
		t.Fatal("diff mirror")
		return "", nil
	})
	if err != nil || sink.String() != `h @ a ({"b" => "two", "a" => "one"}): old -> new`+"\nh @ b (good): old -> new\n\n" {
		t.Fatal(err, sink.String())
	}
}
