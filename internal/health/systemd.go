package health

import (
	"math/big"
	"strings"
)

func SystemdCheck(machine string, properties []Property, pattern *string) (Check, error) {
	if len(properties) == 0 {
		return Check{}, &SchemaError{machine, "systemd property group must not be empty"}
	}
	c := Check{Machine: machine, Remote: true, Argv: []string{"systemctl", "show"}, Pattern: pattern, Properties: append([]Property(nil), properties...)}
	descriptions := make([]string, len(properties))
	for i, property := range properties {
		if property.Timeout == nil || property.Cooldown == nil || property.Timeout.Sign() < 0 || property.Cooldown.Sign() < 0 {
			return Check{}, &SchemaError{machine, "systemd property timing must contain nonnegative seconds"}
		}
		if c.Timeout == nil || property.Timeout.Cmp(c.Timeout) < 0 {
			c.Timeout = new(big.Int).Set(property.Timeout)
		}
		if c.Cooldown == nil || property.Cooldown.Cmp(c.Cooldown) < 0 {
			c.Cooldown = new(big.Int).Set(property.Cooldown)
		}
		descriptions[i] = property.Name + "=" + property.Value
	}
	prefix := "systemd: "
	if pattern != nil {
		prefix = *pattern + ": "
		c.Argv = append(c.Argv, *pattern)
	}
	c.Description = prefix + strings.Join(descriptions, ", ")
	return c, nil
}

func systemdFailures(check Check, result CommandResult) []Failure {
	if result.Status != 0 {
		return []Failure{{Kind: "systemd-status", Status: result.Status}}
	}
	properties := map[string]string{}
	// tty-command Result#each uses split("\n"): trailing empty fields disappear.
	// Empty/no-equals lines are ignored anyway. Values retain whitespace and CR.
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := line[:eq]
		if eq == 0 {
			// Ruby line[0..-1] is the entire line, not the empty key.
			key = line
		}
		properties[key] = line[eq+1:]
	}
	var failures []Failure
	for _, property := range check.Properties {
		value, present := properties[property.Name]
		if !present {
			failures = append(failures, Failure{Kind: "missing-property", Property: property.Name})
		} else if value != property.Value {
			failures = append(failures, Failure{Kind: "property", Property: property.Name, Actual: value, Want: property.Value})
		}
	}
	return failures
}
