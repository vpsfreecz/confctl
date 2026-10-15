package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/inputs"
	"github.com/vpsfreecz/confctl/internal/ui"
)

func TestComparisonSelectionSourceScopeAndNoPostRefusalEffects(t *testing.T) {
	z := connectionMachine("z", "shared", "unused.invalid", true)
	a := connectionMachine("a", "shared", "unused.invalid", true)
	a.Attributes = map[string]any{"tags": []any{"unique-tag"}, "label": "ok"}
	u := connectionMachine("u", "unmanaged", "unused.invalid", false)
	carried := connectionMachine("carried", "shared", "unused.invalid", true)
	carried.CarrierName = &z.Name
	targetless := connectionMachine("targetless", "targetless", "unused.invalid", true)
	targetless.Target.Host = nil
	all := []Machine{z, a, u, carried, targetless}
	for _, tc := range []struct{ args, want []string }{
		{nil, []string{"z", "a", "carried", "targetless"}},
		{[]string{"shared"}, []string{"z", "a", "carried"}},
		{[]string{"a"}, []string{"a"}},
		{[]string{"{z,a}"}, []string{"z", "a"}},
		{[]string{"-t", "unique-tag"}, []string{"a"}},
		{[]string{"-a", "label=ok"}, []string{"a"}},
		{[]string{""}, []string{}},
		{[]string{"u"}, []string{}},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			e, d, _ := comparisonFixture(t, all)
			seen := []string{}
			d.Query = func(st *Status, _ []Machine) { seen = append(seen, st.Machine.Name); up := 1.0; st.Uptime = &up }
			_, err := runComparison(t, e, comparisonInvocation(t, append([]string{"diff", "-y"}, tc.args...)...), "", d)
			if err != nil || !reflect.DeepEqual(seen, tc.want) {
				t.Fatal(seen, tc.want, err)
			}
		})
	}
	for _, tc := range []struct{ input, want, diagnostic string }{
		{"n\n", "\nContinue? [y/N]: \n", "Aborted"},
		{"", "\nContinue? [y/N]: ", "end of file reached"},
		{"\nN\n", "\nContinue? [y/N]: \nContinue? [y/N]: \n", "Aborted"},
	} {
		e, d, trace := comparisonFixture(t, []Machine{z})
		out, err := runComparison(t, e, comparisonInvocation(t, "diff"), tc.input, d)
		if err == nil || err.Error() != tc.diagnostic || out != "Compare inputs on the following machines:\nNAME\nz\n\nGeneration: current configuration\n"+tc.want || !reflect.DeepEqual(*trace, []string{"inventory", "list:z"}) {
			t.Fatal(out, err, *trace)
		}
	}
	// Inventory is constructed before invalid filter diagnostics. A reached
	// table/settings failure retains its already printed heading.
	e, d, trace := comparisonFixture(t, all)
	out, err := runComparison(t, e, comparisonInvocation(t, "diff", "-a", "invalid"), "", d)
	if err == nil || err.Error() != "Invalid filter ''" || out != "" || !reflect.DeepEqual(*trace, []string{"inventory"}) {
		t.Fatal(out, err, *trace)
	}
	e, d, _ = comparisonFixture(t, all)
	d.List = func([]Machine) (string, error) { return "", errors.New("settings failed") }
	out, err = runComparison(t, e, comparisonInvocation(t, "diff"), "", d)
	if err == nil || out != "Compare inputs on the following machines:\n" {
		t.Fatal(out, err)
	}
}

