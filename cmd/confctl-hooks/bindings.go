package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/vpsfreecz/confctl/internal/hookcheck"
)

type toolSpec struct{ Output, Program string }
type bindingSpec struct {
	Checker, Git, Lefthook, Nixfmt, Gofmt, Shell toolSpec
	Path                                         []string
	Launcher                                     string
}
type bindings struct {
	tools    hookcheck.ToolSet
	launcher string
}

func identify(spec toolSpec) (hookcheck.ToolIdentity, error) {
	resolved, err := filepath.EvalSymlinks(spec.Program)
	if err != nil {
		return hookcheck.ToolIdentity{}, err
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return hookcheck.ToolIdentity{}, err
	}
	return hookcheck.ToolIdentity{Output: spec.Output, Program: spec.Program, Resolved: resolved, SHA256: sha256.Sum256(b)}, nil
}

func loadBindings(path string) (bindings, error) {
	f, err := os.Open(path)
	if err != nil {
		return bindings{}, err
	}
	defer f.Close()
	var spec bindingSpec
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&spec); err != nil {
		return bindings{}, err
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return bindings{}, errors.New("trailing tool binding data")
	}
	b := bindings{launcher: spec.Launcher}
	for _, pair := range []struct {
		spec toolSpec
		dest *hookcheck.ToolIdentity
	}{{spec.Checker, &b.tools.Checker}, {spec.Git, &b.tools.Git}, {spec.Lefthook, &b.tools.Lefthook}, {spec.Nixfmt, &b.tools.Nixfmt}, {spec.Gofmt, &b.tools.Gofmt}} {
		*pair.dest, err = identify(pair.spec)
		if err != nil {
			return bindings{}, err
		}
	}
	shell, err := identify(spec.Shell)
	if err != nil {
		return bindings{}, err
	}
	self, err := os.Executable()
	if err != nil {
		return bindings{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return bindings{}, err
	}
	if self != b.tools.Checker.Resolved || !filepath.IsAbs(b.launcher) {
		return bindings{}, errors.New("tool binding selects another checker")
	}
	// Installed hooks must never enter a mutable checkout launcher. The upper
	// bundle is a separate immutable store output, not part of the lower binary.
	launcherOutput := filepath.Dir(filepath.Dir(b.launcher))
	launcher, err := identify(toolSpec{Output: launcherOutput, Program: b.launcher})
	if err != nil {
		return bindings{}, err
	}
	if _, err := hookcheck.LocalTrustIO().IdentifyTool(context.Background(), launcher); err != nil {
		return bindings{}, err
	}
	b.tools.ChildBinding = hookcheck.ChildBinding{Path: append([]string(nil), spec.Path...), Locale: "C.UTF-8", RunnerShell: &shell}
	return b, nil
}

// The only dependency description comes from the trusted Go-tool output. It
// binds both module manifests, including absence, before any vet/test child.
type goFileBinding struct {
	Present *bool   `json:"present"`
	SHA256  *string `json:"sha256"`
}
type goModuleBinding struct {
	Directory string        `json:"directory"`
	GoMod     goFileBinding `json:"goMod"`
	GoSum     goFileBinding `json:"goSum"`
	Proxy     *string       `json:"proxy"`
}
type goModulesManifest struct {
	Modules []goModuleBinding `json:"modules"`
}

func readRegular(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("module binding requires a regular file: %q", path)
	}
	return io.ReadAll(f)
}

func matchGoFile(path string, expected goFileBinding) error {
	if expected.Present == nil || *expected.Present != (expected.SHA256 != nil) {
		return errors.New("invalid module manifest presence/hash")
	}
	data, err := readRegular(path)
	if expected.SHA256 == nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("module manifest must be absent: %q", path)
	}
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if len(*expected.SHA256) != 64 || digest != *expected.SHA256 {
		return fmt.Errorf("module manifest changed: %q", path)
	}
	return nil
}

var proxyOutput = regexp.MustCompile(`^/nix/store/[a-z0-9]{32}-[A-Za-z0-9+._-]+$`)

