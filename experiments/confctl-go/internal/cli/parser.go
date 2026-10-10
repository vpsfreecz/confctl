package cli

import (
	"fmt"
	"regexp"
	"strings"
)

type ParseError struct {
	Message  string
	HelpPath []string
}

func (e *ParseError) Error() string { return e.Message }

// Ruby 3.4.9 OptionParser (optparse.rb:2078-2089) gates syntax before Integer(s).
// Base-zero conversion then rejects invalid octal digits and preserves full range.
var integerPattern = regexp.MustCompile(`(?i)\A[-+]?(?:0(?:[0-7]+(?:_[0-7]+)*|b[01]+(?:_[01]+)*|x[0-9a-f]+(?:_[0-9a-f]+)*)?|[0-9]+(?:_[0-9]+)*)\z`)

func parseInteger(text string) (Value, bool) {
	if !integerPattern.MatchString(text) {
		return Value{}, false
	}
	v := Value{Kind: Number}
	_, ok := v.Integer.SetString(text, 0)
	return v, ok
}

func defaults(specs []OptionSpec) map[string]ParsedValue {
	values := make(map[string]ParsedValue, len(specs))
	for _, s := range specs {
		values[s.Key] = ParsedValue{Value: s.Default.clone()}
	}
	return values
}

func namedOption(specs []OptionSpec, name string, short bool) (OptionSpec, bool, bool) {
	for _, s := range specs {
		for _, n := range s.Names {
			if short && len(n) != 1 || !short && len(n) == 1 {
				continue
			}
			if optionName(n) == name {
				return s, false, true
			}
			if !short && s.Negatable && "no-"+optionName(n) == name {
				return s, true, true
			}
		}
	}
	return OptionSpec{}, false, false
}

// scanOptions is one ordered pass. A positional or -- makes the entire tail literal.
func scanOptions(specs []OptionSpec, argv []string, stopVersion bool) (map[string]ParsedValue, []string, error) {
	values := defaults(specs)
	for i := 0; i < len(argv); i++ {
		token := argv[i]
		if stopVersion && token == "--version" {
			return values, argv[i:], nil
		}
		if token == "--" {
			return values, argv[i+1:], nil
		}
		if token == "-" || !strings.HasPrefix(token, "-") {
			return values, argv[i:], nil
		}
		apply := func(s OptionSpec, negative bool, label, text string, hasValue bool) error {
			v := Value{}
			if s.Kind == Switch {
				if hasValue {
					return fmt.Errorf("option does not take an argument: %s", label)
				}
				v = Bool(!negative)
			} else {
				if !hasValue {
					i++
					if i == len(argv) {
						return fmt.Errorf("missing argument: %s", label)
					}
					text = argv[i]
				}
				if s.Kind == Integer {
					var ok bool
					v, ok = parseInteger(text)
					if !ok {
						return fmt.Errorf("invalid argument: %s %s", label, text)
					}
				} else {
					if len(s.Choices) > 0 && !contains(s.Choices, text) {
						return fmt.Errorf("invalid argument: %s %s", label, text)
					}
					v = Str(text)
				}
			}
			if s.Multiple {
				prior := values[s.Key].Value.clone()
				prior.Strings = append(prior.Strings, text)
				v = prior
			}
			values[s.Key] = ParsedValue{Value: v, Present: true}
			return nil
		}
		if strings.HasPrefix(token, "--") {
			name, value, hasValue := strings.Cut(token[2:], "=")
			s, negative, ok := namedOption(specs, optionName(name), false)
			if !ok {
				return values, nil, fmt.Errorf("invalid option: --%s", name)
			}
			if err := apply(s, negative, "--"+name, value, hasValue); err != nil {
				return values, nil, err
			}
			// GLI raises RequestHelp during command option parsing. Globals keep
			// their existing scan/version behavior; a positional still stops us.
			if !stopVersion && s.Key == "help" && values[s.Key].Value.Bool {
				return values, nil, nil
			}
			continue
		}
		for j := 1; j < len(token); j++ {
			name := token[j : j+1]
			s, _, ok := namedOption(specs, name, true)
			if !ok {
				return values, nil, fmt.Errorf("invalid option: -%s", name)
			}
			hasValue, value := false, ""
			if s.Kind != Switch && j+1 < len(token) {
				hasValue, value = true, token[j+1:]
			}
			if err := apply(s, false, "-"+name, value, hasValue); err != nil {
				return values, nil, err
			}
			if !stopVersion && s.Key == "help" && values[s.Key].Value.Bool {
				return values, nil, nil
			}
			if s.Kind != Switch {
				break
			}
		}
	}
	return values, nil, nil
}

