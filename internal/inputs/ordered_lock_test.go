package inputs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These fields are extracts of accepted original Ruby observations. The full
// original metadata/raw/status hashes and extraction boundaries are recorded
// in testdata/ordered-lock/provenance.json, with no outside-module test reads.
type orderedOriginalString struct {
	BytesHex string `json:"bytes_hex"`
}
type orderedOriginalNode struct {
	Path    []json.RawMessage       `json:"path"`
	Type    string                  `json:"type"`
	Keys    []orderedOriginalString `json:"keys_in_insertion_order"`
	Length  int                     `json:"length"`
	Decimal string                  `json:"decimal"`
	Bits    string                  `json:"binary64_big_endian_hex"`
	String  orderedOriginalString   `json:"string"`
	Value   bool                    `json:"value"`
}
type orderedOriginalRecord struct {
	ID               string                `json:"record"`
	Input            string                `json:"input"`
	ParseRequested   bool                  `json:"parse_requested"`
	ParseCompleted   bool                  `json:"parse_completed"`
	Stage            string                `json:"stage"`
	Outcome          string                `json:"outcome"`
	Stdout           string                `json:"stdout"`
	Warning          string                `json:"warning_selection"`
	DiagnosticStderr string                `json:"diagnostic_stderr"`
	ConstructionBits []string              `json:"construction_binary64_big_endian_hex"`
	Nodes            []orderedOriginalNode `json:"value_nodes"`
	Exception        *struct {
		Class   string                `json:"class"`
		Message orderedOriginalString `json:"message"`
	} `json:"exception"`
}

func orderedAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ordered-lock", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func orderedHex(t *testing.T, text string) []byte {
	t.Helper()
	b, err := hex.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func orderedRecords(t *testing.T) []orderedOriginalRecord {
	t.Helper()
	var records []orderedOriginalRecord
	if err := json.Unmarshal(orderedAsset(t, "observations.json"), &records); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestOrderedLockOriginalObservations(t *testing.T) {
	for _, record := range orderedRecords(t) {
		t.Run(record.ID, func(t *testing.T) {
			input := orderedAsset(t, record.Input)
			var document *orderedLockDocument
			if record.ParseRequested {
				var err error
				document, err = parseOrderedLock(input)
				if !record.ParseCompleted {
					if document != nil {
						t.Fatal("failed parse exposed a document")
					}
					orderedCheckOriginalError(t, record, input, err)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			} else {
				// Exactly the two finite original generator-only constructions;
				// Ruby source bytes are retained for provenance, never executed.
				switch record.ID {
				case "F3":
					if len(record.ConstructionBits) != 32 {
						t.Fatal("missing original32 bit construction")
					}
					a := &orderedLockValue{kind: orderedArray}
					for _, h := range record.ConstructionBits {
						bits, err := strconv.ParseUint(h, 16, 64)
						if err != nil {
							t.Fatal(err)
						}
						a.elements = append(a.elements, &orderedLockValue{kind: orderedFloat, float: math.Float64frombits(bits)})
					}
					document = &orderedLockDocument{root: a}
				case "J15":
					if len(record.Nodes) != 102 || len(record.Nodes[101].Path) != 101 {
						t.Fatal("unexpected original depth construction")
					}
					v := &orderedLockValue{kind: orderedInteger, integer: big.NewInt(0)}
					for i := 0; i < 101; i++ {
						v = &orderedLockValue{kind: orderedArray, elements: []*orderedLockValue{v}}
					}
					document = &orderedLockDocument{root: v}
				default:
					t.Fatal("unknown generator-only original record")
				}
			}
			orderedCheckOriginalNodes(t, document.root, record.Nodes)
			out, err := prettyOrderedLock(document)
			if record.Outcome == "exception" {
				if out != nil {
					t.Fatalf("generator error exposed partial buffer: %q", out)
				}
				orderedCheckOriginalError(t, record, input, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 0 && out[len(out)-1] == '\n' {
				t.Fatal("codec included caller's final LF")
			}
			if !bytes.Equal(append(append([]byte(nil), out...), '\n'), orderedAsset(t, record.Stdout)) {
				t.Fatalf("pretty bytes differ from original:\n%s", out)
			}
			if record.DiagnosticStderr != "" {
				if record.Warning != "deprecated=true" || len(orderedAsset(t, record.DiagnosticStderr)) == 0 {
					t.Fatal("forced-warning diagnostic provenance lost")
				}
			}
		})
	}
}

func orderedCheckOriginalError(t *testing.T, r orderedOriginalRecord, input []byte, err error) {
	t.Helper()
	var e *orderedLockError
	if r.Exception == nil || !errors.As(err, &e) {
		t.Fatalf("expected original %s exception, got %v", r.Stage, err)
	}
	if e.phase != r.Stage || e.kind != r.Exception.Class || !bytes.Equal([]byte(e.message), orderedHex(t, r.Exception.Message.BytesHex)) {
		t.Fatalf("original error differs: phase=%s kind=%s message=%q", e.phase, e.kind, e.message)
	}
	match := regexp.MustCompile(` at line ([0-9]+) column ([0-9]+)$`).FindStringSubmatch(e.message)
	if len(match) == 0 {
		if e.cursor != -1 {
			t.Fatal("invented diagnostic cursor")
		}
		return
	}
	line, _ := strconv.Atoi(match[1])
	column, _ := strconv.Atoi(match[2])
	// Derive the expected byte offset solely from the ORIGINAL message/input.
	start := 0
	for i := 1; i < line; i++ {
		next := bytes.IndexByte(input[start:], '\n')
		if next < 0 {
			t.Fatal("original cursor out of range")
		}
		start += next + 1
	}
	expectedCursor := start + column - 1
	if e.cursor != expectedCursor || e.line != line || e.column != column {
		t.Fatalf("cursor=%d (%d:%d), expected %d (%d:%d)", e.cursor, e.line, e.column, expectedCursor, line, column)
	}
}

func orderedCheckOriginalNodes(t *testing.T, root *orderedLockValue, nodes []orderedOriginalNode) {
	t.Helper()
	var count func(*orderedLockValue) int
	count = func(v *orderedLockValue) int {
		n := 1
		for _, m := range v.members {
			n += count(m.value)
		}
		for _, e := range v.elements {
			n += count(e)
		}
		return n
	}
	if count(root) != len(nodes) {
		t.Fatalf("node count %d != original %d", count(root), len(nodes))
	}
	for _, n := range nodes {
		v := root
		for _, step := range n.Path {
			var key struct {
				Hex string `json:"key_bytes_hex"`
			}
			if len(step) > 0 && step[0] == '{' {
				if err := json.Unmarshal(step, &key); err != nil {
					t.Fatal(err)
				}
				var ok bool
				v, ok = v.lookup(string(orderedHex(t, key.Hex)))
				if !ok {
					t.Fatal("original member absent")
				}
			} else {
				var i int
				if err := json.Unmarshal(step, &i); err != nil {
					t.Fatal(err)
				}
				if v.kind != orderedArray || i >= len(v.elements) {
					t.Fatal("original array position absent")
				}
				v = v.elements[i]
			}
		}
		name := []string{"null", "boolean", "string", "integer", "float", "object", "array"}[v.kind]
		if name != n.Type {
			t.Fatalf("tag at %v: %s != original %s", n.Path, name, n.Type)
		}
		switch v.kind {
		case orderedBool:
			if v.boolean != n.Value {
				t.Fatal("original boolean differs")
			}
		case orderedString:
			if !bytes.Equal([]byte(v.text), orderedHex(t, n.String.BytesHex)) {
				t.Fatal("original string bytes differ")
			}
		case orderedInteger:
			if v.integer.String() != n.Decimal {
				t.Fatal("original integer differs")
			}
		case orderedFloat:
			if fmt.Sprintf("%016x", math.Float64bits(v.float)) != n.Bits {
				t.Fatalf("bits=%016x, original=%s", math.Float64bits(v.float), n.Bits)
			}
		case orderedObject:
			if len(v.members) != len(n.Keys) {
				t.Fatal("original key count differs")
			}
			for i, k := range n.Keys {
				if !bytes.Equal([]byte(v.members[i].key), orderedHex(t, k.BytesHex)) {
					t.Fatal("original insertion order differs")
				}
			}
		case orderedArray:
			if len(v.elements) != n.Length {
				t.Fatal("original array length differs")
			}
		}
	}
}

func TestOrderedLockOriginalAssetProvenance(t *testing.T) {
	var p struct {
		Assets map[string]struct {
			SHA   string `json:"sha256"`
			Bytes int    `json:"bytes"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(orderedAsset(t, "provenance.json"), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Assets) == 0 {
		t.Fatal("missing original-backed asset inventory")
	}
	for name, identity := range p.Assets {
		b := orderedAsset(t, name)
		if len(b) != identity.Bytes || fmt.Sprintf("%x", sha256.Sum256(b)) != identity.SHA {
			t.Fatalf("immutable asset %s changed", name)
		}
	}
}

func TestOrderedLockCloneMutationAndDetachedView(t *testing.T) {
	input := orderedAsset(t, "J1.json")
	first, err := parseOrderedLock(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseOrderedLock(append([]byte(nil), input...))
	if err != nil {
		t.Fatal(err)
	}
	clone := first.clone()
	b, _ := clone.root.lookup("b")
	b.integer.SetInt64(99)
	unknown, _ := clone.root.lookup("unknown")
	unknown.delete("z")
	arr, _ := clone.root.lookup("arr")
	arr.elements[0] = &orderedLockValue{kind: orderedString, text: "changed"}
	if out, _ := prettyOrderedLock(first); !bytes.Equal(append(out, '\n'), orderedAsset(t, "J1.stdout")) {
		t.Fatal("clone aliased source")
	}
	if out, _ := prettyOrderedLock(second); !bytes.Equal(append(out, '\n'), orderedAsset(t, "J1.stdout")) {
		t.Fatal("independent parse aliased source")
	}
	value := &orderedLockValue{kind: orderedInteger, integer: big.NewInt(7)}
	clone.root.set("a", value)
	value.integer.SetInt64(8)
	if a, _ := clone.root.lookup("a"); a.integer.String() != "7" {
		t.Fatal("replacement did not own its value")
	}
	if clone.root.members[1].key != "a" {
		t.Fatal("replacement reordered member")
	}
	if !clone.root.delete("b") || clone.root.delete("missing") {
		t.Fatal("deletion presence contract")
	}
	clone.root.set("b", &orderedLockValue{kind: orderedNull})
	var keys []string
	for _, m := range clone.root.members {
		keys = append(keys, m.key)
	}
	if !reflect.DeepEqual(keys, []string{"a", "unknown", "arr", "b"}) {
		t.Fatal(keys)
	}
	if v, ok := clone.root.lookup("b"); !ok || v.kind != orderedNull {
		t.Fatal("null collapsed into absent")
	}
	if _, ok := clone.root.lookup("missing"); ok {
		t.Fatal("absent lookup invented value")
	}
	view, err := first.lockView()
	if err != nil {
		t.Fatal(err)
	}
	if view.data["b"] != json.Number("3") {
		t.Fatal("integer projection is not canonical decimal")
	}
	u := view.data["unknown"].(map[string]any)
	if u["z"] != false {
		t.Fatal("false collapsed")
	}
	if a, ok := u["a"]; !ok || a != nil {
		t.Fatal("null/absence collapsed")
	}
	u["z"] = true
	view.data["arr"].([]any)[0] = "new"
	delete(view.data, "b")
	view2, _ := first.lockView()
	if view2.data["unknown"].(map[string]any)["z"] != false || view2.data["b"] != json.Number("3") || view2.data["arr"].([]any)[0] != nil {
		t.Fatal("projection aliases document or another view")
	}
	// A caller changing the original read buffer cannot change the parsed bytes.
	for i := range input {
		input[i] = 'x'
	}
	if out, _ := prettyOrderedLock(first); !bytes.Equal(append(out, '\n'), orderedAsset(t, "J1.stdout")) {
		t.Fatal("document aliases input buffer")
	}
}

func TestOrderedLockReadProjectionPreservesExistingConsumers(t *testing.T) {
	d, err := parseOrderedLock([]byte(`{"nodes":{"root":{"inputs":{"b":["node","ignored"],"a":"node"}},"node":{"locked":{"type":"git","rev":"1234567890","url":"git+file:/local"},"custom":[9007199254740993,1.0,false,null]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	view, err := d.lockView()
	if err != nil {
		t.Fatal(err)
	}
	names, err := view.RootInputs()
	if err != nil || !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Fatal(names, err)
	}
	info, err := view.InputInfo("b")
	if err != nil || info.ShortRev != "12345678" || info.URL != "file:/local" {
		t.Fatal(info, err)
	}
	a := view.data["nodes"].(map[string]any)["node"].(map[string]any)["custom"].([]any)
	if a[0] != json.Number("9007199254740993") || a[1] != float64(1) || a[2] != false || a[3] != nil {
		t.Fatal(a)
	}
	for _, input := range []string{"null", "false", "1", "1.0", "[]", `"text"`} {
		d, err := parseOrderedLock([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.lockView(); err == nil {
			t.Fatal("nonobject root projected as lock")
		}
	}
}

func TestOrderedLockArbitraryIntegerCloneOwnership(t *testing.T) {
	d, err := parseOrderedLock(orderedAsset(t, "J3.json"))
	if err != nil {
		t.Fatal(err)
	}
	copy := d.clone()
	copy.root.elements[2].integer.Add(copy.root.elements[2].integer, big.NewInt(1))
	if d.root.elements[2].integer.String() != "9223372036854775808" {
		t.Fatal("big integer clone shares backing words")
	}
	if copy.root.elements[2].integer.String() != "9223372036854775809" {
		t.Fatal("clone mutation lost arbitrary integer precision")
	}
	if out, err := prettyOrderedLock(d); err != nil || !bytes.Equal(append(out, '\n'), orderedAsset(t, "J3.stdout")) {
		t.Fatal("clone changed original integer bytes", err)
	}
}

func TestOrderedLockSeparateErrorOrderingAndBuffers(t *testing.T) {
	// The generator visits members in insertion order and rejects invalid keys
	// when reached, without exposing earlier successfully generated bytes.
	d, err := parseOrderedLock([]byte("{\"ok\":1,\"\xff\":0,\"later\":1e400}"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := prettyOrderedLock(d)
	if out != nil || err == nil || err.Error() != "source sequence is illegal/malformed utf-8" {
		t.Fatal(out, err)
	}
	d.root.delete(string([]byte{0xff}))
	if out, err = prettyOrderedLock(d); out != nil || err == nil || err.Error() != "Infinity not allowed in JSON" {
		t.Fatal(out, err)
	}
	// These scalar grammar/cursor checks are source-derived, not new observations.
	for _, input := range []string{"01", "+1", "1.", "1e", "--1", `"\x"`, "[1,]"} {
		if _, err := parseOrderedLock([]byte(input)); err == nil {
			t.Fatal("accepted invalid source form", input)
		}
	}
	_, err = parseOrderedLock([]byte("\n {not-json"))
	var e *orderedLockError
	if !errors.As(err, &e) || e.cursor != 3 || e.line != 2 || e.column != 3 {
		t.Fatal(err)
	}
	if _, err := parseOrderedLock([]byte("/*missing")); err == nil || !strings.HasPrefix(err.Error(), "unterminated comment") {
		t.Fatal(err)
	}
}
