package generation

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// removeOps is limited to the source's unlink, predicate and rmdir operations.
// It supplies deterministic failure points without changing Writer's save ops.
type removeOps struct {
	unlink func(string) error
	stat   func(string) (os.FileInfo, error)
	lstat  func(string) (os.FileInfo, error)
	rmdir  func(string) error
}

func unlinkGenerationPath(path string) error {
	if err := syscall.Unlink(path); err != nil {
		return &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}

func rmdirGenerationPath(path string) error {
	if err := syscall.Rmdir(path); err != nil {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}

func nativeRemoveOps() removeOps {
	return removeOps{unlink: unlinkGenerationPath, stat: os.Stat, lstat: os.Lstat, rmdir: rmdirGenerationPath}
}

// Remove shares Writer's physical configuration path and lazy root resolver.
// It leaves Save/SetCurrent unchanged and retains the completed operation prefix
// on failure. Removing current does not repair its link or the in-memory list.
func (w *Writer) Remove(build *Build) error {
	return w.remove(build, nativeRemoveOps())
}

func (w *Writer) remove(build *Build, ops removeOps) error {
	dir := w.buildDir(build)
	removeRoot := func(component string) error {
		roots, err := w.roots()
		if err != nil {
			return err
		}
		err = ops.unlink(roots + "/" + RootName(build.Host, build.Name, component))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, component := range []string{"toplevel", "auto_rollback"} {
		if err := removeRoot(component); err != nil {
			return err
		}
	}
	for _, input := range build.Inputs {
		if err := removeRoot("input." + input.Name); err != nil {
			return err
		}
	}
	if err := ops.unlink(dir + "/toplevel"); err != nil {
		return err
	}
	if err := ops.unlink(dir + "/auto_rollback"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, input := range build.Inputs {
		path := dir + "/" + input.Name + ".input"
		// Ruby path conversion raises before a predicate can return false.
		// Check only this reached path, after the preceding root/link effects.
		if strings.IndexByte(path, 0) >= 0 {
			return &os.PathError{Op: "stat", Path: path, Err: syscall.EINVAL}
		}
		_, err := ops.stat(path)
		exists := err == nil
		if !exists {
			info, err := ops.lstat(path)
			exists = err == nil && info.Mode()&os.ModeSymlink != 0
		}
		if exists {
			if err := ops.unlink(path); err != nil {
				return err
			}
		}
	}
	if err := ops.unlink(dir + "/generation.json"); err != nil {
		return err
	}
	return ops.rmdir(dir)
}
