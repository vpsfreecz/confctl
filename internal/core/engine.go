package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	Root          string
	Context       context.Context
	SettingsCache map[string]any
	Inventory     []Machine
	ShowTrace     bool
	Color         string
	Yes           bool
	mu            sync.Mutex
	Log           *os.File
}

func New(ctx context.Context) (*Engine, error) {
	root, e := os.Getwd()
	if e != nil {
		return nil, e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	return &Engine{Root: root, Context: ctx, Color: "auto"}, nil
}
func (e *Engine) OpenLog(name string) error {
	dir := filepath.Join(e.Root, ".confctl", "logs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, time.Now().Format("2006-01-02--15-04-05")+"-confctl-"+name+".log"))
	e.Log = f
	return err
}
func (e *Engine) CloseLog(success bool) {
	if e.Log == nil {
		return
	}
	name := e.Log.Name()
	_ = e.Log.Close()
	if success {
		_ = os.Remove(name)
	}
}
func (e *Engine) Process(argv []string, input []byte) (Result, error) {
	id, start := e.processLogStart(argv)
	r, err := Process(e.Context, e.Root, argv, input)
	e.processLogFinish(id, start, r)
	return r, err
}
func (e *Engine) required() error {
	if i, err := os.Stat(filepath.Join(e.Root, "flake.nix")); err == nil && i.Mode().IsRegular() {
		return nil
	}
	return fmt.Errorf("%s has no flake.nix; migrate software-pin configurations with confctl v3 before using this version (see docs/swpins-to-flakes.md)", e.Root)
}
func (e *Engine) evalSettings(max string) (map[string]any, error) {
	var s map[string]any
	b, err := e.nix("eval", []string{".#confctl.settings"}, false, false, max)
	if err == nil {
		err = decodeJSONNumbers(b, &s)
		demod(s)
	}
	return s, err
}
func (e *Engine) Settings() (map[string]any, error) {
	if e.SettingsCache != nil {
		return e.SettingsCache, nil
	}
	s, err := e.evalSettings("auto")
	if err == nil {
		e.SettingsCache = s
	}
	return s, err
}
func nested(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		h, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = h[k]
	}
	return v
}
func (e *Engine) nix(op string, installables []string, impure, trace bool, max string) ([]byte, error) {
	if err := e.required(); err != nil {
		return nil, err
	}
	extra, noUpdate := false, true
	for {
		a := []string{"nix", op, "--json"}
		if op == "build" {
			a = append(a, "--no-link")
		}
		if extra {
			a = append(a, "--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes")
		}
		if impure {
			a = append(a, "--impure")
		}
		a = append(a, "--no-write-lock-file")
		if noUpdate {
			a = append(a, "--no-update-lock-file")
		}
		if trace {
			a = append(a, "--show-trace")
		}
		if max != "" {
			a = append(a, "--option", "max-jobs", max)
		}
		a = append(a, installables...)
		r, err := e.Process(a, nil)
		if err != nil {
			return nil, err
		}
		if r.ExitCode == 0 {
			return r.Stdout, nil
		}
		msg := string(r.Stderr)
		lower := strings.ToLower(msg)
		if noUpdate && strings.Contains(msg, "--no-update-lock-file") && (strings.Contains(lower, "unknown") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "invalid") || strings.Contains(lower, "unsupported")) {
			noUpdate = false
			continue
		}
		if !extra && strings.Contains(lower, "experimental") && (strings.Contains(lower, "nix-command") || strings.Contains(lower, "flakes")) {
			extra = true
			continue
		}
		return nil, commandError(a, r)
	}
}

type evaluator struct {
	e        *Engine
	settings map[string]any
	max      string
}

func (e *Engine) evaluator() (*evaluator, error) {
	s, err := e.Settings()
	if err != nil {
		return nil, err
	}
	return &evaluator{e: e, max: str(nested(s, "nix", "maxJobs"))}, nil
}
func (n *evaluator) argsSettings() error {
	if n.settings != nil {
		return nil
	}
	s, err := n.e.evalSettings(n.max)
	n.settings = s
	return err
}
func (n *evaluator) eval(installable string) (any, error) {
	if err := n.argsSettings(); err != nil {
		return nil, err
	}
	b, err := n.e.nix("eval", []string{installable}, nested(n.settings, "nix", "impureEval") == true, n.e.ShowTrace, n.max)
	if err != nil {
		return nil, err
	}
	var v any
	err = json.Unmarshal(b, &v)
	return v, err
}
func (e *Engine) Machines(refresh bool) ([]Machine, error) {
	if e.Inventory != nil && !refresh {
		return e.Inventory, nil
	}
	n, err := e.evaluator()
	if err != nil {
		return nil, err
	}
	if err = n.argsSettings(); err != nil {
		return nil, err
	}
	b, err := e.nix("build", []string{".#confctl.machinesJson"}, nested(n.settings, "nix", "impureEval") == true, false, n.max)
	if err != nil {
		return nil, err
	}
	var out []struct {
		Outputs map[string]string `json:"outputs"`
	}
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 || out[0].Outputs["out"] == "" {
		return nil, fmt.Errorf("missing output path for \".#confctl.machinesJson\"")
	}
	b, err = os.ReadFile(out[0].Outputs["out"])
	if err != nil {
		return nil, err
	}
	m, err := DecodeMachines(b)
	if err == nil {
		e.Inventory = m
	}
	return m, err
}
func (e *Engine) List(ms []Machine, cols []string, header bool) (string, error) {
	if cols == nil {
		s, err := e.Settings()
		if err != nil {
			return "", err
		}
		a, _ := nested(s, "list", "columns").([]any)
		cols = []string{}
		for _, v := range a {
			cols = append(cols, str(v))
		}
	}
	rows := []map[string]any{}
	for _, m := range ms {
		r := map[string]any{}
		for _, c := range cols {
			r[c] = m.Attr(c)
		}
		rows = append(rows, r)
	}
	return Table(rows, cols, header), nil
}
func (e *Engine) Confirm(stdin *os.File, message string, always bool) (bool, error) {
	if e.Yes && !always {
		return true, nil
	}
	fmt.Print(message)
	reader := bufio.NewReader(stdin)
	for {
		fmt.Print("\nContinue? [y/N]: ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return false, fmt.Errorf("end of file reached")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y":
			fmt.Println()
			return true, nil
		case "n":
			fmt.Println()
			return false, nil
		}
	}
}
func (e *Engine) Colorize(s, color string) string {
	enabled := e.Color == "always" || (e.Color == "auto" && os.Getenv("NO_COLOR") == "")
	if !enabled {
		return s
	}
	code := map[string]string{"green": "32", "red": "31", "yellow": "33", "bright": "1"}[color]
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
