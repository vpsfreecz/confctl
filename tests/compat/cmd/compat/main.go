package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/vpsfreecz/confctl/compat/harness"
	"os"
	"path/filepath"
)

func main() {
	cases := flag.String("cases", "tests/compat/fixtures/*.json", "case glob")
	binary := flag.String("binary", "", "installed candidate/oracle path")
	tools := flag.String("tools", "", "fixture bin directory")
	evidence := flag.String("evidence", "", "existing output directory (raw captures retained)")
	registry := flag.String("registry", "", "candidate static registry")
	hook := flag.String("hook-driver", "", "fixture-only hook driver")
	observe := flag.Bool("observe", false, "capture Ruby observations without changing expected files")
	flag.Parse()
	if *binary == "" || *tools == "" || *evidence == "" {
		flag.Usage()
		os.Exit(2)
	}
	paths, e := filepath.Glob(*cases)
	if e != nil || len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "no cases", e)
		os.Exit(2)
	}
	failures := 0
	for _, p := range paths {
		c, e := harness.Load(p)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			failures++
			continue
		}
		o, root, e := harness.Run(context.Background(), c, *binary, *tools, *evidence, *registry, *hook)
		if e != nil {
			fmt.Fprintln(os.Stderr, c.ID, e)
			failures++
			continue
		}
		n, e := harness.Normalize(o, c.Normalize, filepath.Join(root, "config"))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			failures++
			continue
		}
		b, _ := json.MarshalIndent(n, "", "  ")
		_ = os.WriteFile(filepath.Join(root, "normalized.json"), b, 0644)
		if *observe {
			fmt.Printf("OBSERVED %s %s\n", c.ID, root)
			continue
		}
		expected := filepath.Join(filepath.Dir(p), c.Expected)
		b, e = os.ReadFile(expected)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			failures++
			continue
		}
		var want harness.Observation
		if e = json.Unmarshal(b, &want); e == nil {
			e = harness.Equal(want, n)
		}
		if e != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s: %v (evidence %s)\n", c.ID, e, root)
			failures++
		} else {
			fmt.Printf("PASS %s %s\n", c.ID, root)
		}
	}
	if failures > 0 {
		os.Exit(1)
	}
}
