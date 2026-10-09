package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

func Synthetic(n int, cell string, extensions bool) Case {
	settings := map[string]any{"list": map[string]any{"columns": []string{"name", "spin", "managed", "host.target"}}, "nix": map[string]any{"maxJobs": "auto", "impureEval": true, "legacyNixPath": false}}
	c := Case{Schema: 1, ID: fmt.Sprintf("bench-%s-%d", cell, n), Tier: "fixture-process", Sources: []Source{{Revision: OracleRevision, Path: "lib/confctl/cli/cluster.rb", Lines: "15-207"}}, Env: map[string]string{}, Files: map[string]File{"flake.nix": {Text: "{ outputs = _: {}; }\n"}, "cluster/cluster.nix": {Text: "[]\n"}, "configs/node/.keep": {Text: ""}}, DeadlineMS: 120000, Extensions: extensions}
	if extensions {
		c.Sources = append(c.Sources, Source{Revision: SiteRevision, Path: "scripts/runtime_kernels.rb", Lines: "7-126"})
	}
	machines := []string{}
	keys := map[string]string{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("site/nodes/node%04d", i)
		key := fmt.Sprintf("m%d", i)
		host := fmt.Sprintf("node%04d.example", i)
		meta := map[string]any{"managed": true, "spin": "vpsadminos", "host": map[string]any{"target": host, "port": 22}, "tags": []string{"node", "fixture"}, "custom": map[string]any{"location": "synthetic", "index": i}, "netboot": map[string]any{"enable": true}, "healthChecks": map[string]any{}}
		machine := map[string]any{"name": name, "key": key, "clusterName": name, "metaConfig": meta}
		b, _ := json.Marshal(machine)
		keybytes, _ := json.Marshal(name)
		machines = append(machines, string(keybytes)+":"+string(b))
		keys[name] = key
		c.Rules = append(c.Rules, Rule{Tool: "nix", Contains: ".#confctl.inputsInfo." + key, Stdout: "{\"nixpkgs\":\"fixture-revision\"}\n"}, Rule{Tool: "ssh", Host: host, Contains: "cat /proc/uptime", Stdout: "86401.0 1.0\n"}, Rule{Tool: "ssh", Host: host, ReadStdin: true, Contains: "bash --norc", Stdout: "/nix/store/fixture-system\n"}, Rule{Tool: "ssh", Host: host, Contains: "cat /etc/confctl/inputs-info.json", Stdout: "{\"nixpkgs\":\"fixture-revision\"}\n"}, Rule{Tool: "ssh", Host: host, Contains: "uname -r", Stdout: "6.12.35.extra\n"})
		if cell == "status-none" {
			c.Normalize.IndependentSSH = append(c.Normalize.IndependentSSH, SSHFlow{Name: name, Steps: []Selector{{Tool: "ssh", Host: host, Contains: "cat /proc/uptime"}, {Tool: "ssh", Host: host, Contains: "bash --norc", StdinContains: "realpath /nix/var/nix/profiles/system\n"}, {Tool: "ssh", Host: host, Contains: "cat /etc/confctl/inputs-info.json"}}})
		}
		c.Selected = append(c.Selected, name)
	}
	c.Files["fixtures/machines.json"] = File{Text: "{" + strings.Join(machines, ",") + "}"}
	b, _ := json.Marshal(settings)
	k, _ := json.Marshal(keys)
	c.Rules = append(c.Rules, Rule{Tool: "nix", Contains: ".#confctl.settings", Stdout: string(b) + "\n"}, Rule{Tool: "nix", Contains: ".#confctl.machinesJson", Stdout: "[{\"outputs\":{\"out\":\"${ROOT}/fixtures/machines.json\"}}]\n"}, Rule{Tool: "nix", Contains: ".#confctl.machineKeys", Stdout: string(k) + "\n"})
	switch cell {
	case "help":
		c.Argv = []string{"--help"}
	case "version":
		c.Argv = []string{"--version"}
	case "ls-full":
		c.Argv = []string{"ls"}
	case "ls-exact":
		c.Argv = []string{"ls", "site/nodes/node0000"}
	case "ls-filter":
		c.Argv = []string{"ls", "-a", "spin=vpsadminos", "-t", "node"}
	case "ls-columns":
		c.Argv = []string{"ls", "-H", "-o", "name,custom.location,custom.index"}
	case "status-none":
		c.Argv = []string{"status", "-g", "none", "-y"}
	case "runtime-kernels":
		c.Argv = []string{"runtime-kernels", "update", "-y"}
	case "netboot-hook":
		c.Hook = "rediscover.after-write"
	default:
		panic("unknown synthetic cell")
	}
	c.Normalize.RootFields = []string{"stdout", "stderr", "events.cwd", "events.argv", "events.stdin", "events.stdout", "events.stderr", "events.env.PWD"}
	c.Normalize.RunRootFields = []string{"events.env.HOME", "events.env.TMPDIR", "events.env.COMPAT_CASE"}
	c.Normalize.VolatileFields = []string{"events.pid", "events.start_ns", "events.end_ns", "wall_ns", "user_ns", "system_ns", "max_rss_kb"}
	c.Normalize.LogFields = []string{"filename-time", "command-uuid", "gli-parent-address", "process-duration", "config-root"}
	if cell == "status-none" || cell == "runtime-kernels" {
		c.Causal = []Constraint{{Before: Selector{Tool: "nix"}, After: Selector{Tool: "ssh"}}}
		c.Normalize.ConcurrentEvents = cell == "runtime-kernels"
	}
	return c
}
