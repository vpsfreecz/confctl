package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
	"github.com/vpsfreecz/confctl/internal/inputs"
)

func comparisonInvocation(t *testing.T, args ...string) cli.Invocation {
	t.Helper()
	inv, err := cli.BuiltinRegistry().Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func comparisonValue(t *testing.T, source string) any {
	t.Helper()
	v, err := generation.DecodeJSONValue([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func comparisonInfo(t *testing.T, source string) map[string]map[string]any {
	t.Helper()
	v, err := comparisonInputs(comparisonValue(t, source))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// These fixtures are SOURCE-DERIVED expectations, not original Ruby/runtime
// observations. Every operation below is injected; no child is executed.
func comparisonFixture(t *testing.T, all []Machine) (*Engine, comparisonDeps, *[]string) {
	t.Helper()
	e := &Engine{Context: context.Background(), Root: t.TempDir()}
	trace := []string{}
	d := comparisonDeps{
		Machines: func() ([]Machine, error) { trace = append(trace, "inventory"); return all, nil },
		List: func(ms []Machine) (string, error) {
			names := []string{}
			for _, m := range ms {
				names = append(names, m.Name)
			}
			trace = append(trace, "list:"+strings.Join(names, ","))
			return "NAME\n" + strings.Join(names, "\n") + "\n", nil
		},
		Builds: func(host string) (*generation.BuildList, error) {
			t.Fatalf("unexpected build read %s", host)
			return nil, nil
		},
		Metadata: func() (*inputs.MachineResolver, error) {
			trace = append(trace, "metadata-new")
			return &inputs.MachineResolver{Eval: func(installable string) ([]byte, error) {
				trace = append(trace, installable)
				if installable == ".#confctl.machineKeys" {
					return []byte(`{"z":"key-z","a":"key-a"}`), nil
				}
				return []byte(`{"os":"target123456"}`), nil
			}}, nil
		},
		Query: func(st *Status, _ []Machine) {
			trace = append(trace, "query:"+st.Machine.Name)
			up := 1.0
			st.Uptime = &up
			st.Deployed = comparisonInfo(t, `{"os":"deploy123456"}`)
		},
		Log: func(context.Context, string, string, string, []string) (string, error) {
			t.Fatal("diff reached comparison mirror")
			return "", nil
		},
		Env: func(string) (string, bool) { return "", false }, OutputTTY: func() bool { return true }, ErrorTTY: func() bool { return true },
	}
	return e, d, &trace
}

func runComparison(t *testing.T, e *Engine, inv cli.Invocation, input string, d comparisonDeps) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := e.compare(inv, bufio.NewReader(strings.NewReader(input)), &out, d)
	return out.String(), err
}

func TestComparisonInputsOrderedAliasesAndReachedValues(t *testing.T) {
	raw := `{"z":{"short_rev":"first","shortRev":"last","rev":"abcdefghijk","lastModified":9007199254740993,"extra":"drop"},"a":{"shortRev":"first","short_rev":"last","rev":"unused"},"nil":{"shortRev":null,"rev":"é雪abcdefgh"},"empty":{"shortRev":"","rev":"unused"},"false":{"shortRev":false,"rev":"123456789"},"false-rev":{"rev":false},"string":"0123456789","other":23}`
	value := comparisonValue(t, raw)
	before := fmt.Sprintf("%#v", value)
	got, err := comparisonInputs(value)
	if err != nil || got["z"]["shortRev"] != "last" || got["a"]["shortRev"] != "last" || got["nil"]["shortRev"] != "é雪abcdef" || got["empty"]["shortRev"] != "" || got["false"]["shortRev"] != false || len(got["other"]) != 0 {
		t.Fatal(got, err)
	}
	if got["z"]["lastModified"] != json.Number("9007199254740993") || got["z"]["extra"] != nil {
		t.Fatal(got)
	}
	if short, err := comparisonShort(got["false"]); err != nil || short != "12345678" {
		t.Fatal(short, err)
	}
	if short, err := comparisonShort(got["false-rev"]); err != nil || short != false {
		t.Fatal(short, err)
	}
	if fmt.Sprintf("%#v", value) != before {
		t.Fatal("normalization mutated borrowed ordered data")
	}
	for _, raw := range []string{`null`, `[]`, `true`, `"text"`} {
		if info, err := comparisonInputs(comparisonValue(t, raw)); info != nil || err != nil {
			t.Fatal(raw, info, err)
		}
	}
	// Array#[] is only reached when a truthy rev needs slicing. Take a copied
	// prefix; an explicit shortRev avoids touching the revision altogether.
	array := []any{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	sliced, err := comparisonSlice(array)
	if err != nil || !reflect.DeepEqual(sliced, array[:8]) {
		t.Fatal(sliced, err)
	}
	sliced.([]any)[0] = "changed"
	if array[0] != "a" {
		t.Fatal("slice aliases metadata")
	}
}

func TestComparisonRevisionSlicesEqualityAndReachedErrors(t *testing.T) {
	// SOURCE-DERIVED finite byte admission, not a Ruby encoding/provider run.
	raw := comparisonValue(t, `"\udc00abcdef"`).(string)
	got, err := comparisonSlice(raw)
	if err != nil || got != raw[:8] || strings.Contains(got.(string), "\ufffd") {
		t.Fatal([]byte(got.(string)), err)
	}
	info, err := comparisonInputs(generation.Object{{Name: "os", Value: raw}})
	if err != nil || info["os"]["shortRev"] != raw[:8] {
		t.Fatal(info, err)
	}
	for _, tc := range []struct{ number, want string }{{"0", "0"}, {"255", "255"}, {"256", "0"}, {"257", "1"}, {"-1", "255"}, {"-257", "255"}, {"999999999999999999999999999999999999999999999999999999999999999999", "255"}} {
		got, err := comparisonSlice(json.Number(tc.number))
		if err != nil || got != json.Number(tc.want) {
			t.Fatal(tc, got, err)
		}
	}
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{`1`, `1.0`, true}, {`1e0`, `1.000`, true}, {`9007199254740993`, `9007199254740992.0`, false},
		{`[1,{"a":"x","b":[false,null]}]`, `[1.0,{"b":[false,null],"a":"x"}]`, true},
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true}, {`{"a":1}`, `{"a":2}`, false}, {`false`, `null`, false}, {`[]`, `{}`, false},
	} {
		if got := comparisonRevisionEqual(comparisonValue(t, tc.a), comparisonValue(t, tc.b)); got != tc.equal {
			t.Fatal(tc, got)
		}
	}
	for _, raw := range []string{`{"os":{"rev":1.2}}`, `{"os":{"rev":true}}`, `{"os":{"rev":{"raw":"revision"}}}`} {
		value := comparisonValue(t, raw)
		if _, err := comparisonInputs(value); err == nil {
			t.Fatal("reached source slice accepted", raw)
		}
	}
	// Explicit shortRev suppresses normalization's slice. Ruby || at display
	// still reaches it for false, while an explicit empty string stays empty.
	for _, shortValue := range []string{`""`, `false`} {
		info, err := comparisonInputs(comparisonValue(t, `{"os":{"rev":1.2,"shortRev":`+shortValue+`}}`))
		if err != nil {
			t.Fatal(err)
		}
		got, err := comparisonShort(info["os"])
		if shortValue == `false` {
			if err == nil {
				t.Fatal(got, err)
			}
		} else if err != nil || got != "" {
			t.Fatal(got, err)
		}
	}
	if got := comparisonText(comparisonValue(t, `["literal",{"b":9007199254740993,"a":false}]`)); got != `["literal", {"b" => 9007199254740993, "a" => false}]` {
		t.Fatal(got)
	}
}

type comparisonSink struct {
	texts []string
	fail  func(int, string) error
}

func (s *comparisonSink) Puts(text string) error {
	s.texts = append(s.texts, text)
	if s.fail != nil {
		return s.fail(len(s.texts), text)
	}
	return nil
}
func (s *comparisonSink) String() string {
	var out strings.Builder
	for _, text := range s.texts {
		out.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestComparisonRenderSourceBytesAndNoDiffMirror(t *testing.T) {
	for _, tc := range []struct {
		name, target, deployed, pattern string
		patternPresent, downgrade       bool
		wantDiff, wantLog               string
	}{
		{"empty-union", `{}`, `{}`, "miss", true, false, "h: no inputs info\n\n", "h: no inputs info\n\n"},
		{"filter-miss", `{"os":"new"}`, `{"os":"old"}`, "", true, false, "", ""},
		{"same-rev-url-ignored", `{"os":{"rev":"same","url":"new-url"}}`, `{"os":{"rev":"same","url":"old-url"}}`, "", false, false, "h: no changes\n\n", "h: no changes\n\n"},
		{"unknown", `{"b":{},"a":null}`, `{"a":{},"b":{}}`, "", false, false, "h @ a in unknown state\nh @ b in unknown state\n\n", "h @ a in unknown state\nh @ b in unknown state\n\n"},
		{"mixed", `{"z":{},"a":"target1234","same":"equal"}`, `{"a":"deploy1234","same":"equal"}`, "", false, false, "h @ a: deploy12 -> target12\nh @ z in unknown state\n\n", "h @ a:\n(changelog unavailable)\n\nh @ z in unknown state\n"},
		{"target-empty-url", `{"os":{"rev":"new123456","url":"","shortRev":""}}`, `{"os":{"rev":"old123456","url":"fallback","shortRev":false}}`, "", false, false, "h @ os (): old12345 -> \n\n", "h @ os:\nlog\n\n"},
		{"downgrade-false-url", `{"os":{"rev":"new123456","url":false}}`, `{"os":{"rev":"old123456","url":"repo"}}`, "", false, true, "h @ os (repo): new12345 -> old12345\n\n", "h @ os:\nlog\n\n"},
		{"same-short-full-change", `{"os":{"rev":"abcdefgh-new","url":"repo"}}`, `{"os":{"rev":"abcdefgh-old"}}`, "", false, false, "h @ os (repo): abcdefgh -> abcdefgh\n\n", "h @ os:\nlog\n\n"},
		{"false-is-nonnil", `{"os":{"rev":false}}`, `{"os":{"rev":"old"}}`, "", false, false, "h @ os: old -> unknown\n\n", "h @ os:\n(changelog unavailable)\n\n"},
	} {
		for _, command := range []string{"diff", "changelog"} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				args := []string{command}
				if tc.downgrade {
					args = append(args, "--downgrade")
				}
				inv := comparisonInvocation(t, args...)
				var pattern *string
				if tc.patternPresent {
					pattern = &tc.pattern
				}
				st := Status{Machine: Machine{Name: "h"}, Target: comparisonInfo(t, tc.target), Deployed: comparisonInfo(t, tc.deployed)}
				logCalls := 0
				sink := &comparisonSink{}
				err := comparisonRender(context.Background(), sink, inv, pattern, st, func(_ context.Context, url, from, to string, options []string) (string, error) {
					logCalls++
					if command == "diff" {
						t.Fatal("diff mirror call")
					}
					if !reflect.DeepEqual(options, []string{"--oneline", "--max-count=50"}) {
						t.Fatal(options)
					}
					if tc.name == "target-empty-url" && url != "" {
						t.Fatal(url)
					}
					if tc.downgrade && (from != "new123456" || to != "old123456") {
						t.Fatal(from, to)
					}
					return "log", nil
				})
				want := tc.wantDiff
				if command == "changelog" {
					want = tc.wantLog
				}
				if err != nil || sink.String() != want {
					t.Fatalf("got%q err%v want%q", sink.String(), err, want)
				}
				if command == "diff" && logCalls != 0 {
					t.Fatal(logCalls)
				}
			})
		}
	}
}

func TestComparisonQuerySharedTransportOrderAndPartialFields(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "private/config")
	carrier := connectionMachine("carrier", "carrier", "carrier.invalid", true)
	carrier.Target.Port = 2222
	logical := connectionMachine("logical", "key", "never.invalid", true)
	logical.Profile = "/literal/alias/../profile/"
	logical.CarrierName = &carrier.Name
	for _, tc := range []struct {
		name, embedded, direct               string
		machineExit, inputExit, upExit       int
		fail                                 string
		wantFallback, wantUnknown, wantError bool
	}{
		{"ordered-object", `{"inputs-info":{"os":{"short_rev":"first","shortRev":"last","rev":"abcdefghijk","lastModified":9007199254740993}}}`, "", 0, 0, 0, "", false, false, false},
		{"ordered-string", `{"inputs-info":"{\"os\":{\"shortRev\":\"first\",\"short_rev\":\"last\",\"rev\":\"abcdefghijk\"}}"}`, "", 0, 0, 0, "", false, false, false},
		{"empty-object", `{"inputs-info":{}}`, "", 0, 0, 0, "", false, false, false},
		{"empty-string", `{"inputs-info":""}`, "", 0, 0, 0, "", false, true, false},
		{"false-fallback", `{"inputs-info":false}`, `{"os":"abcdefghijk"}`, 0, 0, 0, "", true, false, false},
		{"nil-fallback", `{"inputs-info":null}`, `{"os":"abcdefghijk"}`, 0, 0, 0, "", true, false, false},
		{"missing-fallback", `{}`, `{"os":"abcdefghijk"}`, 0, 0, 0, "", true, false, false},
		{"bad-machine-fallback", `bad`, `{"os":"abcdefghijk"}`, 0, 0, 0, "", true, false, false},
		{"failed-machine-fallback", "", `{"os":"abcdefghijk"}`, 1, 0, 0, "", true, false, false},
		{"failed-input", "{}", "", 0, 1, 0, "", true, true, false},
		{"bad-input", "{}", "bad", 0, 0, 0, "", true, true, false},
		{"offline", "", "", 0, 0, 255, "", false, true, false},
		{"infra-uptime", "", "", 0, 0, 0, "uptime", false, true, true},
		{"infra-inputs", `{}`, "", 0, 0, 0, "inputs", true, true, true},
		{"nonobject-machine", `[]`, "", 0, 0, 0, "", false, true, true},
		{"string-machine-substring", `"prefix-inputs-info-suffix"`, "", 0, 0, 0, "", false, true, false},
		{"string-machine-miss", `"missing"`, `{"os":"abcdefghijk"}`, 0, 0, 0, "", true, false, false},
		{"integer-machine", `1`, "", 0, 0, 0, "", false, true, true},
		{"nil-machine", `null`, "", 0, 0, 0, "", false, true, true},
		{"normalization-error", `{"inputs-info":{"os":{"rev":1.2}}}`, "", 0, 0, 0, "", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := []string{}
			sentinel := errors.New("scripted infrastructure")
			e := &Engine{Context: context.Background()}
			st := Status{Machine: logical}
			e.queryWithInputs(&st, []Machine{logical, carrier}, queryOptions{Inputs: true}, func(argv []string, stdin []byte) (Result, error) {
				if stdin != nil {
					t.Fatal("unexpected script")
				}
				if !reflect.DeepEqual(argv[:8], []string{"ssh", "-F", "private/config", "-l", "root", "-p", "2222", "carrier.invalid"}) {
					t.Fatal(argv)
				}
				remote := argv[len(argv)-1]
				trace = append(trace, remote)
				switch remote {
				case "cat /proc/uptime":
					if tc.fail == "uptime" {
						return Result{}, sentinel
					}
					return Result{ExitCode: tc.upExit, Stdout: []byte("600.5 0\n")}, nil
				case "cat /literal/alias/../profile/machine.json":
					return Result{ExitCode: tc.machineExit, Stdout: []byte(tc.embedded)}, nil
				case "cat /literal/alias/../profile/etc/confctl/inputs-info.json":
					if tc.fail == "inputs" {
						return Result{}, sentinel
					}
					return Result{ExitCode: tc.inputExit, Stdout: []byte(tc.direct)}, nil
				default:
					t.Fatal("unselected query", remote)
					return Result{}, nil
				}
			}, true)
			want := []string{"cat /proc/uptime"}
			if tc.upExit == 0 && tc.fail != "uptime" {
				want = append(want, "cat /literal/alias/../profile/machine.json")
			}
			if tc.wantFallback {
				want = append(want, "cat /literal/alias/../profile/etc/confctl/inputs-info.json")
			}
			if !reflect.DeepEqual(trace, want) || (st.Err != nil) != tc.wantError || (st.Deployed == nil) != tc.wantUnknown || (st.Uptime != nil) != (tc.upExit == 0 && tc.fail != "uptime") {
				t.Fatal(trace, want, st)
			}
			if tc.fail != "" && st.Err != sentinel {
				t.Fatal(st.Err)
			}
			if tc.name == "ordered-object" || tc.name == "ordered-string" {
				if st.Deployed["os"]["shortRev"] != "last" {
					t.Fatal(st.Deployed)
				}
			}
		})
	}
	// Default status decoding remains unchanged (short_rev always wins), while
	// comparison keeps the actual nested encounter order through the same reader.
	for _, ordered := range []bool{false, true} {
		st := Status{Machine: carrier}
		e := &Engine{Context: context.Background()}
		calls := []string{}
		e.queryWithInputs(&st, []Machine{carrier}, queryOptions{Inputs: true}, func(argv []string, _ []byte) (Result, error) {
			calls = append(calls, argv[len(argv)-1])
			if len(calls) == 1 {
				return Result{Stdout: []byte("42 0")}, nil
			}
			return Result{Stdout: []byte(`{"os":{"short_rev":"first","shortRev":"last","rev":"abcdefghijk"}}`)}, nil
		}, ordered)
		want := "first"
		if ordered {
			want = "last"
		}
		if st.Err != nil || st.Deployed["os"]["shortRev"] != want || !reflect.DeepEqual(calls, []string{"cat /proc/uptime", "cat /etc/confctl/inputs-info.json"}) {
			t.Fatal(st, calls)
		}
	}
}