func TestComparisonStatusConstructionAndEmptyBoundaries(t *testing.T) {
	m := connectionMachine("z", "key-z", "unused.invalid", true)
	missing := "missing"
	m.CarrierName = &missing
	for _, args := range [][]string{{"diff", "-y"}, {"diff", "-y", "-g", "current"}} {
		e, d, trace := comparisonFixture(t, []Machine{m})
		out, err := runComparison(t, e, comparisonInvocation(t, args...), "", d)
		if err == nil || err.Error() != "Carrier missing not found in machine list" || out != "" || !reflect.DeepEqual(*trace, []string{"inventory"}) {
			t.Fatal(out, err, *trace)
		}
	}
	for _, selected := range []bool{false, true} {
		e, d, trace := comparisonFixture(t, nil)
		args := []string{"diff", "-y"}
		constructorFailure := errors.New("metadata constructor failed")
		d.Metadata = func() (*inputs.MachineResolver, error) {
			*trace = append(*trace, "metadata-new")
			return nil, constructorFailure
		}
		if selected {
			args = append(args, "-g", "none")
		}
		out, err := runComparison(t, e, comparisonInvocation(t, args...), "", d)
		if selected {
			if err == nil || err.Error() != "No generation found" || len(*trace) != 1 {
				t.Fatal(out, err, *trace)
			}
		} else {
			if err != constructorFailure || !reflect.DeepEqual(*trace, []string{"inventory", "metadata-new"}) {
				t.Fatal(out, err, *trace)
			}
		}
	}
	// Ordinary targetless host survives selection and target preparation; its
	// first reached SSHArgs rejects it without invoking injected Process.
	m.CarrierName = nil
	m.Target.Host = nil
	e, d, trace := comparisonFixture(t, []Machine{m})
	d.Query = func(st *Status, all []Machine) {
		*trace = append(*trace, "query:"+st.Machine.Name)
		e.queryWithInputs(st, all, queryOptions{Inputs: true}, func([]string, []byte) (Result, error) { t.Fatal("targetless transport invoked"); return Result{}, nil }, true)
	}
	_, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
	if err == nil || (*trace)[len(*trace)-1] != "query:z" {
		t.Fatal(err, *trace)
	}
}

func TestComparisonSelectedPartialMissingConfirmationAndCurrentError(t *testing.T) {
	ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true), connectionMachine("a", "key-a", "unused.invalid", true)}
	for _, yes := range []bool{false, true} {
		for _, answer := range []string{"y\n", "n\n", ""} {
			e, d, trace := comparisonFixture(t, ms)
			seedCoreGeneration(t, e.Root, "z", "chosen", coreGenerationRecord(e.Root+"/system", "2000-01-01T00:00:00Z", `{"os":"target123456"}`))
			d.Builds = func(host string) (*generation.BuildList, error) {
				*trace = append(*trace, "local:"+host)
				return generation.LoadBuildList(e.Root, host, generation.Options{})
			}
			args := []string{"diff", "-g", "chosen"}
			input := "y\n" + answer
			if yes {
				args = append(args, "-y")
				input = ""
			}
			out, err := runComparison(t, e, comparisonInvocation(t, args...), input, d)
			if yes || answer == "y\n" {
				if err != nil || !strings.HasSuffix(out, "z @ os: deploy12 -> target12\n\n") || (*trace)[len(*trace)-1] != "query:z" {
					t.Fatal(out, err, *trace)
				}
			} else {
				if err == nil || strings.Contains(out, "z @ os:") || strings.Contains(strings.Join(*trace, "|"), "query:") {
					t.Fatal(out, err, *trace)
				}
			}
			if yes && (strings.Contains(out, "Compare inputs") || strings.Contains(out, "These hosts will be ignored") || strings.Contains(out, "Continue?")) {
				t.Fatal(out)
			}
			if !yes && !strings.Contains(out, "Generation 'chosen' was not found on the following hosts:\n  a\n\nThese hosts will be ignored.\n\nContinue? [y/N]: ") {
				t.Fatal(out)
			}
			if !strings.Contains(out, "not found") {
				t.Fatal("missing row absent", out)
			}
			for _, event := range *trace {
				if strings.HasPrefix(event, "metadata") || strings.HasPrefix(event, ".#") {
					t.Fatal("selected current eval", *trace)
				}
			}
		}
	}
	e, d, _ := comparisonFixture(t, ms[:1])
	d.Builds = func(string) (*generation.BuildList, error) {
		return &generation.BuildList{Host: "z", CurrentError: errors.New("dangling current")}, nil
	}
	out, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y", "-g", "current"), "", d)
	if err == nil || err.Error() != "Cannot select current generation for z: dangling current" || out != "" {
		t.Fatal(out, err)
	}
}

type comparisonPipe struct {
	out      bytes.Buffer
	events   *[]string
	writes   int
	failure  func(int, []byte) (int, error)
	closeErr error
}

