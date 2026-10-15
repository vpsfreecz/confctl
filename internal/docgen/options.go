package docgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ModuleOption is one decoded moduleOptions declaration. Values retain exact
// json.Number tokens; public selection precedes description/value validation.
type ModuleOption map[string]any

type OptionsLoader func(context.Context) ([]ModuleOption, error)
type ValueRenderer func(any) (string, error)

// DecodeOptions is also usable by an injected file loader. It does not evaluate
// or import the source represented by the JSON.
func DecodeOptions(reader io.Reader) ([]ModuleOption, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var options []ModuleOption
	if err := decoder.Decode(&options); err != nil {
		return nil, fmt.Errorf("decode moduleOptions: %w", err)
	}
	if options == nil {
		return nil, fmt.Errorf("moduleOptions must be an array")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("decode moduleOptions: %w", err)
	}
	return options, nil
}

type renderedOption struct {
	name, description, optionType string
	defaultValue                  nixExpression
	example                       *nixExpression
	declarations                  []string
}

type nixExpression struct {
	literal *string
	value   any
}

func (e nixExpression) format(render ValueRenderer) (string, error) {
	if e.literal != nil {
		return *e.literal, nil
	}
	return render(e.value)
}

// Options ports the public module-options Markdown template. Primitive/compound
// Nix values go through the injected shared formatter; literalExpression text
// is already Nix and bypasses it. No renderer/loader error opens an output file.
func Options(ctx context.Context, loader OptionsLoader, render ValueRenderer, sourceRoot string, metadata Metadata) ([]byte, error) {
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	if loader == nil || render == nil {
		return nil, fmt.Errorf("moduleOptions loader and value renderer are required")
	}
	options, err := loader(ctx)
	if err != nil {
		return nil, err
	}
	// Sort a separate slice; an injected loader retains ownership of its values.
	selected := make([]ModuleOption, 0, len(options))
	for i, option := range options {
		name, ok := option["name"].(string)
		if !ok {
			return nil, fmt.Errorf("moduleOptions[%d]: name must be a string", i)
		}
		if !strings.HasPrefix(name, "confctl.") && !strings.HasPrefix(name, "cluster.") {
			continue
		}
		if internal, ok := option["internal"].(bool); ok && internal {
			continue
		}
		if internal := option["internal"]; internal != nil {
			if _, ok := internal.(bool); !ok {
				return nil, fmt.Errorf("%s: internal must be a boolean", name)
			}
		}
		selected = append(selected, option)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		return selected[i]["name"].(string) < selected[j]["name"].(string)
	})
	groups := make([][]renderedOption, 3)
	for _, option := range selected {
		rendered, err := prepareOption(option, sourceRoot)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", option["name"], err)
		}
		group := 0
		if strings.HasPrefix(rendered.name, "cluster.") {
			group = 1
		} else if systemOption(rendered.name) {
			group = 2
		}
		groups[group] = append(groups[group], rendered)
	}
	var b strings.Builder
	b.WriteString(metadata.header("confctl-options.nix"))
	b.WriteString("\n# NAME\n`confctl-options.nix` - confctl configuration documentation\n\n# DESCRIPTION\nNix options for confctl(8) cluster configurations and machine systems.\n\n# CONFCTL SETTINGS\nConfigure these `confctl` settings in `configs/confctl.nix`:\n\n")
	if err := writeOptions(&b, groups[0], render); err != nil {
		return nil, err
	}
	b.WriteString("\n# MACHINE CONFIGURATION\nConfigure machine metadata in `cluster/<machine-name>/module.nix`:\n\n")
	if err := writeOptions(&b, groups[1], render); err != nil {
		return nil, err
	}
	b.WriteString("\n# MACHINE SYSTEM CONFIGURATION\nConfigure these options in `cluster/<machine-name>/config.nix` or an imported\nNix module. `confctl` adds them to the options provided by NixOS or vpsAdminOS.\n\n")
	if err := writeOptions(&b, groups[2], render); err != nil {
		return nil, err
	}
	b.WriteString("\n# SEE ALSO\nconfctl(8)\n\n# BUGS\nReport bugs to https://github.com/vpsfreecz/confctl/issues.\n\n# ABOUT\n`confctl` was originally developed for\n[vpsFree.cz](https://vpsfree.org) and its cluster\n[configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration).\n")
	return []byte(b.String()), nil
}

func prepareOption(option ModuleOption, sourceRoot string) (renderedOption, error) {
	result := renderedOption{name: option["name"].(string)}
	switch description := option["description"].(type) {
	case nil:
		result.description = "This option has no description."
	case bool:
		if description {
			return result, fmt.Errorf("description must be a string")
		}
		result.description = "This option has no description."
	case string:
		result.description = markdownDescription(description)
	default:
		return result, fmt.Errorf("description must be a string")
	}
	if value := option["type"]; value != nil {
		var ok bool
		result.optionType, ok = value.(string)
		if !ok {
			return result, fmt.Errorf("type must be a string")
		}
	}
	var err error
	result.defaultValue, err = expression(option["default"])
	if err != nil {
		return result, err
	}
	if value := option["example"]; rubyTruthy(value) {
		text, err := expression(value)
		if err != nil {
			return result, err
		}
		result.example = &text
	}
	switch declarations := option["declarations"].(type) {
	case []any:
		for _, declaration := range declarations {
			text, ok := declaration.(string)
			if !ok {
				return result, fmt.Errorf("declarations must contain strings")
			}
			result.declarations = append(result.declarations, rewriteDeclaration(text, sourceRoot))
		}
	case []string:
		for _, text := range declarations {
			result.declarations = append(result.declarations, rewriteDeclaration(text, sourceRoot))
		}
	default:
		return result, fmt.Errorf("declarations must be an array of strings")
	}
	return result, nil
}

