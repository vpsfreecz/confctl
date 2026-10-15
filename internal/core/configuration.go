package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// configurationOperation keeps the compiled origin through add/rename's internal
// rediscovery. It adds no new hook wire fields or separately parsed option map.
type configurationOperation struct {
	Registry Registry
	Origin   cli.Invocation
}

type configurationArgumentError struct{ message string }

func (e *configurationArgumentError) Error() string { return e.message }

func configurationArgs(args []string, required ...string) error {
	if len(args) < len(required) {
		return &configurationArgumentError{"missing argument <" + required[len(args)] + ">"}
	}
	if len(args) > len(required) {
		unknown := args[len(required):]
		message := "unknown argument: "
		if len(unknown) > 1 {
			message = "unknown arguments: "
		}
		message += strings.Join(unknown, " ")
		for _, arg := range unknown {
			if strings.HasPrefix(arg, "-") {
				message += " (note that options must come before arguments)"
				break
			}
		}
		return &configurationArgumentError{message}
	}
	return nil
}

// File.join/dirname retain lexical .. components. Cleaning with filepath.Join
// would alter mkdir_p's intermediate creations and the printed/source paths.
func configurationJoin(parent, child string) string {
	return strings.TrimRight(parent, "/") + "/" + strings.TrimLeft(child, "/")
}

func configurationDirname(path string) string {
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parent := strings.TrimRight(path[:i], "/")
		if parent == "" {
			return "/"
		}
		return parent
	}
	return "."
}

func (e *Engine) configurationPath(path string) string { return e.Root + "/" + path }

func (e *Engine) configurationDirectory(path string) bool {
	info, err := os.Stat(e.configurationPath(path))
	return err == nil && info.IsDir()
}

func (e *Engine) configurationExists(path string) bool {
	_, err := os.Stat(e.configurationPath(path))
	return err == nil
}

// Ruby's syscall diagnostics name the owning operation and its literal path.
func configurationError(err error, operation, path string) error {
	if err == nil {
		return nil
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err
	}
	message := errno.Error()
	return fmt.Errorf("%s @ %s - %s", strings.ToUpper(message[:1])+message[1:], operation, path)
}

func (e *Engine) configurationMkdir(path string, parents bool) error {
	fmt.Println("mkdir " + path)
	if !parents {
		return configurationError(os.Mkdir(e.configurationPath(path), 0755), "dir_s_mkdir", path)
	}
	// FileUtils.mkdir_p creates parents in order, chmods only newly created
	// directories to its explicit mode, and accepts a concurrent existing dir.
	var missing []string
	current := strings.TrimRight(path, "/")
	for !e.configurationDirectory(current) && configurationDirname(current) != current {
		missing = append(missing, current)
		current = configurationDirname(current)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		path := missing[i]
		err := os.Mkdir(e.configurationPath(path), 0755)
		if err != nil {
			if e.configurationDirectory(path) {
				continue
			}
			return configurationError(err, "dir_s_mkdir", path)
		}
		if err = os.Chmod(e.configurationPath(path), 0755); err != nil {
			return configurationError(err, "chmod", path)
		}
	}
	return nil
}

