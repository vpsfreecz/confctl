package inputs

import "reflect"

type Change struct {
	Name     string
	Old, New Info
}

func (c Change) URL() any { return first(c.New.URL, c.Old.URL) }

// DiffLocks compares revisions only, retaining the supplied order and duplicates.
// The external update owns lock bytes; this reader never serializes them again.
func DiffLocks(old, next *Lock, names []any) ([]Change, error) {
	changes := []Change{}
	if next == nil {
		return changes, nil
	}
	for _, name := range names {
		var before Info
		var err error
		if old != nil {
			before, err = old.InputInfo(name)
		}
		if err != nil {
			return nil, err
		}
		after, err := next.InputInfo(name)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(before.Rev, after.Rev) {
			// A non-string lookup has no node in either lock and cannot change.
			changes = append(changes, Change{Name: name.(string), Old: before, New: after})
		}
	}
	return changes, nil
}
