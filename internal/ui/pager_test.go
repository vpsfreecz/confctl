package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

type fakePagerPipe struct {
	bytes.Buffer
	writeErr error
	closeErr error
	short    bool
	closes   int
}

func (p *fakePagerPipe) Write(data []byte) (int, error) {
	if p.short || p.writeErr != nil {
		n := len(data) / 2
		p.Buffer.Write(data[:n])
		return n, p.writeErr
	}
	return p.Buffer.Write(data)
}
func (p *fakePagerPipe) Close() error { p.closes++; return p.closeErr }

func testPagerOps(environment map[string]string, paths map[string]bool, events *[]string) PagerOps {
	return PagerOps{
		Env: func(name string) (string, bool) {
			*events = append(*events, "env:"+name)
			value, present := environment[name]
			return value, present
		},
		Exists: func(path string) bool {
			*events = append(*events, "exists:"+path)
			return paths[path]
		},
		GitConfig: func(context.Context) (string, error) {
			*events = append(*events, "git-config")
			return "configured -x\n", nil
		},
	}
}

func TestDisabledPagerDiscardAndPutsHaveNoDiscovery(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var out bytes.Buffer
		calls, body := 0, 0
		err := Page(context.Background(), PagerOptions{Output: &out, OutputTTY: func() bool { calls++; return tty }}, PagerOps{}, func(p PagerSink) error {
			for _, text := range []string{"", "a", "b\n", "c\r"} {
				body++
				if err := p.Puts(text); err != nil {
					return err
				}
			}
			return nil
		})
		want := ""
		if tty {
			want = "\na\nb\nc\r\n"
		}
		if err != nil || out.String() != want || calls != 4 || body != 4 {
			t.Fatalf("disabled tty=%v: %q calls=%d body=%d err=%v", tty, out.String(), calls, body, err)
		}
	}
}

func TestSystemDiscoveryEagerTwiceAndNoBodyWriteIsLazy(t *testing.T) {
	var events []string
	ops := testPagerOps(map[string]string{"PATH": "/tools", "GIT_PAGER": " \tchosen --flag\r\n", "PAGER": "chosen --flag"}, map[string]bool{"/tools/git": true, "/tools/chosen": true}, &events)
	ops.Probe = func(context.Context, string) (bool, error) { t.Fatal("eager probe"); return false, nil }
	ops.Start = func(context.Context, string) (PagerPipe, error) { t.Fatal("eager start"); return nil, nil }
	err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(PagerSink) error {
		events = append(events, "body")
		return nil
	})
	want := []string{
		"env:GIT_PAGER", "env:PAGER", "env:PATHEXT", "env:PATH", "exists:/tools/git", "git-config",
		"env:PATHEXT", "env:PATH", "exists:/tools/chosen",
		"env:GIT_PAGER", "env:PAGER", "env:PATHEXT", "env:PATH", "exists:/tools/git", "git-config",
		"env:PATHEXT", "env:PATH", "exists:/tools/chosen", "body",
	}
	if err != nil || !reflect.DeepEqual(events, want) {
		t.Fatalf("discovery order\ngot %q\nwant %q\nerr %v", events, want, err)
	}
}

