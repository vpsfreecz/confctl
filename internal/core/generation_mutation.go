package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/generation"
)

// The adapter owns selection and sequencing; Writer alone owns local paths and
// per-component lazy root resolution. Tests bind roots inside disposable trees.
type generationMutationDeps struct {
	Roots    func() (string, error)
	Load     generation.BuildLoader
	Statuses func([]Machine, []Machine) ([]Status, error)
	Delete   func(Machine, *generation.Host) error
	Settings func() (map[string]any, error)
	Now      func() generation.Timestamp
	GC       func([]gcTask, *big.Int) error
}

func generationMutationNow() generation.Timestamp {
	// Parse the clock's valid ISO representation through the existing timestamp
	// owner, preserving fractions without another epoch/calendar implementation.
	stamp, _ := generation.ParseISO8601(time.Now().UTC().Format(time.RFC3339Nano), time.UTC)
	return stamp
}

func (e *Engine) generationMutationDependencies(stdout, stderr io.Writer) generationMutationDeps {
	return generationMutationDeps{
		Roots: e.generationRootDir,
		Load: func(host string) (*generation.BuildList, error) {
			if err := e.Context.Err(); err != nil {
				return nil, err
			}
			return generation.LoadBuildList(e.Root, host, generation.Options{Warn: e.generationWarning(stderr)})
		},
		Statuses: e.generationStatuses, Delete: e.removeHostGeneration,
		Settings: e.Settings, Now: generationMutationNow,
		GC: func(tasks []gcTask, workers *big.Int) error {
			return runGenerationGC(e.Context, tasks, workers, e.generationGCDependencies(stdout, stderr))
		},
	}
}

func (e *Engine) RemoveGenerations(inv cli.Invocation, stdin, stdout, stderr *os.File) error {
	return e.removeGenerations(inv, stdin, stdout, e.generationMutationDependencies(stdout, stderr))
}

func (e *Engine) RotateGenerations(inv cli.Invocation, stdin, stdout, stderr *os.File) error {
	return e.rotateGenerations(inv, stdin, stdout, e.generationMutationDependencies(stdout, stderr))
}

func (e *Engine) generationMutationSelection(inv cli.Invocation) ([]Machine, []Machine, error) {
	if err := e.Context.Err(); err != nil {
		return nil, nil, err
	}
	all, err := e.Machines(false)
	if err != nil {
		return nil, nil, err
	}
	var pattern *string
	if len(inv.Args) > 0 {
		pattern = &inv.Args[0]
	}
	opts := handlerOptions(inv)
	ms, err := Select(all, pattern, opts.Attrs, opts.Tags, "all")
	if err != nil {
		for _, filter := range opts.Attrs {
			if !strings.Contains(filter, "=") {
				err = fmt.Errorf("Invalid filter ''")
				break
			}
		}
		return nil, nil, &generationArgumentError{err}
	}
	return ms, all, nil
}

func generationWrite(out io.Writer, text string) error {
	n, err := io.WriteString(out, text)
	if err == nil && n != len(text) {
		return io.ErrShortWrite
	}
	return err
}

func generationConfirm(ctx context.Context, inv cli.Invocation, in io.Reader, out io.Writer, text string) error {
	if inv.Options["yes"].Value.Bool {
		return nil
	}
	if err := generationWrite(out, text); err != nil {
		return err
	}
	_, line := commandInput(in)
	return testConnectionConfirm(ctx, line, out)
}

type generationRemoval struct {
	host, name, kind string
	id               *big.Int
	build            *generation.Build
	remote           *generation.Host
}

