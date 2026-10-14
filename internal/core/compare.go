package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
	inputgit "github.com/vpsfreecz/confctl/internal/git"
	"github.com/vpsfreecz/confctl/internal/inputs"
	"github.com/vpsfreecz/confctl/internal/ui"
)

// Private composition dependencies are operations with existing owners. Page
// is always the actual ui.Page; callbacks supply discovery/IO, never a second
// pager or an accumulated comparison transcript.
type comparisonDeps struct {
	Machines  func() ([]Machine, error)
	List      func([]Machine) (string, error)
	Builds    func(string) (*generation.BuildList, error)
	Metadata  func() (*inputs.MachineResolver, error)
	Query     func(*Status, []Machine)
	Log       func(context.Context, string, string, string, []string) (string, error)
	Env       func(string) (string, bool)
	OutputTTY func() bool
	ErrorTTY  func() bool
	Pager     ui.PagerOps
}

type comparisonArgumentError struct{ error }

// Compare is private until the native pager/input/package gates close and the
// dispatcher is separately assigned. Existing CLI initialization owns logging.
// An enabled missing native pager adapter fails at its reached callback.
func (e *Engine) Compare(inv cli.Invocation, stdin, stdout, stderr *os.File) error {
	missing := func() error { return fmt.Errorf("comparison native pager adapter is not integrated") }
	deps := comparisonDeps{
		Machines: func() ([]Machine, error) { return e.Machines(false) },
		List:     func(ms []Machine) (string, error) { return e.List(ms, nil, true) },
		Builds: func(host string) (*generation.BuildList, error) {
			return generation.LoadBuildList(e.Root, host, generation.Options{Warn: e.generationWarning(stderr)})
		},
		// This resolver uses Settings.maxJobs and show_trace, never comparison's
		// parsed max-jobs/cores. Inventory's evaluator does not seed its keys.
		Metadata: e.inputResolver,
		Query: func(st *Status, all []Machine) {
			e.queryWithInputs(st, all, queryOptions{Inputs: true}, e.Process, true)
		},
		Log:       e.comparisonLog,
		Env:       os.LookupEnv,
		OutputTTY: func() bool { return commitStdoutTTY(stdout) },
		ErrorTTY:  func() bool { return commitStdoutTTY(stderr) },
		Pager: ui.PagerOps{
			Env:       os.LookupEnv,
			Exists:    func(path string) bool { _, err := os.Stat(path); return err == nil },
			GitConfig: func(context.Context) (string, error) { return "", missing() },
			Probe:     func(context.Context, string) (bool, error) { return false, missing() },
			Start:     func(context.Context, string) (ui.PagerPipe, error) { return nil, missing() },
			Basic:     func(context.Context, io.Writer) (ui.PagerSinkCloser, error) { return nil, missing() },
		},
	}
	return e.compare(inv, bufio.NewReader(stdin), stdout, deps)
}

func (e *Engine) LogComparisonCLI(inv cli.Invocation) error { return e.LogTestConnectionCLI(inv) }

func comparisonPagerEnabled(deps comparisonDeps) bool {
	if value, _ := deps.Env("CONFCTL_TTY"); value == "0" || !deps.OutputTTY() || !deps.ErrorTTY() {
		return false
	}
	value, present := deps.Env("PAGER")
	return present && testConnectionStrip(value) != ""
}

