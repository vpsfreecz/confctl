package harness

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Rules are an explicit field allowlist. Bytes in state files are never silently
// dropped. Parallel starts can vary; semantic request order within each host
// remains observable. Wall/CPU/RSS remain in raw evidence, not parity assertions.
type Normalization struct {
	IndependentSSH   []SSHFlow `json:"independent_ssh,omitempty"`
	LogFields        []string  `json:"log_fields"`
	RunRootFields    []string  `json:"run_root_fields"`
	RootFields       []string  `json:"root_fields"`
	VolatileFields   []string  `json:"volatile_fields"`
	ConcurrentEvents bool      `json:"concurrent_events"`
}

var rootAllowed = map[string]bool{"stdout": true, "stderr": true, "events.cwd": true, "events.argv": true, "events.stdin": true, "events.stdout": true, "events.stderr": true, "events.env.PWD": true, "events.env.HOME": true, "events.env.TMPDIR": true, "events.env.COMPAT_CASE": true}
var volatileAllowed = map[string]bool{"events.pid": true, "events.start_ns": true, "events.end_ns": true, "wall_ns": true, "user_ns": true, "system_ns": true, "max_rss_kb": true}

func Normalize(o Observation, n Normalization, root string) (Observation, error) {
	for _, f := range n.RootFields {
		if !rootAllowed[f] {
			return o, fmt.Errorf("root normalization not allowed: %s", f)
		}
	}
	for _, f := range n.VolatileFields {
		if !volatileAllowed[f] {
			return o, fmt.Errorf("volatile normalization not allowed: %s", f)
		}
	}
	if len(n.IndependentSSH) > 0 && len(o.Events) > 0 {
		if e := AssertSSHFlows(o.Events, n.IndependentSSH); e != nil {
			return o, e
		}
		if e := AssertCausal(o.Events, []Constraint{{Before: Selector{Tool: "nix"}, After: Selector{Tool: "ssh"}}}); e != nil {
			return o, e
		}
	}
	b, _ := json.Marshal(o)
	var v Observation
	_ = json.Unmarshal(b, &v)
	if o.ConfigRoot != "" {
		root = o.ConfigRoot
	}
	captureRoot := o.RunRoot
	if captureRoot == "" {
		captureRoot = filepath.Dir(root)
	}
	replace := func(s string) string { return strings.ReplaceAll(s, root, "${ROOT}") }
	for _, f := range n.RunRootFields {
		if f != "events.env.HOME" && f != "events.env.TMPDIR" && f != "events.env.COMPAT_CASE" {
			return o, fmt.Errorf("run-root normalization not allowed: %s", f)
		}
		k := strings.TrimPrefix(f, "events.env.")
		for i := range v.Events {
			base := captureRoot
			if k == "HOME" {
				base = filepath.Dir(root)
			}
			v.Events[i].Env[k] = strings.ReplaceAll(v.Events[i].Env[k], base, "${RUN_ROOT}")
		}
	}
	for _, f := range n.RootFields {
		switch f {
		case "stdout":
			v.Stdout = replace(v.Stdout)
		case "stderr":
			v.Stderr = replace(v.Stderr)
		case "events.cwd":
			for i := range v.Events {
				v.Events[i].CWD = replace(v.Events[i].CWD)
			}
		case "events.argv":
			for i := range v.Events {
				for j := range v.Events[i].Argv {
					v.Events[i].Argv[j] = replace(v.Events[i].Argv[j])
				}
			}
		case "events.stdout":
			for i := range v.Events {
				v.Events[i].Stdout = replace(v.Events[i].Stdout)
			}
		case "events.stderr":
			for i := range v.Events {
				v.Events[i].Stderr = replace(v.Events[i].Stderr)
			}
		case "events.stdin":
			for i := range v.Events {
				v.Events[i].Stdin = replace(v.Events[i].Stdin)
			}
		case "events.env.PWD", "events.env.HOME", "events.env.TMPDIR", "events.env.COMPAT_CASE":
			k := strings.TrimPrefix(f, "events.env.")
			for i := range v.Events {
				v.Events[i].Env[k] = replace(v.Events[i].Env[k])
			}
		}
	}
	for _, f := range n.VolatileFields {
		switch f {
		case "events.pid":
			for i := range v.Events {
				v.Events[i].PID = 0
			}
		case "events.start_ns":
			for i := range v.Events {
				v.Events[i].Start = 0
			}
		case "events.end_ns":
			for i := range v.Events {
				v.Events[i].End = 0
			}
		case "wall_ns":
			v.WallNS = 0
		case "user_ns":
			v.UserNS = 0
		case "system_ns":
			v.SystemNS = 0
		case "max_rss_kb":
			v.MaxRSSKB = 0
		}
	}
	if err := normalizeLogs(&v, n.LogFields, root); err != nil {
		return v, err
	}
	if len(n.IndependentSSH) > 0 && len(v.Events) > 0 {
		// Nix chronology stays literal; only the fully validated SSH suffix is ordered.
		first := len(v.Events)
		for i, e := range v.Events {
			if e.Tool == "ssh" {
				first = i
				break
			}
		}
		for _, e := range v.Events[first:] {
			if e.Tool != "ssh" {
				return v, fmt.Errorf("non-SSH event after SSH phase")
			}
		}
		sort.SliceStable(v.Events[first:], func(i, j int) bool {
			a, b := v.Events[first+i], v.Events[first+j]
			ka, _ := json.Marshal([]any{a.Host, a.Argv, a.Stdin, a.Occurrence})
			kb, _ := json.Marshal([]any{b.Host, b.Argv, b.Stdin, b.Occurrence})
			return string(ka) < string(kb)
		})
	} else if n.ConcurrentEvents {
		sort.SliceStable(v.Events, func(i, j int) bool { return v.Events[i].Host < v.Events[j].Host })
	}
	return v, nil
}
func Equal(a, b Observation) error {
	a.ConfigRoot = ""
	b.ConfigRoot = ""
	a.RunRoot = ""
	b.RunRoot = ""
	a.SourceRevision = ""
	b.SourceRevision = ""
	a.Executable = ""
	b.Executable = ""
	a.ExecutableSHA256 = ""
	b.ExecutableSHA256 = ""
	if reflect.DeepEqual(a, b) {
		return nil
	}
	// Return useful field names without hiding differing raw evidence.
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	typ := av.Type()
	var fields []string
	for i := 0; i < av.NumField(); i++ {
		if !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
			fields = append(fields, typ.Field(i).Name)
		}
	}
	return fmt.Errorf("differential mismatch: %s", strings.Join(fields, ", "))
}
