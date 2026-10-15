package netboot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Filesystem struct {
	Entries            func(string) ([]string, error)
	Read               func(string) ([]byte, error)
	Readlink, Realpath func(string) (string, error)
	Lstat, Stat        func(string) (os.FileInfo, error)
	Mkdir              func(string) error
	Copy, Symlink      func(string, string) error
	Write              func(string, []byte) error
	RemoveTree         func(string) error
}
type Dependencies struct {
	FS                 Filesystem
	Profiles, LockFile string
	Lock               func(string) (func() error, error)
	Suffix             func() (string, error)
	LocalTime          func(time.Time) string
	Child              func([]string) error
	EUID               uint32
	Out                io.Writer
}

func OSDependencies() Dependencies {
	return Dependencies{FS: Filesystem{Entries: nativeEntries, Read: os.ReadFile, Readlink: os.Readlink, Realpath: realpath, Lstat: os.Lstat, Stat: os.Stat, Mkdir: func(p string) error { return os.MkdirAll(p, 0777) }, Copy: copyFile, Symlink: os.Symlink, Write: func(p string, b []byte) error { return os.WriteFile(p, b, 0666) }, RemoveTree: removeNoFollow}, Profiles: "/nix/var/nix/profiles", LockFile: "/run/confctl/build-netboot-server.lock", Lock: acquireLock, Suffix: func() (string, error) {
		b := make([]byte, 3)
		_, err := rand.Read(b)
		return hex.EncodeToString(b), err
	}, LocalTime: func(t time.Time) string { return t.In(time.Local).Format("2006-01-02 15:04:05") }, Child: inheritedChild, EUID: uint32(os.Geteuid()), Out: os.Stdout}
}
func nativeEntries(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = v.Name()
	}
	return out, nil
}
func realpath(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func acquireLock(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f.Close, nil
}
func inheritedChild(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait %s: %w", argv[0], err)
	}
	return nil
}

