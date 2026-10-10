// Package cli owns the experimental command declarations, parsing and help.
// It has no execution, SDK or configuration filesystem dependencies.
package cli

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
)

type OptionKind uint8

const (
	Switch OptionKind = iota
	String
	Integer
)

type ValueKind uint8

const (
	Null ValueKind = iota
	Boolean
	Text
	Number
	TextList
)

// Value preserves null, false, explicit empty strings and ordered repeated values.
// Struct/map copies borrow numeric storage; clone before mutating a Number.
type Value struct {
	Kind    ValueKind
	Bool    bool
	String  string
	Integer big.Int
	Strings []string
}

func Bool(v bool) Value  { return Value{Kind: Boolean, Bool: v} }
func Str(v string) Value { return Value{Kind: Text, String: v} }
func Int(v int) Value {
	n := Value{Kind: Number}
	n.Integer.SetInt64(int64(v))
	return n
}
func List(v ...string) Value { return Value{Kind: TextList, Strings: append([]string{}, v...)} }

func (v Value) clone() Value {
	if v.Kind == Number {
		// A fresh destination prevents Set from reusing borrowed limb storage.
		var n big.Int
		n.Set(&v.Integer)
		v.Integer = n
	}
	if v.Kind == TextList {
		v.Strings = append([]string{}, v.Strings...)
	}
	return v
}

type OptionSpec struct {
	Key, Metavar, Description string
	Names                     []string
	Kind                      OptionKind
	Default                   Value
	// DefaultPresent is declaration presence, including an explicit null.
	// Parser initialization alone does not establish invocation wire presence.
	DefaultPresent      bool
	Multiple, Negatable bool
	Choices             []string
}

type ParsedValue struct {
	Value   Value
	Present bool
}

type ArgumentSpec struct {
	Name               string
	Required, Variadic bool
}

type ArgumentPolicy uint8

const (
	// FreeForm is GLI's display-only arg_name contract. Handlers may validate later.
	FreeForm ArgumentPolicy = iota
	NoArguments
	GroupArguments
)

type Handler uint8

const (
	None Handler = iota
	ListHandler
	StatusNone
	KnownExtension
	ConfigurationHandler
	InputsReadHandler
)

type AvailabilityMode uint8

const (
	Unavailable AvailabilityMode = iota
	Available
	Conditional
)

type Availability struct {
	Mode           AvailabilityMode
	Reason, Option string
	Equals         Value
}

type CommandSpec struct {
	Path           []string
	Summary, Usage string
	Arguments      []ArgumentSpec
	ArgumentPolicy ArgumentPolicy
	Options        []OptionSpec
	Handler        Handler
	Availability   Availability
}

type Invocation struct {
	Command          CommandSpec
	Globals, Options map[string]ParsedValue
	Args, RawArgv    []string
	Help             bool
	HelpPath         []string
}

type Registry struct {
	globals  []OptionSpec
	commands []CommandSpec
	index    map[string]int
	children map[string][]int
}

func pathKey(path []string) string  { return strings.Join(path, " ") }
func optionName(name string) string { return strings.ReplaceAll(name, "_", "-") }

func cloneOptions(options []OptionSpec) []OptionSpec {
	out := append([]OptionSpec(nil), options...)
	for i := range out {
		out[i].Names = append([]string(nil), out[i].Names...)
		out[i].Choices = append([]string(nil), out[i].Choices...)
		out[i].Default = out[i].Default.clone()
	}
	return out
}

func cloneCommand(c CommandSpec) CommandSpec {
	c.Path = append([]string(nil), c.Path...)
	c.Arguments = append([]ArgumentSpec(nil), c.Arguments...)
	c.Options = cloneOptions(c.Options)
	c.Availability.Equals = c.Availability.Equals.clone()
	return c
}