func TestComparisonNormalizationErrorsCurrentRescueSelectedPropagation(t *testing.T) {
	m := connectionMachine("z", "key-z", "unused.invalid", true)
	const metadata = `{"os":{"rev":{"value":"cannot-slice"}}}`
	e, d, _ := comparisonFixture(t, []Machine{m})
	d.Metadata = func() (*inputs.MachineResolver, error) {
		return &inputs.MachineResolver{Eval: func(s string) ([]byte, error) {
			if s == ".#confctl.machineKeys" {
				return []byte(`{"z":"key-z"}`), nil
			}
			return []byte(metadata), nil
		}}, nil
	}
	out, err := runComparison(t, e, comparisonInvocation(t, "diff", "-y"), "", d)
	if err != nil || out != "z @ os in unknown state\n\n" {
		t.Fatal(out, err)
	}
	seedCoreGeneration(t, e.Root, "z", "chosen", coreGenerationRecord(e.Root+"/system", "2000-01-01T00:00:00Z", metadata))
	d.Builds = func(host string) (*generation.BuildList, error) {
		return generation.LoadBuildList(e.Root, host, generation.Options{})
	}
	out, err = runComparison(t, e, comparisonInvocation(t, "diff", "-y", "-g", "chosen"), "", d)
	if err == nil || err.Error() != "wrong number of arguments (given 2, expected 1)" || out != "Resolved host generations:\n" {
		t.Fatal(out, err)
	}
}

