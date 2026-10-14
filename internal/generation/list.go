package generation

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

type Options struct {
	Location *time.Location
	// Enumerate returns names in filesystem enumeration order, not sorted order.
	Enumerate func(string) ([]string, error)
	// Warn receives newline-free messages. Core may copy them to stderr and log.
	Warn func(string) error
}
type Rejected struct{ Name, Path, Reason string }
type BuildList struct {
	Host         string
	Builds       []*Build
	Rejected     []Rejected
	Current      *Build
	CurrentError error
	index        map[string]*Build
	rejected     map[string]Rejected
}

func directoryNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
func LoadBuildList(root, host string, options Options) (*BuildList, error) {
	real, err := configurationPath(root)
	if err != nil {
		return nil, err
	}
	root = real
	list := &BuildList{Host: host, Builds: []*Build{}, Rejected: []Rejected{}, index: map[string]*Build{}, rejected: map[string]Rejected{}}
	dir := root + "/.confctl/generations/" + SafeHost(host)
	st, err := os.Stat(dir)
	// Dir.exist? returns false for failed stat as well as nondirectories.
	if err != nil || !st.IsDir() {
		return list, nil
	}
	enumerate := options.Enumerate
	if enumerate == nil {
		enumerate = directoryNames
	}
	names, err := enumerate(dir)
	if err != nil {
		return nil, err
	}
	warn := func(s string) error {
		if options.Warn != nil {
			return options.Warn(s)
		}
		return nil
	}
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		path := dir + "/" + name
		// Ruby Dir.exist? excludes nonexistent/non-directory entries before load.
		st, err := os.Lstat(path)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		build, err := LoadBuild(root, host, name, options)
		if err != nil {
			rejection := Rejected{Name: name, Path: path, Reason: err.Error()}
			list.Rejected = append(list.Rejected, rejection)
			list.rejected[name] = rejection
			if warningErr := warn(fmt.Sprintf("Skipping generation %s: %s. Records and GC roots were left untouched; use confctl v3 for old software-pin generations.", path, err)); warningErr != nil {
				return nil, warningErr
			}
			continue
		}
		list.Builds = append(list.Builds, build)
		list.index[name] = build
	}
	// The source comparison has no tie breaker. Retain its enumerated tie order.
	sort.SliceStable(list.Builds, func(i, j int) bool { return list.Builds[i].Date.Compare(list.Builds[j].Date) < 0 })
	current := dir + "/current"
	st, err = os.Lstat(current)
	if err == nil && st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(current)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(target)
		build := list.index[name]
		if build == nil {
			if rejection, ok := list.rejected[name]; ok {
				list.CurrentError = errors.New(rejection.Reason)
			} else {
				list.CurrentError = fmt.Errorf("%s: missing generation %s", current, name)
			}
		} else {
			resolved, err := filepath.EvalSymlinks(current)
			expected, expectedErr := filepath.EvalSymlinks(build.Dir)
			if err == nil {
				err = expectedErr
			}
			if err != nil {
				list.CurrentError = fmt.Errorf("%s: cannot resolve current target: %v", current, err)
			} else if resolved != expected {
				list.CurrentError = fmt.Errorf("%s: target does not resolve to local generation %s", current, name)
			} else {
				list.Current = build
			}
		}
	} else if err == nil {
		list.CurrentError = fmt.Errorf("%s: expected a symlink", current)
	} else {
		if len(list.Builds) != 0 {
			list.Current = list.Builds[len(list.Builds)-1]
			if len(list.Rejected) != 0 {
				if warningErr := warn(fmt.Sprintf("No current link for %s; selecting newest supported generation %s. Excluded records and GC roots were left untouched.", host, list.Current.Name)); warningErr != nil {
					return nil, warningErr
				}
			}
		}
	}
	if list.Current != nil {
		list.Current.Current = true
	}
	return list, nil
}
func (l *BuildList) RequireResolvedCurrent() error {
	if l.CurrentError != nil {
		return fmt.Errorf("Cannot select current generation for %s: %v", l.Host, l.CurrentError)
	}
	return nil
}

var decimalSelector = regexp.MustCompile(`^-?[0-9]+$`)

func (l *BuildList) CheckSelection(name string) error {
	if decimalSelector.MatchString(name) && len(l.Rejected) != 0 {
		return fmt.Errorf("Numeric generation selection for %s is ambiguous because records were excluded; select an explicit supported generation name", l.Host)
	}
	if name == "current" {
		if err := l.RequireResolvedCurrent(); err != nil {
			return err
		}
	}
	if r, ok := l.rejected[name]; ok {
		return fmt.Errorf("Cannot select generation %s for %s: %s", name, l.Host, r.Reason)
	}
	return nil
}

// ResolveBuild is the per-host cluster lookup, not generation ls pattern
// selection. Missing names/offsets return nil; callers own missing-host UI.
func ResolveBuild(l *BuildList, selector string) (*Build, error) {
	if err := l.CheckSelection(selector); err != nil {
		return nil, err
	}
	if selector == "current" {
		return l.Current, nil
	}
	if !decimalSelector.MatchString(selector) {
		return l.index[selector], nil
	}
	offset, _ := new(big.Int).SetString(selector, 10)
	n := big.NewInt(int64(len(l.Builds)))
	var index *big.Int
	if offset.Sign() == 0 {
		index = new(big.Int).Sub(n, big.NewInt(1))
	} else {
		index = new(big.Int).Sub(offset, big.NewInt(1))
		if index.Sign() < 0 {
			index.Add(index, n)
		}
	}
	if index.Sign() < 0 || index.Cmp(n) >= 0 {
		return nil, nil
	}
	return l.Builds[int(index.Int64())], nil
}