func (e *Engine) compare(inv cli.Invocation, in *bufio.Reader, out io.Writer, deps comparisonDeps) error {
	ctx := e.Context
	if err := ctx.Err(); err != nil {
		return err
	}
	// Ruby caches this decision at command construction, before inventory,
	// confirmation and target metadata. Null's output TTY is checked on writes.
	enabled := comparisonPagerEnabled(deps)
	all, err := deps.Machines()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var machinePattern, rolePattern *string
	if len(inv.Args) > 0 {
		machinePattern = &inv.Args[0]
	}
	if len(inv.Args) > 1 {
		rolePattern = &inv.Args[1]
	}
	options := handlerOptions(inv)
	ms, err := Select(all, machinePattern, options.Attrs, options.Tags, "yes")
	if err != nil {
		for _, filter := range options.Attrs {
			if !strings.Contains(filter, "=") {
				return &comparisonArgumentError{fmt.Errorf("Invalid filter ''")}
			}
		}
		return &comparisonArgumentError{err}
	}
	yes := inv.Options["yes"].Value.Bool
	selected := inv.Options["generation"].Present
	selector := inv.Options["generation"].Value.String
	if !yes {
		if _, err := io.WriteString(out, "Compare inputs on the following machines:\n"); err != nil {
			return err
		}
		table, err := deps.List(ms)
		if err != nil {
			return err
		}
		label := "current configuration"
		if selected {
			label = selector
		}
		if _, err = fmt.Fprint(out, table+"\nGeneration: "+label+"\n"); err != nil {
			return err
		}
		if err = testConnectionConfirm(in, out); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	statuses := make([]Status, len(ms))
	for i, machine := range ms {
		if _, err := statusConnection(machine, all); err != nil {
			return err
		}
		statuses[i].Machine = machine
	}
	if selected {
		builds := []resolvedBuild{}
		missing := []string{}
		for _, machine := range ms {
			if err := ctx.Err(); err != nil {
				return err
			}
			list, err := deps.Builds(machine.Name)
			if err != nil {
				return err
			}
			build, err := generation.ResolveBuild(list, selector)
			if err != nil {
				return err
			}
			if build == nil {
				missing = append(missing, machine.Name)
			} else {
				builds = append(builds, resolvedBuild{machine.Name, build})
			}
		}
		if len(builds) == 0 {
			return fmt.Errorf("No generation found")
		}
		if _, err := io.WriteString(out, "Resolved host generations:\n"); err != nil {
			return err
		}
		table, err := resolvedBuildTable(builds, missing)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprint(out, table+"\n"); err != nil {
			return err
		}
		if len(missing) > 0 && !yes {
			var text strings.Builder
			fmt.Fprintf(&text, "Generation '%s' was not found on the following hosts:\n", selector)
			for _, host := range missing {
				fmt.Fprintf(&text, "  %s\n", host)
			}
			text.WriteString("\nThese hosts will be ignored.\n")
			if _, err = io.WriteString(out, text.String()); err != nil {
				return err
			}
			if err = testConnectionConfirm(in, out); err != nil {
				return err
			}
		}
		kept := make([]Status, 0, len(builds))
		for _, st := range statuses {
			for _, resolved := range builds {
				if st.Machine.Name != resolved.Host {
					continue
				}
				st.Target, err = comparisonInputs(resolved.Build.InputsInfo)
				if err != nil {
					return err
				}
				kept = append(kept, st)
				break
			}
		}
		statuses = kept
	} else {
		// Constructor failure is outside per-host rescue, including zero hosts.
		resolver, err := deps.Metadata()
		if err != nil {
			return err
		}
		for i := range statuses {
			if err := ctx.Err(); err != nil {
				return err
			}
			b, err := resolver.InputInfo(statuses[i].Machine.Name)
			if stopped := comparisonCanceled(ctx, err); stopped != nil {
				return stopped
			}
			if err != nil {
				continue
			}
			value, err := generation.DecodeJSONValue(b)
			if err == nil {
				statuses[i].Target, err = comparisonInputs(value)
			}
			if err != nil {
				statuses[i].Target = nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return ui.Page(ctx, ui.PagerOptions{Enabled: enabled, Output: out, OutputTTY: deps.OutputTTY}, deps.Pager, func(sink ui.PagerSink) error {
		for i := range statuses {
			if err := ctx.Err(); err != nil {
				return err
			}
			st := &statuses[i]
			deps.Query(st, all)
			if stopped := comparisonCanceled(ctx, st.Err); stopped != nil {
				return stopped
			}
			if st.Err != nil {
				return st.Err
			}
			if st.Uptime == nil {
				if err := sink.Puts(st.Machine.Name + " is offline"); err != nil {
					return err
				}
				continue
			}
			if err := comparisonRender(ctx, sink, inv, rolePattern, *st, deps.Log); err != nil {
				return err
			}
		}
		return nil
	})
}

func comparisonCanceled(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// This semantic adapter consumes the existing ordered JSON model once. Alias
// overwrite follows encounter order; only a reached rev slice can fail.
func comparisonInputs(value any) (map[string]map[string]any, error) {
	object, ok := value.(generation.Object)
	if !ok {
		return nil, nil
	}
	result := map[string]map[string]any{}
	for _, role := range object {
		info := map[string]any{}
		switch v := role.Value.(type) {
		case string:
			info["rev"], info["shortRev"] = v, comparisonStringSlice(v)
		case generation.Object:
			for _, field := range v {
				switch field.Name {
				case "input", "url", "rev", "shortRev", "lastModified":
					info[field.Name] = field.Value
				case "short_rev":
					info["shortRev"] = field.Value
				}
			}
			if info["shortRev"] == nil && truth(info["rev"]) {
				var err error
				info["shortRev"], err = comparisonSlice(info["rev"])
				if err != nil {
					return nil, err
				}
			}
		}
		result[role.Name] = info
	}
	return result, nil
}

func comparisonSlice(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return comparisonStringSlice(v), nil
	case []any:
		n := len(v)
		if n > 8 {
			n = 8
		}
		return append([]any{}, v[:n]...), nil
	case json.Number:
		if !strings.ContainsAny(string(v), ".eE") {
			// Ruby Integer#[0, 8] selects the low eight bits, including
			// negative integers. This conversion is a reached rev use, not
			// a JSON codec or eager numeric metadata normalization.
			n, _ := new(big.Int).SetString(string(v), 10)
			return json.Number(n.And(n, big.NewInt(255)).String()), nil
		}
		return nil, fmt.Errorf("undefined method '[]' for an instance of Float")
	case generation.Object:
		return nil, fmt.Errorf("wrong number of arguments (given 2, expected 1)")
	default:
		return nil, fmt.Errorf("undefined method '[]' for %s", rubyInspect(value))
	}
}

// Keep original bytes at reached character boundaries. The admitted ordered
// decoder can retain standalone low-surrogate bytes; []rune/string conversion
// here would invent replacement characters in a revision.
func comparisonStringSlice(value string) string {
	characters := 0
	for offset := range value {
		if characters == 8 {
			return value[:offset]
		}
		characters++
	}
	return value
}

func comparisonShort(info map[string]any) (any, error) {
	if truth(info["shortRev"]) {
		return info["shortRev"], nil
	}
	if truth(info["rev"]) {
		return comparisonSlice(info["rev"])
	}
	return info["rev"], nil
}

func comparisonState(target, deployed map[string]any) string {
	if target == nil || deployed == nil || target["rev"] == nil || deployed["rev"] == nil {
		return "unknown"
	}
	if comparisonRevisionEqual(target["rev"], deployed["rev"]) {
		return "same"
	}
	return "changed"
}

// Ruby's reached rev equality compares arrays recursively and hashes without
// member order. Numeric tokens are interpreted here only: integers remain
// exact; Float comparisons use the JSON provider's binary64 value.
func comparisonRevisionEqual(a, b any) bool {
	switch x := a.(type) {
	case generation.Object:
		y, ok := b.(generation.Object)
		if !ok || len(x) != len(y) {
			return false
		}
		for _, field := range x {
			value, found := y.Lookup(field.Name)
			if !found || !comparisonRevisionEqual(field.Value, value) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !comparisonRevisionEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		xInt, yInt := !strings.ContainsAny(string(x), ".eE"), !strings.ContainsAny(string(y), ".eE")
		if xInt && yInt {
			xi, _ := new(big.Int).SetString(string(x), 10)
			yi, _ := new(big.Int).SetString(string(y), 10)
			return xi.Cmp(yi) == 0
		}
		if !xInt && !yInt {
			xf, _ := x.Float64()
			yf, _ := y.Float64()
			return xf == yf
		}
		if !xInt {
			x, y = y, x
		}
		xi, _ := new(big.Int).SetString(string(x), 10)
		yf, _ := y.Float64()
		yr := new(big.Rat).SetFloat64(yf)
		return yr != nil && new(big.Rat).SetInt(xi).Cmp(yr) == 0
	default:
		return reflect.DeepEqual(a, b)
	}
}

// Interpolation of a reached ordered Hash/Array uses Ruby inspection, retaining
// its member order. It never serializes metadata back through JSON.
func comparisonText(value any) string {
	inspect := func(v any) string {
		if s, ok := v.(string); ok {
			return strconv.Quote(s)
		}
		if v == nil {
			return "nil"
		}
		return comparisonText(v)
	}
	switch v := value.(type) {
	case json.Number:
		// The ordered decoder keeps the token. Reached Integer interpolation
		// uses its arbitrary-precision meaning, including -0 becoming 0.
		if !strings.ContainsAny(string(v), ".eE") {
			n, _ := new(big.Int).SetString(string(v), 10)
			return n.String()
		}
		return str(v)
	case generation.Object:
		parts := make([]string, len(v))
		for i, field := range v {
			parts[i] = strconv.Quote(field.Name) + " => " + inspect(field.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = inspect(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return str(value)
	}
}

func comparisonCarriedInfo(value any) (any, error) {
	switch v := value.(type) {
	case generation.Object:
		info, _ := v.Lookup("inputs-info")
		return info, nil
	case string:
		// The source invokes String#[] with this literal substring too.
		if strings.Contains(v, "inputs-info") {
			return "inputs-info", nil
		}
		return nil, nil
	case []any:
		return nil, fmt.Errorf("no implicit conversion of String into Integer")
	case json.Number:
		if !strings.ContainsAny(string(v), ".eE") {
			return nil, fmt.Errorf("no implicit conversion of String into Integer")
		}
		return nil, fmt.Errorf("undefined method '[]' for an instance of Float")
	default:
		return nil, fmt.Errorf("undefined method '[]' for %s", rubyInspect(value))
	}
}

func comparisonURL(target, deployed map[string]any) any {
	if truth(target["url"]) {
		return target["url"]
	}
	return deployed["url"]
}

func comparisonRender(ctx context.Context, sink ui.PagerSink, inv cli.Invocation, pattern *string, st Status, log func(context.Context, string, string, string, []string) (string, error)) error {
	seen := map[string]bool{}
	for role := range st.Target {
		seen[role] = true
	}
	for role := range st.Deployed {
		seen[role] = true
	}
	roles := make([]string, 0, len(seen))
	for role := range seen {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		if err := sink.Puts(st.Machine.Name + ": no inputs info"); err != nil {
			return err
		}
		return sink.Puts("")
	}
	filtered := roles[:0]
	for _, role := range roles {
		if pattern == nil || Match(*pattern, role) {
			filtered = append(filtered, role)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	changelog := inv.Command.Path[0] == "changelog"
	changed, unknown := false, false
	for _, role := range filtered {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, deployed := st.Target[role], st.Deployed[role]
		switch comparisonState(target, deployed) {
		case "unknown":
			unknown = true
			if err := sink.Puts(st.Machine.Name + " @ " + role + " in unknown state"); err != nil {
				return err
			}
		case "changed":
			changed = true
			url := comparisonURL(target, deployed)
			if !changelog {
				from, err := comparisonShort(deployed)
				if err != nil {
					return err
				}
				to, err := comparisonShort(target)
				if err != nil {
					return err
				}
				if !truth(from) {
					from = "unknown"
				}
				if !truth(to) {
					to = "unknown"
				}
				if inv.Options["downgrade"].Value.Bool {
					from, to = to, from
				}
				label := role
				if truth(url) {
					label += " (" + comparisonText(url) + ")"
				}
				if err := sink.Puts(st.Machine.Name + " @ " + label + ": " + comparisonText(from) + " -> " + comparisonText(to)); err != nil {
					return err
				}
				continue
			}
			if err := sink.Puts(st.Machine.Name + " @ " + role + ":"); err != nil {
				return err
			}
			from, to := deployed["rev"], target["rev"]
			text, err := "(changelog unavailable)", error(nil)
			if url != nil && from != nil && to != nil {
				if inv.Options["downgrade"].Value.Bool {
					from, to = to, from
				}
				options := []string{}
				verbose, patch := inv.Options["verbose"].Value.Bool, inv.Options["patch"].Value.Bool
				if !verbose {
					options = append(options, "--oneline")
				}
				if patch {
					options = append(options, "-p")
				}
				if !verbose && !patch {
					options = append(options, "--max-count=50")
				}
				if urlString, ok := url.(string); ok {
					text, err = log(ctx, urlString, comparisonText(from), comparisonText(to), options)
				} else {
					// Digest::SHA256 rejects nonstring URLs when constructing the
					// mirror, inside this per-role rescue, before setup effects.
					typeName := "Hash"
					switch url.(type) {
					case []any:
						typeName = "Array"
					case json.Number:
						if strings.ContainsAny(string(url.(json.Number)), ".eE") {
							typeName = "Float"
						} else {
							typeName = "Integer"
						}
					case bool:
						typeName = comparisonText(url)
					}
					err = fmt.Errorf("no implicit conversion of %s into String", typeName)
				}
			}
			if stopped := comparisonCanceled(ctx, err); stopped != nil {
				return stopped
			}
			// Source rescues both Git and this puts(log), including a single
			// PagerClosed message-write retry. Header/trailing blank are outside.
			if err == nil {
				err = sink.Puts(text)
			}
			if stopped := comparisonCanceled(ctx, err); stopped != nil {
				return stopped
			}
			if err != nil {
				if err := sink.Puts(err.Error()); err != nil {
					return err
				}
			}
			if err := sink.Puts(""); err != nil {
				return err
			}
		}
	}
	if !changed && !unknown {
		if err := sink.Puts(st.Machine.Name + ": no changes"); err != nil {
			return err
		}
	}
	if !changelog || !changed {
		return sink.Puts("")
	}
	return nil
}

func (e *Engine) comparisonLog(ctx context.Context, url, from, to string, options []string) (string, error) {
	return e.comparisonMirrorLog(ctx, url, from, to, options, func(ctx context.Context, root string, argv []string) (string, error) {
		out, err := e.inputCommand(ctx, root, argv)
		return out.Stdout, err
	})
}

func (e *Engine) comparisonMirrorLog(ctx context.Context, url, from, to string, options []string, run inputgit.CapturedRunner) (string, error) {
	cache, err := inputMirrorCache()
	if err != nil {
		return "", err
	}
	mirror := inputgit.Mirror{Root: e.Root, Cache: cache, URL: url, Run: run}
	if err := mirror.Setup(ctx); err != nil {
		var fileError *os.PathError
		if errors.As(err, &fileError) {
			switch fileError.Op {
			case "stat":
				return "", configurationError(err, "rb_file_s_stat", mirror.Path())
			case "mkdir":
				return "", configurationError(err, "dir_s_mkdir", fileError.Path)
			}
		}
		return "", err
	}
	return mirror.LogWithOptions(ctx, from, to, options)
}