func TestComparisonQueryLegacySwitchesAndPartialCountsRemainExact(t *testing.T) {
	t.Setenv("CONFCTL_SSH_CONFIG", "")
	m := connectionMachine("logical", "key", "unused.invalid", true)
	e := &Engine{Context: context.Background()}
	for _, tc := range []struct {
		name, fail            string
		options               queryOptions
		want                  []string
		up, top, gens, inputs bool
	}{
		{"none", "", queryOptions{}, []string{"cat /proc/uptime"}, true, false, false, false},
		{"status", "", queryOptions{Generations: true, Inputs: true}, []string{"cat /proc/uptime", "bash --norc", "cat /etc/confctl/inputs-info.json"}, true, false, true, true},
		{"listing", "", queryOptions{Generations: true}, []string{"cat /proc/uptime", "bash --norc"}, true, false, true, false},
		{"all", "", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc", "cat /etc/confctl/inputs-info.json"}, true, true, true, true},
		{"offline", "cat /proc/uptime", queryOptions{true, true, true}, []string{"cat /proc/uptime"}, false, false, false, false},
		{"top-failed", "realpath /run/current-system", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system"}, true, false, false, false},
		{"gens-failed", "bash --norc", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc"}, true, true, false, false},
		{"inputs-failed", "cat /etc/confctl/inputs-info.json", queryOptions{true, true, true}, []string{"cat /proc/uptime", "realpath /run/current-system", "bash --norc", "cat /etc/confctl/inputs-info.json"}, true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := []string{}
			st := Status{Machine: m}
			e.queryWithInputs(&st, []Machine{m}, tc.options, func(argv []string, stdin []byte) (Result, error) {
				remote := argv[len(argv)-1]
				trace = append(trace, remote)
				if remote == "bash --norc" && string(stdin) != string(generation.ProfileScript(m.Profile)) {
					t.Fatal("profile script changed")
				}
				if remote == tc.fail {
					return Result{ExitCode: 255}, nil
				}
				switch remote {
				case "cat /proc/uptime":
					return Result{Stdout: []byte("600.5 0\n")}, nil
				case "realpath /run/current-system":
					return Result{Stdout: []byte("\x00 \t/top/../system\n")}, nil
				case "bash --norc":
					return Result{Stdout: []byte{}}, nil
				case "cat /etc/confctl/inputs-info.json":
					return Result{Stdout: []byte(`{"role":{"rev":"abcdefghijk","lastModified":9007199254740993}}`)}, nil
				default:
					t.Fatal(remote)
					return Result{}, nil
				}
			}, false)
			if st.Err != nil || !reflect.DeepEqual(trace, tc.want) || (st.Uptime != nil) != tc.up || (st.CurrentToplevel != nil) != tc.top || (st.Generations != nil) != tc.gens || (st.Deployed != nil) != tc.inputs {
				t.Fatal(st, trace, tc.want)
			}
			if tc.gens && (len(st.Generations.Generations) != 0 || st.Generations.Host != "logical") {
				t.Fatal(st.Generations)
			}
			if tc.inputs && st.Deployed["role"]["lastModified"] != json.Number("9007199254740993") {
				t.Fatal(st.Deployed)
			}
		})
	}
}

func TestComparisonSelectedLocalReadersNoWritesAndLiteralSelectors(t *testing.T) {
	root := t.TempDir()
	m := connectionMachine("z", "key-z", "unused.invalid", true)
	seedCoreGeneration(t, root, "z", "old", coreGenerationRecord(root+"/old", "2000-01-01T00:00:00Z", `{"os":"oldrecord"}`))
	seedCoreGeneration(t, root, "z", "none", coreGenerationRecord(root+"/new", "2001-01-01T00:00:00Z", `{"os":"newrecord"}`))
	if err := os.Symlink("old", filepath.Join(root, ".confctl/generations/z/current")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		selector, rev string
		missing       bool
	}{
		{"current", "oldrecor", false}, {"0", "newrecor", false}, {"1", "oldrecor", false}, {"-1", "oldrecor", false}, {"none", "newrecor", false}, {"old", "oldrecor", false}, {"", "", true}, {"9999999999999999999999999999999999999", "", true},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			e, d, trace := comparisonFixture(t, []Machine{m})
			e.Root = root
			d.Builds = func(host string) (*generation.BuildList, error) {
				*trace = append(*trace, "local:"+host)
				return generation.LoadBuildList(root, host, generation.Options{})
			}
			d.Metadata = func() (*inputs.MachineResolver, error) {
				t.Fatal("selected generation reached current eval")
				return nil, nil
			}
			before := testConfigurationState(t, root)
			out, err := runComparison(t, e, comparisonInvocation(t, "diff", "--yes", "--generation", tc.selector), "", d)
			if tc.missing {
				if err == nil || err.Error() != "No generation found" || out != "" {
					t.Fatal(out, err)
				}
			} else {
				if err != nil || !strings.Contains(out, "z @ os: deploy12 -> "+tc.rev+"\n\n") || !strings.HasPrefix(out, "Resolved host generations:\n") {
					t.Fatal(out, err)
				}
			}
			if !reflect.DeepEqual(before, testConfigurationState(t, root)) {
				t.Fatal("comparison changed bytes/modes/links")
			}
		})
	}
	bad := seedCoreGeneration(t, root, "z", "bad", `{"mode":"software-pins"}`)
	before := testConfigurationState(t, root)
	warnings := []string{}
	e, d, _ := comparisonFixture(t, []Machine{m})
	d.Builds = func(host string) (*generation.BuildList, error) {
		return generation.LoadBuildList(root, host, generation.Options{Warn: func(s string) error { warnings = append(warnings, s); return nil }})
	}
	for _, selector := range []string{"0", "bad"} {
		out, err := runComparison(t, e, comparisonInvocation(t, "diff", "--yes", "-g", selector), "", d)
		if err == nil || out != "" {
			t.Fatal(out, err)
		}
		if selector == "0" && !strings.Contains(err.Error(), "ambiguous") {
			t.Fatal(err)
		}
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], bad) || !reflect.DeepEqual(before, testConfigurationState(t, root)) {
		t.Fatal(warnings)
	}
}

