package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/generation"
)

type Status struct {
	Machine         Machine
	Uptime          *float64
	Generations     *generation.HostList
	CurrentToplevel *string
	TargetToplevel  *string
	Target          map[string]map[string]any
	Deployed        map[string]map[string]any
	Err             error
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

// queryOptions are explicit because listing must not request toplevel or inputs.
type queryOptions struct{ Toplevel, Generations, Inputs bool }

func (e *Engine) query(st *Status, all []Machine, options queryOptions) {
	e.queryWithInputs(st, all, options, e.Process, false)
}

// Status construction and querying share carrier lookup. Comparison constructs
// its statuses before reading target metadata; ordinary callers still resolve
// at their existing query point.
func statusConnection(machine Machine, all []Machine) (Machine, error) {
	m := machine
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
			return Machine{}, fmt.Errorf("Carrier %s not found in machine list", *machine.CarrierName)
		}
	}
	return m, nil
}

// Comparison selects ordered input decoding here, at the shared read/fallback
// owner. The existing status/listing route retains its original decoder and
// normalization. Injecting Process scripts transport without another runner.
func (e *Engine) queryWithInputs(st *Status, all []Machine, options queryOptions, process func([]string, []byte) (Result, error), comparison bool) {
	m, err := statusConnection(st.Machine, all)
	if err != nil {
		st.Err = err
		return
	}
	run := func(a []string, input []byte) (Result, error) {
		args, err := SSHArgs(m, a)
		if err != nil {
			return Result{}, err
		}
		return process(args, input)
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
	if options.Toplevel {
		path := "/run/current-system"
		if st.Machine.CarrierName != nil {
			path = st.Machine.Profile
		}
		r, err = run([]string{"realpath", path}, nil)
		if err != nil {
			st.Err = err
			return
		}
		if r.ExitCode != 0 {
			return
		}
		top := strings.Trim(string(r.Stdout), "\x00 \t\r\n\v\f")
		st.CurrentToplevel = &top
	}
	if options.Generations {
		list, err := generation.FetchHostList(e.Context, st.Machine.Name, st.Machine.Profile, e.generationOptions(), func(ctx context.Context, argv []string, stdin []byte) ([]byte, bool, error) {
			// Fetch receives the same invocation context used by Engine.Process.
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			r, err := run(argv, stdin)
			return r.Stdout, r.ExitCode == 0, err
		})
		if err != nil {
			st.Err = err
			return
		}
		if list == nil {
			return
		}
		st.Generations = list
	}
	if !options.Inputs {
		return
	}
	path := "/etc/confctl/inputs-info.json"
	var value any
	if st.Machine.CarrierName != nil {
		r, err = run([]string{"cat", strings.TrimRight(st.Machine.Profile, "/") + "/machine.json"}, nil)
		if err != nil {
			st.Err = err
			return
		}
		if r.ExitCode == 0 {
			if comparison {
				parsed, parseErr := generation.DecodeJSONValue(r.Stdout)
				if parseErr == nil {
					value, err = comparisonCarriedInfo(parsed)
					if err != nil {
						st.Err = err
						return
					}
				}
			} else {
				var h map[string]any
				if decodeJSONNumbers(r.Stdout, &h) == nil {
					value = h["inputs-info"]
				}
			}
		}
		if !truth(value) {
			path = strings.TrimRight(st.Machine.Profile, "/") + path
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
		if comparison {
			value, err = generation.DecodeJSONValue([]byte(s))
		} else {
			err = decodeJSONNumbers([]byte(s), &value)
		}
		if err != nil {
			return
		}
	}
	if comparison {
		st.Deployed, st.Err = comparisonInputs(value)
	} else {
		st.Deployed = normalizeInputs(value)
	}
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

// Status retains the public none-mode compatibility entrypoint.
func (e *Engine) Status(ms, all []Machine) (string, error) {
	statuses := make([]Status, len(ms))
	for i, machine := range ms {
		statuses[i].Machine = machine
	}
	if err := e.statusTargetInputs(statuses); err != nil {
		return "", err
	}
	e.statusQuery(statuses, all, queryOptions{Generations: true, Inputs: true}, len(ms))
	return e.statusTable(statuses)
}

// Target metadata remains best-effort at the existing status boundary. This
// fresh evaluator runs after build and does not reuse stored generation data.
func (e *Engine) statusTargetInputs(statuses []Status) error {
	n, err := e.evaluator()
	if err != nil {
		return err
	}
	mapping, err := n.eval(".#confctl.machineKeys")
	keys, _ := mapping.(map[string]any)
	for i := range statuses {
		m := statuses[i].Machine
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
	return nil
}

func (e *Engine) statusQuery(statuses []Status, all []Machine, options queryOptions, workers int) {
	if len(statuses) > 0 && workers > 0 {
		done := e.beginTiming(timingStatusChecks)
		defer done()
	}
	// Keep the selected-machine worker count even after missing generations
	// remove statuses. Each job still uses the existing per-host query owner.
	jobs := make(chan int, len(statuses))
	for i := range statuses {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				e.query(&statuses[i], all, options)
			}
		}()
	}
	wg.Wait()
}

func (e *Engine) statusTable(statuses []Status) (string, error) {
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
		builds, err := generation.LoadBuildList(e.Root, s.Machine.Name, e.generationOptions())
		if err != nil {
			return "", err
		}
		n := len(builds.Builds)
		row := map[string]any{"host": s.Machine.Name, "generations": fmt.Sprint(n) + ":"}
		ok := s.Uptime != nil && len(s.Target) > 0
		for k, t := range s.Target {
			if inputState(t, s.Deployed[k]) != "same" {
				ok = false
			}
		}
		if s.TargetToplevel != nil {
			ok = ok && s.CurrentToplevel != nil && *s.TargetToplevel == *s.CurrentToplevel
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
			row["generations"] = fmt.Sprintf("%d:%d", n, len(s.Generations.Generations))
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
