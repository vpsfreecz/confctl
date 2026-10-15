package kexecnetboot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

type record map[string]any

func readIndex(r io.Reader) (record, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("ERROR: Could not parse JSON: invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := indexValue(d, data, 0)
	if err != nil {
		return nil, fmt.Errorf("ERROR: Could not parse JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("ERROR: Could not parse JSON: extra data")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("index must be an object")
	}
	return record(m), nil
}

// The original JSON2.9.1 reader limits containers to100 and retains its
// surrogate bytes. encoding/json remains the syntax and numeric token owner;
// ordinary strings decode through stdlib, without a replacement-char marker.
func indexValue(d *json.Decoder, data []byte, depth int) (any, error) {
	start := d.InputOffset()
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, container := t.(json.Delim)
	if !container {
		if s, ok := t.(string); ok {
			return indexString(data[start:d.InputOffset()], s)
		}
		return t, nil
	}
	if depth >= 100 {
		return nil, fmt.Errorf("JSON nesting exceeds 100")
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			start := d.InputOffset()
			t, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := t.(string)
			if !ok {
				return nil, fmt.Errorf("invalid JSON key")
			}
			key, err = indexString(data[start:d.InputOffset()], key)
			if err != nil {
				return nil, err
			}
			v, err := indexValue(d, data, depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		_, err = d.Token()
		return m, err
	case '[':
		a := []any{}
		for d.More() {
			v, err := indexValue(d, data, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter")
	}
}
func unicodeEscape(data []byte, i int) (uint16, bool) {
	if i < 0 || i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
		return 0, false
	}
	var n uint16
	for _, c := range data[i+2 : i+6] {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += uint16(c - '0')
		case c >= 'a' && c <= 'f':
			n += uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			n += uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return n, true
}
func indexString(token []byte, decoded string) (string, error) {
	begin := bytes.IndexByte(token, '"')
	if begin < 0 || len(token) < begin+2 {
		return "", fmt.Errorf("missing JSON string token")
	}
	raw := token[begin+1 : len(token)-1]
	var b strings.Builder
	chunk := 0
	changed := false
	ordinary := func(end int) error {
		part := append([]byte{'"'}, raw[chunk:end]...)
		part = append(part, '"')
		var s string
		if err := json.Unmarshal(part, &s); err != nil {
			return err
		}
		b.WriteString(s)
		return nil
	}
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			i++
			continue
		}
		v, ok := unicodeEscape(raw, i)
		if !ok {
			i += 2
			continue
		}
		if v < 0xd800 || v > 0xdfff {
			i += 6
			continue
		}
		if err := ordinary(i); err != nil {
			return "", err
		}
		changed = true
		if v <= 0xdbff {
			next, ok := unicodeEscape(raw, i+6)
			if !ok {
				return "", fmt.Errorf("incomplete surrogate pair in JSON")
			}
			b.WriteRune(rune(0x10000 + ((uint32(v) & 0x3ff) << 10) + (uint32(next) & 0x3ff)))
			i += 12
		} else {
			b.WriteByte(0xe0 | byte(v>>12))
			b.WriteByte(0x80 | byte(v>>6)&0x3f)
			b.WriteByte(0x80 | byte(v)&0x3f)
			i += 6
		}
		chunk = i
	}
	if !changed {
		return decoded, nil
	}
	if err := ordinary(len(raw)); err != nil {
		return "", err
	}
	return b.String(), nil
}
func records(v any, label string, falseEmpty bool) ([]record, error) {
	if v == nil || (falseEmpty && v == false) {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", label)
	}
	out := make([]record, len(a))
	for i, x := range a {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s entry must be an object", label)
		}
		out[i] = record(m)
	}
	return out, nil
}
func requiredString(m record, key string) (string, error) {
	s, ok := m[key].(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}
func params(v any) ([]string, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("kernel_params must be an array")
	}
	out := make([]string, len(a))
	for i, v := range a {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("kernel_params element must be a string")
		}
		out[i] = s
	}
	return out, nil
}
func truthy(v any) bool { return v != nil && v != false }
func display(v any) string {
	if v == nil {
		return ""
	}
	if n, ok := v.(json.Number); ok {
		if !strings.ContainsAny(string(n), ".eE") {
			if i, ok := new(big.Int).SetString(string(n), 10); ok {
				return i.String()
			}
		}
		f, err := strconv.ParseFloat(string(n), 64)
		if err == nil {
			format := byte('f')
			if a := math.Abs(f); a != 0 && (a < 1e-4 || a >= 1e15) {
				format = 'e'
			}
			s := strconv.FormatFloat(f, format, -1, 64)
			if i := strings.IndexByte(s, 'e'); i >= 0 {
				mant := s[:i]
				if !strings.Contains(mant, ".") {
					mant += ".0"
				}
				return mant + s[i:]
			}
			if !strings.Contains(s, ".") {
				s += ".0"
			}
			return s
		}
	}
	return fmt.Sprint(v)
}
func numberEquals(v any, i *big.Int) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		a, ok := new(big.Int).SetString(s, 10)
		return ok && a.Cmp(i) == 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return false
	}
	r := new(big.Rat).SetFloat64(f)
	return r != nil && r.Cmp(new(big.Rat).SetInt(i)) == 0
}

func strip(s string) string { return strings.Trim(s, "\x00\t\n\v\f\r ") }
func integer(s string) (*big.Int, bool) {
	s = strings.Trim(s, "\t\n\v\f\r ")
	if s == "" {
		return nil, false
	}
	sign := ""
	if s[0] == '+' || s[0] == '-' {
		sign = s[:1]
		s = s[1:]
		if s == "" {
			return nil, false
		}
	}
	base := 10
	digits := s
	if len(s) > 1 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			base, digits = 16, s[2:]
		case 'b', 'B':
			base, digits = 2, s[2:]
		case 'o', 'O':
			base, digits = 8, s[2:]
		case 'd', 'D':
			base, digits = 10, s[2:]
		default:
			base = 8
		}
	}
	if digits == "" || digits[0] == '_' || digits[len(digits)-1] == '_' || strings.Contains(digits, "__") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(sign+strings.ReplaceAll(digits, "_", ""), base)
	return n, ok
}
func toInt(s string) *big.Int {
	s = strings.TrimLeft(s, "\t\n\v\f\r ")
	sign := ""
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		sign, s = s[:1], s[1:]
	}
	end := 0
	for end < len(s) {
		if s[end] >= '0' && s[end] <= '9' {
			end++
			continue
		}
		if s[end] == '_' && end > 0 && end+1 < len(s) && s[end+1] >= '0' && s[end+1] <= '9' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return new(big.Int)
	}
	n, ok := new(big.Int).SetString(sign+strings.ReplaceAll(s[:end], "_", ""), 10)
	if !ok {
		return new(big.Int)
	}
	return n
}