func (p *comparisonPipe) Write(b []byte) (int, error) {
	p.writes++
	*p.events = append(*p.events, "write:"+string(b))
	if p.failure != nil {
		n, err := p.failure(p.writes, b)
		if n > 0 {
			p.out.Write(b[:n])
		}
		return n, err
	}
	return p.out.Write(b)
}
func (p *comparisonPipe) Close() error { *p.events = append(*p.events, "close"); return p.closeErr }

func comparisonSystemPager(d *comparisonDeps, events *[]string, p *comparisonPipe) {
	env := map[string]string{"PAGER": " pager --flag ", "PATH": "/p"}
	d.Env = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	d.Pager = ui.PagerOps{
		Env:       d.Env,
		Exists:    func(path string) bool { return path == "/p/git" || path == "/p/pager" },
		GitConfig: func(context.Context) (string, error) { *events = append(*events, "git-config"); return "", nil },
		Probe: func(_ context.Context, command string) (bool, error) {
			*events = append(*events, "probe:"+command)
			return true, nil
		},
		Start: func(_ context.Context, command string) (ui.PagerPipe, error) {
			*events = append(*events, "start:"+command)
			return p, nil
		},
	}
}

func TestComparisonActualPagePreparationSerialQueriesAndNullDiscard(t *testing.T) {
	ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true), connectionMachine("a", "key-a", "unused.invalid", true)}
	e, d, events := comparisonFixture(t, ms)
	pipe := &comparisonPipe{events: events}
	comparisonSystemPager(&d, events, pipe)
	out, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
	want := []string{"inventory", "metadata-new", ".#confctl.machineKeys", ".#confctl.inputsInfo.key-z", ".#confctl.inputsInfo.key-a", "git-config", "git-config", "query:z", "probe:pager --flag", "start:pager --flag", "write:z @ os: deploy12 -> target12\n", "write:\n", "query:a", "write:a @ os: deploy12 -> target12\n", "write:\n", "close"}
	if err != nil || out != "" || !reflect.DeepEqual(*events, want) || pipe.out.String() != "z @ os: deploy12 -> target12\n\na @ os: deploy12 -> target12\n\n" {
		t.Fatal(out, err, *events, pipe.out.String())
	}
	// Redirected Null discards body but still queries/logs each changed role.
	e, d, events = comparisonFixture(t, ms)
	d.OutputTTY = func() bool { return false }
	d.Log = func(_ context.Context, url, from, to string, opts []string) (string, error) {
		*events = append(*events, "mirror:"+url)
		return "log", nil
	}
	d.Metadata = func() (*inputs.MachineResolver, error) {
		return &inputs.MachineResolver{Eval: func(s string) ([]byte, error) {
			if s == ".#confctl.machineKeys" {
				return []byte(`{"z":"key-z","a":"key-a"}`), nil
			}
			return []byte(`{"os":{"rev":"target","url":"repo"}}`), nil
		}}, nil
	}
	out, err = runComparison(t, e, comparisonInvocation(t, "changelog", "-y"), "", d)
	if err != nil || out != "" || !reflect.DeepEqual(*events, []string{"inventory", "query:z", "mirror:repo", "query:a", "mirror:repo"}) {
		t.Fatal(out, err, *events)
	}
}

