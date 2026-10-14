package core

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

// generationWarning preserves stderr-before-log; the P1 raw logger remains
// void until the separately committed live-log binding.
func (e *Engine) generationWarning(stderr io.Writer) func(string) error {
	return func(message string) error {
		body := []byte(message + "\n")
		n, err := stderr.Write(body)
		if err != nil {
			return err
		}
		if n != len(body) {
			return io.ErrShortWrite
		}
		e.logText(message + "\n")
		return nil
	}
}
func (e *Engine) generationOptions() generation.Options {
	return generation.Options{Warn: e.generationWarning(os.Stderr)}
}

// Queries run concurrently, but results retain inventory order. The existing
// process adapter owns cancellation and logging; this adds no transport policy.
func (e *Engine) generationStatuses(ms, all []Machine) ([]Status, error) {
	statuses := make([]Status, len(ms))
	var wg sync.WaitGroup
	for i, machine := range ms {
		statuses[i].Machine = machine
		wg.Add(1)
		go func(i int) { defer wg.Done(); e.query(&statuses[i], all, queryOptions{Generations: true}) }(i)
	}
	wg.Wait()
	for _, st := range statuses {
		if st.Err != nil {
			return nil, st.Err
		}
	}
	return statuses, nil
}

type generationArgumentError struct{ error }

func (e *Engine) ListGenerations(inv cli.Invocation) (string, error) {
	if err := e.Context.Err(); err != nil {
		return "", err
	}
	all, err := e.Machines(false)
	if err != nil {
		return "", err
	}
	var machinePattern, pattern *string
	if len(inv.Args) > 0 {
		machinePattern = &inv.Args[0]
	}
	if len(inv.Args) > 1 {
		pattern = &inv.Args[1]
	}
	options := handlerOptions(inv)
	// Unlike status/ssh, generation selection includes unmanaged, carried and
	// targetless machines. Remote routing fails only when that query is needed.
	ms, err := Select(all, machinePattern, options.Attrs, options.Tags, "all")
	if err != nil {
		for _, f := range options.Attrs {
			if !strings.Contains(f, "=") {
				return "", &generationArgumentError{fmt.Errorf("Invalid filter ''")}
			}
		}
		return "", &generationArgumentError{err}
	}
	remote := map[string]*generation.HostList{}
	if inv.Options["remote"].Value.Bool {
		statuses, err := e.generationStatuses(ms, all)
		if err != nil {
			return "", err
		}
		for _, st := range statuses {
			remote[st.Machine.Name] = st.Generations
		}
	}
	hosts := make([]string, len(ms))
	for i, machine := range ms {
		hosts[i] = machine.Name
	}
	list, err := generation.SelectGenerations(hosts, generation.SelectionOptions{Local: inv.Options["local"].Value.Bool, Remote: inv.Options["remote"].Value.Bool, Pattern: pattern, Match: Match},
		func(host string) (*generation.HostList, error) { return remote[host], nil },
		func(host string) (*generation.BuildList, error) {
			if err := e.Context.Err(); err != nil {
				return nil, err
			}
			return generation.LoadBuildList(e.Root, host, e.generationOptions())
		})
	if err != nil {
		return "", err
	}
	return generationTable(list), nil
}

// Normalize the ordered R1 metadata directly. Neither member order nor exact
// JSON-number tokens pass through a JSON encode/decode roundtrip.
func generationInputInfo(value any) map[string]any {
	if object, ok := value.(generation.Object); ok {
		info := map[string]any{}
		for _, member := range object {
			switch member.Name {
			case "input", "url", "rev", "shortRev", "lastModified":
				info[member.Name] = member.Value
			case "short_rev":
				info["shortRev"] = member.Value
			}
		}
		if info["shortRev"] == nil && truth(info["rev"]) {
			info["shortRev"] = short(str(info["rev"]))
		}
		return info
	}
	return normalizeInputs(map[string]any{"role": value})["role"]
}

func generationTable(list *generation.UnifiedList) string {
	roles, seen := []string{}, map[string]bool{}
	for _, g := range list.Generations {
		for _, member := range g.InputsInfo() {
			if !seen[member.Name] {
				roles = append(roles, member.Name)
				seen[member.Name] = true
			}
		}
	}
	cols := append([]string{"host", "name", "id", "presence", "current", "kernel"}, roles...)
	rows := make([]map[string]any, 0, len(list.Generations))
	for _, g := range list.Generations {
		var id, kernel any
		if g.ID != nil {
			id = g.ID.String()
		}
		if g.KernelVersion != nil {
			kernel = *g.KernelVersion
		}
		row := map[string]any{"host": g.Host, "name": g.Name, "id": id, "presence": g.PresenceString(), "current": g.CurrentString(), "kernel": kernel}
		for _, role := range roles {
			value, _ := g.InputsInfo().Lookup(role)
			info := generationInputInfo(value)
			rev := info["shortRev"]
			if !truth(rev) && truth(info["rev"]) {
				rev = short(str(info["rev"]))
			}
			row[role] = rev
		}
		rows = append(rows, row)
	}
	// Sort only display rows, after input columns have their source order. Input
	// role collisions overwrite row keys in the source too, including sort keys.
	cell := func(value any) string {
		if !truth(value) || str(value) == "" {
			return "-"
		}
		return str(value)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := cell(rows[i]["name"]), cell(rows[j]["name"])
		if a != b {
			return a < b
		}
		return cell(rows[i]["host"]) < cell(rows[j]["host"])
	})
	return Table(rows, cols, true)
}

type resolvedBuild struct {
	Host  string
	Build *generation.Build
}

