package netboot

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

// This ordered record is only the netboot JSON owner, not a shared serializer.
type member struct {
	key   string
	value any
}
type object []member

func (o object) get(key string) (any, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}
func parseJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8 JSON")
	}
	if err := admitJSON(data); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := readValue(d, data)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("extra JSON data")
	}
	return v, nil
}

// The selected source JSON provider limits all containers to depth 100 and
// requires a second Unicode escape after a high surrogate. Its second escape
// need not be a low surrogate; syntax/order/numbers remain encoding/json's job.
func admitJSON(data []byte) error {
	depth := 0
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			if c == '"' {
				inString = false
				continue
			}
			if c != '\\' {
				continue
			}
			if i+1 >= len(data) {
				continue
			}
			if data[i+1] == 'u' {
				value, ok := unicodeEscape(data, i)
				if ok && value >= 0xd800 && value <= 0xdbff {
					if _, ok := unicodeEscape(data, i+6); !ok {
						return fmt.Errorf("incomplete surrogate pair in JSON")
					}
					i += 12 - 1
					continue
				}
				if ok {
					i += 6 - 1
					continue
				}
			}
			i++
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > 100 {
				return fmt.Errorf("JSON nesting exceeds 100")
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}
func unicodeEscape(data []byte, at int) (uint16, bool) {
	if at < 0 || at+6 > len(data) || data[at] != '\\' || data[at+1] != 'u' {
		return 0, false
	}
	var n uint16
	for _, c := range data[at+2 : at+6] {
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
func readValue(d *json.Decoder, data []byte) (any, error) {
	start := d.InputOffset()
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		if s, ok := t.(string); ok {
			return preserveSurrogates(data[start:d.InputOffset()], s)
		}
		return t, nil
	}
	switch delim {
	case '{':
		o := object{}
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
			key, err = preserveSurrogates(data[start:d.InputOffset()], key)
			if err != nil {
				return nil, err
			}
			v, err := readValue(d, data)
			if err != nil {
				return nil, err
			}
			replaced := false
			for i := range o {
				if o[i].key == key {
					o[i].value = v
					replaced = true
					break
				}
			}
			if !replaced {
				o = append(o, member{key, v})
			}
		}
		_, err := d.Token()
		return o, err
	case '[':
		a := []any{}
		for d.More() {
			v, err := readValue(d, data)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := d.Token()
		return a, err
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter")
	}
}

// Restore only the selected provider's surrogate-bearing string tokens using
// their raw coordinates. Ordinary string chunks still use encoding/json;
// literal/escaped U+FFFD is never treated as a surrogate marker.
func preserveSurrogates(token []byte, decoded string) (string, error) {
	begin := bytes.IndexByte(token, '"')
	if begin < 0 || len(token) < begin+2 {
		return "", fmt.Errorf("missing JSON string token")
	}
	raw := token[begin+1 : len(token)-1]
	var b strings.Builder
	chunk := 0
	changed := false
	appendChunk := func(end int) error {
		part := make([]byte, 0, end-chunk+2)
		part = append(part, '"')
		part = append(part, raw[chunk:end]...)
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
		value, ok := unicodeEscape(raw, i)
		if !ok {
			i += 2
			continue
		}
		if value < 0xd800 || value > 0xdfff {
			i += 6
			continue
		}
		if err := appendChunk(i); err != nil {
			return "", err
		}
		changed = true
		if value <= 0xdbff {
			next, ok := unicodeEscape(raw, i+6)
			if !ok {
				return "", fmt.Errorf("incomplete surrogate pair in JSON")
			}
			b.WriteRune(rune(0x10000 + ((uint32(value) & 0x3ff) << 10) + (uint32(next) & 0x3ff)))
			i += 12
		} else {
			b.WriteByte(0xe0 | byte(value>>12))
			b.WriteByte(0x80 | byte(value>>6)&0x3f)
			b.WriteByte(0x80 | byte(value)&0x3f)
			i += 6
		}
		chunk = i
	}
	if !changed {
		return decoded, nil
	}
	if err := appendChunk(len(raw)); err != nil {
		return "", err
	}
	return b.String(), nil
}

func numberText(s string) (string, error) {
	if !strings.ContainsAny(s, ".eE") {
		n, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return "", fmt.Errorf("invalid integer")
		}
		return n.String(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", err
	}
	// Ruby JSON uses Float's shortest representation, including .0 and an
	// padded exponent. Preserve exact integers independently of this path.
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-4 || abs >= 1e15) {
		format = 'e'
	}
	v := strconv.FormatFloat(f, format, -1, 64)
	if i := strings.IndexByte(v, 'e'); i >= 0 {
		mant, exp := v[:i], v[i+1:]
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + "e" + exp, nil
	}
	if !strings.Contains(v, ".") {
		v += ".0"
	}
	return v, nil
}
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 32 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
func pretty(v any) ([]byte, error) {
	var b strings.Builder
	if err := writeJSON(&b, v, 0); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
func writeJSON(b *strings.Builder, v any, depth int) error {
	indent := func(n int) string { return strings.Repeat("  ", n) }
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case string:
		if !utf8.ValidString(v) {
			return fmt.Errorf("invalid UTF-8 string in netboot index output")
		}
		b.WriteString(quote(v))
	case json.Number:
		s, err := numberText(string(v))
		if err != nil {
			return err
		}
		b.WriteString(s)
	case *big.Int:
		b.WriteString(v.String())
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case object:
		// Count containers at this actual wrapped-output access, including an
		// empty deepest container. Scalar values at depth 100 remain supported.
		if depth >= 100 {
			return fmt.Errorf("JSON output nesting exceeds 100")
		}
		b.WriteByte('{')
		if len(v) > 0 {
			b.WriteByte('\n')
			for i, m := range v {
				if !utf8.ValidString(m.key) {
					return fmt.Errorf("invalid UTF-8 key in netboot index output")
				}
				b.WriteString(indent(depth+1) + quote(m.key) + ": ")
				if err := writeJSON(b, m.value, depth+1); err != nil {
					return err
				}
				if i+1 < len(v) {
					b.WriteByte(',')
				}
				b.WriteByte('\n')
			}
			b.WriteString(indent(depth))
		}
		b.WriteByte('}')
	case []any:
		if depth >= 100 {
			return fmt.Errorf("JSON output nesting exceeds 100")
		}
		b.WriteByte('[')
		if len(v) > 0 {
			b.WriteByte('\n')
			for i, x := range v {
				b.WriteString(indent(depth + 1))
				if err := writeJSON(b, x, depth+1); err != nil {
					return err
				}
				if i+1 < len(v) {
					b.WriteByte(',')
				}
				b.WriteByte('\n')
			}
			b.WriteString(indent(depth))
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("unsupported index value %T", v)
	}
	return nil
}
func stringsValue(a []string) []any {
	out := make([]any, len(a))
	for i, s := range a {
		out[i] = s
	}
	return out
}
func generationRecord(g *Generation) object {
	boot := object{}
	for _, b := range g.Boot {
		boot = append(boot, member{b.Name, b.URL})
	}
	variants := []any{}
	for _, v := range g.Variants {
		variants = append(variants, object{{"name", v.Name}, {"label", v.Label}, {"kernel_params", stringsValue(v.Params)}})
	}
	o := object{{"url", g.URL}, {"store_path", g.Store}, {"generation", g.ID}, {"time", g.Time.Unix()}, {"time_s", g.TimeS}, {"current", g.Current}, {"toplevel", g.Toplevel}, {"version", g.Version}, {"revision", g.Revision}, {"shortrev", g.ShortRev}, {"macs", g.MACs}, {"kernel_version", g.KernelVersion}, {"kernel_params", stringsValue(g.Params)}, {"boot_files", boot}, {"variants", variants}}
	if truthy(g.Inputs) {
		o = append(o, member{"inputs_info", g.Inputs})
	}
	return o
}
func machineRecord(m *Machine) object {
	gens := []any{}
	for _, g := range m.Generations {
		gens = append(gens, generationRecord(g))
	}
	return object{{"url", m.URL}, {"name", m.Name}, {"spin", m.Spin}, {"fqdn", m.FQDN}, {"label", m.Label}, {"generations", gens}}
}
func BuildHTTP(c Config, root string, machines []*Machine, d Dependencies) error {
	write := func(path string, v any) error {
		b, err := pretty(v)
		if err != nil {
			return err
		}
		return writeTo(root, path, b, d)
	}
	for _, m := range machines {
		for _, g := range m.Generations {
			path := join(m.FQDN, g.ID.String())
			if err := d.FS.Mkdir(join(root, path)); err != nil {
				return err
			}
			for _, b := range g.Boot {
				if err := d.FS.Symlink(b.Path, join(root, path, b.Name)); err != nil {
					return err
				}
			}
			if err := write(join(path, "generation.json"), generationRecord(g)); err != nil {
				return err
			}
			if err := writeTo(root, join(path, "kernel-params"), []byte(strings.Join(g.Params, " ")), d); err != nil {
				return err
			}
			if g.Current {
				if err := d.FS.Symlink(g.ID.String(), join(root, m.FQDN, "current")); err != nil {
					return err
				}
			}
		}
		if err := write(join(m.FQDN, "machine.json"), machineRecord(m)); err != nil {
			return err
		}
	}
	a := []any{}
	for _, m := range machines {
		a = append(a, machineRecord(m))
	}
	return write("machines.json", object{{"machines", a}})
}
