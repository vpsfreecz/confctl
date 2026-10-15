package docgen

import (
	"fmt"
	"io"
	"os"
)

type fileOps struct {
	open   func(string) (io.WriteCloser, error)
	rename func(string, string) error
}

// WriteFile renders completely, writes the fixed .new sibling, closes it and
// renames it over the destination. It creates no directories and removes no
// failure residue. Existing .new permissions follow ordinary file-write rules.
func WriteFile(path string, render func() ([]byte, error)) error {
	return writeFile(path, render, fileOps{
		open: func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		},
		rename: os.Rename,
	})
}

func writeFile(path string, render func() ([]byte, error), files fileOps) error {
	content, err := render()
	if err != nil {
		return err
	}
	sibling := path + ".new"
	file, err := files.open(sibling)
	if err != nil {
		return fmt.Errorf("write %s: %w", sibling, err)
	}
	n, writeErr := file.Write(content)
	if writeErr == nil && n != len(content) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write %s: %w", sibling, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", sibling, closeErr)
	}
	if err := files.rename(sibling, path); err != nil {
		return fmt.Errorf("rename %s: %w", sibling, err)
	}
	return nil
}
