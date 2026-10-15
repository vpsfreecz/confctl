package harness

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func matches(e Event, s Selector) bool {
	if e.Tool != s.Tool || (s.Host != "" && e.Host != s.Host) {
		return false
	}
	if s.StdinContains != "" && !strings.Contains(e.Stdin, s.StdinContains) {
		return false
	}
	if s.Occurrence != 0 && e.Occurrence != s.Occurrence {
		return false
	}
	if s.Contains != "" {
		for _, a := range e.Argv {
			if strings.Contains(a, s.Contains) {
				return true
			}
		}
		return false
	}
	return true
}
func AssertCausal(events []Event, constraints []Constraint) error {
	for _, c := range constraints {
		before, after := []Event{}, []Event{}
		for _, e := range events {
			if matches(e, c.Before) {
				before = append(before, e)
			}
			if matches(e, c.After) {
				after = append(after, e)
			}
		}
		if len(before) == 0 || len(after) == 0 {
			return fmt.Errorf("causal constraint lacks events: %#v", c)
		}
		for _, b := range before {
			for _, a := range after {
				if b.End > a.Start {
					return fmt.Errorf("causal violation: %s %v not complete before %s %v", b.Tool, b.Argv, a.Tool, a.Argv)
				}
			}
		}
	}
	return nil
}
func NextOccurrence(trace, key string) (int, error) {
	f, e := os.OpenFile(filepath.Join(trace, key+".count"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return 0, e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	b, e := io.ReadAll(f)
	if e != nil {
		return 0, e
	}
	n := 0
	_, _ = fmt.Sscanf(string(b), "%d", &n)
	n++
	if e = f.Truncate(0); e != nil {
		return 0, e
	}
	if _, e = f.Seek(0, 0); e != nil {
		return 0, e
	}
	_, e = fmt.Fprint(f, n)
	return n, e
}

// SSHFlow declares one machine/profile's sequential calls. Identical uptime
// requests may be assigned to either profile, but no event may satisfy two steps.
type SSHFlow struct {
	Name  string     `json:"name"`
	Steps []Selector `json:"steps"`
}

func AssertSSHFlows(events []Event, flows []SSHFlow) error {
	if len(flows) == 0 {
		return fmt.Errorf("independent SSH requires declared flows")
	}
	var steps []Selector
	var prior []int
	for _, f := range flows {
		if f.Name == "" || len(f.Steps) == 0 {
			return fmt.Errorf("invalid SSH flow")
		}
		base := len(steps)
		for i, s := range f.Steps {
			if s.Tool != "ssh" || s.Host == "" {
				return fmt.Errorf("flow requires SSH host")
			}
			steps = append(steps, s)
			if i == 0 {
				prior = append(prior, -1)
			} else {
				prior = append(prior, base+i-1)
			}
		}
	}
	ssh := []Event{}
	for _, e := range events {
		if e.Tool == "ssh" {
			ssh = append(ssh, e)
		}
	}
	if len(ssh) != len(steps) {
		return fmt.Errorf("SSH flow coverage: %d events, %d declared steps", len(ssh), len(steps))
	}
	assigned := make([]int, len(steps))
	used := make([]bool, len(ssh))
	var assign func(int) bool
	assign = func(i int) bool {
		if i == len(steps) {
			return true
		}
		for j, e := range ssh {
			if used[j] || !matches(e, steps[i]) {
				continue
			}
			if prev := prior[i]; prev >= 0 && ssh[assigned[prev]].End > e.Start {
				continue
			}
			assigned[i] = j
			used[j] = true
			if assign(i + 1) {
				return true
			}
			used[j] = false
		}
		return false
	}
	if !assign(0) {
		return fmt.Errorf("SSH profile causal order violated")
	}
	return nil
}