func TestComparisonResolvedTableOrderCollisionsAndBorrowing(t *testing.T) {
	builds := []resolvedBuild{{"z", &generation.Build{Name: "Z", InputsInfo: comparisonValue(t, `{"second":"222222222","first":"111111111"}`).(generation.Object)}}, {"a", &generation.Build{Name: "A", InputsInfo: comparisonValue(t, `{"third":"333333333","second":{"rev":"ignored","shortRev":""}}`).(generation.Object)}}}
	before := fmt.Sprintf("%#v", builds)
	got, err := resolvedBuildTable(builds, []string{"m"})
	const want = "NAME   GENERATION   KERNEL   SECOND     FIRST      THIRD\na      A            -        -          -          33333333\nm      not found    -        -          -          -\nz      Z            -        22222222   11111111   -\n"
	if err != nil || got != want {
		t.Fatalf("got%q err%v want%q", got, err, want)
	}
	if fmt.Sprintf("%#v", builds) != before || builds[0].Host != "z" {
		t.Fatal("table sorted borrowed insertion order")
	}
	builds = []resolvedBuild{{"z", &generation.Build{Name: "Z", InputsInfo: comparisonValue(t, `{"name":"aaa-name","generation":"overwritten-gen","kernel":"kernel-role"}`).(generation.Object)}}, {"a", &generation.Build{Name: "A", InputsInfo: comparisonValue(t, `{}`).(generation.Object)}}}
	got, err = resolvedBuildTable(builds, nil)
	if err != nil || !strings.HasPrefix(got, "NAME       GENERATION   KERNEL     NAME       GENERATION   KERNEL\n") || !strings.Contains(got, "aaa-name   overwrit") {
		t.Fatal(got, err)
	}
	builds = []resolvedBuild{{"z", &generation.Build{Name: "Z", InputsInfo: comparisonValue(t, `{"os":{"rev":["x",{"b":2,"a":1}]}}`).(generation.Object)}}}
	got, err = resolvedBuildTable(builds, nil)
	if want := "NAME   GENERATION   KERNEL   OS\nz      Z            -        [\"x\", {\"b\" => 2, \"a\" => 1}]\n"; err != nil || got != want {
		t.Fatal(got, err, want)
	}
}