// Terminal symlinks are unlinked, never opened. Directory opens also refuse a
// replaced symlink. This assumes a stable trusted parent, not a privileged-race
// sandbox or a mount-namespace guarantee.
func removeNoFollow(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		if err := removeNoFollow(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
func writeTo(root, path string, data []byte, d Dependencies) error {
	if err := d.FS.Mkdir(join(root, filepath.Dir(path))); err != nil {
		return err
	}
	return d.FS.Write(join(root, path), data)
}

type identity struct {
	dev, ino uint64
	uid      uint32
	kind     os.FileMode
}

func identify(i os.FileInfo) (identity, error) {
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, fmt.Errorf("unavailable filesystem identity")
	}
	return identity{uint64(s.Dev), uint64(s.Ino), s.Uid, i.Mode().Type()}, nil
}

type rootPlan struct {
	target, newRoot, oldText, parent, oldPath, refusal string
	oldID, parentID                                    identity
	hasOld                                             bool
}

func prepareRoot(target, suffix string, d Dependencies) (rootPlan, error) {
	p := rootPlan{target: target, newRoot: target + "." + suffix}
	text, err := d.FS.Readlink(target)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.hasOld = true
	p.oldText = text
	parent, err := d.FS.Realpath(filepath.Dir(target))
	if err != nil {
		p.refusal = "cannot bind configured parent"
		return p, nil
	}
	p.parent = parent
	info, err := d.FS.Lstat(parent)
	if err != nil {
		p.refusal = "cannot stat configured parent"
		return p, nil
	}
	p.parentID, err = identify(info)
	if err != nil || !info.IsDir() {
		p.refusal = "invalid configured parent identity"
		return p, nil
	}
	base := filepath.Base(target)
	pattern := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `\.[0-9a-f]{6}$`)
	name := text
	if filepath.IsAbs(text) {
		if filepath.Clean(text) != text || filepath.Dir(text) != parent {
			p.refusal = "old root is not a canonical sibling"
			return p, nil
		}
		name = filepath.Base(text)
	}
	if strings.ContainsAny(name, "/\\") || !pattern.MatchString(name) {
		p.refusal = "old root is not a generated six-hex sibling"
		return p, nil
	}
	p.oldPath = filepath.Join(parent, name)
	info, err = d.FS.Lstat(p.oldPath)
	if err != nil {
		p.refusal = "recorded old root is missing or inaccessible"
		return p, nil
	}
	p.oldID, err = identify(info)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		p.refusal = "old root is not a real directory"
		return p, nil
	}
	if p.oldID.uid != d.EUID {
		p.refusal = "old root has a different owner"
	}
	return p, nil
}
func cleanup(p rootPlan, plans []rootPlan, d Dependencies) error {
	if !p.hasOld {
		return nil
	}
	refuse := func(why string) error { return fmt.Errorf("refusing cleanup of %s (%s): %s", p.target, p.oldText, why) }
	if p.refusal != "" {
		return refuse(p.refusal)
	}
	parent, err := d.FS.Realpath(filepath.Dir(p.target))
	if err != nil || parent != p.parent {
		return refuse("configured parent changed")
	}
	i, err := d.FS.Lstat(parent)
	if err != nil {
		return refuse("configured parent missing")
	}
	id, err := identify(i)
	if err != nil || id != p.parentID {
		return refuse("configured parent identity changed")
	}
	i, err = d.FS.Lstat(p.oldPath)
	if err != nil {
		return refuse("old root missing")
	}
	id, err = identify(i)
	if err != nil || id != p.oldID || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 || id.uid != d.EUID {
		return refuse("old root identity/type/owner changed")
	}
	for _, other := range plans {
		newParent, err := d.FS.Realpath(filepath.Dir(other.newRoot))
		if err != nil {
			return refuse("cannot verify new root parent")
		}
		newPath := filepath.Join(newParent, filepath.Base(other.newRoot))
		if p.oldPath == newPath {
			return refuse("old root is a new root")
		}
		if info, err := d.FS.Lstat(newPath); err == nil {
			otherID, e := identify(info)
			if e == nil && otherID == id {
				return refuse("old root shares new root identity")
			}
		} else if !os.IsNotExist(err) {
			return refuse("cannot verify new root identity")
		}
		live, err := d.FS.Realpath(other.target)
		if err == nil {
			if live == p.oldPath {
				return refuse("old root is live")
			}
			info, err := d.FS.Lstat(live)
			if err != nil {
				return refuse("cannot verify live root identity")
			}
			otherID, e := identify(info)
			if e == nil && otherID == id {
				return refuse("old root shares live root identity")
			}
		} else if !os.IsNotExist(err) {
			return refuse("cannot resolve live root")
		}
	}
	return d.FS.RemoveTree(p.oldPath)
}

func Run(c Config, d Dependencies) (err error) {
	closeLock, err := d.Lock(d.LockFile)
	if err != nil {
		return err
	}
	defer func() {
		e := closeLock()
		if err == nil {
			err = e
		}
	}()
	machines, err := LoadMachines(c, d)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if _, err := fmt.Fprintln(d.Out, m.FQDN); err != nil {
			return err
		}
		for _, g := range m.Generations {
			current := ""
			if g.Current {
				current = " (current)"
			}
			if _, err := fmt.Fprintf(d.Out, "  - %s - %s%s\n", g.TimeS, display(g.Version), current); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(d.Out); err != nil {
			return err
		}
	}
	suffix, err := d.Suffix()
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{6}$`).MatchString(suffix) {
		return fmt.Errorf("invalid random root suffix")
	}
	plans := make([]rootPlan, 2)
	for i, target := range []string{c.TFTPRoot, c.HTTPRoot} {
		plans[i], err = prepareRoot(target, suffix, d)
		if err != nil {
			return err
		}
	}
	for i, p := range plans {
		if err := d.FS.Mkdir(p.newRoot); err != nil {
			return err
		}
		if i == 0 {
			err = BuildTFTP(c, p.newRoot, machines, d)
		} else {
			err = BuildHTTP(c, p.newRoot, machines, d)
		}
		if err != nil {
			return err
		}
	}
	for _, p := range plans {
		argv := []string{join(c.Coreutils, "bin/ln"), "-sfn", filepath.Base(p.newRoot), p.target}
		if err := d.Child(argv); err != nil {
			return fmt.Errorf("Failed to install new root to %s: %w", p.target, err)
		}
	}
	for _, p := range plans {
		if err := cleanup(p, plans, d); err != nil {
			return err
		}
	}
	return nil
}
func display(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
