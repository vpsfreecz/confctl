package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/core"
	"github.com/vpsfreecz/confctl/internal/docgen"
)

type dependencies struct {
	loadOptions func(context.Context, string, string) ([]docgen.ModuleOption, error)
	renderValue docgen.ValueRenderer
}

func main() {
	ctx := context.Background()
	deps := dependencies{
		loadOptions: optionsLoader(os.Stderr, evaluateOptions),
		renderValue: core.DocumentationNixLiteral,
	}
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, deps))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	flags := flag.NewFlagSet("confctl-docs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	kind := flags.String("kind", "commands", "Document to generate: commands or options")
	version := flags.String("version", "", "Version displayed in the document (required)")
	date := flags.String("date", "", "Document date as YYYY-MM-DD (required)")
	output := flags.String("output", "", "Output path; omit to write Markdown to stdout")
	sourceRoot := flags.String("source-root", ".", "Confctl source directory for moduleOptions and declaration paths")
	optionsFile := flags.String("module-options", "", "Read moduleOptions JSON from this file instead of evaluating Nix")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "confctl-docs: unexpected positional arguments")
		return 2
	}
	metadata := docgen.Metadata{Version: *version, Date: *date}
	var render func() ([]byte, error)
	switch *kind {
	case "commands":
		if *optionsFile != "" {
			fmt.Fprintln(stderr, "confctl-docs: --module-options requires --kind options")
			return 2
		}
		render = func() ([]byte, error) { return docgen.Commands(cli.BuiltinRegistry(), metadata) }
	case "options":
		root, err := filepath.Abs(*sourceRoot)
		if err != nil {
			fmt.Fprintf(stderr, "confctl-docs: source directory: %v\n", err)
			return 1
		}
		loader := func(ctx context.Context) ([]docgen.ModuleOption, error) {
			return deps.loadOptions(ctx, root, *optionsFile)
		}
		render = func() ([]byte, error) { return docgen.Options(ctx, loader, deps.renderValue, root, metadata) }
	default:
		fmt.Fprintf(stderr, "confctl-docs: unknown document kind %q\n", *kind)
		return 2
	}
	var err error
	if *output != "" {
		err = docgen.WriteFile(*output, render)
	} else {
		var content []byte
		content, err = render()
		if err == nil {
			var n int
			n, err = stdout.Write(content)
			if err == nil && n != len(content) {
				err = io.ErrShortWrite
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "confctl-docs: %v\n", err)
		return 1
	}
	return 0
}

type evaluator func(context.Context, string, []string, io.Writer) ([]byte, error)

// Nix execution is confined to this entry adapter. The private renderer receives
// decoded declarations and does not create a machine evaluator or load settings.
func optionsLoader(stderr io.Writer, evaluate evaluator) func(context.Context, string, string) ([]docgen.ModuleOption, error) {
	return func(ctx context.Context, sourceRoot, optionsFile string) ([]docgen.ModuleOption, error) {
		if optionsFile != "" {
			file, err := os.Open(optionsFile)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			return docgen.DecodeOptions(file)
		}
		content, err := evaluate(ctx, "nix", []string{
			"--extra-experimental-features", "nix-command flakes", "eval", "--json", sourceRoot + "#moduleOptions",
		}, stderr)
		if err != nil {
			return nil, fmt.Errorf("evaluate moduleOptions: %w", err)
		}
		return docgen.DecodeOptions(bytes.NewReader(content))
	}
}

func evaluateOptions(ctx context.Context, program string, args []string, stderr io.Writer) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Stderr = stderr
	return command.Output()
}
