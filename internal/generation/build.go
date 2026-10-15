// Package generation owns read-only local flake generation state.
// It never repairs records, current links or GC roots.
package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Object retains JSON member order, duplicate-key replacement order and exact
// number tokens, including numbers nested inside objects and arrays.
type Object []Member
type Member struct {
	Name  string
	Value any
}

func (o Object) Lookup(name string) (any, bool) {
	for _, m := range o {
		if m.Name == name {
			return m.Value, true
		}
	}
	return nil, false
}
func (o Object) value(name string) any { v, _ := o.Lookup(name); return v }
func truth(v any) bool                 { return v != nil && v != false }

// Keep encoding/json as the syntax/token owner, but preserve the finite
// json2.21.2 string admission boundary it otherwise replaces with U+FFFD.
// Raw token slices also distinguish an escaped backslash from a Unicode escape.
func recordToken(d *json.Decoder, source []byte) (any, error) {
	start := d.InputOffset()
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	decoded, ok := token.(string)
	if !ok {
		return token, nil
	}
	raw := source[start:d.InputOffset()]
	raw = raw[bytes.IndexByte(raw, '"'):]
	var preserved strings.Builder
	last := 1
	for i := 1; i < len(raw)-1; {
		if raw[i] != '\\' {
			i++
			continue
		}
		if raw[i+1] != 'u' {
			i += 2
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
		if code >= 0xd800 && code <= 0xdbff {
			if i+12 > len(raw)-1 || string(raw[i+6:i+8]) != `\u` {
				return nil, fmt.Errorf("incomplete surrogate pair")
			}
			low, _ := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
			if low < 0xdc00 || low > 0xdfff {
				return nil, fmt.Errorf("invalid surrogate pair")
			}
			i += 12
			continue
		}
		if code >= 0xdc00 && code <= 0xdfff {
			// Ruby admits a standalone low surrogate as its three raw bytes.
			// Decode only the normal segments with encoding/json.
			var segment string
			quoted := append([]byte{'"'}, raw[last:i]...)
			quoted = append(quoted, '"')
			if err := json.Unmarshal(quoted, &segment); err != nil {
				return nil, err
			}
			preserved.WriteString(segment)
			preserved.WriteByte(byte(0xe0 | code>>12))
			preserved.WriteByte(byte(0x80 | code>>6&0x3f))
			preserved.WriteByte(byte(0x80 | code&0x3f))
			last = i + 6
		}
		i += 6
	}
	if last == 1 {
		return decoded, nil
	}
	var suffix string
	quoted := append([]byte{'"'}, raw[last:]...)
	if err := json.Unmarshal(quoted, &suffix); err != nil {
		return nil, err
	}
	preserved.WriteString(suffix)
	return preserved.String(), nil
}

func decodeValue(d *json.Decoder, source []byte, nesting int) (any, error) {
	t, err := recordToken(d, source)
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := Object{}
		indexes := map[string]int{}
		if d.More() {
			nesting++
			if nesting > 100 {
				return nil, fmt.Errorf("nesting of %d is too deep", nesting)
			}
		}
		for d.More() {
			key, err := recordToken(d, source)
			if err != nil {
				return nil, err
			}
			value, err := decodeValue(d, source, nesting)
			if err != nil {
				return nil, err
			}
			name := key.(string)
			if i, ok := indexes[name]; ok {
				o[i].Value = value
			} else {
				indexes[name] = len(o)
				o = append(o, Member{name, value})
			}
		}
		_, err = d.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		if d.More() {
			nesting++
			if nesting > 100 {
				return nil, fmt.Errorf("nesting of %d is too deep", nesting)
			}
		}
		for d.More() {
			v, err := decodeValue(d, source, nesting)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	default:
		return t, nil
	}
}
func decodeRecord(bytes []byte) (any, error) {
	d := json.NewDecoder(strings.NewReader(string(bytes)))
	d.UseNumber()
	v, err := decodeValue(d, bytes, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected data after JSON value")
		}
		return nil, err
	}
	return v, nil
}

