package harness

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var logPathRE = regexp.MustCompile(`^\.confctl/logs/([0-9]{4}-[0-9]{2}-[0-9]{2}--[0-9]{2}-[0-9]{2}-[0-9]{2})-confctl-(.+)\.log$`)
var logUUIDRE = regexp.MustCompile(`(?m)^\[([0-9a-f]{8})\] `)
var parentAddressRE = regexp.MustCompile(`#<GLI::Command::ParentKey:0x[0-9a-f]+>`)
var durationRE = regexp.MustCompile(`(?m)(\[[^\]\n]+\] Finished in )[0-9]+\.[0-9]{3} seconds?( with exit status -?[0-9]+ \((?:successful|failed)\))`)

func normalizeLogs(o *Observation, fields []string, root string) error {
	allowed := map[string]bool{"filename-time": true, "command-uuid": true, "gli-parent-address": true, "process-duration": true, "config-root": true}
	enabled := map[string]bool{}
	for _, f := range fields {
		if !allowed[f] {
			return fmt.Errorf("log normalization not allowed: %s", f)
		}
		enabled[f] = true
	}
	ids := map[string]string{}
	dates := map[string]string{}
	parents := map[string]string{}
	text := func(s string) string {
		if enabled["config-root"] {
			s = strings.ReplaceAll(s, root, "${ROOT}")
		}
		if enabled["command-uuid"] {
			s = logUUIDRE.ReplaceAllStringFunc(s, func(match string) string {
				id := logUUIDRE.FindStringSubmatch(match)[1]
				to, ok := ids[id]
				if !ok {
					to = fmt.Sprintf("${COMMAND_ID:%d}", len(ids)+1)
					ids[id] = to
				}
				return "[" + to + "] "
			})
		}
		if enabled["gli-parent-address"] {
			s = parentAddressRE.ReplaceAllStringFunc(s, func(match string) string {
				to, ok := parents[match]
				if !ok {
					to = fmt.Sprintf("#<GLI::Command::ParentKey:${PARENT_ADDRESS:%d}>", len(parents)+1)
					parents[match] = to
				}
				return to
			})
		}
		if enabled["process-duration"] {
			s = durationRE.ReplaceAllStringFunc(s, func(match string) string {
				m := durationRE.FindStringSubmatch(match)
				return m[1] + "${DURATION} seconds" + m[2]
			})
		}
		return s
	}
	path := func(p string) string {
		if !enabled["filename-time"] {
			return p
		}
		m := logPathRE.FindStringSubmatch(p)
		if m == nil {
			return p
		}
		to, ok := dates[m[1]]
		if !ok {
			to = fmt.Sprintf("${LOG_TIME:%d}", len(dates)+1)
			dates[m[1]] = to
		}
		return ".confctl/logs/" + to + "-confctl-" + m[2] + ".log"
	}
	normalize := func(state map[string]File) (map[string]File, error) {
		out := map[string]File{}
		keys := []string{}
		for k := range state {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := state[k]
			to := k
			if logPathRE.MatchString(k) {
				to = path(k)
				f.Text = text(f.Text)
			}
			if _, exists := out[to]; exists {
				return nil, fmt.Errorf("log normalization path collision %s", to)
			}
			out[to] = f
		}
		return out, nil
	}
	var err error
	o.Before, err = normalize(o.Before)
	if err != nil {
		return err
	}
	o.After, err = normalize(o.After)
	if err != nil {
		return err
	}
	// Only the explicit diagnostic's path is volatile, not arbitrary stderr hex.
	if enabled["filename-time"] {
		for date, to := range dates {
			o.Stderr = strings.ReplaceAll(o.Stderr, "/.confctl/logs/"+date+"-confctl-", "/.confctl/logs/"+to+"-confctl-")
		}
	}
	return nil
}