func TestDiscoveryUsesSecondWinnerOrFailsWithoutBasicFallback(t *testing.T) {
	for _, second := range []string{"second --arg", ""} {
		var events []string
		env := map[string]string{"PATH": "/p", "GIT_PAGER": "first"}
		paths := map[string]bool{"/p/git": true, "/p/first": true, "/p/second": true}
		ops := testPagerOps(env, paths, &events)
		queries := 0
		ops.GitConfig = func(context.Context) (string, error) {
			queries++
			if queries == 1 {
				env["GIT_PAGER"] = second
				delete(paths, "/p/first")
			}
			return "", nil
		}
		ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) {
			t.Fatal("second discovery must not fallback")
			return nil, nil
		}
		ops.Probe = func(_ context.Context, command string) (bool, error) {
			if command != second {
				t.Fatalf("second winner %q", command)
			}
			return true, nil
		}
		pipe := &fakePagerPipe{}
		ops.Start = func(_ context.Context, command string) (PagerPipe, error) {
			if command != second {
				t.Fatalf("second start %q", command)
			}
			return pipe, nil
		}
		// The eager array reads GIT_PAGER before its first GitConfig call;
		// existence mutation above must not remove the first winner prematurely.
		checks := 0
		ops.Exists = func(path string) bool {
			if path == "/p/first" && checks == 0 {
				checks++
				return true
			}
			return paths[path]
		}
		err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(p PagerSink) error { return p.Puts("text") })
		if second == "" {
			if err == nil || !strings.Contains(err.Error(), "lack of appropriate pager executable") {
				t.Fatal(err)
			}
		} else if err != nil || pipe.String() != "text\n" || pipe.closes != 1 {
			t.Fatalf("%q %d %v", pipe.String(), pipe.closes, err)
		}
		if queries != 2 {
			t.Fatalf("queries %d", queries)
		}
	}
}

func TestBasicSelectionDiscoversOnceAndClosesAfterBodyError(t *testing.T) {
	var events []string
	ops := testPagerOps(map[string]string{"PATH": "/none"}, map[string]bool{"/none/git": true}, &events)
	bodyErr := errors.New("body")
	resource := &fakeSinkCloser{}
	ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) {
		events = append(events, "basic")
		return resource, nil
	}
	err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(PagerSink) error { return bodyErr })
	if err != bodyErr || resource.closes != 1 || strings.Count(strings.Join(events, "|"), "git-config") != 1 || events[len(events)-1] != "basic" {
		t.Fatalf("events %q close %d err %v", events, resource.closes, err)
	}
}

type fakeSinkCloser struct {
	closes   int
	closeErr error
}

func (*fakeSinkCloser) Puts(string) error { return nil }
func (p *fakeSinkCloser) Close() error    { p.closes++; return p.closeErr }

func TestDiscoveryCandidateOrderStripDedupeAndFirstWord(t *testing.T) {
	var events []string
	ops := testPagerOps(map[string]string{"PATH": "/p", "GIT_PAGER": " \x00missing --arg\x00 ", "PAGER": "missing --arg"}, map[string]bool{"/p/git": true, "/p/cat": true}, &events)
	command, err := findPager(context.Background(), ops)
	var exists []string
	for _, event := range events {
		if strings.HasPrefix(event, "exists:") {
			exists = append(exists, strings.TrimPrefix(event, "exists:"))
		}
	}
	want := []string{"/p/git", "/p/missing", "/p/configured", "/p/less", "/p/more", "/p/most", "/p/pg", "/p/cat"}
	if err != nil || command != "cat" || !reflect.DeepEqual(exists, want) {
		t.Fatalf("%q %q %v", command, exists, err)
	}
	if firstWord(`"/path with space" --arg`) != `"/path` {
		t.Fatal("discovery must not parse shell quotes")
	}
	if rubyStrip("\u00a0x\u00a0") != "\u00a0x\u00a0" {
		t.Fatal("Unicode whitespace must not replace Ruby strip")
	}
}

func TestAbsentEnvironmentValuesAreNotCandidatesOrPaths(t *testing.T) {
	var queried []string
	ops := PagerOps{
		Env:       func(string) (string, bool) { return "unused absent value", false },
		Exists:    func(path string) bool { queried = append(queried, path); return true },
		GitConfig: func(context.Context) (string, error) { t.Fatal("GitConfig from absent PATH"); return "", nil },
	}
	if command, err := findPager(context.Background(), ops); command != "" || err != nil || len(queried) != 0 {
		t.Fatalf("%q paths%q err%v", command, queried, err)
	}
}