type RecordError struct {
	Path, Reason string
	Unsupported  bool
}

func (e *RecordError) Error() string { return e.Path + ": " + e.Reason }

type Build struct {
	Host, Name, Mode, Dir string
	Date                  Timestamp
	Toplevel              string
	AutoRollback          *string
	Inputs                Object
	InputsInfo            Object
	Raw                   Object
	Current               bool
	KernelVersion         *string
}

// LoadBuild implements the Ruby Build#load payload contract. JSON and OS
// diagnostics retain their native causes; no parse failure causes a write.
func LoadBuild(root, host, name string, options Options) (*Build, error) {
	real, err := configurationPath(root)
	if err != nil {
		return nil, err
	}
	root = real
	dir := root + "/.confctl/generations/" + SafeHost(host) + "/" + name
	path := dir + "/generation.json"
	fail := func(reason string) (*Build, error) { return nil, &RecordError{Path: path, Reason: reason} }
	bytes, err := os.ReadFile(path)
	if err != nil {
		return fail(err.Error())
	}
	value, err := decodeRecord(bytes)
	if err != nil {
		return fail(err.Error())
	}
	cfg, ok := value.(Object)
	if !ok {
		return fail("expected a JSON object")
	}
	mode := cfg.value("mode")
	if mode != "flakes" {
		return nil, &RecordError{Path: path, Unsupported: true, Reason: fmt.Sprintf("unsupported generation mode %s; only explicit flakes mode is supported", inspect(mode))}
	}
	top, exists := cfg.Lookup("toplevel")
	if !exists {
		return fail("key not found: \"toplevel\"")
	}
	toplevel, ok := top.(string)
	if !ok || toplevel == "" {
		return fail("invalid flake generation payload")
	}
	rollback := cfg.value("auto_rollback")
	var auto *string
	if rollback != nil {
		s, ok := rollback.(string)
		if !ok {
			return fail("invalid flake generation payload")
		}
		auto = &s
	}
	rawInputs, exists := cfg.Lookup("inputs")
	if !exists {
		rawInputs = Object{}
	}
	inputs, ok := rawInputs.(Object)
	if !ok {
		return fail("invalid flake generation payload")
	}
	for _, m := range inputs {
		if _, ok := m.Value.(string); !ok {
			return fail("invalid flake generation payload")
		}
	}
	info := cfg.value("inputs_info")
	if !truth(info) {
		info = cfg.value("inputsInfo")
	}
	if !truth(info) {
		info = Object{}
	}
	inputsInfo, ok := info.(Object)
	if !ok {
		return fail("invalid flake generation payload")
	}
	date, ok := cfg.value("date").(string)
	if !ok {
		return fail("date must be a string")
	}
	parsed, err := ParseISO8601(date, options.Location)
	if err != nil {
		return fail(err.Error())
	}
	// The Ruby CLI reads relative store paths from configuration CWD. Keep that
	// context explicit without changing this process's working directory.
	kernelPath := toplevel
	if !filepath.IsAbs(kernelPath) {
		kernelPath = root + "/" + kernelPath
	}
	kernel, err := kernelVersion(kernelPath)
	if err != nil {
		return fail(err.Error())
	}
	return &Build{Host: host, Name: name, Mode: "flakes", Dir: dir, Date: parsed, Toplevel: toplevel, AutoRollback: auto, Inputs: inputs, InputsInfo: inputsInfo, Raw: cfg, KernelVersion: kernel}, nil
}

func inspect(v any) string {
	if v == nil {
		return "nil"
	}
	if s, ok := v.(string); ok {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return fmt.Sprint(v)
}

var kernelPattern = regexp.MustCompile(`^/nix/store/[^-]+-linux-([^/]+)`)

func kernelVersion(toplevel string) (*string, error) {
	for _, name := range []string{"kernel", "bzImage"} {
		link, err := os.Readlink(toplevel + "/" + name)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EINVAL) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if match := kernelPattern.FindStringSubmatch(link); match != nil {
			version := match[1]
			return &version, nil
		}
	}
	return nil, nil
}