func validateOptions(options []OptionSpec, framework bool) error {
	keys, names := map[string]bool{}, map[string]bool{}
	for _, o := range options {
		if o.Key == "" || keys[o.Key] || len(o.Names) == 0 || o.Kind > Integer {
			return fmt.Errorf("invalid/duplicate option %q", o.Key)
		}
		keys[o.Key] = true
		if !framework && (o.Key == "help" || o.Key == "version") {
			return fmt.Errorf("reserved option %q", o.Key)
		}
		if o.Negatable && o.Kind != Switch || o.Multiple && o.Kind != String {
			return fmt.Errorf("invalid negation/repetition for %q", o.Key)
		}
		validDefault := false
		switch {
		case o.Multiple:
			validDefault = o.Default.Kind == TextList
		case o.Kind == Switch:
			validDefault = o.Default.Kind == Boolean
		case o.Kind == String:
			validDefault = o.Default.Kind == Null || o.Default.Kind == Text
		case o.Kind == Integer:
			validDefault = o.Default.Kind == Null || o.Default.Kind == Number
		}
		if !validDefault || len(o.Choices) > 0 && (o.Kind != String || o.Multiple) {
			return fmt.Errorf("invalid default/choices for %q", o.Key)
		}
		if o.Default.Kind == Text && len(o.Choices) > 0 && !contains(o.Choices, o.Default.String) {
			return fmt.Errorf("default outside choices for %q", o.Key)
		}
		for _, name := range o.Names {
			if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\n=") {
				return fmt.Errorf("invalid option name %q", name)
			}
			n := optionName(name)
			if !framework && (n == "help" || n == "h" || n == "version") {
				return fmt.Errorf("reserved option name %q", name)
			}
			if names[n] {
				return fmt.Errorf("option alias collision %q", name)
			}
			names[n] = true
			if o.Negatable && len(name) > 1 {
				n = "no-" + n
				if names[n] {
					return fmt.Errorf("option negation collision %q", name)
				}
				names[n] = true
			}
		}
	}
	return nil
}

// NewRegistry validates and copies declarations. Callers cannot mutate its indexes.
func NewRegistry(globals []OptionSpec, commands []CommandSpec) (*Registry, error) {
	if err := validateOptions(globals, true); err != nil {
		return nil, err
	}
	r := &Registry{globals: cloneOptions(globals), index: map[string]int{}, children: map[string][]int{}}
	for _, c := range commands {
		key := pathKey(c.Path)
		if _, exists := r.index[key]; exists {
			return nil, fmt.Errorf("command collision %q", key)
		}
		for _, p := range c.Path {
			if p == "" || strings.HasPrefix(p, "-") || strings.ContainsAny(p, " \t\n") {
				return nil, fmt.Errorf("invalid command path %q", key)
			}
		}
		if c.ArgumentPolicy > GroupArguments || c.Handler > InputsReadHandler || c.Availability.Mode > Conditional {
			return nil, fmt.Errorf("invalid command policy %q", key)
		}
		if c.ArgumentPolicy == GroupArguments {
			if c.Handler != None || len(c.Arguments) > 0 {
				return nil, fmt.Errorf("group/handler conflict %q", key)
			}
		} else if c.ArgumentPolicy == NoArguments && len(c.Arguments) > 0 {
			return nil, fmt.Errorf("argument policy conflict %q", key)
		}
		if c.Handler == None && c.Availability.Mode != Unavailable && key != "help" {
			return nil, fmt.Errorf("available command lacks handler %q", key)
		}
		if c.Handler != None && c.Availability.Mode == Unavailable {
			return nil, fmt.Errorf("unavailable command has handler %q", key)
		}
		if c.Handler == StatusNone && c.Availability.Mode != Conditional || (c.Handler == ListHandler || c.Handler == KnownExtension || c.Handler == ConfigurationHandler || c.Handler == InputsReadHandler) && c.Availability.Mode != Available {
			return nil, fmt.Errorf("handler/availability conflict %q", key)
		}
		argumentNames := map[string]bool{}
		for i, a := range c.Arguments {
			if a.Name == "" || argumentNames[a.Name] || a.Variadic && i != len(c.Arguments)-1 {
				return nil, fmt.Errorf("invalid argument declaration %q", key)
			}
			argumentNames[a.Name] = true
		}
		if err := validateOptions(c.Options, false); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if c.Availability.Mode == Conditional {
			found := false
			for _, o := range c.Options {
				if o.Key == c.Availability.Option {
					found = !o.Multiple && (o.Kind == String && c.Availability.Equals.Kind == Text || o.Kind == Switch && c.Availability.Equals.Kind == Boolean || o.Kind == Integer && c.Availability.Equals.Kind == Number)
				}
			}
			if !found || c.Availability.Reason == "" {
				return nil, fmt.Errorf("invalid availability condition %q", key)
			}
		}
		r.index[key] = len(r.commands)
		r.commands = append(r.commands, cloneCommand(c))
	}
	root, ok := r.Lookup(nil)
	if !ok || root.ArgumentPolicy != GroupArguments {
		return nil, fmt.Errorf("registry lacks root group")
	}
	for i, c := range r.commands {
		if len(c.Path) == 0 {
			continue
		}
		parent := c.Path[:len(c.Path)-1]
		p, ok := r.Lookup(parent)
		if !ok || p.ArgumentPolicy != GroupArguments {
			return nil, fmt.Errorf("missing group/leaf parent conflict for %q", pathKey(c.Path))
		}
		r.children[pathKey(parent)] = append(r.children[pathKey(parent)], i)
	}
	for key := range r.children {
		sort.SliceStable(r.children[key], func(i, j int) bool {
			a, b := r.commands[r.children[key][i]], r.commands[r.children[key][j]]
			return a.Path[len(a.Path)-1] < b.Path[len(b.Path)-1]
		})
	}
	return r, nil
}

