package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/internal/cli"
)

// The DTOs have a public owner; loading and compilation remain private to core.
type Registry = ext.Registry
type Registration = ext.Registration
type Group = ext.Group
type Command = ext.Command
type Hook = ext.Hook

// LoadRegistry validates the two explicit authorities before any CLI effects.
// Absent authorities mean builtin-only; even an explicitly empty pair is invalid.
func LoadRegistry() (Registry, error) {
	path, hasPath := os.LookupEnv("CONFCTL_EXTENSION_REGISTRY")
	root, hasRoot := os.LookupEnv("CONFCTL_EXTENSION_ROOT")
	if !hasPath && !hasRoot {
		return Registry{}, nil
	}
	if !hasPath || !hasRoot || path == "" || root == "" {
		return Registry{}, fmt.Errorf("extension registry requires nonempty CONFCTL_EXTENSION_REGISTRY and CONFCTL_EXTENSION_ROOT")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return Registry{}, err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return Registry{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Registry{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Registry{}, err
	}
	if cwd != root {
		return Registry{}, fmt.Errorf("extension root differs from current directory")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, err
	}
	r, err := decodeRegistry(b)
	if err != nil {
		return Registry{}, err
	}
	if err = validateBoundSources(root, r.BoundSources); err != nil {
		return Registry{}, err
	}
	if _, _, err = commandRegistry(r); err != nil {
		return Registry{}, err
	}
	return r, nil
}

func decodeRegistry(b []byte) (Registry, error) {
	var r Registry
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&r); err != nil {
		return r, err
	}
	var trailing json.RawMessage
	if err := d.Decode(&trailing); err != io.EOF {
		return r, fmt.Errorf("trailing registry JSON")
	}
	if r.Schema != 1 {
		return r, fmt.Errorf("unsupported registry schema %d", r.Schema)
	}
	return r, nil
}

var sourceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validateBoundSources(root string, sources []ext.BoundSource) error {
	if len(sources) == 0 {
		return fmt.Errorf("registry requires nonempty bound_sources")
	}
	seen := map[string]bool{}
	for _, s := range sources {
		if s.Path == "" || s.Path == "." || filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path || s.Path == ".." || strings.HasPrefix(s.Path, "../") || seen[s.Path] || !sourceDigest.MatchString(s.SHA256) {
			return fmt.Errorf("invalid/duplicate bound source %q", s.Path)
		}
		seen[s.Path] = true
		path := root
		for _, part := range strings.Split(s.Path, string(filepath.Separator)) {
			path = filepath.Join(path, part)
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("bound source %q: %w", s.Path, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinked bound source %q", s.Path)
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular bound source %q", s.Path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(b)) != s.SHA256 {
			return fmt.Errorf("stale bound source %q", s.Path)
		}
	}
	return nil
}

var decimalInteger = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func compileOption(o ext.Option) (cli.OptionSpec, error) {
	s := cli.OptionSpec{Key: o.Key, Names: o.Names, Multiple: o.Multiple, Negatable: o.Negatable, Choices: o.Choices, Metavar: o.Metavar, Description: o.Description, DefaultPresent: len(o.Default) > 0}
	switch o.Kind {
	case "switch":
		s.Kind, s.Default = cli.Switch, cli.Bool(false)
	case "string":
		s.Kind = cli.String
		if o.Multiple {
			s.Default = cli.List()
		}
	case "integer":
		s.Kind = cli.Integer
	default:
		return s, fmt.Errorf("unknown option kind %q", o.Kind)
	}
	if !s.DefaultPresent {
		return s, nil
	}
	d := json.NewDecoder(bytes.NewReader(o.Default))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return s, err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return s, fmt.Errorf("trailing option default")
	}
	switch {
	case o.Multiple:
		values, ok := value.([]any)
		if !ok {
			return s, fmt.Errorf("list default required for %q", o.Key)
		}
		s.Default = cli.List()
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return s, fmt.Errorf("string list default required for %q", o.Key)
			}
			s.Default.Strings = append(s.Default.Strings, text)
		}
	case s.Kind == cli.Switch:
		value, ok := value.(bool)
		if !ok {
			return s, fmt.Errorf("boolean default required for %q", o.Key)
		}
		s.Default = cli.Bool(value)
	case value == nil:
		s.Default = cli.Value{}
	case s.Kind == cli.String:
		value, ok := value.(string)
		if !ok {
			return s, fmt.Errorf("string default required for %q", o.Key)
		}
		s.Default = cli.Str(value)
	case s.Kind == cli.Integer:
		value, ok := value.(json.Number)
		if !ok || !decimalInteger.MatchString(value.String()) {
			return s, fmt.Errorf("decimal integer default required for %q", o.Key)
		}
		s.Default.Kind = cli.Number
		if _, ok = s.Default.Integer.SetString(value.String(), 10); !ok {
			return s, fmt.Errorf("invalid integer default for %q", o.Key)
		}
	}
	return s, nil
}