// ParseGlobals allows Main to retain the observed bare-version failure before
// reading extension metadata. Version is not an application-declared option.
func (r *Registry) ParseGlobals(argv []string) (map[string]ParsedValue, []string, error) {
	return scanOptions(r.globals, argv, true)
}

func (r *Registry) helpOption() OptionSpec {
	for _, o := range r.globals {
		if o.Key == "help" {
			return o
		}
	}
	return OptionSpec{}
}

func parseError(err error, path []string) *ParseError {
	return &ParseError{Message: err.Error(), HelpPath: append([]string(nil), path...)}
}

func (r *Registry) helpRequest(inv Invocation, path []string) (Invocation, error) {
	c, ok := r.Lookup(path)
	if !ok {
		parent := append([]string(nil), path...)
		for len(parent) > 0 {
			parent = parent[:len(parent)-1]
			if _, ok := r.Lookup(parent); ok {
				break
			}
		}
		return inv, parseError(fmt.Errorf("unknown command: %s", pathKey(path)), parent)
	}
	inv.Command = c
	inv.Help, inv.HelpPath = true, append([]string(nil), path...)
	return inv, nil
}

func (r *Registry) Parse(argv []string) (Invocation, error) {
	inv := Invocation{RawArgv: append([]string(nil), argv...)}
	globals, remaining, err := r.ParseGlobals(argv)
	inv.Globals = globals
	if err != nil {
		return inv, parseError(err, nil)
	}
	if globals["help"].Value.Bool {
		return r.helpRequest(inv, remaining)
	}
	if len(remaining) == 0 {
		return r.helpRequest(inv, nil)
	}
	if remaining[0] == "help" {
		if c, ok := r.Lookup([]string{"help"}); ok && c.Handler == None && c.Availability.Mode == Available {
			return r.helpRequest(inv, remaining[1:])
		}
	}
	path := []string{}
	for len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
		next := append(append([]string(nil), path...), remaining[0])
		c, ok := r.Lookup(next)
		if !ok {
			return inv, parseError(fmt.Errorf("unknown command: %s", pathKey(next)), path)
		}
		path, remaining, inv.Command = next, remaining[1:], c
		if c.ArgumentPolicy != GroupArguments {
			break
		}
	}
	if len(path) == 0 {
		return inv, parseError(fmt.Errorf("invalid option: %s", remaining[0]), nil)
	}
	c := inv.Command
	specs := cloneOptions(c.Options)
	if h := r.helpOption(); h.Key != "" {
		specs = append(specs, h)
	}
	inv.Options, remaining, err = scanOptions(specs, remaining, false)
	if err != nil {
		return inv, parseError(err, path)
	}
	if inv.Options["help"].Value.Bool {
		return r.helpRequest(inv, path)
	}
	delete(inv.Options, "help") // Framework controls are not handler options.
	inv.Args = append([]string(nil), remaining...)
	if c.ArgumentPolicy == GroupArguments {
		if len(inv.Args) > 0 {
			return inv, parseError(fmt.Errorf("unknown command: %s", pathKey(append(path, inv.Args...))), path)
		}
		return r.helpRequest(inv, path)
	}
	if c.ArgumentPolicy == NoArguments && len(inv.Args) > 0 {
		label := "unknown argument: "
		if len(inv.Args) > 1 {
			label = "unknown arguments: "
		}
		return inv, parseError(fmt.Errorf("%s%s", label, strings.Join(inv.Args, " ")), path)
	}
	return inv, nil
}