func (e *Engine) removeGenerations(inv cli.Invocation, in io.Reader, out io.Writer, d generationMutationDeps) error {
	ms, all, err := e.generationMutationSelection(inv)
	if err != nil {
		return err
	}
	remote := map[string]*generation.HostList{}
	if inv.Options["remote"].Value.Bool {
		statuses, err := d.Statuses(ms, all)
		if err != nil {
			return err
		}
		for _, st := range statuses {
			remote[st.Machine.Name] = st.Generations
		}
	}
	hosts := make([]string, len(ms))
	for i, m := range ms {
		hosts[i] = m.Name
	}
	var pattern *string
	if len(inv.Args) > 1 {
		pattern = &inv.Args[1]
	}
	list, err := generation.SelectGenerations(hosts, generation.SelectionOptions{
		Local: inv.Options["local"].Value.Bool, Remote: inv.Options["remote"].Value.Bool,
		Pattern: pattern, Match: Match, Now: d.Now,
	}, func(host string) (*generation.HostList, error) { return remote[host], nil }, d.Load)
	if err != nil {
		return err
	}
	if len(list.Generations) == 0 {
		return generationWrite(out, "No generations found\n")
	}
	enableGC := generationPostGC(inv)
	gcText := "no"
	if enableGC {
		gcText = "yes"
	}
	if err := generationConfirm(e.Context, inv, in, out, "The following generations will be removed:\n"+generationTable(list)+"\nGarbage collection: "+gcText+"\n"); err != nil {
		return err
	}
	rows := make([]generationRemoval, len(list.Generations))
	for i, g := range list.Generations {
		rows[i] = generationRemoval{g.Host, g.Name, g.PresenceString(), g.ID, g.BuildGeneration, g.HostGeneration}
	}
	changed, err := e.removeGenerationRows(rows, ms, out, d)
	if err != nil || !enableGC {
		return err
	}
	tasks, err := generationGCTargets(ms, all, changed, false, nil)
	if err != nil {
		return err
	}
	return d.GC(tasks, generationGCRequest(inv)) // rm deliberately reaches empty GC.
}

func generationPostGC(inv cli.Invocation) bool {
	return inv.Options["remote"].Value.Bool && inv.Options["collect-garbage"].Value.Bool
}

func generationGCRequest(inv cli.Invocation) *big.Int {
	v := inv.Options["max-concurrent-gc"].Value
	return new(big.Int).Set(&v.Integer)
}

func (e *Engine) removeGenerationRows(rows []generationRemoval, ms []Machine, out io.Writer, d generationMutationDeps) (map[string]bool, error) {
	changed := map[string]bool{}
	var writer *generation.Writer
	for _, row := range rows {
		if err := e.Context.Err(); err != nil {
			return changed, err
		}
		if err := generationWrite(out, fmt.Sprintf("Removing %s generation %s@%s\n", row.kind, row.host, row.name)); err != nil {
			return changed, err
		}
		if row.build != nil {
			if writer == nil {
				var err error
				writer, err = generation.NewWriter(e.Root, d.Roots)
				if err != nil {
					return changed, err
				}
			}
			if err := writer.Remove(row.build); err != nil {
				return changed, err
			}
		}
		if row.remote != nil {
			if err := e.Context.Err(); err != nil {
				return changed, err
			}
			var machine Machine
			for _, m := range ms {
				if m.Name == row.host {
					machine = m
					break
				}
			}
			if err := d.Delete(machine, row.remote); err != nil {
				return changed, err
			}
		}
		changed[row.host] = true
	}
	return changed, nil
}

func (e *Engine) removeHostGeneration(machine Machine, host *generation.Host) error {
	connection, err := statusConnection(machine, e.Inventory)
	if err != nil {
		return err
	}
	command := "nix-env"
	if machine.CarrierName != nil {
		command = "carrier-env"
	}
	argv, err := SSHArgs(connection, []string{command, "-p", host.Profile, "--delete-generations", host.ID.String()})
	if err != nil {
		return err
	}
	r, err := e.Process(argv, nil)
	if err == nil && (r.ExitCode != 0 || r.Signal != "") {
		err = commandError(argv, r)
	}
	return err
}

func generationRotationPolicy(machine Machine, global map[string]any, key string) (generation.RotationPolicy, error) {
	var policy generation.RotationPolicy
	for _, field := range []struct {
		name string
		dst  *big.Int
	}{{"min", &policy.Min}, {"max", &policy.Max}, {"maxAge", &policy.MaxAgeSeconds}} {
		value := nested(machine.Attributes, key, field.name)
		if !truth(value) {
			value = nested(global, key, field.name)
		}
		n, ok := value.(json.Number)
		if !ok {
			return policy, fmt.Errorf("%s.%s for %s is not an integer", key, field.name, machine.Name)
		}
		if _, ok := field.dst.SetString(string(n), 10); !ok {
			return policy, fmt.Errorf("%s.%s for %s is not an integer", key, field.name, machine.Name)
		}
	}
	return policy, nil
}