func (r *Registry) Globals() []OptionSpec { return cloneOptions(r.globals) }
func (r *Registry) Commands() []CommandSpec {
	out := make([]CommandSpec, len(r.commands))
	for i, c := range r.commands {
		out[i] = cloneCommand(c)
	}
	return out
}
func (r *Registry) Lookup(path []string) (CommandSpec, bool) {
	i, ok := r.index[pathKey(path)]
	if !ok {
		return CommandSpec{}, false
	}
	return cloneCommand(r.commands[i]), true
}
func (r *Registry) Children(path []string) []CommandSpec {
	out := []CommandSpec{}
	for _, i := range r.children[pathKey(path)] {
		out = append(out, cloneCommand(r.commands[i]))
	}
	return out
}
func (r *Registry) Leaves() []CommandSpec {
	out := []CommandSpec{}
	for _, c := range r.commands {
		if c.ArgumentPolicy != GroupArguments && pathKey(c.Path) != "help" {
			out = append(out, cloneCommand(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return pathKey(out[i].Path) < pathKey(out[j].Path) })
	return out
}
func (r *Registry) ReservedRoot(name string) bool {
	_, ok := r.Lookup([]string{name})
	return ok
}

func (r *Registry) CheckAvailability(inv Invocation) error {
	c, ok := r.Lookup(inv.Command.Path)
	if !ok {
		return fmt.Errorf("unknown command %q", pathKey(inv.Command.Path))
	}
	a := c.Availability
	if a.Mode == Available {
		return nil
	}
	if a.Mode == Conditional {
		v := inv.Options[a.Option].Value
		if v.Kind == a.Equals.Kind && (v.Kind == Text && v.String == a.Equals.String || v.Kind == Boolean && v.Bool == a.Equals.Bool || v.Kind == Number && v.Integer.Cmp(&a.Equals.Integer) == 0) {
			return nil
		}
		return fmt.Errorf("%s", a.Reason)
	}
	return fmt.Errorf("%s is unavailable in the measurement prototype", pathKey(c.Path))
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
