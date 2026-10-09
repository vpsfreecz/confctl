// compat-bench measures installed binaries after an instrumented parity gate.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/vpsfreecz/confctl/compat/harness"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type sample struct {
	Run                int      `json:"run"`
	Size               int      `json:"machines"`
	Cell               string   `json:"cell"`
	Cache              string   `json:"cache"`
	Pair               int      `json:"pair"`
	Order              []string `json:"order"`
	Tool               string   `json:"tool"`
	WallNS             int64    `json:"wall_ns"`
	UserNS             int64    `json:"user_ns"`
	SystemNS           int64    `json:"system_ns"`
	MaxRSSKB           int64    `json:"max_rss_kb"`
	Exit               int      `json:"exit"`
	Signal             string   `json:"signal"`
	Evidence           string   `json:"evidence"`
	Error              string   `json:"error,omitempty"`
	Mismatch           string   `json:"mismatch,omitempty"`
	Instrumentation    string   `json:"instrumentation"`
	ExternalRequests   *int     `json:"external_tool_requests"`
	ExternalSpanNS     *int64   `json:"first_external_start_to_last_completion_ns"`
	ExtensionProcesses *int     `json:"extension_processes"`
}

func checksum(path string) string {
	b, e := os.ReadFile(path)
	if e != nil {
		return "unavailable: " + e.Error()
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func write(path string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	if e == nil {
		e = os.WriteFile(path, b, 0644)
	}
	if e != nil {
		panic(e)
	}
}
func values(s string, allowed map[string]bool) []string {
	a := strings.Split(s, ",")
	seen := map[string]bool{}
	for _, v := range a {
		if !allowed[v] || seen[v] {
			panic("invalid/duplicate selection: " + v)
		}
		seen[v] = true
	}
	return a
}
func allowed(a ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	return m
}
func command(name string, a ...string) string {
	b, e := exec.Command(name, a...).CombinedOutput()
	if e != nil {
		return string(b) + "\n[unavailable: " + e.Error() + "]"
	}
	return string(b)
}
func percentile(a []int64, p int) int64 {
	b := append([]int64(nil), a...)
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	i := (len(b)*p+99)/100 - 1
	if i < 0 {
		i = 0
	}
	return b[i]
}
func bootMedian(a []int64, r *rand.Rand) []int64 {
	out := make([]int64, 2000)
	v := make([]int64, len(a))
	for i := range out {
		for j := range v {
			v[j] = a[r.Intn(len(a))]
		}
		out[i] = percentile(v, 50)
	}
	return out
}
func main() {
	ruby := flag.String("ruby", "", "installed fixture oracle executable")
	native := flag.String("go", "", "installed fixture candidate executable")
	rubyHooks := flag.String("ruby-hooks", "", "installed original hook driver")
	goHooks := flag.String("go-hooks", "", "installed native hook driver")
	registry := flag.String("registry", "", "candidate registry")
	tools := flag.String("tools", "", "same fixture bin directory")
	output := flag.String("output", "", "new evidence directory")
	samples := flag.Int("samples", 30, "pairs per selected cell, minimum 30")
	runs := flag.Int("runs", 2, "independent runs (two required for recommendation)")
	cellsFlag := flag.String("cells", "help,version,help-extensions,version-extensions,ls-full,ls-exact,ls-filter,ls-columns,status-none,runtime-kernels,netboot-hook", "comma separated cells")
	scalesFlag := flag.String("scales", "1,10,100,1000", "comma separated scales")
	cachesFlag := flag.String("cache", "warm,fresh-application-cache", "comma separated cache modes")
	seed := flag.Int64("seed", time.Now().UnixNano(), "paired order seed")
	revision := flag.String("candidate-revision", "", "actual commit plus dirty marker")
	sourceHash := flag.String("candidate-source-hash", "", "candidate source tree digest")
	flag.Parse()
	if *ruby == "" || *native == "" || *tools == "" || *output == "" || *registry == "" || *rubyHooks == "" || *goHooks == "" || *samples < 30 || *runs < 1 || *revision == "" || *sourceHash == "" {
		flag.Usage()
		os.Exit(2)
	}
	cells := values(*cellsFlag, allowed("help", "version", "help-extensions", "version-extensions", "ls-full", "ls-exact", "ls-filter", "ls-columns", "status-none", "runtime-kernels", "netboot-hook"))
	sizes := values(*scalesFlag, allowed("1", "10", "100", "1000"))
	caches := values(*cachesFlag, allowed("warm", "fresh-application-cache"))
	if e := os.Mkdir(*output, 0755); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(2)
	}
	host, _ := os.Hostname()
	paths := map[string]string{"oracle": *ruby, "candidate": *native, "oracle_hook_driver": *rubyHooks, "candidate_hook_driver": *goHooks, "registry": *registry, "fixture_tool": filepath.Join(*tools, "fixture-tool")}
	hashes := map[string]string{}
	stores := []string{}
	for k, p := range paths {
		hashes[k] = checksum(p)
		if q, e := filepath.EvalSymlinks(p); e == nil {
			paths[k] = q
			parts := strings.Split(q, "/")
			if len(parts) > 3 && parts[1] == "nix" && parts[2] == "store" {
				stores = append(stores, "/nix/store/"+parts[3])
			}
		}
	}
	manifest := map[string]any{"schema": 1, "tier": "fixture-process", "contract_revision": harness.OracleRevision, "site_revision": harness.SiteRevision, "candidate_revision": *revision, "candidate_source_hash": *sourceHash, "paths": paths, "sha256": hashes, "toolchain": runtime.Version(), "host": host, "os": runtime.GOOS, "arch": runtime.GOARCH, "seed": *seed, "samples_per_cell": *samples, "runs": *runs, "cells": cells, "scales": sizes, "cache_modes": caches, "warmup_pairs": 3, "resources_before": harness.Resources(), "package_closure": command("nix", append([]string{"path-info", "--recursive", "--json"}, stores...)...), "nix_version": command("nix", "--version"), "openssh_version": command("ssh", "-V"), "git_version": command("git", "--version"), "go_version": command("go", "version"), "cache_policy": "warm retains HOME/configuration/application cache; mutation inputs reset before every invocation; fresh uses new HOME/application state; OS page cache is not flushed", "timing_scope": "wall includes command start/wait, ownedTree start/finish, recursive procfs observation every 5ms and cleanup; dispatcher process/semantic validation included; fixture preparation/state snapshot/normalization excluded; diagnostic dispatcher event writes/counters disabled in measured samples, ownership observer always active", "cpu_scope": "Linux child rusage includes CLI and waited descendants; excludes controller CPU including ownership observer", "fixture_overhead": "compact per-tool/host/request plans compiled outside command timing; dispatcher executable startup/read/JSON/matching remains artificial external cost, not real Nix/SSH latency", "rss_scope": "Linux wait4 child rusage ru_maxrss maximum propagated waited descendant RSS; not live aggregate process tree peak", "counts_scope": "instrumented gates count external fixture requests; measured process/extension counts unavailable without profiling", "phase_scope": "first external start to last completion includes overlap and local gaps, never interpreted as removable local fraction", "concurrency": "machines.length; no worker cap", "policy": "successful cells retain process/logging behavior; explicit invocation cancellation has approved TERM then 2s KILL grace", "capture_policy": "test driver always cleans/reaps proven PID/starttime descendants; live work after root exit rejects capture; driver TERM escalates after 250ms, reap bound 1s; no candidate routine timeout added"}
	write(filepath.Join(*output, "manifest.json"), manifest)
	f, e := os.Create(filepath.Join(*output, "samples.jsonl"))
	if e != nil {
		panic(e)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	rng := rand.New(rand.NewSource(*seed))
	groups := map[string][]int64{}
	differences := map[string][]int64{}
	failures := 0
	for run := 1; run <= *runs; run++ {
		for _, size := range sizes {
			n, _ := strconv.Atoi(size)
			for _, cache := range caches {
				for _, cell := range cells {
					key := fmt.Sprintf("run%d/%d/%s/%s", run, n, cache, cell)
					cellRoot := filepath.Join(*output, strings.ReplaceAll(key, "/", "-"))
					if e := os.Mkdir(cellRoot, 0755); e != nil {
						panic(e)
					}
					write(filepath.Join(cellRoot, "resources-before.json"), harness.Resources())
					extensions := cell == "help-extensions" || cell == "version-extensions" || cell == "runtime-kernels" || cell == "netboot-hook"
					base := strings.TrimSuffix(cell, "-extensions")
					c := harness.Synthetic(n, base, extensions)
					expectedExit := 0
					if base == "version" {
						expectedExit = 1
					}
					write(filepath.Join(cellRoot, "case.json"), c)
					runPair := func(pair int, trace bool) ([]sample, bool) {
						order := []string{"ruby", "go"}
						if rng.Intn(2) == 1 {
							order[0], order[1] = order[1], order[0]
						}
						normalized := map[string]harness.Observation{}
						records := []sample{}
						good := true
						for _, name := range order {
							binary, reg, hook := *ruby, "", *rubyHooks
							if name == "go" {
								binary, reg, hook = *native, *registry, *goHooks
							}
							persistent := ""
							if cache == "warm" {
								persistent = filepath.Join(cellRoot, name+"-application")
								_ = os.MkdirAll(persistent, 0755)
							}
							c.Env["COMPAT_TRACE_MODE"] = "off"
							if trace {
								c.Env["COMPAT_TRACE_MODE"] = "on"
							}
							o, root, err := harness.RunCached(context.Background(), c, binary, *tools, cellRoot, reg, hook, persistent)
							record := sample{Run: run, Size: n, Cell: cell, Cache: cache, Pair: pair, Order: order, Tool: name, WallNS: o.WallNS, UserNS: o.UserNS, SystemNS: o.SystemNS, MaxRSSKB: o.MaxRSSKB, Exit: o.Exit, Signal: o.Signal, Evidence: root, Instrumentation: "fixture-diagnostic-trace-off; owned-tree-observer-on"}
							if trace {
								record.Instrumentation = "diagnostic-gate; owned-tree-observer-on"
								count := len(o.Events)
								record.ExternalRequests = &count
								if count > 0 {
									first, last := o.Events[0].Start, o.Events[0].End
									for _, event := range o.Events {
										if event.Start < first {
											first = event.Start
										}
										if event.End > last {
											last = event.End
										}
									}
									span := last - first
									record.ExternalSpanNS = &span
								}
							}
							if err == nil && (o.Exit != expectedExit || o.Signal != "") {
								err = fmt.Errorf("unexpected exit/signal: expected %d, got %d %s", expectedExit, o.Exit, o.Signal)
							}
							if err != nil {
								record.Error = err.Error()
								good = false
							}
							norm, nerr := harness.Normalize(o, c.Normalize, o.ConfigRoot)
							if nerr != nil {
								record.Error += " normalization: " + nerr.Error()
								good = false
							}
							normalized[name] = norm
							write(filepath.Join(root, "normalized.json"), norm)
							records = append(records, record)
						}
						if err := harness.Equal(normalized["ruby"], normalized["go"]); err != nil {
							good = false
							for i := range records {
								records[i].Mismatch = err.Error()
							}
						}
						for _, record := range records {
							_ = enc.Encode(record)
						}
						return records, good
					}
					_, gate := runPair(-4, true)
					if !gate {
						failures++
						write(filepath.Join(cellRoot, "resources-after.json"), harness.Resources())
						continue
					}
					warmGood := true
					for pair := -3; pair < 0; pair++ {
						_, ok := runPair(pair, false)
						if !ok {
							failures++
							warmGood = false
						}
					}
					if warmGood {
						for pair := 0; pair < *samples; pair++ {
							records, ok := runPair(pair, false)
							if !ok {
								failures++
								continue
							}
							walls := map[string]int64{}
							for _, record := range records {
								groups[key+"/"+record.Tool] = append(groups[key+"/"+record.Tool], record.WallNS)
								walls[record.Tool] = record.WallNS
							}
							differences[key] = append(differences[key], walls["go"]-walls["ruby"])
						}
					}
					_ = f.Sync()
					write(filepath.Join(cellRoot, "resources-after.json"), harness.Resources())
					fmt.Println(key)
				}
			}
		}
	}
	summary := map[string]any{}
	paired := map[string]any{}
	for key, a := range groups {
		bootstrap := bootMedian(a, rng)
		summary[key] = map[string]any{"accepted_n": len(a), "median_ns": percentile(a, 50), "p95_ns": percentile(a, 95), "median_ci95_ns": []int64{percentile(bootstrap, 3), percentile(bootstrap, 98)}}
	}
	for key, a := range differences {
		bootstrap := bootMedian(a, rng)
		paired[key] = map[string]any{"accepted_pairs": len(a), "median_go_minus_ruby_ns": percentile(a, 50), "paired_bootstrap_median_ci95_ns": []int64{percentile(bootstrap, 3), percentile(bootstrap, 98)}, "bootstrap_resamples": 2000}
	}
	write(filepath.Join(*output, "summary.json"), map[string]any{"distributions": summary, "paired_differences": paired, "failed_pairs_or_gates": failures, "resources_after": harness.Resources(), "interpretation": "only pairs passing exit, raw causal diagnostic gate, normalized process/UI/state parity enter distributions; one run is incomplete evidence; fixture timings cannot establish real SSH/Nix gains"})
	if failures > 0 {
		fmt.Fprintln(os.Stderr, "failed pairs/gates retained:", failures)
		os.Exit(1)
	}
}