func preflightGoModules(snapshot, output string) ([]goModuleBinding, error) {
	data, err := readRegular(filepath.Join(output, "share/confctl-hooks/go-modules.json"))
	if err != nil {
		return nil, err
	}
	var manifest goModulesManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || manifest.Modules == nil {
		return nil, errors.New("invalid Go module manifest")
	}
	// Nix's fixed encoder always writes every field, even null. Require those
	// fields rather than silently treating an omitted proxy or sum as approval.
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(data, &outer); err != nil || len(outer) != 1 || outer["modules"] == nil {
		return nil, errors.New("invalid Go module manifest shape")
	}
	var shape struct {
		Modules []map[string]json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, err
	}
	declared := map[string]goModuleBinding{}
	for i, binding := range manifest.Modules {
		if len(shape.Modules[i]) != 4 || shape.Modules[i]["proxy"] == nil || shape.Modules[i]["goMod"] == nil || shape.Modules[i]["goSum"] == nil || shape.Modules[i]["directory"] == nil {
			return nil, errors.New("incomplete Go module binding")
		}
		for _, name := range []string{"goMod", "goSum"} {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(shape.Modules[i][name], &fields); err != nil || len(fields) != 2 || fields["present"] == nil || fields["sha256"] == nil {
				return nil, errors.New("incomplete module file binding")
			}
		}
		if binding.Directory != "." && binding.Directory != "extensions" {
			return nil, errors.New("unsupported hook module directory")
		}
		if _, exists := declared[binding.Directory]; exists {
			return nil, errors.New("duplicate hook module directory")
		}
		if binding.GoMod.Present == nil || !*binding.GoMod.Present {
			return nil, errors.New("declared module requires go.mod")
		}
		declared[binding.Directory] = binding
	}
	var reached []goModuleBinding
	for _, name := range []string{".", "extensions"} {
		dir := filepath.Join(snapshot, name)
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			if _, declared := declared[name]; declared {
				return nil, fmt.Errorf("declared hook module is absent: %s", name)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("hook module directory must not be a symlink: %q", dir)
		}
		_, modErr := os.Lstat(filepath.Join(dir, "go.mod"))
		_, sumErr := os.Lstat(filepath.Join(dir, "go.sum"))
		binding, ok := declared[name]
		if errors.Is(modErr, os.ErrNotExist) && errors.Is(sumErr, os.ErrNotExist) && !ok {
			continue
		}
		if !ok {
			return nil, fmt.Errorf("no immutable Go dependency binding for %s", name)
		}
		if err := matchGoFile(filepath.Join(dir, "go.mod"), binding.GoMod); err != nil {
			return nil, err
		}
		if err := matchGoFile(filepath.Join(dir, "go.sum"), binding.GoSum); err != nil {
			return nil, err
		}
		if binding.Proxy != nil {
			if !proxyOutput.MatchString(*binding.Proxy) {
				return nil, errors.New("hook proxy must be a declared Nix store output")
			}
			info, err := os.Lstat(*binding.Proxy)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("hook proxy output is not a directory")
			}
		}
		reached = append(reached, binding)
	}
	return reached, nil
}

func moduleGoEnv(base []string, binding goModuleBinding) ([]string, error) {
	// These paths originate in CheckPrepared's validated owned control context.
	// Strip every other Go control, including ambient direct/private/cache values.
	values := map[string]string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	home := values["HOME"]
	control := filepath.Dir(home)
	if !filepath.IsAbs(home) || filepath.Base(home) != "home" || values["TMPDIR"] != filepath.Join(control, "tmp") || values["GOPATH"] != filepath.Join(control, "gopath") || values["GOCACHE"] != filepath.Join(control, "gocache") {
		return nil, errors.New("Go checks require the owned invocation cache paths")
	}
	var env []string
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GO") {
			continue
		}
		env = append(env, entry)
	}
	proxy := "off"
	if binding.Proxy != nil {
		proxy = "file://" + *binding.Proxy
	}
	// The parent already owns gopath. Writable extracted directories let it
	// remove this private cache; readonly module mode still protects manifests.
	env = append(env, "GOPROXY="+proxy, "GONOPROXY=none", "GOPRIVATE=", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOFLAGS=-mod=readonly -modcacherw",
		"GOPATH="+filepath.Join(control, "gopath"), "GOMODCACHE="+filepath.Join(control, "gopath", "pkg", "mod"), "GOCACHE="+filepath.Join(control, "gocache"))
	return env, nil
}