func rubyTruthy(value any) bool {
	if boolean, ok := value.(bool); ok {
		return boolean
	}
	return value != nil
}

func expression(value any) (nixExpression, error) {
	if object, ok := value.(map[string]any); ok {
		if object["_type"] != "literalExpression" {
			return nixExpression{}, fmt.Errorf("unsupported expression type %v", object["_type"])
		}
		text, ok := object["text"].(string)
		if !ok {
			return nixExpression{}, fmt.Errorf("literalExpression text must be a string")
		}
		return nixExpression{literal: &text}, nil
	}
	return nixExpression{value: value}, nil
}

func systemOption(name string) bool {
	for _, prefix := range []string{"confctl.carrier.", "confctl.programs.", "confctl.inputsInfo", "confctl.configurationInfo"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var storeDeclaration = regexp.MustCompile(`^/nix/store/[^/]+/(nix/modules/(?:confctl|cluster)(?:/.*)?)$`)

func rewriteDeclaration(path, sourceRoot string) string {
	if match := storeDeclaration.FindStringSubmatch(path); match != nil {
		return "<confctl/" + match[1] + ">"
	}
	if sourceRoot != "" {
		prefix := strings.TrimRight(filepath.ToSlash(sourceRoot), "/") + "/"
		if strings.HasPrefix(path, prefix) {
			relative := strings.TrimPrefix(path, prefix)
			if matched, _ := regexp.MatchString(`^nix/modules/(?:confctl|cluster)(?:/.*)?$`, relative); matched {
				return "<confctl/" + relative + ">"
			}
		}
	}
	return path
}

var literalTag = regexp.MustCompile(`<literal>([^<]+)</literal>`)
var optionTag = regexp.MustCompile(`<option>([^<]+)</option>`)
var htmlEntity = regexp.MustCompile(`&(?:amp|quot|gt|lt|apos|#[0-9]+|#x[0-9a-fA-F]+);`)

func markdownDescription(description string) string {
	// Ruby substitutes literal tags first, then option tags. Retain that order
	// even when an option encloses a converted literal. Track the first-pass
	// spans so HTML decoding happens once, before choosing safe delimiters.
	type literalSpan struct {
		start, end int
		content    string
	}
	var first strings.Builder
	var spans []literalSpan
	end := 0
	for _, match := range literalTag.FindAllStringSubmatchIndex(description, -1) {
		first.WriteString(description[end:match[0]])
		content := description[match[2]:match[3]]
		start := first.Len()
		first.WriteString("`" + content + "`")
		spans = append(spans, literalSpan{start, first.Len(), content})
		end = match[1]
	}
	first.WriteString(description[end:])
	source := first.String()
	var text strings.Builder
	writeLiteral := func(content string) {
		if !strings.ContainsAny(content, "\r\n") {
			text.WriteString(codeSpan(content))
			return
		}
		// Separate surrounding prose from the complete multiline literal. The
		// enclosing definition later adds its own four-space body indentation.
		if text.Len() != 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(codeBlock(content, "text"))
		text.WriteByte('\n')
	}
	writeRange := func(start, end int) {
		for _, span := range spans {
			if span.start < start || span.end > end {
				continue
			}
			text.WriteString(unescapeHTML(source[start:span.start]))
			writeLiteral(unescapeHTML(span.content))
			start = span.end
		}
		text.WriteString(unescapeHTML(source[start:end]))
	}
	end = 0
	for _, match := range optionTag.FindAllStringSubmatchIndex(source, -1) {
		writeRange(end, match[0])
		writeLiteral(unescapeHTML(source[match[2]:match[3]]))
		end = match[1]
	}
	writeRange(end, len(source))
	return text.String()
}

func unescapeHTML(text string) string {
	// CGI.unescapeHTML recognizes these five named entities and numeric entities,
	// rather than the additional HTML5 names accepted by html.UnescapeString.
	return htmlEntity.ReplaceAllStringFunc(text, func(entity string) string {
		if named, ok := map[string]string{"&amp;": "&", "&quot;": "\"", "&gt;": ">", "&lt;": "<", "&apos;": "'"}[entity]; ok {
			return named
		}
		digits, base := entity[2:len(entity)-1], 10
		if strings.HasPrefix(digits, "x") {
			digits, base = digits[1:], 16
		}
		n, err := strconv.ParseInt(digits, base, 32)
		if err != nil || !utf8.ValidRune(rune(n)) {
			return entity
		}
		return string(rune(n))
	})
}

func writeOptions(b *strings.Builder, options []renderedOption, render ValueRenderer) error {
	for _, option := range options {
		defaultValue, err := option.defaultValue.format(render)
		if err != nil {
			return fmt.Errorf("%s: %w", option.name, err)
		}
		fmt.Fprintf(b, "%s\n\n:   %s\n\n    *Type:* %s\n\n    *Default:*\n\n", codeSpan(option.name), indentDescription(option.description), option.optionType)
		b.WriteString(indentDefinition(codeBlock(defaultValue, "nix")))
		if option.example != nil {
			example, err := option.example.format(render)
			if err != nil {
				return fmt.Errorf("%s: %w", option.name, err)
			}
			b.WriteString("\n    *Example:*\n\n")
			b.WriteString(indentDefinition(codeBlock(example, "nix")))
		}
		declarations := make([]string, len(option.declarations))
		for i, declaration := range option.declarations {
			declarations[i] = codeSpan(declaration)
		}
		fmt.Fprintf(b, "\n    *Declared by:* %s\n\n", strings.Join(declarations, ", "))
	}
	return nil
}

func indentDescription(description string) string {
	lines := strings.Split(description, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = "    " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func indentDefinition(block string) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "    " + line
		}
	}
	return strings.Join(lines, "\n")
}
