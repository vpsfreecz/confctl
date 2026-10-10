// Package inputs owns read-only flake lock and input selection behavior.
package inputs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Lock deliberately reads nodes.root, independently of the document's root
// field. It retains JSON numbers without float conversion and does not rewrite
// the document. Mutation and its ordered serialization have a separate owner.
type Lock struct{ data map[string]any }

func LoadLock(path string) (*Lock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeLock(b)
}

func DecodeLock(b []byte) (*Lock, error) {
	var data map[string]any
	if err := decode(b, &data); err != nil {
		return nil, lockJSONDiagnostic(b, err)
	}
	return &Lock{data: data}, nil
}

// JSON gem 2.21.2's initial JSON_PHASE_VALUE default reports a byte-limited
// token fragment and cursor position. Only this observed SyntaxError class is
// translated; other malformed-JSON diagnostics retain the Go error.
func lockJSONDiagnostic(b []byte, err error) error {
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		return err
	}
	i := 0
	for i < len(b) && strings.ContainsRune(" \t\r\n", rune(b[i])) {
		i++
	}
	if i == len(b) || syntax.Offset != int64(i+1) || !strings.Contains(syntax.Error(), "looking for beginning of value") || b[i] == 0 || strings.ContainsRune("ntfNI-0123456789\"[{", rune(b[i])) {
		return err
	}
	end := i
	for end < len(b) && end-i < 32 && b[end] != 0 && !strings.ContainsRune(" \t\r\n", rune(b[end])) {
		end++
	}
	for end > i && b[end-1] >= 0x80 && b[end-1] < 0xc0 {
		end--
	}
	if end > i && b[end-1] >= 0xc0 {
		end--
	}
	line := bytes.Count(b[:i], []byte{'\n'}) + 1
	column := i - bytes.LastIndexByte(b[:i], '\n')
	return fmt.Errorf("unexpected character: '%s' at line %d column %d", b[i:end], line, column)
}

func decode(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return err
	}
	var suffix json.RawMessage
	if err := d.Decode(&suffix); err != io.EOF {
		if err == nil {
			return fmt.Errorf("extra JSON content")
		}
		return err
	}
	return nil
}

func object(v any) (map[string]any, error) {
	if v == nil || v == false {
		return nil, nil
	}
	h, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected input object")
	}
	return h, nil
}

func (l *Lock) root() (map[string]any, map[string]any, error) {
	nodes, err := object(l.data["nodes"])
	if err != nil {
		return nil, nil, err
	}
	root, err := object(nodes["root"])
	if err != nil {
		return nil, nil, err
	}
	inputs, err := object(root["inputs"])
	return nodes, inputs, err
}

func (l *Lock) RootInputs() ([]string, error) {
	_, inputs, err := l.root()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Info carries nil/false/empty values to the owning UI's existing placeholder
// conversion. In particular, a missing revision is distinct from an empty one.
type Info struct{ Type, Ref, Rev, ShortRev, URL any }

func first(a, b any) any {
	if a != nil && a != false {
		return a
	}
	return b
}

func (l *Lock) InputInfo(name any) (Info, error) {
	nodes, inputs, err := l.root()
	if err != nil {
		return Info{}, err
	}
	input, _ := name.(string)
	var ref any
	if _, ok := name.(string); ok {
		ref = inputs[input]
	}
	if a, ok := ref.([]any); ok {
		ref = nil
		if len(a) > 0 {
			ref, _ = a[0].(string)
		}
	}
	var nodeValue any
	if id, ok := ref.(string); ok {
		nodeValue = nodes[id]
	}
	node, err := object(nodeValue)
	if err != nil {
		return Info{}, err
	}
	locked, err := object(node["locked"])
	if err != nil {
		return Info{}, err
	}
	original, err := object(node["original"])
	if err != nil {
		return Info{}, err
	}
	info := Info{Type: first(first(locked["type"], original["type"]), "-"), Ref: first(original["ref"], locked["ref"]), Rev: locked["rev"]}
	if rev, ok := info.Rev.(string); ok {
		r := []rune(rev)
		if len(r) > 8 {
			r = r[:8]
		}
		info.ShortRev = string(r)
	} else if info.Rev != nil && info.Rev != false {
		return Info{}, fmt.Errorf("expected input revision string")
	}
	info.URL = first(locked["url"], original["url"])
	if url, ok := info.URL.(string); ok {
		info.URL = strings.TrimPrefix(url, "git+")
	}
	if locked["type"] == "github" || original["type"] == "github" {
		owner, repo := first(locked["owner"], original["owner"]), first(locked["repo"], original["repo"])
		if owner != nil && owner != false && repo != nil && repo != false {
			info.URL = fmt.Sprintf("https://github.com/%v/%v", owner, repo)
		}
	}
	return info, nil
}
