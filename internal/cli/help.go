package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func optionLabel(o OptionSpec, global bool) string {
	labels := []string{}
	for _, name := range o.Names {
		// GLI advertises only the long framework help name, although -h works.
		if global && o.Key == "help" && len(name) == 1 {
			continue
		}
		if len(name) == 1 {
			labels = append(labels, "-"+name)
			continue
		}
		if o.Negatable {
			name = "[no-]" + name
		}
		label := "--" + name
		if o.Kind != Switch {
			label += "=" + o.Metavar
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, ", ")
}
func defaultText(v Value) string {
	switch v.Kind {
	case Null:
		return "none"
	case Text:
		return v.String
	case Number:
		return v.Integer.String()
	case Boolean:
		return strconv.FormatBool(v.Bool)
	case TextList:
		if len(v.Strings) == 0 {
			return "none"
		}
		return strings.Join(v.Strings, ", ")
	}
	return "none"
}
func optionDescription(o OptionSpec) string {
	d := o.Description
	if o.Kind == Switch {
		return d
	}
	qualifier := "default: " + defaultText(o.Default)
	if o.Multiple {
		qualifier = "may be used more than once, " + qualifier
	}
	if d != "" {
		d += " "
	}
	return d + "(" + qualifier + ")"
}

type helpRow struct{ label, description string }

func renderRows(rows []helpRow, width int) string {
	longest := 0
	for _, row := range rows {
		if len(row.label) > longest {
			longest = len(row.label)
		}
	}
	indent := longest + 7 // four-space indent plus the " - " separator
	var b strings.Builder
	for _, row := range rows {
		prefix := "    " + row.label + strings.Repeat(" ", longest-len(row.label)) + " - "
		words := strings.Fields(row.description)
		b.WriteString(prefix)
		column := len(prefix)
		for i, word := range words {
			space := 0
			if i > 0 {
				space = 1
			}
			if i > 0 && column+space+len(word) > width {
				b.WriteString("\n" + strings.Repeat(" ", indent))
				column, space = indent, 0
			}
			if space > 0 {
				b.WriteByte(' ')
				column++
			}
			b.WriteString(word)
			column += len(word)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func renderOptions(specs []OptionSpec, global bool, width int) string {
	ordered := cloneOptions(specs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Names[0] < ordered[j].Names[0] })
	rows := make([]helpRow, len(ordered))
	for i, o := range ordered {
		rows[i] = helpRow{optionLabel(o, global), optionDescription(o)}
	}
	return renderRows(rows, width)
}

func availabilityText(c CommandSpec) string {
	switch c.Availability.Mode {
	case Available:
		return "Available"
	case Conditional:
		return "Only --" + c.Availability.Option + " " + defaultText(c.Availability.Equals)
	default:
		return "Unavailable"
	}
}

// Help renders declaration metadata; the checked-in Ruby help files are test
// assets only. Root help intentionally remains the reference command inventory.
func (r *Registry) Help(path []string, width int) (string, error) {
	c, ok := r.Lookup(path)
	if !ok {
		return "", fmt.Errorf("unknown help command: %s", pathKey(path))
	}
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	name := pathKey(path)
	if len(path) == 0 {
		b.WriteString("NAME\n    confctl - " + c.Summary + "\n\n\nSYNOPSIS\n    confctl [global options] " + c.Usage + "\n\n\nGLOBAL OPTIONS\n")
		b.WriteString(renderOptions(r.globals, true, width))
		b.WriteString("\n\nCOMMANDS\n")
	} else {
		inputsWithoutOptions := c.Handler == InputsReadHandler && len(c.Options) == 0 && (name == "inputs ls" || name == "inputs channel ls")
		heading := name
		if inputsWithoutOptions {
			heading = c.Path[len(c.Path)-1]
		}
		b.WriteString("NAME\n    " + heading + " - " + c.Summary + "\n\nSYNOPSIS\n\n    confctl [global options] " + name)
		if c.ArgumentPolicy == GroupArguments {
			b.WriteString(" command")
		}
		configurationWithoutOptions := c.Handler == ConfigurationHandler && len(c.Options) == 0
		if !configurationWithoutOptions && !inputsWithoutOptions {
			b.WriteString(" [command options]")
		}
		if c.Usage != "" {
			b.WriteString(" " + c.Usage)
		}
		b.WriteString("\n")
		if !configurationWithoutOptions && !inputsWithoutOptions {
			b.WriteString("\n")
		}
		if c.ArgumentPolicy == GroupArguments {
			b.WriteString("COMMANDS\n")
		} else if len(c.Options) > 0 {
			b.WriteString("COMMAND OPTIONS\n" + renderOptions(c.Options, false, width) + "\n")
		}
	}
	if c.ArgumentPolicy == GroupArguments {
		rows := []helpRow{}
		for _, child := range r.Children(path) {
			d := child.Summary
			if len(path) > 0 {
				state := availabilityText(child)
				if child.ArgumentPolicy == GroupArguments {
					state = "Command group"
				}
				d += " (" + state + ")"
			}
			rows = append(rows, helpRow{child.Path[len(child.Path)-1], d})
		}
		b.WriteString(renderRows(rows, width))
	} else if c.Availability.Mode == Conditional {
		b.WriteString("AVAILABILITY\n    Execution requires --" + c.Availability.Option + " " + defaultText(c.Availability.Equals) + ".\n\n")
	} else if c.Availability.Mode == Unavailable {
		b.WriteString("AVAILABILITY\n    " + c.Availability.Reason + "\n\n")
	}
	return b.String(), nil
}

// CapabilityTable provides the README table from the same leaf declarations.
func (r *Registry) CapabilityTable() string {
	var b strings.Builder
	b.WriteString("| Builtin command | Execution |\n| --- | --- |\n")
	for _, c := range r.Leaves() {
		b.WriteString("| `" + pathKey(c.Path) + "` | " + availabilityText(c) + " |\n")
	}
	return b.String()
}
