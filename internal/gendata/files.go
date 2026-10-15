package gendata

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	containersFile = "data/vpsadmin/containers.nix"
	networksFile   = "data/vpsadmin/networks/containers.nix"
)

// Generator writes the two existing gen-data files using an invocation's
// already authenticated Source and the core's injected private Nix formatter.
// Parent directories must exist. It creates no locks, directories or recovery
// state; sequential old/new consumers keep the same filenames and Nix values.
type Generator struct {
	Root    string
	Source  Source
	Literal func(any) (string, error)
}

func (g Generator) Containers(ctx context.Context, machines []MachineInput) error {
	data, err := Containers(ctx, g.Source, machines)
	if err != nil {
		return err
	}
	return g.updateFile(containersFile, data)
}

func (g Generator) Network(ctx context.Context) error {
	data, err := Networks(ctx, g.Source)
	if err != nil {
		return err
	}
	return g.updateFile(networksFile, data)
}

// All replaces containers before making any network request with the same
// Source. A later network error leaves the completed containers file in place.
func (g Generator) All(ctx context.Context, machines []MachineInput) error {
	if err := g.Containers(ctx, machines); err != nil {
		return err
	}
	return g.Network(ctx)
}

func (g Generator) updateFile(relative string, data map[string]any) error {
	path := filepath.Join(g.Root, filepath.FromSlash(relative))
	tmp := path + ".new"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("%s: open: %w", tmp, err)
	}
	// Ruby opens/truncates .new before entering the serialization block. Never
	// render before this point or clean up the actual sibling on failure.
	if err := writeAndClose(file, data, g.Literal); err != nil {
		return fmt.Errorf("%s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%s -> %s: rename: %w", tmp, path, err)
	}
	return nil
}

// writeAndClose is the opened-file block, with exactly one close even when
// formatting or writing fails. A close error does not hide a primary error.
// It is separate solely to exercise synthetic write/close failures without a
// filesystem abstraction or relying on process privileges for chmod failures.
func writeAndClose(file io.WriteCloser, data map[string]any, literal func(any) (string, error)) (err error) {
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close: %w", closeErr)
		}
	}()
	if literal == nil {
		return fmt.Errorf("render: gen-data literal formatter is not configured")
	}
	text, err := literal(data)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	// The fixed formatter contract returns no final newline; append exactly
	// one LF without trimming or rewriting its contents.
	body := []byte(text + "\n")
	written, err := file.Write(body)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if written != len(body) {
		return fmt.Errorf("write: %w", io.ErrShortWrite)
	}
	return nil
}
