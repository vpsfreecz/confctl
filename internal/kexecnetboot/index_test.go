package kexecnetboot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
)

func TestReaderAdmissionExactNumbersAndOrdering(t *testing.T) {
	for _, bad := range []string{`{"ignored":"\ud800"}`, `{"ignored":"\ud800x"}`, `{"ignored":` + strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100) + `}`, `{} {}`, `null`, "\xff"} {
		if _, err := readIndex(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, tc := range []struct{ raw, want string }{{`\udc00`, "\xed\xb0\x80"}, {`\udc00\udfff`, "\xed\xb0\x80\xed\xbf\xbf"}, {`\ud800\u0041`, "\U00010041"}, {`\ud800\udc00`, "\U00010000"}, {`\ufffd`, "�"}, {"�", "�"}} {
		m, err := readIndex(strings.NewReader(`{"ignored":"` + tc.raw + `"}`))
		if err != nil || m["ignored"] != tc.want {
			t.Fatalf("%q %#v %v", tc.raw, m, err)
		}
	}
	if _, err := readIndex(strings.NewReader(`{"ignored":` + strings.Repeat("[", 99) + "0" + strings.Repeat("]", 99) + `}`)); err != nil {
		t.Fatal(err)
	}
	m, err := readIndex(strings.NewReader(`{"machines":[],"ignored":{"a":1},"huge":9007199254740993123456789}`))
	if err != nil {
		t.Fatal(err)
	}
	if m["huge"].(json.Number).String() != "9007199254740993123456789" {
		t.Fatal(m)
	}
	for _, tc := range []struct {
		number, input string
		want          bool
	}{{"6", "6", true}, {"6.0", "6", true}, {"6.5", "6", false}, {"9007199254740993", "9007199254740993", true}, {"9007199254740993.0", "9007199254740993", false}, {"9007199254740993.0", "9007199254740992", true}} {
		i, _ := new(big.Int).SetString(tc.input, 10)
		if numberEquals(json.Number(tc.number), i) != tc.want {
			t.Fatal(tc)
		}
	}
	if numberEquals(true, big.NewInt(1)) {
		t.Fatal("true equated with1")
	}
}
func TestGenerationIntegerVersusInteractiveToI(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"6", "6"}, {" +6 ", "6"}, {"0x6", "6"}, {"0b110", "6"}, {"0o6", "6"}, {"0d6", "6"}, {"06", "6"}, {"0_6", "6"}, {"6_0", "60"}, {"-0x1", "-1"}, {"9007199254740993123456789", "9007199254740993123456789"}} {
		n, ok := integer(tc.raw)
		if !ok || n.String() != tc.want {
			t.Fatal(tc, n)
		}
	}
	for _, raw := range []string{"", "08", "0x_6", "6__0", "6_", "_6", "1.0", "1e0", "6x", "\x006"} {
		if _, ok := integer(raw); ok {
			t.Fatal("accepted", raw)
		}
	}
	for _, tc := range []struct{ raw, want string }{{"oops", "0"}, {"1junk", "1"}, {"  +2_3tail", "23"}, {"0x6", "0"}, {"6__0", "6"}, {"-1", "-1"}} {
		if n := toInt(tc.raw); n.String() != tc.want {
			t.Fatal(tc, n)
		}
	}
}

func baseMachine(t *testing.T) record {
	t.Helper()
	b, err := os.ReadFile("testdata/kn-nominations.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Cases []struct {
			Literal struct {
				Index struct {
					Fixture string `json:"fixture"`
				} `json:"index_response"`
			} `json:"literal_inputs"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("testdata/" + d.Cases[0].Literal.Index.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	m, err := readIndex(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return record(m["machines"].([]any)[0].(map[string]any))
}
func TestSelectionCurrentTruthinessAndEOF(t *testing.T) {
	m := baseMachine(t)
	gs, _ := records(m["generations"], "generations", true)
	gs[0]["current"] = json.Number("1")
	gs[1]["current"] = true
	for _, req := range []*string{nil, strptr("6"), strptr("0x6"), strptr("-1")} {
		s := selector{in: bufio.NewReader(strings.NewReader("")), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		g, err := s.generation(m, req)
		if err != nil || g["generation"] != json.Number("6") {
			t.Fatal(g, err)
		}
	}
	var out bytes.Buffer
	s := selector{in: bufio.NewReader(strings.NewReader("\n")), out: &out, err: &bytes.Buffer{}, interactive: true}
	g, err := s.generation(m, nil)
	if err != nil || g["generation"] != json.Number("6") || !strings.Contains(out.String(), "[7]*") {
		t.Fatal(g, err, out.String())
	}
	s.in = bufio.NewReader(strings.NewReader(""))
	g, err = s.generation(m, nil)
	if err != nil || g != nil {
		t.Fatal(g, err)
	}
	for _, v := range []any{nil, false, []any{}} {
		m["generations"] = v
		s.interactive = false
		g, err = s.generation(m, nil)
		if err != nil || g != nil {
			t.Fatal(v, g, err)
		}
	}
}
func strptr(s string) *string { return &s }