func (e *Engine) configurationWrite(path, text string) error {
	f, err := os.OpenFile(e.configurationPath(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return configurationError(err, "rb_sysopen", path)
	}
	_, err = f.WriteString(text)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (e *Engine) Init() error {
	dir, err := os.Open(e.Root)
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "shell.nix", ".confctl", ".gems", ".gitignore":
		default:
			return fmt.Errorf("init must be called in an empty directory")
		}
	}
	for _, template := range configurationInitTemplates {
		if template.directory {
			err = e.configurationMkdir(template.path, false)
		} else {
			fmt.Println("mkfile " + template.path)
			err = e.configurationWrite(template.path, template.text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Add(operation configurationOperation) (int, error) {
	if err := e.required(); err != nil {
		return 1, err
	}
	if err := configurationArgs(operation.Origin.Args, "name"); err != nil {
		return 64, err
	}
	name := operation.Origin.Args[0]
	dir := configurationJoin("cluster", name)
	if e.configurationDirectory(dir) {
		return 1, fmt.Errorf("%s already exists", dir)
	}
	if err := e.configurationMkdir(dir, true); err != nil {
		return 1, err
	}
	fmt.Println("mkfile " + configurationJoin(dir, "module.nix"))
	if err := e.configurationWrite(configurationJoin(dir, "module.nix"), fmt.Sprintf(configurationMachineModule, name)); err != nil {
		return 1, err
	}
	fmt.Println("mkfile " + configurationJoin(dir, "config.nix"))
	text := fmt.Sprintf(configurationMachineConfig, strings.Repeat("../", strings.Count(name, "/")+2), strings.ReplaceAll(name, "/", "-"))
	if err := e.configurationWrite(configurationJoin(dir, "config.nix"), text); err != nil {
		return 1, err
	}
	return e.Rediscover(operation)
}

func (e *Engine) Rename(operation configurationOperation) (int, error) {
	if err := e.required(); err != nil {
		return 1, err
	}
	if err := configurationArgs(operation.Origin.Args, "old-name", "new-name"); err != nil {
		return 64, err
	}
	src, dst := operation.Origin.Args[0], operation.Origin.Args[1]
	source, destination := configurationJoin("cluster", src), configurationJoin("cluster", dst)
	if !e.configurationDirectory(source) {
		return 1, fmt.Errorf("'%s' not found", src)
	}
	if e.configurationDirectory(destination) {
		return 1, fmt.Errorf("'%s' already exists", dst)
	}
	if err := e.configurationMkdir(configurationDirname(destination), true); err != nil {
		return 1, err
	}
	fmt.Printf("mv %s %s\n", source, destination)
	if err := os.Rename(e.configurationPath(source), e.configurationPath(destination)); err != nil {
		return 1, configurationError(err, "rb_file_s_rename", "("+source+", "+destination+")")
	}
	return e.Rediscover(operation)
}

func (e *Engine) discoverConfiguration(dir, relative string) ([]string, error) {
	f, err := os.Open(e.configurationPath(dir))
	if err != nil {
		return nil, configurationError(err, "dir_initialize", dir)
	}
	// Preserve directory enumeration order through recursion and errors; sort the
	// completed relative inventory only, as Ruby discover_dir(...).sort does.
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return nil, configurationError(err, "dir_initialize", dir)
	}
	var hosts []string
	for _, entry := range entries {
		path := configurationJoin(dir, entry.Name())
		if !e.configurationDirectory(path) {
			continue
		}
		name := entry.Name()
		if relative != "" {
			name = configurationJoin(relative, name)
		}
		if e.configurationExists(configurationJoin(path, "module.nix")) && e.configurationExists(configurationJoin(path, "config.nix")) {
			hosts = append(hosts, name)
		}
		children, err := e.discoverConfiguration(path, name)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, children...)
	}
	return hosts, nil
}

func (e *Engine) Rediscover(operation configurationOperation) (int, error) {
	if err := e.required(); err != nil {
		return 1, err
	}
	hosts, err := e.discoverConfiguration("cluster", "")
	if err != nil {
		return 1, err
	}
	sort.Strings(hosts)
	text := "# This file is generated by confctl, changes will be lost\n[\n"
	for _, host := range hosts {
		text += "  ./" + host + "/module.nix\n"
	}
	text += "]\n"
	fmt.Println("replace cluster/cluster.nix")
	var random [3]byte
	if _, err = rand.Read(random[:]); err != nil {
		return 1, err
	}
	replacement := "cluster/cluster.nix.new-" + hex.EncodeToString(random[:])
	if err = e.configurationWrite(replacement, text); err != nil {
		return 1, err
	}
	if err = os.Rename(e.configurationPath(replacement), e.configurationPath("cluster/cluster.nix")); err != nil {
		return 1, configurationError(err, "rb_file_s_rename", "("+replacement+", cluster/cluster.nix)")
	}
	// Hooks see the newly written inventory; no subscriber means no Nix request.
	e.SettingsCache = nil
	e.Inventory = nil
	return e.HooksFrom(operation.Registry, "rediscover.after-write", nil, operation.Origin)
}