func TestCommandExistenceRubyPathAndSuffixRules(t *testing.T) {
	for _, test := range []struct {
		path, ext, command string
		found              map[string]bool
		want               []string
		exists             bool
	}{
		{"", "", "cat", map[string]bool{"cat": true}, nil, false},
		{":", "", "cat", map[string]bool{"/cat": true}, nil, false},
		{":/p:", ".x:.y:", "cat", map[string]bool{"/p/cat.y": true}, []string{"/cat", "/cat.x", "/cat.y", "/p/cat", "/p/cat.x", "/p/cat.y"}, true},
		{"/p::/q:", "", "cat", map[string]bool{"/cat": true}, []string{"/p/cat", "/cat"}, true},
		{":/p::/q:", "", "cat", map[string]bool{"cat": true}, []string{"/cat", "/p/cat", "/cat", "/q/cat"}, false},
		{"/p//:", "", "cat", map[string]bool{"/p//cat": true}, []string{"/p//cat"}, true},
		{"/p//:/q///:", ".x:", "cat", map[string]bool{"/q///cat.x": true}, []string{"/p//cat", "/p//cat.x", "/q///cat", "/q///cat.x"}, true},
		{"./p//:../q///", "", "cat", map[string]bool{"../q///cat": true}, []string{"./p//cat", "../q///cat"}, true},
		{".", "", "cat", map[string]bool{"./cat": true}, []string{"./cat"}, true},
		{"", ".x:", "/absolute", map[string]bool{"/absolute.x": true}, []string{"/absolute", "/absolute.x"}, true},
		{"/p", "", "directory", map[string]bool{"/p/directory": true}, []string{"/p/directory"}, true},
	} {
		var paths []string
		ops := PagerOps{Env: func(name string) (string, bool) {
			if name == "PATH" {
				return test.path, true
			}
			return test.ext, true
		}, Exists: func(path string) bool { paths = append(paths, path); return test.found[path] }}
		got, err := commandExists(context.Background(), ops, test.command)
		if got != test.exists || err != nil || !reflect.DeepEqual(paths, test.want) {
			t.Fatalf("%+v got %v paths%q err%v", test, got, paths, err)
		}
	}
}

func TestEmptyPathComponentsDoNotAdmitCwdGitOrPager(t *testing.T) {
	for _, test := range []struct {
		name, path string
		want       []string
	}{
		{"leading", ":/empty", []string{
			"/git", "/empty/git", "/cwd-pager", "/empty/cwd-pager",
			"/less", "/empty/less", "/more", "/empty/more", "/most", "/empty/most",
			"/pg", "/empty/pg", "/cat", "/empty/cat", "/pager", "/empty/pager",
		}},
		{"interior", "/empty::/other", []string{
			"/empty/git", "/git", "/other/git", "/empty/cwd-pager", "/cwd-pager", "/other/cwd-pager",
			"/empty/less", "/less", "/other/less", "/empty/more", "/more", "/other/more",
			"/empty/most", "/most", "/other/most", "/empty/pg", "/pg", "/other/pg",
			"/empty/cat", "/cat", "/other/cat", "/empty/pager", "/pager", "/other/pager",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			ops := testPagerOps(map[string]string{"PATH": test.path, "PAGER": "cwd-pager --arg"}, map[string]bool{
				"git": true, "cwd-pager": true, "cat": true,
			}, &events)
			ops.GitConfig = func(context.Context) (string, error) {
				t.Fatal("cwd-only git must not trigger GitConfig")
				return "", nil
			}
			ops.Probe = func(context.Context, string) (bool, error) {
				t.Fatal("cwd-only pager must not be probed")
				return false, nil
			}
			ops.Start = func(context.Context, string) (PagerPipe, error) {
				t.Fatal("cwd-only pager must not be started")
				return nil, nil
			}
			resource := &fakeSinkCloser{}
			basicCalls, bodyCalls := 0, 0
			ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) {
				basicCalls++
				return resource, nil
			}
			err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(p PagerSink) error {
				bodyCalls++
				if p != resource {
					t.Fatal("body must use the acquired Basic pager")
				}
				return p.Puts("body")
			})
			var paths []string
			for _, event := range events {
				if strings.HasPrefix(event, "exists:") {
					paths = append(paths, strings.TrimPrefix(event, "exists:"))
				}
			}
			if err != nil || basicCalls != 1 || bodyCalls != 1 || resource.closes != 1 || !reflect.DeepEqual(paths, test.want) {
				t.Fatalf("paths%q basic%d body%d closes%d err%v", paths, basicCalls, bodyCalls, resource.closes, err)
			}
		})
	}
}

func TestProbePriorityFallbackAndLiveDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name  string
		clean bool
		err   error
		live  string
	}{
		{"exit0-empty-stderr", true, nil, "less -r"},
		{"exit0-whitespace-stderr", false, nil, "less"},
		{"nonzero-or-signal", false, nil, "less"},
		{"ENOENT", false, syscall.ENOENT, "less"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			pipe := &fakePagerPipe{writeErr: syscall.EPIPE}
			p := &systemPager{ctx: context.Background(), command: "less -r", ops: PagerOps{
				Probe: func(_ context.Context, command string) (bool, error) {
					events = append(events, "probe:"+command)
					return test.clean, test.err
				},
				Start: func(_ context.Context, command string) (PagerPipe, error) {
					events = append(events, "start:"+command)
					return pipe, nil
				},
			}}
			err := p.Puts("original bytes\xff")
			if !errors.Is(err, ErrPagerClosed) || err.Error() != "The pager process (`"+test.live+"`) was closed" || !reflect.DeepEqual(events, []string{"probe:less -r", "start:" + test.live}) {
				t.Fatalf("%q %v", events, err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if pipe.closes != 1 {
				t.Fatal(pipe.closes)
			}
		})
	}
	probeErr := errors.New("probe denied")
	p := &systemPager{ctx: context.Background(), command: "less -r", ops: PagerOps{Probe: func(context.Context, string) (bool, error) { return false, probeErr }, Start: func(context.Context, string) (PagerPipe, error) { t.Fatal("start after probe error"); return nil, nil }}}
	if err := p.Puts("x"); err != probeErr {
		t.Fatal(err)
	}
}

func TestSystemWriteShortOtherStartAndRepeatedClose(t *testing.T) {
	for _, test := range []struct {
		short    bool
		writeErr error
		want     error
	}{{true, nil, io.ErrShortWrite}, {false, errors.New("write"), nil}} {
		pipe := &fakePagerPipe{short: test.short, writeErr: test.writeErr}
		p := &systemPager{ctx: context.Background(), pipe: pipe}
		err := p.Puts("abcdef")
		want := test.want
		if want == nil {
			want = test.writeErr
		}
		if err != want || pipe.String() != "abc" {
			t.Fatalf("%q %v", pipe.String(), err)
		}
	}
	startErr := errors.New("start")
	p := &systemPager{ctx: context.Background(), command: "cat", ops: PagerOps{Probe: func(context.Context, string) (bool, error) { return true, nil }, Start: func(context.Context, string) (PagerPipe, error) { return nil, startErr }}}
	if err := p.Puts("x"); err != startErr {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	pipe := &fakePagerPipe{}
	p = &systemPager{ctx: context.Background(), pipe: pipe}
	for _, s := range []string{"", "x\n", "y\r"} {
		if err := p.Puts(s); err != nil {
			t.Fatal(err)
		}
	}
	if pipe.String() != "\nx\ny\r\n" {
		t.Fatalf("%q", pipe.String())
	}
}

func TestPageClosePrecedenceClosedAndCancellation(t *testing.T) {
	bodyErr, closeErr := errors.New("body"), errors.New("close")
	for _, body := range []error{nil, bodyErr, &pagerClosed{message: "closed"}} {
		for _, closing := range []error{nil, closeErr} {
			var events []string
			ops := testPagerOps(map[string]string{}, nil, &events)
			resource := &fakeSinkCloser{closeErr: closing}
			ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) { return resource, nil }
			err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(PagerSink) error { return body })
			want := body
			if errors.Is(body, ErrPagerClosed) {
				want = nil
			}
			if closing != nil {
				want = closing
			}
			if err != want || resource.closes != 1 {
				t.Fatalf("body%v close%v got%v count%d", body, closing, err, resource.closes)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var events []string
	ops := testPagerOps(nil, nil, &events)
	resource := &fakeSinkCloser{closeErr: closeErr}
	ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) { return resource, nil }
	err := Page(ctx, PagerOptions{Enabled: true}, ops, func(PagerSink) error { cancel(); return &pagerClosed{message: "closed"} })
	if err != context.Canceled || resource.closes != 1 {
		t.Fatal(err, resource.closes)
	}
}

func TestGitConfigAndBasicFailureStopBeforeBody(t *testing.T) {
	for _, phase := range []string{"git-config", "basic"} {
		var events []string
		failure := errors.New(phase)
		ops := testPagerOps(map[string]string{"PATH": "/p"}, map[string]bool{"/p/git": phase == "git-config"}, &events)
		ops.GitConfig = func(context.Context) (string, error) { return "", failure }
		ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) { return nil, failure }
		if err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(PagerSink) error { t.Fatal("body after acquisition error"); return nil }); err != failure {
			t.Fatal(err)
		}
	}
}