func TestComparisonCurrentResolverFailuresRemainPerHost(t *testing.T) {
	ms := []Machine{connectionMachine("z", "key-z", "unused.invalid", true), connectionMachine("a", "key-a", "unused.invalid", true)}
	for _, tc := range []struct {
		name                     string
		firstKeys, firstMetadata string
		keyError, metadataError  error
		want                     []string
	}{
		{"key-eval", "", `{}`, errors.New("keys failed"), nil, []string{".#confctl.machineKeys", ".#confctl.machineKeys", ".#confctl.inputsInfo.key-a"}},
		{"key-json", `{"z":"prefix-z","a":"prefix-a"} {}`, `{}`, nil, nil, []string{".#confctl.machineKeys", ".#confctl.machineKeys", ".#confctl.inputsInfo.key-a"}},
		{"metadata-eval", `{"z":"key-z","a":"key-a"}`, `{}`, nil, errors.New("metadata failed"), []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key-z", ".#confctl.inputsInfo.key-a"}},
		{"metadata-json", `{"z":"key-z","a":"key-a"}`, `bad`, nil, nil, []string{".#confctl.machineKeys", ".#confctl.inputsInfo.key-z", ".#confctl.inputsInfo.key-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, d, _ := comparisonFixture(t, ms)
			calls := []string{}
			keyCalls := 0
			d.Metadata = func() (*inputs.MachineResolver, error) {
				return &inputs.MachineResolver{Eval: func(s string) ([]byte, error) {
					calls = append(calls, s)
					if s == ".#confctl.machineKeys" {
						keyCalls++
						if keyCalls == 1 {
							return []byte(tc.firstKeys), tc.keyError
						}
						return []byte(`{"z":"key-z","a":"key-a"}`), nil
					}
					if s == ".#confctl.inputsInfo.key-z" {
						return []byte(tc.firstMetadata), tc.metadataError
					}
					return []byte(`{"os":"target123456"}`), nil
				}}, nil
			}
			out, err := runComparison(t, e, comparisonInvocation(t, "diff", "--yes"), "", d)
			if err != nil || out != "z @ os in unknown state\n\na @ os: deploy12 -> target12\n\n" || !reflect.DeepEqual(calls, tc.want) {
				t.Fatal(out, err, calls)
			}
		})
	}
}

func TestComparisonResolvedTableTypedSortKeys(t *testing.T) {
	// OutputFormatter.prepare retains typed cells through [name,generation]<=>;
	// nil/false/empty String alone become "-". These are literal source fixtures.
	for _, tc := range []struct{ name, first, second, want string }{
		{"integer-name", `{"name":{"rev":10}}`, `{"name":{"rev":2}}`, "NAME   GENERATION   KERNEL   NAME\n2      G            -        2\n10     G            -        10\n"},
		{"integer-generation", `{"generation":{"rev":10}}`, `{"generation":{"rev":2}}`, "NAME   GENERATION   KERNEL   GENERATION\nh      2            -        2\nh      10           -        10\n"},
		{"array-name", `{"name":{"rev":[10]}}`, `{"name":{"rev":[2]}}`, "NAME   GENERATION   KERNEL   NAME\n[2]    G            -        [2]\n[10]   G            -        [10]\n"},
		{"empty-array-retained", `{"name":{"rev":[2]}}`, `{"name":{"rev":[]}}`, "NAME   GENERATION   KERNEL   NAME\n[]     G            -        []\n[2]    G            -        [2]\n"},
		{"placeholder-before-integer", `{"name":{"rev":2}}`, `{"name":{"rev":false}}`, "NAME   GENERATION   KERNEL   NAME\n-      G            -        -\n2      G            -        2\n"},
		{"nil-placeholder", `{"name":{"rev":2}}`, `{"name":{"rev":null}}`, "NAME   GENERATION   KERNEL   NAME\n-      G            -        -\n2      G            -        2\n"},
		{"empty-string-placeholder", `{"name":{"rev":2}}`, `{"name":{"rev":""}}`, "NAME   GENERATION   KERNEL   NAME\n-      G            -        -\n2      G            -        2\n"},
		{"mixed-incomparable-stable", `{"name":{"rev":2}}`, `{"name":{"rev":"a"}}`, "NAME   GENERATION   KERNEL   NAME\n2      G            -        2\na      G            -        a\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builds := []resolvedBuild{}
			for _, info := range []string{tc.first, tc.second} {
				builds = append(builds, resolvedBuild{"h", &generation.Build{Name: "G", InputsInfo: comparisonValue(t, info).(generation.Object)}})
			}
			before := fmt.Sprintf("%#v/%#v", builds[0].Build.InputsInfo, builds[1].Build.InputsInfo)
			got, err := resolvedBuildTable(builds, nil)
			if err != nil || got != tc.want {
				t.Errorf("got=%q err=%v want=%q", got, err, tc.want)
			}
			if fmt.Sprintf("%#v/%#v", builds[0].Build.InputsInfo, builds[1].Build.InputsInfo) != before {
				t.Fatal("mutated borrowed metadata")
			}
		})
	}
}