func commandRegistry(extensions Registry) (*cli.Registry, map[string]registeredCommand, error) {
	builtins := cli.BuiltinRegistry()
	commands := builtins.Commands()
	registered := map[string]registeredCommand{}
	ids, owners := map[string]bool{}, map[string]string{}
	for _, reg := range extensions.Extensions {
		if strings.TrimSpace(reg.ID) == "" || ids[reg.ID] || len(reg.Argv) == 0 || !filepath.IsAbs(reg.Argv[0]) || reg.Protocol.Major != ext.Major || reg.Protocol.Minor < 0 {
			return nil, nil, fmt.Errorf("invalid extension %q", reg.ID)
		}
		ids[reg.ID] = true
		claim := func(path []string) error {
			if len(path) == 0 || builtins.ReservedRoot(path[0]) {
				return fmt.Errorf("command root collision %q", strings.Join(path, " "))
			}
			if owner, exists := owners[path[0]]; exists && owner != reg.ID {
				return fmt.Errorf("extension root collision %q", path[0])
			}
			owners[path[0]] = reg.ID
			return nil
		}
		for _, g := range reg.Groups {
			if err := claim(g.Path); err != nil {
				return nil, nil, err
			}
			commands = append(commands, cli.CommandSpec{Path: g.Path, Summary: g.Description, ArgumentPolicy: cli.GroupArguments})
		}
		for _, c := range reg.Commands {
			if err := claim(c.Path); err != nil {
				return nil, nil, err
			}
			if strings.TrimSpace(c.Handler) == "" {
				return nil, nil, fmt.Errorf("empty command handler")
			}
			opts := []cli.OptionSpec{}
			for _, set := range c.OptionSets {
				switch set {
				case "machine-filter":
					opts = append(opts, cli.MachineFilterOptions()...)
				case "confirmation":
					opts = append(opts, cli.ConfirmationOptions()...)
				default:
					return nil, nil, fmt.Errorf("unsupported option set %q", set)
				}
			}
			for _, option := range c.Options {
				compiled, err := compileOption(option)
				if err != nil {
					return nil, nil, err
				}
				opts = append(opts, compiled)
			}
			arguments := []cli.ArgumentSpec{}
			usage := []string{}
			optional := false
			for _, a := range c.Arguments {
				if a.Required && optional {
					return nil, nil, fmt.Errorf("required argument follows optional argument")
				}
				optional = optional || !a.Required
				arguments = append(arguments, cli.ArgumentSpec{Name: a.Name, Required: a.Required, Variadic: a.Variadic})
				text := a.Name
				if a.Variadic {
					text += "..."
				}
				if a.Required {
					text = "<" + text + ">"
				} else {
					text = "[" + text + "]"
				}
				usage = append(usage, text)
			}
			commands = append(commands, cli.CommandSpec{Path: c.Path, Summary: c.Description, Usage: strings.Join(usage, " "), Arguments: arguments, ArgumentPolicy: cli.FreeForm, Options: opts, Handler: cli.KnownExtension, Availability: cli.Availability{Mode: cli.Available}})
			registered[strings.Join(c.Path, " ")] = registeredCommand{reg, c}
		}
		for _, h := range reg.Hooks {
			if (h.Event != "rediscover.after-write" && h.Event != "deploy.prepare") || strings.TrimSpace(h.Handler) == "" {
				return nil, nil, fmt.Errorf("invalid hook %q", h.Event)
			}
		}
	}
	r, err := cli.NewRegistry(builtins.Globals(), commands)
	return r, registered, err
}

// The established site's adapter stays exact; all other declarations are generic.
func supportedRuntimeCommand(c Command) bool {
	return len(c.Path) == 2 && c.Path[0] == "runtime-kernels" && c.Path[1] == "update" && c.Handler == "runtime.update" && len(c.Options) == 0 && len(c.OptionSets) == 2 && c.OptionSets[0] == "machine-filter" && c.OptionSets[1] == "confirmation" && len(c.Arguments) == 1 && c.Arguments[0] == (ext.Argument{Name: "machine-pattern"})
}

func checkExtensionArity(inv cli.Invocation) error {
	required := 0
	variadic := false
	for _, a := range inv.Command.Arguments {
		if a.Required {
			required++
		}
		variadic = variadic || a.Variadic
	}
	if len(inv.Args) < required {
		return fmt.Errorf("missing required argument for %s", strings.Join(inv.Command.Path, " "))
	}
	if !variadic && len(inv.Args) > len(inv.Command.Arguments) {
		return fmt.Errorf("unknown argument: %s", strings.Join(inv.Args[len(inv.Command.Arguments):], " "))
	}
	return nil
}