func TestComparisonActualPageEPIPEStopsLaterWorkAndClosePriority(t *testing.T) {
	ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true), connectionMachine("a", "key-a", "unused.invalid", true)}
	for _, command := range []string{"diff", "changelog"} {
		for _, closeFailure := range []bool{false, true} {
			e, d, events := comparisonFixture(t, ms)
			pipe := &comparisonPipe{events: events}
			pipe.failure = func(n int, b []byte) (int, error) {
				if command == "diff" || n >= 2 {
					return 0, syscall.EPIPE
				}
				return len(b), nil
			}
			failure := errors.New("close failed")
			if closeFailure {
				pipe.closeErr = failure
			}
			comparisonSystemPager(&d, events, pipe)
			d.Log = func(context.Context, string, string, string, []string) (string, error) {
				*events = append(*events, "mirror")
				return "git log text", nil
			}
			d.Metadata = func() (*inputs.MachineResolver, error) {
				return &inputs.MachineResolver{Eval: func(s string) ([]byte, error) {
					if s == ".#confctl.machineKeys" {
						return []byte(`{"z":"key-z","a":"key-a"}`), nil
					}
					return []byte(`{"os":{"rev":"target","url":"repo"}}`), nil
				}}, nil
			}
			_, err := runComparison(t, e, comparisonInvocation(t, command, "-y"), "", d)
			if closeFailure && err != failure || !closeFailure && err != nil || (*events)[len(*events)-1] != "close" || strings.Contains(strings.Join(*events, "|"), "query:a") {
				t.Fatal(err, *events)
			}
			if command == "diff" && strings.Contains(strings.Join(*events, "|"), "mirror") {
				t.Fatal(*events)
			}
			if command == "changelog" && !strings.Contains(strings.Join(*events, "|"), "write:The pager process (`pager --flag`) was closed\n") {
				t.Fatal("missing reached diagnostic retry", *events)
			}
		}
	}
	// Cancellation is not swallowed as unknown or a successful pager quit.
	e, d, events := comparisonFixture(t, ms)
	ctx, cancel := context.WithCancel(context.Background())
	e.Context = ctx
	d.Metadata = func() (*inputs.MachineResolver, error) {
		return &inputs.MachineResolver{Eval: func(string) ([]byte, error) { cancel(); return nil, errors.New("ordinary error after cancel") }}, nil
	}
	_, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
	if err != context.Canceled || !reflect.DeepEqual(*events, []string{"inventory"}) {
		t.Fatal(err, *events)
	}
}

func TestComparisonPagerDecisionCachedLazyProbeAndBasicInputBuffer(t *testing.T) {
	for _, tc := range []struct {
		name, pager, gitPager, tty string
		outTTY, errTTY, want       bool
	}{
		{"normal", "pager", "", "", true, true, true}, {"override-zero", "pager", "", "0", true, true, false}, {"stdout", "pager", "", "", false, true, false}, {"stderr", "pager", "", "", true, false, false}, {"blank", " \n\t ", "", "", true, true, false}, {"only-git", "", "pager", "", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := comparisonDeps{Env: func(k string) (string, bool) {
				switch k {
				case "PAGER":
					return tc.pager, tc.pager != ""
				case "GIT_PAGER":
					return tc.gitPager, tc.gitPager != ""
				case "CONFCTL_TTY":
					return tc.tty, tc.tty != ""
				}
				return "", false
			}, OutputTTY: func() bool { return tc.outTTY }, ErrorTTY: func() bool { return tc.errTTY }}
			if got := comparisonPagerEnabled(d); got != tc.want {
				t.Fatal(got)
			}
		})
	}
	e, d, events := comparisonFixture(t, nil)
	pipe := &comparisonPipe{events: events}
	comparisonSystemPager(&d, events, pipe)
	_, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
	if err != nil || !reflect.DeepEqual(*events, []string{"inventory", "metadata-new", "git-config", "git-config"}) {
		t.Fatal(err, *events)
	}
	// A later configuration/environment change cannot undo the command's
	// construction-time enablement; discovery uses its own reached samples.
	e, d, events = comparisonFixture(t, nil)
	pipe = &comparisonPipe{events: events}
	comparisonSystemPager(&d, events, pipe)
	originalEnv := d.Env
	emptyPager := false
	d.Env = func(key string) (string, bool) {
		if key == "PAGER" && emptyPager {
			return "", true
		}
		return originalEnv(key)
	}
	d.Pager.Env = d.Env
	d.Metadata = func() (*inputs.MachineResolver, error) { emptyPager = true; return &inputs.MachineResolver{}, nil }
	_, err = runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
	if err != nil || !reflect.DeepEqual(*events, []string{"inventory", "git-config", "git-config"}) {
		t.Fatal(err, *events)
	}
	// Basic consumes a queued key from the SAME confirmation reader. No second
	// reader, native getch, real terminal or provider is involved.
	m := connectionMachine("z", "key-z", "unused.invalid", true)
	e, d, events = comparisonFixture(t, []Machine{m})
	reader := bufio.NewReader(strings.NewReader("\ny\nQremaining"))
	d.Env = func(k string) (string, bool) {
		if k == "PAGER" {
			return "absent", true
		}
		if k == "PATH" {
			return "/empty", true
		}
		return "", false
	}
	d.Pager = ui.PagerOps{Env: d.Env, Exists: func(string) bool { return false }, Basic: func(ctx context.Context, out io.Writer) (ui.PagerSinkCloser, error) {
		*events = append(*events, "basic-new")
		return ui.NewBasicPager(ctx, out, ui.BasicIO{Size: func(context.Context) (ui.BasicSize, error) {
			*events = append(*events, "size")
			return ui.BasicSize{Rows: big.NewInt(3), Columns: big.NewInt(100)}, nil
		}, Wrap: ui.WrapBasic, Key: func(context.Context) (string, error) {
			*events = append(*events, "key")
			r, _, err := reader.ReadRune()
			return string(r), err
		}})
	}}
	var out bytes.Buffer
	err = e.compare(comparisonInvocation(t, "diff"), reader, &out, d)
	if err != nil || !strings.Contains(out.String(), "z @ os: deploy12 -> target12\n") || !strings.Contains(out.String(), "\n--- Page -1- Press enter/return to continue (or q to quit) ---\n") || (*events)[len(*events)-1] != "key" {
		t.Fatal(out.String(), err, *events)
	}
	left, err := io.ReadAll(reader)
	if err != nil || string(left) != "remaining" {
		t.Fatal(string(left), err)
	}
}

