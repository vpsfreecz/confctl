// compat-real runs only inside the reviewed rootless fixture image.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vpsfreecz/confctl/compat/harness"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	Schema    int      `json:"schema"`
	Tier      string   `json:"tier"`
	Scales    []int    `json:"scales"`
	Workflows []string `json:"workflows"`
	RubyOnly  []string `json:"rubyOnly"`
	Samples   int      `json:"samplesPerCell"`
	Session   int      `json:"sessionIndex"`
	Seed      int64    `json:"pairOrderSeed"`
	Cache     string   `json:"cacheMode"`
	Trace     bool     `json:"trace"`
}

func save(path string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	if e == nil {
		e = os.WriteFile(path, b, 0644)
	}
	if e != nil {
		panic(e)
	}
}
func main() {
	if len(os.Args) != 2 || os.Getuid() != 0 || os.Getenv("CONFCTL_REAL_FIXTURE") != "1" {
		fmt.Fprintln(os.Stderr, "requires isolated fixture supervisor")
		os.Exit(2)
	}
	b, e := os.ReadFile(os.Args[1])
	if e != nil {
		panic(e)
	}
	var c config
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		panic(e)
	}
	if c.Schema != 1 || c.Tier != "real-local-container" || c.Samples < 30 || c.Session < 1 || (c.Cache != "warm" && c.Cache != "fresh-application-cache") {
		panic("invalid run contract")
	}
	if c.Trace {
		panic("timing run cannot enable tracing; use supervisor diagnostic trace command separately")
	}
	for _, n := range c.Scales {
		if n != 1 && n != 10 && n != 100 && n != 1000 {
			panic("invalid scale")
		}
	}
	for _, w := range c.Workflows {
		if w != "help" && w != "ls" && w != "status-none" {
			panic("unsupported real paired workflow")
		}
	}
	for _, w := range c.RubyOnly {
		if w != "status-current" && w != "status-cached-noop-build" && w != "build-cached-noop" {
			panic("unsupported Ruby-only workflow")
		}
	}
	out := filepath.Dir(os.Args[1])
	f, e := os.Create(filepath.Join(out, "real-samples.jsonl"))
	if e != nil {
		panic(e)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	rng := rand.New(rand.NewSource(c.Seed))
	failed := 0
	ruby, native := os.Getenv("CONFCTL_REAL_ORACLE"), os.Getenv("CONFCTL_REAL_NATIVE")
	if ruby == "" || native == "" {
		panic("package paths missing")
	}
	save(filepath.Join(out, "resources-before.json"), harness.Resources())
	norm := harness.Normalization{RootFields: []string{"stdout", "stderr"}, VolatileFields: []string{"wall_ns", "user_ns", "system_ns", "max_rss_kb"}, LogFields: []string{"filename-time", "command-uuid", "gli-parent-address", "process-duration", "config-root"}}
	for _, n := range c.Scales {
		for _, workflow := range c.Workflows {
			cell := fmt.Sprintf("%d-%s", n, workflow)
			save(filepath.Join(out, cell+"-resources-before.json"), harness.Resources())
			initial := filepath.Join("/work", fmt.Sprintf("config-%d", n))
			argv := []string{"--help"}
			if workflow == "ls" {
				argv = []string{"ls"}
			}
			if workflow == "status-none" {
				argv = []string{"status", "--yes", "--generation", "none"}
			}
			for pair := -3; pair < c.Samples; pair++ {
				order := []string{"ruby", "go"}
				if rng.Intn(2) == 1 {
					order[0], order[1] = order[1], order[0]
				}
				observations := map[string]harness.Observation{}
				records := []map[string]any{}
				for _, name := range order {
					binary := ruby
					if name == "go" {
						binary = native
					}
					home := filepath.Join("/work/homes", cell, name)
					if c.Cache != "warm" {
						home = filepath.Join(home, fmt.Sprint(pair))
					}
					if e = os.MkdirAll(home, 0700); e != nil {
						panic(e)
					}
					configRoot := filepath.Join("/work/application", cell, name)
					if c.Cache != "warm" {
						configRoot = filepath.Join(configRoot, fmt.Sprint(pair))
					}
					if _, err := os.Stat(configRoot); os.IsNotExist(err) {
						if err = os.MkdirAll(configRoot, 0755); err != nil {
							panic(err)
						}
						if b, err := exec.Command("/bin/cp", "-a", initial+"/.", configRoot).CombinedOutput(); err != nil {
							panic(string(b) + err.Error())
						}
					}
					env := []string{"USER=root", "HOME=" + home, "XDG_CACHE_HOME=" + home + "/.cache", "PWD=" + configRoot, "PATH=" + os.Getenv("PATH"), "NIX_REMOTE=local", "CONFCTL_SSH_CONFIG=/run/fixture/ssh_config", "CONFCTL_TTY=0", "NO_COLOR=1", "PAGER=", "TZ=UTC", "LANG=C.UTF-8"}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					o, first, err := harness.RunInstalled(ctx, binary, argv, configRoot, env)
					cancel()
					evidence := filepath.Join(out, fmt.Sprintf("%s-%d-%s.json", cell, pair, name))
					save(evidence, o)
					v, nerr := harness.Normalize(o, norm, configRoot)
					observations[name] = v
					record := map[string]any{"session": c.Session, "machines": n, "workflow": workflow, "cache": c.Cache, "pair": pair, "order": order, "tool": name, "wall_ns": o.WallNS, "first_stdout_ns": first, "user_ns": o.UserNS, "system_ns": o.SystemNS, "max_rss_kb": o.MaxRSSKB, "exit": o.Exit, "signal": o.Signal, "evidence": evidence, "process_counts": "unavailable in uninstrumented timing"}
					if err != nil || nerr != nil || o.Exit != 0 || o.Signal != "" {
						record["error"] = fmt.Sprint(err, nerr, " unexpected exit/signal ", o.Exit, o.Signal)
						failed++
					}
					records = append(records, record)
				}
				if err := harness.Equal(observations["ruby"], observations["go"]); err != nil {
					failed++
					for _, r := range records {
						r["mismatch"] = err.Error()
					}
				}
				for _, r := range records {
					_ = enc.Encode(r)
				}
			}
			save(filepath.Join(out, cell+"-resources-after.json"), harness.Resources())
		}
	}
	// Ruby-only existing-generation/no-op outputs are explicitly diagnostic.
	for _, w := range c.RubyOnly {
		argv := []string{"status", "--yes", "--generation", "current"}
		if w == "status-cached-noop-build" {
			argv = []string{"status", "--yes"}
		}
		if w == "build-cached-noop" {
			argv = []string{"build", "--yes", "--max-jobs", "0", "lab/nodes/node0001"}
		}
		for i := -3; i < c.Samples; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			o, first, err := harness.RunInstalled(ctx, ruby, argv, "/work/config-1", os.Environ())
			cancel()
			path := filepath.Join(out, fmt.Sprintf("ruby-%s-%d.json", w, i))
			save(path, o)
			_ = enc.Encode(map[string]any{"session": c.Session, "workflow": w, "pair": i, "tool": "ruby-only", "wall_ns": o.WallNS, "first_stdout_ns": first, "exit": o.Exit, "error": fmt.Sprint(err), "evidence": path})
			if err != nil || o.Exit != 0 {
				failed++
			}
		}
	}
	save(filepath.Join(out, "resources-after.json"), harness.Resources())
	save(filepath.Join(out, "real-result.json"), map[string]any{"failures": failed, "run": c, "timing_scope": "wall includes command start/wait, ownedTree start/finish, recursive procfs observation every 5ms and cleanup; preparation/state snapshot/normalization excluded; ownership observer remains active with diagnostic tracing off", "cpu_scope": "Linux child rusage includes CLI and waited descendants; excludes controller CPU including ownership observer", "limits": "one loopback SSH service, rootless namespace/vfs/service contention; no WAN/fleet/full OS deployment evidence; RSS is not live aggregate peak; fresh cache resets HOME/configuration application state from frozen seeded bytes, metadata store is preseeded"})
	if failed > 0 {
		os.Exit(1)
	}
}
