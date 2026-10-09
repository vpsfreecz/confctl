package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func safePath(root, path string) (string, error) {
	if filepath.IsAbs(path) || path == "" || path == ".." || strings.HasPrefix(filepath.Clean(path), "../") {
		return "", fmt.Errorf("unsafe fixture path %q", path)
	}
	dst := filepath.Join(root, path)
	// Do not follow a fixture symlink through a parent when initializing/writing.
	for p := filepath.Dir(dst); p != root; p = filepath.Dir(p) {
		if i, e := os.Lstat(p); e == nil && i.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink parent %s", p)
		}
	}
	return dst, nil
}
func WriteFiles(root string, files map[string]File) error {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := files[k]
		dst, e := safePath(root, k)
		if e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
			return e
		}
		if f.Link != "" {
			e = os.Symlink(f.Link, dst)
		} else {
			m := os.FileMode(f.Mode)
			if m == 0 {
				m = 0644
			}
			e = os.WriteFile(dst, []byte(strings.ReplaceAll(f.Text, "${ROOT}", root)), m)
		}
		if e != nil {
			return e
		}
	}
	return nil
}
func Snapshot(root string) (map[string]File, error) {
	out := map[string]File{}
	e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		i, e := d.Info()
		if e != nil {
			return e
		}
		f := File{Mode: uint32(i.Mode())}
		if d.Type()&os.ModeSymlink != 0 {
			f.Link, e = os.Readlink(path)
		} else if !d.IsDir() {
			var b []byte
			b, e = os.ReadFile(path)
			f.Text = string(b)
		}
		out[rel] = f
		return e
	})
	return out, e
}
func gitState(root, git string) map[string]string {
	out := map[string]string{}
	if git == "" {
		return out
	}
	for _, a := range [][]string{{"status", "--porcelain=v1", "--untracked-files=all"}, {"show-ref"}, {"diff", "--cached", "--binary"}, {"diff", "--binary"}} {
		c := exec.Command(git, a...)
		c.Dir = root
		b, e := c.CombinedOutput()
		out[strings.Join(a, " ")] = string(b)
		if e != nil {
			out[strings.Join(a, " ")] += "[" + e.Error() + "]"
		}
	}
	return out
}
func Run(ctx context.Context, c Case, binary, tools, evidence, registry, hookDriver string) (Observation, string, error) {
	return RunCached(ctx, c, binary, tools, evidence, registry, hookDriver, "")
}
func RunCached(ctx context.Context, c Case, binary, tools, evidence, registry, hookDriver, cacheRoot string) (Observation, string, error) {
	var o Observation
	o.Schema = 1
	o.Case = c.ID
	o.ContractRevision = OracleRevision
	o.SourceRevision = OracleRevision
	if strings.Contains(binary, "confctl-go-prototype") || (c.Hook != "" && registry != "") {
		o.SourceRevision = "experimental-working-tree:" + os.Getenv("COMPAT_CANDIDATE_REVISION")
	}
	root, e := os.MkdirTemp(evidence, c.ID+"-")
	if e != nil {
		return o, "", e
	}
	stateRoot := root
	if cacheRoot != "" {
		stateRoot = cacheRoot
	}
	config := filepath.Join(stateRoot, "config")
	if e = os.MkdirAll(config, 0755); e != nil {
		return o, root, e
	}
	// Restore mutation inputs between pairs while preserving the application cache.
	for _, p := range []string{"configs/node/kernels.json", "cluster/netbootable.nix"} {
		if _, ok := c.Files[p]; !ok {
			dst, err := safePath(config, p)
			if err != nil {
				return o, root, err
			}
			if err = os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return o, root, err
			}
		}
	}
	if e = WriteFiles(config, c.Files); e != nil {
		return o, root, e
	}
	casePath := filepath.Join(root, "case.json")
	b, _ := json.Marshal(c)
	if e = os.WriteFile(casePath, b, 0644); e != nil {
		return o, root, e
	}
	plan := filepath.Join(root, "request-plan")
	if e = PreparePlan(plan, c); e != nil {
		return o, root, e
	}
	trace := filepath.Join(root, "trace")
	_ = os.MkdirAll(trace, 0755)
	home := filepath.Join(stateRoot, "home")
	_ = os.MkdirAll(home, 0755)
	tmp := filepath.Join(root, "tmp")
	_ = os.MkdirAll(tmp, 0755)
	env := map[string]string{"HOME": home, "TMPDIR": tmp, "PWD": config, "PATH": tools, "CONFCTL_TTY": "0", "NO_COLOR": "1", "PAGER": "", "LANG": "C.UTF-8", "COMPAT_CASE": casePath, "COMPAT_ROOT": config, "COMPAT_TRACE": trace, "COMPAT_PLAN": plan}
	for k, v := range c.Env {
		env[k] = strings.ReplaceAll(v, "${ROOT}", config)
	}
	if registry != "" && c.Extensions {
		env["CONFCTL_EXTENSION_REGISTRY"] = registry
		env["CONFCTL_EXTENSION_ROOT"] = config
	}
	// The pinned scripts are copied verbatim by the caller for the Ruby baseline.
	if c.Extensions {
		source := os.Getenv("COMPAT_SITE_SOURCE")
		if source == "" {
			return o, root, fmt.Errorf("extension Ruby cases require COMPAT_SITE_SOURCE")
		}
		for _, s := range []string{"discover_netbootable.rb", "runtime_kernels.rb"} {
			b, e := os.ReadFile(filepath.Join(source, "scripts", s))
			if e != nil {
				return o, root, e
			}
			if e = WriteFiles(config, map[string]File{"scripts/" + s: {Text: string(b)}}); e != nil {
				return o, root, e
			}
		}
	}
	o.ConfigRoot = config
	o.RunRoot = root
	o.Before, e = Snapshot(config)
	if e != nil {
		return o, root, e
	}
	o.GitBefore = gitState(config, os.Getenv("COMPAT_REAL_GIT"))
	argv := c.Argv
	if c.Hook != "" {
		if hookDriver == "" {
			return o, root, fmt.Errorf("hook-driver required for hook cases")
		}
		binary = hookDriver
		argv = []string{c.Hook, strings.Join(c.Selected, ",")}
	}
	o.Executable = binary
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		o.Executable = resolved
	}
	if bytes, err := os.ReadFile(binary); err == nil {
		o.ExecutableSHA256 = fmt.Sprintf("%x", sha256.Sum256(bytes))
	}
	o.FixtureSHA256 = c.FixtureDigest
	if o.FixtureSHA256 == "" {
		o.FixtureSHA256 = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.DeadlineMS)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, argv...)
	cmd.Dir = config
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tree := newOwnedTree()
	cmd.Cancel = func() error {
		tree.signal(syscall.SIGTERM)
		go func() { time.Sleep(250 * time.Millisecond); tree.signal(syscall.SIGKILL) }()
		return nil
	}
	cmd.WaitDelay = 500 * time.Millisecond
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader(c.Stdin)
	start := time.Now()
	var err error
	var captureErr error
	if c.TTY {
		err, captureErr = runPTY(ctx, cmd, c.Stdin, &stdout, &stderr, tree)
	} else {
		err = cmd.Start()
		if err == nil {
			tree.start(cmd.Process.Pid)
			err = cmd.Wait()
			captureErr = tree.finish(ctx.Err() != nil || err == exec.ErrWaitDelay)
		}
	}
	o.WallNS = time.Since(start).Nanoseconds()
	o.Stdout = stdout.String()
	o.Stderr = stderr.String()
	if cmd.ProcessState != nil {
		o.Exit = cmd.ProcessState.ExitCode()
		o.UserNS = cmd.ProcessState.UserTime().Nanoseconds()
		o.SystemNS = cmd.ProcessState.SystemTime().Nanoseconds()
		if r, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			o.MaxRSSKB = r.Maxrss
		}
		if s, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && s.Signaled() {
			o.Signal = s.Signal().String()
		}
	}
	if err != nil && cmd.ProcessState == nil {
		return o, root, err
	}
	o.After, e = Snapshot(config)
	if e != nil {
		return o, root, e
	}
	o.GitAfter = gitState(config, os.Getenv("COMPAT_REAL_GIT"))
	paths, _ := filepath.Glob(filepath.Join(trace, "*.json"))
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			return o, root, e
		}
		var v Event
		if e = json.Unmarshal(b, &v); e != nil {
			return o, root, e
		}
		o.Events = append(o.Events, v)
	}
	sort.Slice(o.Events, func(i, j int) bool { return o.Events[i].Start < o.Events[j].Start })
	b, _ = json.MarshalIndent(o, "", "  ")
	_ = os.WriteFile(filepath.Join(root, "raw.json"), b, 0644)
	for _, v := range o.Events {
		if v.Rule < 0 {
			return o, root, fmt.Errorf("unexpected process: %s %v (raw evidence %s)", v.Tool, v.Argv, root)
		}
	}
	instrumented := env["COMPAT_TRACE_MODE"] != "off"
	if c.ExpectedNoCalls && len(o.Events) != 0 {
		return o, root, fmt.Errorf("unexpected calls in no-call case")
	}
	if instrumented && len(c.Normalize.IndependentSSH) > 0 {
		if e = AssertSSHFlows(o.Events, c.Normalize.IndependentSSH); e != nil {
			return o, root, e
		}
	}
	if instrumented {
		e = AssertCausal(o.Events, c.Causal)
	}
	if e != nil {
		return o, root, e
	}
	b, _ = json.MarshalIndent(o, "", "  ")
	e = os.WriteFile(filepath.Join(root, "raw.json"), b, 0644)
	if err == exec.ErrWaitDelay {
		return o, root, fmt.Errorf("test driver inherited pipe cleanup: %w", err)
	}
	if ctx.Err() != nil {
		return o, root, fmt.Errorf("test driver deadline/cancellation: %w", ctx.Err())
	}
	if captureErr != nil {
		return o, root, captureErr
	}
	return o, root, e
}
