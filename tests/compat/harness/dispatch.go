package harness

import (
	"reflect"
	"strings"
)

func MatchRequest(r Rule, tool, host string, argv []string, root string) bool {
	if r.Tool != tool || (r.Host != "" && r.Host != host) {
		return false
	}
	expand := func(s string) string { return strings.ReplaceAll(s, "${ROOT}", root) }
	if r.Argv != nil {
		a := make([]string, len(r.Argv))
		for i, v := range r.Argv {
			a[i] = expand(v)
		}
		if !reflect.DeepEqual(a, argv) {
			return false
		}
	}
	if r.Contains != "" {
		found := false
		for _, v := range argv {
			if strings.Contains(v, expand(r.Contains)) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
