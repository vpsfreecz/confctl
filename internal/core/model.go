package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Machine struct {
	Name         string         `json:"name"`
	Key          string         `json:"key"`
	Spin         string         `json:"spin"`
	Managed      bool           `json:"managed"`
	ClusterName  string         `json:"cluster_name"`
	CarrierName  *string        `json:"carrier_name"`
	CarriedAlias string         `json:"carried_alias"`
	Target       Target         `json:"target"`
	Profile      string         `json:"profile"`
	Attributes   map[string]any `json:"attributes"`
	rawMeta      json.RawMessage
}
type Target struct {
	Host      *string `json:"host"`
	Port      int     `json:"port"`
	Localhost bool    `json:"localhost"`
}

func (m Machine) Attr(k string) any {
	if strings.Contains(k, ".") {
		var v any = m.Attributes
		for _, p := range strings.Split(k, ".") {
			h, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = h[p]
		}
		return m.displayAttr(k, v)
	}
	switch k {
	case "name":
		return m.Name
	case "key", "machineKey", "flakeKey":
		return m.Key
	case "checks":
		return m.Checks()
	}
	return m.displayAttr(k, m.Attributes[k])
}
func (m Machine) Checks() int {

	n := 0
	h, _ := m.Attributes["healthChecks"].(map[string]any)
	for typ, v := range h {
		switch typ {
		case "builderCommands", "machineCommands":
			if typ == "machineCommands" && (m.CarrierName != nil || m.Target.Host == nil) {
				continue
			}
			a, _ := v.([]any)
			n += len(a)
		case "systemd":
			o, _ := v.(map[string]any)
			if o["enable"] != true || m.Spin != "nixos" {
				continue
			}
			a, _ := o["systemProperties"].([]any)
			if len(a) > 0 {
				n++
			}
			b, _ := o["unitProperties"].(map[string]any)
			n += len(b)
		}
	}
	return n
}
func demod(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "_module")
		for _, v := range x {
			demod(v)
		}
	case []any:
		for _, v := range x {
			demod(v)
		}
	}
}
func DecodeMachines(b []byte) ([]Machine, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("machine output is not an object")
	}
	var out []Machine
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return nil, e
		}
		var raw json.RawMessage
		if e = d.Decode(&raw); e != nil {
			return nil, e
		}
		var rawFields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &rawFields)
		var v map[string]any
		numberDecoder := json.NewDecoder(bytes.NewReader(raw))
		numberDecoder.UseNumber()
		if e = numberDecoder.Decode(&v); e != nil {
			return nil, e
		}
		demod(v)
		a, ok := v["metaConfig"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: missing metaConfig", k)
		}
		m := Machine{rawMeta: rawFields["metaConfig"], Attributes: a, Name: str(v["name"]), ClusterName: str(v["clusterName"]), Key: str(v["key"]), Spin: str(a["spin"]), Managed: truth(a["managed"])}
		if m.Key == "" {
			m.Key = str(v["machineKey"])
		}
		if m.Key == "" {
			m.Key = str(v["flakeKey"])
		}
		if s, ok := v["carrier"].(string); ok {
			m.CarrierName = &s
		}
		m.CarriedAlias = str(v["alias"])
		if m.CarriedAlias == "" {
			m.CarriedAlias = m.ClusterName
		}
		h, _ := a["host"].(map[string]any)
		target, exists := h["target"]
		if !exists {
			target = m.Name
		}
		if s, ok := target.(string); ok {
			m.Target.Host = &s
		}
		m.Target.Port = 22
		if v, ok := h["port"]; ok {
			n, _ := strconv.Atoi(str(v))
			m.Target.Port = n
		}
		m.Target.Localhost = m.Target.Host != nil && *m.Target.Host == "localhost"
		m.Profile = "/nix/var/nix/profiles/system"
		if m.CarrierName != nil {
			m.Profile = "/nix/var/nix/profiles/confctl-" + strings.ReplaceAll(m.CarriedAlias, "/", ":")
		}
		out = append(out, m)
	}
	_, e = d.Token()
	return out, e
}
func str(v any) string {
	switch x := v.(type) {
	case rubyValue:
		return string(x)
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return rubyNumber(x)
	case float64:
		return rubyFloat(x)
	default:
		return rubyInspect(x)
	}
}
func truth(v any) bool { return v != nil && v != false }
func rubyInspect(v any) string {
	switch x := v.(type) {
	case json.Number:
		return rubyNumber(x)
	case float64:
		return rubyFloat(x)
	case nil:
		return "nil"
	case string:
		return strconv.Quote(x)
	case []any:
		a := make([]string, len(x))
		for i, v := range x {
			a[i] = rubyInspect(v)
		}
		return "[" + strings.Join(a, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		a := []string{}
		for _, k := range keys {
			a = append(a, strconv.Quote(k)+" => "+rubyInspect(x[k]))
		}
		return "{" + strings.Join(a, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}
func Select(all []Machine, pattern *string, attrs, tags []string, managed string) ([]Machine, error) {
	filters := []func(Machine) bool{}
	for _, f := range attrs {
		k, v, neq := "", "", false
		if i := strings.Index(f, "!="); i >= 0 {
			k, v, neq = f[:i], f[i+2:], true
		} else if i := strings.IndexByte(f, '='); i >= 0 {
			k, v = f[:i], f[i+1:]
		} else {
			return nil, fmt.Errorf("Invalid filter 'false'")
		}
		filters = append(filters, func(m Machine) bool { eq := str(m.Attr(k)) == v; return eq != neq })
	}
	pass := func(m Machine) bool {
		for _, f := range filters {
			if !f(m) {
				return false
			}
		}
		for _, t := range tags {
			neg := strings.HasPrefix(t, "^")
			name := strings.TrimPrefix(t, "^")
			a, _ := m.Attributes["tags"].([]any)
			found := false
			for _, v := range a {
				if v == name {
					found = true
				}
			}
			if found == neg {
				return false
			}
		}
		return true
	}
	out := []Machine{}
	byName := false
	if pattern != nil {
		for _, m := range all {
			if m.Name == *pattern {
				byName = true
				break
			}
		}
	}
	keys := []Machine{}
	if pattern != nil && !byName {
		for _, m := range all {
			if m.Key == *pattern && pass(m) {
				keys = append(keys, m)
			}
		}
	}
	for _, m := range all {
		if !pass(m) {
			continue
		}
		if pattern != nil {
			if byName {
				if m.Name != *pattern {
					continue
				}
			} else if len(keys) > 0 {
				if m.Key != *pattern {
					continue
				}
			} else if !Match(*pattern, m.Name) {
				continue
			}
		}
		switch managed {
		case "all", "a", "":
		case "no", "n":
			if m.Managed {
				continue
			}
		default:
			if !m.Managed {
				continue
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// Match implements File.fnmatch?(..., FNM_EXTGLOB), with no FNM_PATHNAME:
// stars span '/', and dot protection applies only at the beginning of the name.
func Match(pattern, name string) bool {
	for _, p := range expandBraces(pattern) {
		if matchRunes([]rune(p), []rune(name), 0, 0, map[[2]int]bool{}, map[[2]int]bool{}) {
			return true
		}
	}
	return false
}
func expandBraces(s string) []string {
	start := -1
	depth := 0
	escaped := false
	for i, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if r == '}' && depth > 0 {
			depth--
			if depth == 0 {
				body := s[start+1 : i]
				parts := []string{}
				last, d := 0, 0
				esc := false
				for j, r := range body {
					if esc {
						esc = false
						continue
					}
					if r == '\\' {
						esc = true
						continue
					}
					if r == '{' {
						d++
					}
					if r == '}' {
						d--
					}
					if r == ',' && d == 0 {
						parts = append(parts, body[last:j])
						last = j + 1
					}
				}
				parts = append(parts, body[last:])
				out := []string{}
				for _, p := range parts {
					out = append(out, expandBraces(s[:start]+p+s[i+1:])...)
				}
				return out
			}
		}
	}
	if depth != 0 {
		return nil
	}
	return []string{s}
}
func matchRunes(p, n []rune, i, j int, memo, seen map[[2]int]bool) bool {
	key := [2]int{i, j}
	if seen[key] {
		return memo[key]
	}
	seen[key] = true
	result := false
	defer func() { memo[key] = result }()
	if i == len(p) {
		result = j == len(n)
		return result
	}
	if j == 0 && len(n) > 0 && n[0] == '.' && p[i] != '.' && (p[i] != '\\' || i+1 == len(p) || p[i+1] != '.') {
		return false
	}
	switch p[i] {
	case '*':
		result = matchRunes(p, n, i+1, j, memo, seen) || (j < len(n) && matchRunes(p, n, i, j+1, memo, seen))
	case '?':
		result = j < len(n) && matchRunes(p, n, i+1, j+1, memo, seen)
	case '\\':
		if i+1 < len(p) {
			result = j < len(n) && p[i+1] == n[j] && matchRunes(p, n, i+2, j+1, memo, seen)
		}
	case '[':
		end := i + 1
		for end < len(p) && p[end] != ']' {
			if p[end] == '\\' && end+1 < len(p) {
				end++
			}
			end++
		}
		if end == len(p) || j == len(n) {
			return false
		}
		a := p[i+1 : end]
		neg := len(a) > 0 && (a[0] == '!' || a[0] == '^')
		if neg {
			a = a[1:]
		}
		hit := false
		for k := 0; k < len(a); k++ {
			v := a[k]
			if v == '\\' && k+1 < len(a) {
				k++
				v = a[k]
			}
			if k+2 < len(a) && a[k+1] == '-' {
				hi := a[k+2]
				k += 2
				if n[j] >= v && n[j] <= hi {
					hit = true
				}
			} else if n[j] == v {
				hit = true
			}
		}
		result = hit != neg && matchRunes(p, n, end+1, j+1, memo, seen)
	default:
		result = j < len(n) && p[i] == n[j] && matchRunes(p, n, i+1, j+1, memo, seen)
	}
	return result
}
func width(s string) int { return utf8.RuneCountInString(stripANSI(s)) }
func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		if r == 27 {
			esc = true
			continue
		}
		if esc {
			if r == 'm' {
				esc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
func Table(rows []map[string]any, cols []string, header bool) string {
	if len(cols) == 0 {
		return ""
	}
	widths := make([]int, len(cols))
	vals := make([][]string, len(rows))
	for i, c := range cols {
		widths[i] = width(strings.ToUpper(c)) + 1
	}
	for i, row := range rows {
		vals[i] = make([]string, len(cols))
		for j, c := range cols {
			v := row[c]
			s := str(v)
			if !truth(v) || s == "" {
				s = "-"
			}
			vals[i][j] = s
			if width(s)+1 > widths[j] {
				widths[j] = width(s) + 1
			}
		}
	}
	var b strings.Builder
	line := func(a []string) {
		for i, s := range a {
			b.WriteString(s)
			if i < len(a)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-width(s)+2))
			}
		}
		b.WriteByte('\n')
	}
	if header {
		a := make([]string, len(cols))
		for i, c := range cols {
			a[i] = strings.ToUpper(c)
		}
		line(a)
	}
	for _, a := range vals {
		line(a)
	}
	return b.String()
}

type rubyValue string

func (m Machine) displayAttr(path string, value any) any {
	switch value.(type) {
	case map[string]any, []any:
		raw := m.rawMeta
		for _, k := range strings.Split(path, ".") {
			var h map[string]json.RawMessage
			if json.Unmarshal(raw, &h) != nil {
				return value
			}
			raw = h[k]
		}
		if len(raw) > 0 {
			return rubyValue(rubyRaw(raw))
		}
	}
	return value
}
func rubyRaw(raw json.RawMessage) string {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	token, err := d.Token()
	if err != nil {
		return "nil"
	}
	switch token {
	case json.Delim('{'):
		parts := []string{}
		for d.More() {
			k, _ := d.Token()
			var v json.RawMessage
			_ = d.Decode(&v)
			parts = append(parts, strconv.Quote(k.(string))+" => "+rubyRaw(v))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case json.Delim('['):
		parts := []string{}
		for d.More() {
			var v json.RawMessage
			_ = d.Decode(&v)
			parts = append(parts, rubyRaw(v))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return rubyInspect(token)
	}
}

// JSON integer tokens remain exact; a decimal/exponent denotes a Ruby Float.
func rubyNumber(n json.Number) string {
	if !strings.ContainsAny(string(n), ".eE") {
		return string(n)
	}
	f, err := n.Float64()
	if err != nil {
		return string(n)
	}
	return rubyFloat(f)
}
func rubyFloat(f float64) string {
	format := byte('f')
	a := math.Abs(f)
	if a != 0 && (a < 0.0001 || a >= 1e15) {
		format = 'e'
	}
	s := strconv.FormatFloat(f, format, -1, 64)
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		if !strings.Contains(s[:i], ".") {
			s = s[:i] + ".0" + s[i:]
		}
	} else if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
