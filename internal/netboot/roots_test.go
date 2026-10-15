package netboot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNBNominationsRootEffects(t *testing.T) {
	t.Run("NB01_generated_suffix_success", func(t *testing.T) {
		f := makeFixture(t)
		oldT := f.old(f.c.TFTPRoot, "123abc")
		oldH := f.old(f.c.HTTPRoot, "456def")
		// A terminal symlink in the old tree must not delete its target.
		sentinel := filepath.Join(f.root, "sentinel")
		f.mkdir(sentinel)
		f.write(sentinel+"/keep", "keep", 0644)
		f.link(sentinel, oldT+"/external")
		if err := Run(f.c, f.d); err != nil {
			t.Fatal(err)
		}
		requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
		requireLink(t, f.c.HTTPRoot, "http.abcdef")
		requireAbsent(t, oldT)
		requireAbsent(t, oldH)
		b, err := os.ReadFile(sentinel + "/keep")
		if err != nil || string(b) != "keep" {
			t.Fatalf("sentinel %q %v", b, err)
		}
		want := []string{"lock", "profiles", "suffix", "install:tftp", "install:http", "cleanup:tftp.123abc", "cleanup:http.456def", "unlock"}
		if !reflect.DeepEqual(f.trace, want) {
			t.Fatalf("trace %v", f.trace)
		}
		if !strings.HasPrefix(f.out.String(), "node.test\n  - ") || !strings.HasSuffix(f.out.String(), "\n\n") || strings.Count(f.out.String(), " (current)") != 1 {
			t.Fatal(f.out.String())
		}
	})
	t.Run("NB19_regular_target_readlink", func(t *testing.T) {
		f := makeFixture(t)
		f.write(f.c.TFTPRoot, "regular", 0644)
		err := Run(f.c, f.d)
		if !errors.Is(err, syscall.EINVAL) {
			t.Fatal(err)
		}
		requireAbsent(t, f.c.TFTPRoot+".abcdef")
		requireAbsent(t, f.c.HTTPRoot+".abcdef")
	})
	for _, second := range []bool{false, true} {
		t.Run(fmt.Sprintf("NB%d_install_failure", 20+btoi(second)), func(t *testing.T) {
			f := makeFixture(t)
			oldT := f.old(f.c.TFTPRoot, "123abc")
			oldH := f.old(f.c.HTTPRoot, "456def")
			orig := f.d.Child
			n := 0
			f.d.Child = func(a []string) error {
				n++
				if n == 1 && !second {
					return errors.New("fake first start refusal")
				}
				if n == 2 { // A failing child can mutate the link before returning nonzero.
					if err := orig(a); err != nil {
						return err
					}
					return &exec.ExitError{}
				}
				return orig(a)
			}
			err := Run(f.c, f.d)
			requireError(t, err, "Failed to install new root")
			if _, err := os.Stat(f.c.HTTPRoot + ".abcdef/machines.json"); err != nil {
				t.Fatal("both trees not built", err)
			}
			if second {
				requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
				requireLink(t, f.c.HTTPRoot, "http.abcdef")
				if n != 2 {
					t.Fatal(n)
				}
			} else {
				requireLink(t, f.c.TFTPRoot, "tftp.123abc")
				requireLink(t, f.c.HTTPRoot, "http.456def")
				if n != 1 {
					t.Fatal(n)
				}
			}
			for _, p := range []string{oldT + "/old", oldH + "/old"} {
				if _, err := os.Stat(p); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range f.trace {
				if strings.HasPrefix(s, "cleanup:") {
					t.Fatal("cleanup after failed install")
				}
			}
		})
	}
	t.Run("NB22_first_cleanup_partial_failure", func(t *testing.T) {
		f := makeFixture(t)
		oldT := f.old(f.c.TFTPRoot, "123abc")
		oldH := f.old(f.c.HTTPRoot, "456def")
		f.d.FS.RemoveTree = func(p string) error {
			if p != oldT {
				t.Fatal("second cleanup attempted")
			}
			if err := os.Remove(p + "/old"); err != nil {
				return err
			}
			return syscall.EACCES
		}
		err := Run(f.c, f.d)
		if !errors.Is(err, syscall.EACCES) {
			t.Fatal(err)
		}
		requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
		requireLink(t, f.c.HTTPRoot, "http.abcdef")
		requireAbsent(t, oldT+"/old")
		if _, err := os.Stat(oldH + "/old"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("NB23_HTTP_partial_write_failure", func(t *testing.T) {
		f := makeFixture(t)
		orig := f.d.FS.Write
		f.d.FS.Write = func(p string, b []byte) error {
			if strings.HasSuffix(p, "/7/generation.json") {
				if err := orig(p, b[:11]); err != nil {
					return err
				}
				return syscall.ENOSPC
			}
			return orig(p, b)
		}
		err := Run(f.c, f.d)
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f.c.HTTPRoot + ".abcdef/node.test/7/generation.json")
		if err != nil || len(b) != 11 {
			t.Fatalf("partial bytes %q %v", b, err)
		}
		requireAbsent(t, f.c.TFTPRoot)
		requireAbsent(t, f.c.HTTPRoot)
		if _, err := os.Stat(f.c.TFTPRoot + ".abcdef/pxelinux.cfg/default"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("NB24_traversal_postinstall_refusal", func(t *testing.T) {
		f := makeFixture(t)
		outside := filepath.Join(f.root, "other/old")
		f.mkdir(outside)
		f.write(outside+"/keep", "keep", 0644)
		f.link("../other/old", f.c.TFTPRoot)
		oldH := f.old(f.c.HTTPRoot, "456def")
		err := Run(f.c, f.d)
		requireError(t, err, "refusing cleanup")
		requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
		requireLink(t, f.c.HTTPRoot, "http.abcdef")
		for _, p := range []string{outside + "/keep", oldH + "/old"} {
			if _, err := os.Stat(p); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("NB26_initially_absent", func(t *testing.T) {
		f := makeFixture(t)
		if err := Run(f.c, f.d); err != nil {
			t.Fatal(err)
		}
		requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
		requireLink(t, f.c.HTTPRoot, "http.abcdef")
		for _, s := range f.trace {
			if strings.HasPrefix(s, "cleanup:") {
				t.Fatal(f.trace)
			}
		}
	})
	t.Run("NB27_lock_EACCES", func(t *testing.T) {
		f := makeFixture(t)
		f.d.Lock = func(string) (func() error, error) { return nil, syscall.EACCES }
		err := Run(f.c, f.d)
		if !errors.Is(err, syscall.EACCES) || len(f.trace) != 0 {
			t.Fatal(err, f.trace)
		}
		requireAbsent(t, f.c.TFTPRoot+".abcdef")
	})
}
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestCleanupAcceptedDifferenceAndRefusals(t *testing.T) {
	for _, kind := range []string{"historical_handmade_old_changed_refusal", "absolute_generated", "absolute_alias", "missing_recorded", "old_symlink", "wrong_owner", "replaced_after_prepare", "missing_after_prepare", "new_suffix_collision", "live_noop_install", "parent_identity_change", "HTTP_refusal_after_TFTP_cleanup"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			suffix := "123abc"
			if kind == "historical_handmade_old_changed_refusal" {
				suffix = "old"
			}
			if kind == "new_suffix_collision" {
				suffix = "abcdef"
			}
			oldT := f.old(f.c.TFTPRoot, suffix)
			oldH := f.old(f.c.HTTPRoot, "456def")
			if kind == "absolute_generated" || kind == "absolute_alias" {
				if err := os.Remove(f.c.TFTPRoot); err != nil {
					t.Fatal(err)
				}
				src := oldT
				if kind == "absolute_alias" {
					src = filepath.Dir(oldT) + "/./" + filepath.Base(oldT)
				}
				f.link(src, f.c.TFTPRoot)
			}
			if kind == "old_symlink" {
				if err := os.Remove(oldT + "/old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(oldT); err != nil {
					t.Fatal(err)
				}
				f.link(oldH, oldT)
			}
			if kind == "missing_recorded" {
				if err := os.RemoveAll(oldT); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "wrong_owner" {
				orig := f.d.FS.Lstat
				f.d.FS.Lstat = func(p string) (os.FileInfo, error) {
					i, err := orig(p)
					if err != nil {
						return nil, err
					}
					if p == oldT {
						s := *i.Sys().(*syscall.Stat_t)
						s.Uid = f.d.EUID + 1
						return identityInfo{i, s}, nil
					}
					return i, nil
				}
			}
			if kind == "HTTP_refusal_after_TFTP_cleanup" {
				if err := os.Remove(f.c.HTTPRoot); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(oldH, f.c.HTTPRoot+".old"); err != nil {
					t.Fatal(err)
				}
				oldH = f.c.HTTPRoot + ".old"
				f.link("http.old", f.c.HTTPRoot)
			}
			origChild := f.d.Child
			n := 0
			if kind == "parent_identity_change" {
				orig := f.d.FS.Lstat
				f.d.FS.Lstat = func(p string) (os.FileInfo, error) {
					i, err := orig(p)
					if err != nil {
						return nil, err
					}
					if n > 0 && p == filepath.Dir(f.c.TFTPRoot) {
						s := *i.Sys().(*syscall.Stat_t)
						s.Ino++
						return identityInfo{i, s}, nil
					}
					return i, nil
				}
			}
			f.d.Child = func(a []string) error {
				n++
				if kind == "live_noop_install" {
					return nil
				}
				if n == 1 {
					switch kind {
					case "replaced_after_prepare":
						if err := os.Rename(oldT, oldT+"-retained"); err != nil {
							return err
						}
						f.mkdir(oldT)
						f.write(oldT+"/keep", "replacement", 0644)
					case "missing_after_prepare":
						if err := os.RemoveAll(oldT); err != nil {
							return err
						}
					case "missing_recorded":
						f.mkdir(oldT)
						f.write(oldT+"/keep", "late", 0644)
					}
				}
				return origChild(a)
			}
			err := Run(f.c, f.d)
			if kind == "absolute_generated" {
				if err != nil {
					t.Fatal(err)
				}
				requireAbsent(t, oldT)
				requireAbsent(t, oldH)
				return
			}
			requireError(t, err, "refusing cleanup")
			if kind != "live_noop_install" {
				requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
				requireLink(t, f.c.HTTPRoot, "http.abcdef")
			}
			if kind == "HTTP_refusal_after_TFTP_cleanup" {
				requireAbsent(t, oldT)
			} else if _, err := os.Lstat(oldH); err != nil {
				t.Fatal("second cleanup changed HTTP", err)
			}
			if kind != "missing_after_prepare" && kind != "HTTP_refusal_after_TFTP_cleanup" {
				if _, err := os.Lstat(oldT); err != nil {
					t.Fatal("refused old root removed", err)
				}
			}
		})
	}
}

type identityInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i identityInfo) Sys() any { return &i.stat }

func TestFullPrivateTreeLayoutAndModes(t *testing.T) {
	f := makeFixture(t)
	if err := Run(f.c, f.d); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pxelinux.0", "ldlinux.c32", "libcom32.c32", "libutil.c32", "memdisk", "menu.c32", "reboot.c32"} {
		src := filepath.Join(f.c.Syslinux, "share/syslinux", n)
		dst := f.c.TFTPRoot + ".abcdef/" + n
		b, err := os.ReadFile(dst)
		if err != nil || string(b) != "asset:"+n {
			t.Fatalf("asset=%q err=%v", b, err)
		}
		a, _ := os.Stat(src)
		z, _ := os.Stat(dst)
		if a.Mode().Perm() != z.Mode().Perm() {
			t.Fatal("asset mode", a.Mode(), z.Mode())
		}
	}
	var wantT, wantH []string
	wantT = append(wantT, "boot", "boot/node.test", "boot/node.test/current", "pxelinux.cfg", "pxelinux.cfg/01-AA-BB-CC-DD-EE-FF", "pxelinux.cfg/default", "pxeserver", "pxeserver/nixos.cfg", "pxeserver/machines", "pxeserver/machines/node.test", "pxeserver/machines/node.test/menu.cfg", "pxeserver/machines/node.test/auto.cfg")
	wantH = append(wantH, "machines.json", "node.test", "node.test/current", "node.test/machine.json")
	for _, id := range []string{"7", "6", "5"} {
		tp := f.c.TFTPRoot + ".abcdef/boot/node.test/" + id
		hp := f.c.HTTPRoot + ".abcdef/node.test/" + id
		wantT = append(wantT, "boot/node.test/"+id)
		wantH = append(wantH, "node.test/"+id, "node.test/"+id+"/generation.json", "node.test/"+id+"/kernel-params")
		for _, n := range []string{"bzImage", "initrd", "root.squashfs"} {
			asset, err := realpath(filepath.Join(f.stores["rack:node/"+id], n))
			if err != nil {
				t.Fatal(err)
			}
			requireLink(t, hp+"/"+n, asset)
			wantH = append(wantH, "node.test/"+id+"/"+n)
			if n != "root.squashfs" {
				requireLink(t, tp+"/"+n, asset)
				wantT = append(wantT, "boot/node.test/"+id+"/"+n)
			}
		}
		b, err := os.ReadFile(hp + "/kernel-params")
		if err != nil || strings.HasSuffix(string(b), "\n") {
			t.Fatal(string(b), err)
		}
	}
	for _, n := range []string{"pxelinux.0", "ldlinux.c32", "libcom32.c32", "libutil.c32", "memdisk", "menu.c32", "reboot.c32"} {
		wantT = append(wantT, n)
	}
	requireLink(t, f.c.TFTPRoot+".abcdef/boot/node.test/current", "7")
	requireLink(t, f.c.HTTPRoot+".abcdef/node.test/current", "7")
	checkTree(t, f.c.TFTPRoot+".abcdef", wantT)
	checkTree(t, f.c.HTTPRoot+".abcdef", wantH)
	// Compare generated directory/file modes with independent native creations
	// using the source's 0777/0666 requests under the actual inherited umask.
	refDir := filepath.Join(f.root, "mode-reference")
	if err := os.Mkdir(refDir, 0777); err != nil {
		t.Fatal(err)
	}
	refFile := filepath.Join(refDir, "file")
	if err := os.WriteFile(refFile, nil, 0666); err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(refDir)
	fi, _ := os.Stat(refFile)
	for _, root := range []string{f.c.TFTPRoot + ".abcdef", f.c.HTTPRoot + ".abcdef"} {
		if err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			i, err := e.Info()
			if err != nil {
				return err
			}
			if e.IsDir() {
				if i.Mode().Perm() != di.Mode().Perm() {
					t.Fatalf("directory mode %s=%v want %v", p, i.Mode(), di.Mode())
				}
			} else if e.Type()&os.ModeSymlink == 0 && filepath.Dir(p) != f.c.TFTPRoot+".abcdef" {
				if i.Mode().Perm() != fi.Mode().Perm() {
					t.Fatalf("generated file mode %s=%v want %v", p, i.Mode(), fi.Mode())
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConsoleExactBytesNilVersionAndCurrentOrder(t *testing.T) {
	f := makeFixture(t)
	f.current("rack:node", "6")
	for _, id := range []string{"7", "6", "5"} {
		f.metadata(id, func(m map[string]any) { delete(m, "version") })
	}
	f.d.LocalTime = func(_ time.Time) string { return "2024-10-01 00:00:00" }
	if err := Run(f.c, f.d); err != nil {
		t.Fatal(err)
	}
	want := "node.test\n  - 2024-10-01 00:00:00 - \n  - 2024-10-01 00:00:00 -  (current)\n  - 2024-10-01 00:00:00 - \n\n"
	if f.out.String() != want {
		t.Fatalf("stdout=%q want %q", f.out.String(), want)
	}
}
func checkTree(t *testing.T, root string, want []string) {
	t.Helper()
	set := map[string]bool{}
	for _, p := range want {
		set[p] = true
	}
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if !set[rel] {
			t.Fatalf("extra tree path %s", rel)
		}
		delete(set, rel)
		if e.Type()&os.ModeSymlink == 0 && e.IsDir() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0700 != 0700 {
				t.Fatalf("directory inaccessible %s", rel)
			}
		}
		return nil
	})
	if err != nil || len(set) != 0 {
		t.Fatalf("tree missing %v err=%v", set, err)
	}
}

func TestBuildOrderAndInjectedOperationFailures(t *testing.T) {
	for _, op := range []string{"entries", "mkdir", "copy", "read", "readlink", "realpath", "lstat", "write", "symlink", "suffix", "output", "lock_close"} {
		t.Run(op, func(t *testing.T) {
			f := makeFixture(t)
			boom := errors.New("injected " + op)
			switch op {
			case "entries":
				f.d.FS.Entries = func(string) ([]string, error) { return nil, boom }
			case "mkdir":
				f.d.FS.Mkdir = func(string) error { return boom }
			case "copy":
				f.d.FS.Copy = func(string, string) error { return boom }
			case "read":
				f.d.FS.Read = func(string) ([]byte, error) { return nil, boom }
			case "readlink":
				f.d.FS.Readlink = func(string) (string, error) { return "", boom }
			case "realpath":
				f.d.FS.Realpath = func(string) (string, error) { return "", boom }
			case "lstat":
				f.d.FS.Lstat = func(string) (os.FileInfo, error) { return nil, boom }
			case "write":
				f.d.FS.Write = func(string, []byte) error { return boom }
			case "symlink":
				f.d.FS.Symlink = func(string, string) error { return boom }
			case "suffix":
				f.d.Suffix = func() (string, error) { return "", boom }
			case "output":
				f.d.Out = errorWriter{boom}
			case "lock_close":
				orig := f.d.Lock
				f.d.Lock = func(p string) (func() error, error) {
					close, err := orig(p)
					if err != nil {
						return nil, err
					}
					return func() error { _ = close(); return boom }, nil
				}
			}
			err := Run(f.c, f.d)
			if !errors.Is(err, boom) {
				t.Fatal(err)
			}
			if op != "lock_close" {
				requireAbsent(t, f.c.TFTPRoot)
				requireAbsent(t, f.c.HTTPRoot)
			}
		})
	}
	t.Run("both_built_before_install", func(t *testing.T) {
		f := makeFixture(t)
		orig := f.d.Child
		f.d.Child = func(a []string) error {
			for _, p := range []string{f.c.TFTPRoot + ".abcdef/pxelinux.cfg/default", f.c.HTTPRoot + ".abcdef/machines.json"} {
				if _, err := os.Stat(p); err != nil {
					t.Fatal("install before build completed", err)
				}
			}
			return orig(a)
		}
		if err := Run(f.c, f.d); err != nil {
			t.Fatal(err)
		}
	})
}

func TestInjectedEachInstallStartWaitAndSecondCleanupErrors(t *testing.T) {
	for _, call := range []int{1, 2} {
		for _, phase := range []string{"start", "wait"} {
			t.Run(fmt.Sprintf("install%d_%s_synthetic_error", call, phase), func(t *testing.T) {
				f := makeFixture(t)
				oldT := f.old(f.c.TFTPRoot, "123abc")
				oldH := f.old(f.c.HTTPRoot, "456def")
				orig := f.d.Child
				n := 0
				boom := errors.New("synthetic " + phase + " error")
				f.d.Child = func(a []string) error {
					n++
					if n == call && phase == "start" {
						return boom
					}
					if err := orig(a); err != nil {
						return err
					}
					if n == call {
						return boom
					}
					return nil
				}
				if err := Run(f.c, f.d); !errors.Is(err, boom) {
					t.Fatal(err)
				}
				if n != call {
					t.Fatal("child count", n)
				}
				for _, p := range []string{oldT + "/old", oldH + "/old"} {
					if _, err := os.Stat(p); err != nil {
						t.Fatal("cleanup on install error", err)
					}
				}
				wantT := "tftp.123abc"
				if call == 2 || phase == "wait" {
					wantT = "tftp.abcdef"
				}
				wantH := "http.456def"
				if call == 2 && phase == "wait" {
					wantH = "http.abcdef"
				}
				requireLink(t, f.c.TFTPRoot, wantT)
				requireLink(t, f.c.HTTPRoot, wantH)
			})
		}
	}
	t.Run("HTTP_cleanup_partial_error", func(t *testing.T) {
		f := makeFixture(t)
		oldT := f.old(f.c.TFTPRoot, "123abc")
		oldH := f.old(f.c.HTTPRoot, "456def")
		f.write(oldH+"/keep", "retained", 0644)
		orig := f.d.FS.RemoveTree
		f.d.FS.RemoveTree = func(p string) error {
			if p == oldH {
				if err := os.Remove(oldH + "/old"); err != nil {
					return err
				}
				return syscall.EIO
			}
			return orig(p)
		}
		err := Run(f.c, f.d)
		if !errors.Is(err, syscall.EIO) {
			t.Fatal(err)
		}
		requireAbsent(t, oldT)
		requireAbsent(t, oldH+"/old")
		b, err := os.ReadFile(oldH + "/keep")
		if err != nil || string(b) != "retained" {
			t.Fatal(string(b), err)
		}
		requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
		requireLink(t, f.c.HTTPRoot, "http.abcdef")
	})
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestLockRetainsDescriptorAndCompatibleContention(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "run/lock")
	holder, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
		t.Fatalf("shared lock protocol did not contend: %v", err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		closeLock, err := acquireLock(path)
		if err == nil {
			err = closeLock()
		}
		done <- err
	}()
	<-started
	if err := holder(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lock removed", err)
	}
	f := makeFixture(t)
	orig := f.d.Child
	f.d.Child = func(a []string) error {
		other, err := os.OpenFile(f.d.LockFile, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer other.Close()
		if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != syscall.EWOULDBLOCK {
			t.Fatalf("lock released during install: %v", err)
		}
		return orig(a)
	}
	if err := Run(f.c, f.d); err != nil {
		t.Fatal(err)
	}
}

func TestNetbootFakeChild(t *testing.T) {
	if os.Getenv("CONFCTL_NETBOOT_FAKE_CHILD") != "yes" {
		return
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(31)
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(32)
	}
	fmt.Fprintf(os.Stdout, "stdout:%s:%s:%s", b, os.Getenv("CONFCTL_NETBOOT_LITERAL"), cwd)
	fmt.Fprintf(os.Stderr, "stderr:%s", strings.Join(os.Args[1:], "|"))
	if os.Getenv("CONFCTL_NETBOOT_FAIL") == "yes" {
		os.Exit(23)
	}
	os.Exit(0)
}

func TestInheritedDirectChild(t *testing.T) {
	root := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldCWD) }()
	t.Setenv("CONFCTL_NETBOOT_FAKE_CHILD", "yes")
	t.Setenv("CONFCTL_NETBOOT_LITERAL", "literal $() ; <>&")
	stdin, err := os.CreateTemp(root, "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.WriteString("input\x00\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(root, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(root, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	in, out, e := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
	defer func() { os.Stdin, os.Stdout, os.Stderr = in, out, e }()
	argv := []string{bin, "-test.run=^TestNetbootFakeChild$", "--", "literal $() ; ' \""}
	if err := inheritedChild(argv); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(stdout.Name())
	if err != nil || string(b) != "stdout:input\x00\n:literal $() ; <>&:"+root {
		t.Fatalf("stdout=%q err=%v", b, err)
	}
	b, err = os.ReadFile(stderr.Name())
	if err != nil || string(b) != "stderr:"+strings.Join(argv[1:], "|") {
		t.Fatalf("stderr=%q err=%v", b, err)
	}
	t.Setenv("CONFCTL_NETBOOT_FAIL", "yes")
	err = inheritedChild(argv)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 || !strings.HasPrefix(err.Error(), "wait ") {
		t.Fatal(err)
	}
	err = inheritedChild([]string{filepath.Join(root, "absent")})
	if err == nil || !strings.HasPrefix(err.Error(), "start ") {
		t.Fatal(err)
	}
	nonexec := filepath.Join(root, "nonexec")
	if err := os.WriteFile(nonexec, []byte("literal nonexecutable"), 0644); err != nil {
		t.Fatal(err)
	}
	err = inheritedChild([]string{nonexec})
	if !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
	// A child exit and a start error remain distinct; no second wait or retry.
}
