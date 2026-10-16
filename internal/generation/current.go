package generation

import "fmt"

// SetCurrent mutates the exact list before the random/link/rename operations,
// as BuildList#current= does. It neither sorts nor reclassifies rejected state.
func (w *Writer) SetCurrent(list *BuildList, build *Build) error {
	list.CurrentError = nil
	list.Current = build
	for _, old := range list.Builds {
		old.Current = false
	}
	build.Current = true
	if list.index == nil {
		list.index = map[string]*Build{}
	}
	list.index[build.Name] = build
	found := false
	for _, old := range list.Builds {
		if old == build {
			found = true
			break
		}
	}
	if !found {
		list.Builds = append(list.Builds, build)
	}
	suffix, err := w.suffix()
	if err != nil {
		return err
	}
	if len(suffix) != 6 {
		return fmt.Errorf("current suffix must contain six lowercase hexadecimal characters")
	}
	for _, c := range suffix {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return fmt.Errorf("current suffix must contain six lowercase hexadecimal characters")
		}
	}
	path := w.root + "/.confctl/generations/" + SafeHost(list.Host) + "/current"
	replacement := path + ".new-" + suffix
	if err := w.ops.symlink(build.Name, replacement); err != nil {
		return err
	}
	return w.ops.rename(replacement, path)
}