func TestComparisonIntegerInterpolation(t *testing.T) {
	const huge = "12345678901234567890123456789012345678901234567890"
	for _, tc := range []struct{ name, source, from, short string }{
		{"numeric-negative-zero", `-0`, "0", "0"},
		{"numeric-negative-zero-derived-short", `-0`, "0", "0"},
		{"literal-string-negative-zero", `"-0"`, "-0", "-0"},
		{"huge-integer", huge, huge, huge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Explicit shortRev also reaches interpolation without Integer bit slicing.
			shortField := `,"shortRev":` + tc.source
			if tc.name == "numeric-negative-zero-derived-short" {
				shortField = ""
			}
			deployed := comparisonInfo(t, `{"os":{"rev":`+tc.source+shortField+`}}`)
			target := comparisonInfo(t, `{"os":{"rev":1,"url":"repo"}}`)
			if comparisonText(deployed["os"]["rev"]) != tc.from {
				t.Errorf("integer meaning lost: %q", comparisonText(deployed["os"]["rev"]))
			}
			if tc.name == "numeric-negative-zero" && deployed["os"]["rev"] != json.Number("-0") {
				t.Fatal("decoder lexical contract changed")
			}
			st := Status{Machine: Machine{Name: "z"}, Target: target, Deployed: deployed}
			diff := &comparisonSink{}
			err := comparisonRender(context.Background(), diff, comparisonInvocation(t, "diff"), nil, st, func(context.Context, string, string, string, []string) (string, error) {
				t.Fatal("diff mirror")
				return "", nil
			})
			want := "z @ os (repo): " + tc.short + " -> 1\n\n"
			if err != nil || diff.String() != want {
				t.Errorf("diff=%q err=%v want=%q", diff.String(), err, want)
			}
			log := &comparisonSink{}
			calls := 0
			err = comparisonRender(context.Background(), log, comparisonInvocation(t, "changelog"), nil, st, func(_ context.Context, url, from, to string, options []string) (string, error) {
				calls++
				if url != "repo" || from != tc.from || to != "1" || !reflect.DeepEqual(options, []string{"--oneline", "--max-count=50"}) {
					t.Errorf("literal mirror: %q %q %q %v", url, from, to, options)
				}
				return "git log for " + from + ".." + to + "\nentry", nil
			})
			want = "z @ os:\ngit log for " + tc.from + "..1\nentry\n\n"
			if err != nil || calls != 1 || log.String() != want {
				t.Errorf("log=%q calls=%d err=%v want=%q", log.String(), calls, err, want)
			}
		})
	}
}
