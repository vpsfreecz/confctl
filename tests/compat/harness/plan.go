package harness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type PlannedRule struct {
	Index int  `json:"index"`
	Rule  Rule `json:"rule"`
}

func planName(tool, key string) string {
	if key == "" {
		return tool + ".json"
	}
	return fmt.Sprintf("%s-%x.json", tool, sha256.Sum256([]byte(key)))
}

// PreparePlan runs outside the command timing. Per-host and per-installable
// plans avoid reparsing the full inventory/rule matrix for every SSH request.
func PreparePlan(dir string, c Case) error {
	if e := os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	plans := map[string][]PlannedRule{}
	for i, r := range c.Rules {
		key := r.Host
		if r.Tool == "nix" && strings.HasPrefix(r.Contains, ".#") {
			key = r.Contains
		}
		name := planName(r.Tool, key)
		plans[name] = append(plans[name], PlannedRule{Index: i, Rule: r})
	}
	for name, p := range plans {
		b, e := json.Marshal(p)
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(dir, name), b, 0644); e != nil {
			return e
		}
	}
	return nil
}
func LoadPlan(dir, tool, host string, argv []string) ([]PlannedRule, error) {
	names := []string{planName(tool, "")}
	if host != "" {
		names = append(names, planName(tool, host))
	}
	if tool == "nix" {
		for _, a := range argv {
			if strings.HasPrefix(a, ".#") {
				names = append(names, planName(tool, a))
			}
		}
	}
	out := []PlannedRule{}
	for _, name := range names {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		var p []PlannedRule
		if e = json.Unmarshal(b, &p); e != nil {
			return nil, e
		}
		out = append(out, p...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}