func TestComparisonActualPageShortWritesErrorsAndCancellationClose(t *testing.T) {
	ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true), connectionMachine("a", "key-a", "unused.invalid", true)}
	for _, kind := range []string{"short", "write", "start", "probe", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			e, d, events := comparisonFixture(t, ms)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.Context = ctx
			pipe := &comparisonPipe{events: events}
			comparisonSystemPager(&d, events, pipe)
			failure := errors.New("scripted " + kind)
			switch kind {
			case "short":
				pipe.failure = func(_ int, b []byte) (int, error) { return len(b) - 1, nil }
			case "write":
				pipe.failure = func(int, []byte) (int, error) { return 0, failure }
			case "start":
				d.Pager.Start = func(context.Context, string) (ui.PagerPipe, error) {
					*events = append(*events, "start-failed")
					return nil, failure
				}
			case "probe":
				d.Pager.Probe = func(context.Context, string) (bool, error) {
					*events = append(*events, "probe-failed")
					return false, failure
				}
			case "cancel":
				pipe.failure = func(_ int, b []byte) (int, error) { cancel(); return 0, syscall.EPIPE }
			}
			_, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
			want := failure
			if kind == "short" {
				want = io.ErrShortWrite
			}
			if kind == "cancel" {
				want = context.Canceled
			}
			if err != want || strings.Contains(strings.Join(*events, "|"), "query:a") {
				t.Fatal(err, *events)
			}
			closed := (*events)[len(*events)-1] == "close"
			if closed != (kind != "start" && kind != "probe") {
				t.Fatal("wrong acquired-pipe close", *events)
			}
		})
	}
}

func TestComparisonActualBasicZeroNegativeHeightsAndEmptyPuts(t *testing.T) {
	for _, rows := range []int64{2, 0, -5} {
		m := connectionMachine("z", "key-z", "unused.invalid", true)
		e, d, _ := comparisonFixture(t, []Machine{m})
		d.Env = func(key string) (string, bool) {
			if key == "PAGER" {
				return "absent", true
			}
			return "", false
		}
		d.Pager = ui.PagerOps{Env: d.Env, Exists: func(string) bool { return false }, Basic: func(ctx context.Context, out io.Writer) (ui.PagerSinkCloser, error) {
			return ui.NewBasicPager(ctx, out, ui.BasicIO{Size: func(context.Context) (ui.BasicSize, error) {
				return ui.BasicSize{Rows: big.NewInt(rows), Columns: big.NewInt(100)}, nil
			}, Wrap: ui.WrapBasic, Key: func(context.Context) (string, error) { return "q", nil }})
		}}
		out, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
		want := "z @ os: deploy12 -> target12\n" // Basic Puts("") has no lines.
		if rows == 2 {
			want = "\n\n--- Page -1- Press enter/return to continue (or q to quit) ---\n"
		}
		if err != nil || out != want {
			t.Fatal(rows, out, err, want)
		}
	}
}