// Cluster comparison's table uses local host/build pairs, not unified records.
// Source column order is first role occurrence; input names can overwrite base
// row keys and create duplicate columns. Placeholder preparation precedes
// typed sorting; display conversion follows.
func resolvedBuildTable(builds []resolvedBuild, missing []string) (string, error) {
	roles, seen := []string{}, map[string]bool{}
	infos := make([]map[string]map[string]any, len(builds))
	for i, resolved := range builds {
		var err error
		infos[i], err = comparisonInputs(resolved.Build.InputsInfo)
		if err != nil {
			return "", err
		}
		for _, member := range resolved.Build.InputsInfo {
			if !seen[member.Name] {
				roles = append(roles, member.Name)
				seen[member.Name] = true
			}
		}
	}
	rows := make([]map[string]any, 0, len(builds)+len(missing))
	for i, resolved := range builds {
		var kernel any
		if resolved.Build.KernelVersion != nil {
			kernel = *resolved.Build.KernelVersion
		}
		row := map[string]any{"name": resolved.Host, "generation": resolved.Build.Name, "kernel": kernel}
		for _, role := range roles {
			rev, err := comparisonShort(infos[i][role])
			if err != nil {
				return "", err
			}
			row[role] = rev
		}
		rows = append(rows, row)
	}
	for _, host := range missing {
		rows = append(rows, map[string]any{"name": host, "generation": "not found"})
	}
	// OutputFormatter.prepare replaces nil/false/empty String, retaining all
	// other values for its [name,generation] comparison before to_s display.
	cols := append([]string{"name", "generation", "kernel"}, roles...)
	for _, row := range rows {
		for _, col := range cols {
			v := row[col]
			if !truth(v) || v == "" {
				row[col] = "-"
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a := []any{rows[i]["name"], rows[i]["generation"]}
		b := []any{rows[j]["name"], rows[j]["generation"]}
		if cmp, comparable := resolvedBuildValueCompare(a, b); comparable {
			return cmp < 0
		}
		// Source fallback is used only when <=> is nil. A literal "-" also
		// matches @empty; a mixed pair without a placeholder stays stable.
		for _, v := range a {
			if v == nil || v == "-" {
				return true
			}
		}
		for _, v := range b {
			if v == nil || v == "-" {
				return false
			}
		}
		return false
	})
	for _, row := range rows {
		for key, value := range row {
			row[key] = rubyValue(comparisonText(value))
		}
	}
	return Table(rows, cols, true), nil
}

// Only the private resolved table reaches typed JSON sort keys. Array ordering
// is lexicographic; unequal Hash/boolean or mixed types yield source <=> nil.
// Numeric meaning uses the comparison owner's exact Integer/binary64 boundary.
func resolvedBuildValueCompare(a, b any) (int, bool) {
	if comparisonRevisionEqual(a, b) {
		return 0, true
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		if ok {
			return strings.Compare(x, y), true
		}
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			break
		}
		number := func(n json.Number) (*big.Rat, bool) {
			if !strings.ContainsAny(string(n), ".eE") {
				i, _ := new(big.Int).SetString(string(n), 10)
				return new(big.Rat).SetInt(i), false
			}
			f, _ := n.Float64()
			return new(big.Rat).SetFloat64(f), f > 0
		}
		xr, xp := number(x)
		yr, yp := number(y)
		if xr != nil && yr != nil {
			return xr.Cmp(yr), true
		}
		// JSON exponents may overflow the existing binary64 interpretation.
		if xr == nil {
			if xp {
				return 1, true
			}
			return -1, true
		}
		if yp {
			return -1, true
		}
		return 1, true
	case []any:
		y, ok := b.([]any)
		if !ok {
			break
		}
		for i := 0; i < len(x) && i < len(y); i++ {
			cmp, comparable := resolvedBuildValueCompare(x[i], y[i])
			if !comparable || cmp != 0 {
				return cmp, comparable
			}
		}
		if len(x) < len(y) {
			return -1, true
		}
		if len(x) == len(y) {
			return 0, true
		}
		return 1, true
	}
	return 0, false
}

func (e *Engine) LogGenerationCLI(inv cli.Invocation) {
	command, arguments := make([]any, len(inv.Command.Path)), make([]any, len(inv.Args))
	for i, p := range inv.Command.Path {
		command[i] = p
	}
	for i, a := range inv.Args {
		arguments[i] = a
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{command: %s,\n global_options: {\"c\" => %s, \"color\" => %s, \"help\" => false},\n command_options:\n  {", rubyInspect(command), rubyInspect(e.Color), rubyInspect(e.Color))
	specs := append([]cli.OptionSpec(nil), inv.Command.Options...)
	sort.SliceStable(specs, func(i, j int) bool {
		a, b := inv.Options[specs[i].Key].FirstEncounter, inv.Options[specs[j].Key].FirstEncounter
		return a != 0 && (b == 0 || a < b)
	})
	values, separator := invocationOptions(inv), ""
	for _, spec := range specs {
		value := values[spec.Key]
		if list, ok := value.([]string); ok {
			items := make([]any, len(list))
			for i, item := range list {
				items[i] = item
			}
			value = items
		}
		for _, name := range spec.Names {
			fmt.Fprintf(&b, "%s%s => %s", separator, rubyInspect(name), rubyInspect(value))
			separator = ",\n   "
		}
	}
	b.WriteString(separator + "#<GLI::Command::ParentKey:0x0000000000000000> => {}}" + inputLogArgumentsField(arguments))
	e.logText(b.String())
}