func (e *Engine) rotateGenerations(inv cli.Invocation, in io.Reader, out io.Writer, d generationMutationDeps) error {
	ms, all, err := e.generationMutationSelection(inv)
	if err != nil {
		return err
	}
	rows := []generationRemoval{}
	if inv.Options["remote"].Value.Bool {
		settings, err := d.Settings()
		if err != nil {
			return err
		}
		statuses, err := d.Statuses(ms, all)
		if err != nil {
			return err
		}
		for _, st := range statuses {
			if st.Generations == nil {
				continue
			}
			policy, err := generationRotationPolicy(st.Machine, settings, "hostGenerations")
			if err != nil {
				return err
			}
			entries := make([]generation.RotationEntry, len(st.Generations.Generations))
			for i, g := range st.Generations.Generations {
				entries[i] = generation.RotationEntry{Date: g.Date, Current: g.Current}
			}
			for _, i := range generation.PlanRotation(entries, policy, d.Now) {
				g := st.Generations.Generations[i]
				rows = append(rows, generationRemoval{host: g.Host, name: g.ApproxName(), kind: "host", id: g.ID, remote: g})
			}
		}
	}
	if inv.Options["local"].Value.Bool || !inv.Options["remote"].Value.Bool {
		settings, err := d.Settings()
		if err != nil {
			return err
		}
		for _, m := range ms {
			if err := e.Context.Err(); err != nil {
				return err
			}
			list, err := d.Load(m.Name)
			if err != nil {
				return err
			}
			if err := list.RequireResolvedCurrent(); err != nil {
				return err
			}
			policy, err := generationRotationPolicy(m, settings, "buildGenerations")
			if err != nil {
				return err
			}
			entries := make([]generation.RotationEntry, len(list.Builds))
			for i, g := range list.Builds {
				entries[i] = generation.RotationEntry{Date: g.Date, Current: g.Current}
			}
			for _, i := range generation.PlanRotation(entries, policy, d.Now) {
				g := list.Builds[i]
				rows = append(rows, generationRemoval{host: g.Host, name: g.Name, kind: "build", build: g})
			}
		}
	}
	if len(rows) == 0 {
		return generationWrite(out, "No generations to delete\n")
	}
	enableGC := generationPostGC(inv)
	gcText := "no"
	if enableGC {
		gcText = "when enabled in configuration"
	}
	table := make([]map[string]any, len(rows))
	for i, row := range rows {
		var id any
		if row.id != nil {
			id = row.id.String()
		}
		table[i] = map[string]any{"host": row.host, "name": row.name, "type": row.kind, "id": id}
	}
	if err := generationConfirm(e.Context, inv, in, out, "The following generations will be removed:\n"+Table(table, []string{"host", "name", "type", "id"}, true)+"\nGarbage collection: "+gcText+"\n"); err != nil {
		return err
	}
	changed, err := e.removeGenerationRows(rows, ms, out, d)
	if err != nil || !enableGC {
		return err
	}
	settings, err := d.Settings()
	if err != nil {
		return err
	}
	tasks, err := generationGCTargets(ms, all, changed, true, settings)
	if err != nil || len(tasks) == 0 {
		return err
	}
	return d.GC(tasks, generationGCRequest(inv))
}

func generationGCTargets(ms, all []Machine, changed map[string]bool, rotate bool, settings map[string]any) ([]gcTask, error) {
	tasks := []gcTask{}
	seen := map[string]bool{}
	for _, m := range ms {
		if !changed[m.Name] {
			continue
		}
		effective, err := statusConnection(m, all)
		if err != nil {
			return nil, err
		}
		if effective.Target.Host == nil || seen[effective.Name] {
			continue
		}
		// This source lookup deliberately uses buildGenerations on the
		// effective machine and hostGenerations globally, with Ruby OR.
		if rotate && !truth(nested(effective.Attributes, "buildGenerations", "collectGarbage")) && !truth(nested(settings, "hostGenerations", "collectGarbage")) {
			continue
		}
		seen[effective.Name] = true
		tasks = append(tasks, gcTask{Host: effective.Name, Machine: effective})
	}
	return tasks, nil
}