func TestDiscoveryCancellationStopsBeforeReachedChildCallbacks(t *testing.T) {
	for _, phase := range []string{"existence", "git-config"} {
		ctx, cancel := context.WithCancel(context.Background())
		var events []string
		ops := testPagerOps(map[string]string{"PATH": "/p"}, map[string]bool{"/p/git": true}, &events)
		if phase == "existence" {
			ops.Exists = func(string) bool { cancel(); return true }
			ops.GitConfig = func(context.Context) (string, error) { t.Fatal("GitConfig after cancellation"); return "", nil }
		} else {
			ops.GitConfig = func(context.Context) (string, error) { cancel(); return "cat", nil }
		}
		ops.Basic = func(context.Context, io.Writer) (PagerSinkCloser, error) {
			t.Fatal("Basic after cancellation")
			return nil, nil
		}
		if err := Page(ctx, PagerOptions{Enabled: true}, ops, func(PagerSink) error { t.Fatal("body after cancellation"); return nil }); err != context.Canceled {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &systemPager{ctx: ctx, command: "cat", ops: PagerOps{
		Probe: func(context.Context, string) (bool, error) { cancel(); return true, nil },
		Start: func(context.Context, string) (PagerPipe, error) {
			t.Fatal("Start after canceled probe")
			return nil, nil
		},
	}}
	if err := p.Puts("text"); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestPageClosedWriteStopsBodyAndClosesExactlyOnce(t *testing.T) {
	var events []string
	ops := testPagerOps(map[string]string{"PATH": "/p", "PAGER": "cat"}, map[string]bool{"/p/cat": true}, &events)
	pipe := &fakePagerPipe{writeErr: syscall.EPIPE}
	ops.Probe = func(context.Context, string) (bool, error) { return true, nil }
	ops.Start = func(context.Context, string) (PagerPipe, error) { return pipe, nil }
	completed := 0
	err := Page(context.Background(), PagerOptions{Enabled: true}, ops, func(p PagerSink) error {
		for _, item := range []string{"first", "later"} {
			if err := p.Puts(item); err != nil {
				return err
			}
			completed++
		}
		return nil
	})
	if err != nil || completed != 0 || pipe.closes != 1 || pipe.String() != "fir" {
		t.Fatalf("out%q completed%d closes%d err%v", pipe.String(), completed, pipe.closes, err)
	}
}
