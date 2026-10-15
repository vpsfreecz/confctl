// Package netboot owns the carrier's private file/index producer. Its injected
// dependencies are test seams; the installed command exposes no path overrides.
package netboot

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Config struct {
	Coreutils, Syslinux, TFTPRoot, HTTPRoot, HostName, HTTPURL string
	Memtest                                                    *Memtest
	ISO                                                        []ISOImage
}
type Memtest struct {
	Package string
	Params  []string
}
type ISOImage struct{ File, Label, Name string }

func ParseConfig(data []byte) (Config, error) {
	var c Config
	v, err := parseJSON(data)
	if err != nil {
		return c, err
	}
	o, ok := v.(object)
	if !ok {
		return c, fmt.Errorf("config must be an object")
	}
	for _, field := range []struct {
		name string
		dst  *string
	}{
		{"coreutils", &c.Coreutils}, {"syslinux", &c.Syslinux}, {"tftpRoot", &c.TFTPRoot}, {"httpRoot", &c.HTTPRoot}, {"hostName", &c.HostName}, {"httpUrl", &c.HTTPURL},
	} {
		s, err := requiredString(o, field.name)
		if err != nil {
			return c, err
		}
		*field.dst = s
	}
	v, ok = o.get("memtest")
	if !ok {
		return c, fmt.Errorf("missing config memtest")
	}
	if v != nil {
		m, ok := v.(object)
		if !ok {
			return c, fmt.Errorf("memtest must be null or an object")
		}
		p, err := requiredString(m, "package")
		if err != nil {
			return c, err
		}
		params, ok := m.get("params")
		if !ok {
			return c, fmt.Errorf("missing memtest params")
		}
		list, err := stringArray(params, "memtest params")
		if err != nil {
			return c, err
		}
		c.Memtest = &Memtest{p, list}
	}
	v, ok = o.get("isoImages")
	if !ok {
		return c, fmt.Errorf("missing config isoImages")
	}
	list, ok := v.([]any)
	if !ok {
		return c, fmt.Errorf("isoImages must be an array")
	}
	for i, item := range list {
		o, ok := item.(object)
		if !ok {
			return c, fmt.Errorf("ISO image must be an object")
		}
		file, err := requiredString(o, "file")
		if err != nil {
			return c, err
		}
		label, err := requiredString(o, "label")
		if err != nil {
			return c, err
		}
		base := filepath.Base(file)
		if label == "" {
			label = base
		}
		c.ISO = append(c.ISO, ISOImage{file, label, fmt.Sprintf("%02d-%s", i, base)})
	}
	// The legacy ruby member is accepted but is not an operational dependency.
	return c, nil
}

func requiredString(o object, key string) (string, error) {
	v, ok := o.get(key)
	if !ok {
		return "", fmt.Errorf("missing %s", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}
func stringArray(v any, label string) ([]string, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", label)
	}
	out := make([]string, len(a))
	for i, v := range a {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s element must be a string", label)
		}
		out[i] = s
	}
	return out, nil
}
func truthy(v any) bool { return v != nil && v != false }
func join(parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	s := parts[0]
	for _, part := range parts[1:] {
		s = strings.TrimRight(s, "/") + "/" + strings.TrimLeft(part, "/")
	}
	return s
}
func strip(s string) string { return strings.Trim(s, "\x00\t\n\v\f\r ") }
