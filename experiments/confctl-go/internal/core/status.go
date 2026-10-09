package core

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Status struct {
	Machine     Machine
	Uptime      *float64
	Generations *int
	Target      map[string]map[string]any
	Deployed    map[string]map[string]any
	Err         error
}

func normalizeInputs(v any) map[string]map[string]any {
	h, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for role, v := range h {
		x := map[string]any{}
		switch info := v.(type) {
		case string:
			x["rev"] = info
			x["shortRev"] = short(info)
		case map[string]any:
			for _, k := range []string{"input", "url", "rev", "shortRev", "lastModified"} {
				if v, ok := info[k]; ok {
					x[k] = v
				}
			}
			if v, ok := info["short_rev"]; ok {
				x["shortRev"] = v
			}
			if x["shortRev"] == nil && x["rev"] != nil {
				x["shortRev"] = short(str(x["rev"]))
			}
		}
		out[role] = x
	}
	return out
}
func short(s string) string {
	r := []rune(s)
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}
func hostScript(profile string) string {
	return fmt.Sprintf("realpath %s\n\nfor generation in `ls -d -1 %s-*-link` ; do\n  echo -n \"$generation;\"\n  echo -n \"$(readlink $generation);\"\n  echo -n \"$(stat --format=%%Y $generation);\"\n\n  for kernel_file in kernel bzImage ; do\n    [ -h \"$generation/$kernel_file\" ] && echo -n $(readlink \"$generation/$kernel_file\")\n  done\n\n  echo\ndone\n", profile, profile)
}
func (e *Engine) query(st *Status, all []Machine) {
	m := st.Machine
	if m.CarrierName != nil {
		found := false
		for _, c := range all {
			if c.Name == *m.CarrierName {
				m = c
				found = true
				break
			}
		}
		if !found {
			st.Err = fmt.Errorf("Carrier %s not found in machine list", *st.Machine.CarrierName)
			return
		}
	}
	run := func(a []string, input []byte) (Result, error) {
		args, err := SSHArgs(m, a)
		if err != nil {
			return Result{}, err
		}
		return e.Process(args, input)
	}
	r, err := run([]string{"cat", "/proc/uptime"}, nil)
	if err != nil {
		st.Err = err
		return
	}
	if r.ExitCode != 0 {
		return
	}
	parts := strings.Fields(string(r.Stdout))
	up := 0.0
	if len(parts) > 0 {
		up, _ = strconv.ParseFloat(parts[0], 64)
	}
	st.Uptime = &up
	r, err = run([]string{"bash", "--norc"}, []byte(hostScript(st.Machine.Profile)))
	if err != nil {
		st.Err = err
		return
	}
	if r.ExitCode != 0 {
		return
	}
	n := 0
	lines := strings.Split(strings.TrimSpace(string(r.Stdout)), "\n")
	for _, l := range lines[1:] {
		p := strings.Split(l, ";")
		if len(p) < 3 {
			continue
		}
		prefix := st.Machine.Profile + "-"
		if strings.HasPrefix(p[0], prefix) && strings.HasSuffix(p[0], "-link") {
			id := strings.TrimSuffix(strings.TrimPrefix(p[0], prefix), "-link")
			if _, err := strconv.Atoi(id); err == nil {
				n++
			}
		}
	}
	st.Generations = &n
	path := "/etc/confctl/inputs-info.json"
	var value any
	if st.Machine.CarrierName != nil {
		r, err = run([]string{"cat", filepath.Join(st.Machine.Profile, "machine.json")}, nil)
		if err != nil {
			st.Err = err
			return
		}
		if r.ExitCode == 0 {
			var h map[string]any
			if json.Unmarshal(r.Stdout, &h) == nil {
				value = h["inputs-info"]
			}
		}
		if !truth(value) {
			path = filepath.Join(st.Machine.Profile, path)
		}
	}
	if !truth(value) {
		r, err = run([]string{"cat", path}, nil)
		if err != nil {
			st.Err = err
			return
		}
		if r.ExitCode != 0 {
			return
		}
		value = string(r.Stdout)
	}
	if s, ok := value.(string); ok {
		if json.Unmarshal([]byte(s), &value) != nil {
			return
		}
	}
	st.Deployed = normalizeInputs(value)
}
func formatDuration(up float64) (string, error) {
	for _, u := range []struct {
		s string
		n float64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}} {
		if up > u.n {
			return fmt.Sprintf("%.1f%s", math.Round(up/u.n*10)/10, u.s), nil
		}
	}
	return "", fmt.Errorf("invalid time duration '%s'", strconv.FormatFloat(up, 'f', 1, 64))
}
func inputState(t, d map[string]any) string {
	if t == nil || d == nil || t["rev"] == nil || d["rev"] == nil {
		return "unknown"
	}
	if t["rev"] == d["rev"] {
		return "same"
	}
	return "changed"
}
func (e *Engine) localGenerationCount(host string) (int, error) {
	dir := filepath.Join(e.Root, ".confctl", "generations", strings.ReplaceAll(host, "/", ":"))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range entries {
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		p := filepath.Join(dir, d.Name(), "generation.json")
		b, err := os.ReadFile(p)
		var h map[string]any
		if err == nil {
			err = json.Unmarshal(b, &h)
		}
		reason := ""
		if err != nil {
			reason = p + ": " + err.Error()
		} else if h["mode"] != "flakes" {
			reason = fmt.Sprintf("%s: unsupported generation mode %s; only explicit flakes mode is supported", p, rubyInspect(h["mode"]))
		} else {
			_, dateErr := time.Parse(time.RFC3339Nano, str(h["date"]))
			inputs, ok := h["inputs"].(map[string]any)
			if h["inputs"] == nil {
				inputs = map[string]any{}
				ok = true
			}
			valid := ok
			for _, v := range inputs {
				if _, ok := v.(string); !ok {
					valid = false
				}
			}
			info := h["inputs_info"]
			if info == nil {
				info = h["inputsInfo"]
			}
			if info != nil {
				if _, ok := info.(map[string]any); !ok {
					valid = false
				}
			}
			if h["auto_rollback"] != nil {
				if _, ok := h["auto_rollback"].(string); !ok {
					valid = false
				}
			}
			if s, ok := h["toplevel"].(string); !ok || s == "" {
				valid = false
			}
			if !valid {
				reason = p + ": invalid flake generation payload"
			} else if dateErr != nil {
				reason = p + ": " + dateErr.Error()
			}
		}
		if reason != "" {
			fmt.Fprintf(os.Stderr, "Skipping generation %s: %s. Records and GC roots were left untouched; use confctl v3 for old software-pin generations.\n", filepath.Dir(p), reason)
			continue
		}
		n++
	}
	return n, nil
}
func (e *Engine) Status(ms, all []Machine) (string, error) {
	statuses := make([]Status, len(ms))
	n, err := e.evaluator()
	if err != nil {
		return "", err
	}
	mapping, err := n.eval(".#confctl.machineKeys")
	keys, _ := mapping.(map[string]any)
	for i, m := range ms {
		statuses[i].Machine = m
		if err == nil {
			k := str(keys[m.Name])
			if k == "" {
				continue
			}
			v, er := n.eval(".#confctl.inputsInfo." + k)
			if er == nil {
				statuses[i].Target = normalizeInputs(v)
			}
		}
	}
	// Baseline creates machines.length workers; one goroutine per selected machine.
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) { defer wg.Done(); e.query(&statuses[i], all) }(i)
	}
	wg.Wait()
	roleSet := map[string]bool{}
	for _, s := range statuses {
		if s.Err != nil {
			return "", s.Err
		}
		for k := range s.Target {
			roleSet[k] = true
		}
		for k := range s.Deployed {
			roleSet[k] = true
		}
	}
	roles := []string{}
	for k := range roleSet {
		roles = append(roles, k)
	}
	sort.Strings(roles)
	cols := append([]string{"host", "online", "uptime", "status", "generations"}, roles...)
	rows := []map[string]any{}
	for _, s := range statuses {
		n, err := e.localGenerationCount(s.Machine.Name)
		if err != nil {
			return "", err
		}
		row := map[string]any{"host": s.Machine.Name, "generations": fmt.Sprint(n) + ":"}
		ok := s.Uptime != nil && len(s.Target) > 0
		for k, t := range s.Target {
			if inputState(t, s.Deployed[k]) != "same" {
				ok = false
			}
		}
		if s.Uptime != nil {
			row["online"] = e.Colorize("yes", "green")
			up, err := formatDuration(*s.Uptime)
			if err != nil {
				return "", err
			}
			row["uptime"] = up
		}
		if s.Generations != nil {
			row["generations"] = fmt.Sprintf("%d:%d", n, *s.Generations)
		}
		if ok {
			row["status"] = e.Colorize("ok", "green")
		} else {
			row["status"] = e.Colorize("outdated", "red")
		}
		for _, role := range roles {
			d := s.Deployed[role]
			rev := str(d["shortRev"])
			if rev == "" {
				rev = short(str(d["rev"]))
			}
			if rev == "" {
				rev = "unknown"
			}
			color := map[string]string{"same": "green", "changed": "red", "unknown": "yellow"}[inputState(s.Target[role], d)]
			row[role] = e.Colorize(rev, color)
		}
		rows = append(rows, row)
	}
	return Table(rows, cols, true), nil
}