func TestComparisonCLIStillUnavailableAndBorrowedInvocation(t *testing.T) {
	for _, command := range []string{"diff", "changelog"} {
		inv := comparisonInvocation(t, command, "-y", "-g", "", "z", "", "ignored", "--help")
		before := fmt.Sprintf("%#v", inv)
		if err := cli.BuiltinRegistry().CheckAvailability(inv); err == nil {
			t.Fatal("private comparison available")
		}
		e, d, _ := comparisonFixture(t, []Machine{connectionMachine("z", "key-z", "unused.invalid", true)})
		d.Builds = func(string) (*generation.BuildList, error) { return &generation.BuildList{Host: "z"}, nil }
		out, err := runComparison(t, e, inv, "", d)
		if err == nil || err.Error() != "No generation found" || out != "" || !inv.Options["generation"].Present || inv.Args[1] != "" || fmt.Sprintf("%#v", inv) != before {
			t.Fatal(out, err, inv)
		}
	}
}

// Injected writer errors are source-derived confirmation boundary cases, not
// observations of Ruby or a terminal. Earlier heading/table writes succeed.
type confirmationFailWriter struct {
	bytes.Buffer
	err                         error
	prompt, failPrompt, failAck int
}

func (w *confirmationFailWriter) Write(b []byte) (int, error) {
	if string(b) == "\nContinue? [y/N]: " {
		w.prompt++
		if w.prompt == w.failPrompt {
			return 0, w.err
		}
	}
	if string(b) == "\n" && w.prompt == w.failAck {
		return 0, w.err
	}
	return w.Buffer.Write(b)
}

func TestComparisonConfirmationWriteFailures(t *testing.T) {
	for _, partial := range []bool{false, true} {
		for _, boundary := range []string{"prompt", "yes-ack", "no-ack"} {
			t.Run(fmt.Sprintf("partial=%t/%s", partial, boundary), func(t *testing.T) {
				ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true)}
				args := []string{"diff"}
				nth, prefix := 1, ""
				wantTrace := []string{"inventory", "list:z"}
				if partial {
					ms = append(ms, connectionMachine("a", "key-a", "unused.invalid", true))
					args = append(args, "-g", "0")
					nth, prefix = 2, "y\n"
					wantTrace = []string{"inventory", "list:z,a", "local:z", "local:a"}
				}
				e, d, trace := comparisonFixture(t, ms)
				d.Builds = func(host string) (*generation.BuildList, error) {
					*trace = append(*trace, "local:"+host)
					list := &generation.BuildList{Host: host}
					if host == "z" {
						list.Builds = []*generation.Build{{Name: "chosen", InputsInfo: comparisonValue(t, `{"os":"target123456"}`).(generation.Object)}}
					}
					return list, nil
				}
				// Enabled Page would discover and then query; none may occur after error.
				d.Env = func(k string) (string, bool) {
					if k == "PAGER" {
						return "pager", true
					}
					return "", false
				}
				d.Pager = ui.PagerOps{
					Env:    d.Env,
					Exists: func(string) bool { *trace = append(*trace, "pager-discovery"); return false },
					Basic: func(context.Context, io.Writer) (ui.PagerSinkCloser, error) {
						*trace = append(*trace, "pager-acquire")
						return nil, errors.New("unexpected pager acquisition")
					},
				}
				answer := "y\n"
				if boundary == "no-ack" {
					answer = "n\n"
				}
				input := bufio.NewReader(strings.NewReader(prefix + answer + "TAIL"))
				failure := errors.New("confirmation writer failed")
				out := &confirmationFailWriter{err: failure}
				if boundary == "prompt" {
					out.failPrompt = nth
				} else {
					out.failAck = nth
				}
				err := e.compare(comparisonInvocation(t, args...), input, out, d)
				if !errors.Is(err, failure) || !reflect.DeepEqual(*trace, wantTrace) {
					t.Errorf("error=%v trace=%v want=%v", err, *trace, wantTrace)
				}
				remaining, readErr := io.ReadAll(input)
				wantRemaining := "TAIL"
				if boundary == "prompt" {
					wantRemaining = answer + wantRemaining
				}
				if readErr != nil || string(remaining) != wantRemaining {
					t.Errorf("consumed logical input: remaining=%q want=%q err=%v", remaining, wantRemaining, readErr)
				}
				if out.prompt != nth || !strings.HasPrefix(out.String(), "Compare inputs on the following machines:\n") || partial && !strings.Contains(out.String(), "These hosts will be ignored.\n") {
					t.Fatal("did not reach exact selected write boundary", out.prompt, out.String())
				}
			})
		}
	}
}
