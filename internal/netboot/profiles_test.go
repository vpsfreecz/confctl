package netboot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fixture struct {
	t      *testing.T
	root   string
	c      Config
	d      Dependencies
	names  []string
	trace  []string
	out    bytes.Buffer
	stores map[string]string
}

func makeFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir(), stores: map[string]string{}}
	f.c = Config{Coreutils: filepath.Join(f.root, "tools/coreutils"), Syslinux: filepath.Join(f.root, "tools/syslinux"), TFTPRoot: filepath.Join(f.root, "roots/tftp"), HTTPRoot: filepath.Join(f.root, "roots/http"), HostName: "carrier.test", HTTPURL: "http://netboot.test"}
	f.d = OSDependencies()
	f.d.Profiles = filepath.Join(f.root, "profiles")
	f.d.LockFile = filepath.Join(f.root, "lock/build.lock")
	f.d.Out = &f.out
	f.d.Suffix = func() (string, error) { f.trace = append(f.trace, "suffix"); return "abcdef", nil }
	f.d.LocalTime = func(tm time.Time) string { return tm.In(time.UTC).Format("2006-01-02 15:04:05") }
	f.mkdir(f.d.Profiles)
	f.mkdir(filepath.Dir(f.c.TFTPRoot))
	for _, n := range []string{"pxelinux.0", "ldlinux.c32", "libcom32.c32", "libutil.c32", "memdisk", "menu.c32", "reboot.c32"} {
		f.write(filepath.Join(f.c.Syslinux, "share/syslinux", n), "asset:"+n, 0755)
	}
	f.d.FS.Entries = func(p string) ([]string, error) {
		f.trace = append(f.trace, "profiles")
		if p != f.d.Profiles {
			t.Fatalf("unexpected entry path %s", p)
		}
		return append([]string(nil), f.names...), nil
	}
	f.d.Lock = func(p string) (func() error, error) {
		f.trace = append(f.trace, "lock")
		close, err := acquireLock(p)
		if err != nil {
			return nil, err
		}
		return func() error { f.trace = append(f.trace, "unlock"); return close() }, nil
	}
	f.d.Child = func(a []string) error {
		f.trace = append(f.trace, "install:"+filepath.Base(a[3]))
		want := []string{filepath.Join(f.c.Coreutils, "bin/ln"), "-sfn", filepath.Base(a[3]) + ".abcdef", a[3]}
		if !reflect.DeepEqual(a, want) {
			t.Fatalf("literal child argv %q want %q", a, want)
		}
		// Finite stand-in for successful ln. It only unlinks this fixture's
		// target symlink and creates the nominated replacement in its parent.
		if err := os.Remove(a[3]); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(a[2], a[3])
	}
	remove := f.d.FS.RemoveTree
	f.d.FS.RemoveTree = func(p string) error { f.trace = append(f.trace, "cleanup:"+filepath.Base(p)); return remove(p) }
	f.add("rack:node", "7", map[string]any{"spin": "nixos", "fqdn": "node.test", "version": "24.11.20241001", "revision": "0123456789abcdef", "macs": []string{"AA:BB:CC:DD:EE:FF"}, "kernelParams": []string{"httproot=stale", "console=ttyS0", "console=ttyS0"}})
	f.add("rack:node", "6", map[string]any{"spin": "nixos", "fqdn": "node.test", "version": "24.11.20241001", "revision": "0123456789abcdef", "macs": []string{"AA:BB:CC:DD:EE:FF"}, "kernelParams": []string{"httproot=stale", "console=ttyS0", "console=ttyS0"}})
	f.add("rack:node", "5", map[string]any{"spin": "nixos", "fqdn": "node.test", "version": "24.11.20241001", "revision": "0123456789abcdef", "macs": []string{"AA:BB:CC:DD:EE:FF"}, "kernelParams": []string{"httproot=stale", "console=ttyS0", "console=ttyS0"}})
	f.current("rack:node", "7")
	return f
}
func (f *fixture) mkdir(p string) {
	f.t.Helper()
	if err := os.MkdirAll(p, 0777); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) write(p, s string, mode os.FileMode) {
	f.t.Helper()
	f.mkdir(filepath.Dir(p))
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) link(src, dst string) {
	f.t.Helper()
	if err := os.Symlink(src, dst); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) add(name, id string, meta map[string]any) {
	f.t.Helper()
	store := filepath.Join(f.root, "store", name+"-"+id)
	top := filepath.Join(f.root, "system", name+"-"+id)
	f.mkdir(store)
	f.mkdir(top)
	meta["toplevel"] = top
	b, err := json.Marshal(meta)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(store, "machine.json"), string(b), 0644)
	// This literal kernel link is only readlink'ed; it never resolves a host
	// store object. Boot payload links point solely at temporary fixture files.
	f.link("/nix/store/fixture-linux-6.6.52/bzImage", filepath.Join(top, "kernel"))
	for _, n := range []string{"bzImage", "initrd", "root.squashfs"} {
		asset := filepath.Join(f.root, "assets", name+"-"+id+"-"+n)
		f.write(asset, "payload:"+id+":"+n, 0644)
		f.link(asset, filepath.Join(store, n))
	}
	entry := "confctl-" + name + "-" + id + "-link"
	f.link(store, filepath.Join(f.d.Profiles, entry))
	f.names = append(f.names, entry)
	f.stores[name+"/"+id] = store
}
func (f *fixture) current(name, id string) {
	f.t.Helper()
	p := filepath.Join(f.d.Profiles, "confctl-"+name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	f.link(f.stores[name+"/"+id], p)
}
func (f *fixture) metadata(id string, edit func(map[string]any)) {
	f.t.Helper()
	p := filepath.Join(f.stores["rack:node/"+id], "machine.json")
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		f.t.Fatal(err)
	}
	edit(m)
	b, err = json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(p, string(b), 0644)
}
func (f *fixture) old(target, suffix string) string {
	f.t.Helper()
	old := target + "." + suffix
	f.mkdir(old)
	f.write(filepath.Join(old, "old"), "old sentinel", 0644)
	f.link(filepath.Base(old), target)
	return old
}
func (f *fixture) load() []*Machine {
	f.t.Helper()
	ms, err := LoadMachines(f.c, f.d)
	if err != nil {
		f.t.Fatal(err)
	}
	return ms
}
func requireError(t *testing.T, err error, part string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), part) {
		t.Fatalf("error=%v want %q", err, part)
	}
}
func requireLink(t *testing.T, p, want string) {
	t.Helper()
	s, err := os.Readlink(p)
	if err != nil || s != want {
		t.Fatalf("link %s=%q err=%v want %q", p, s, err, want)
	}
}
func requireAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("expected absent %s: %v", p, err)
	}
}

func TestNBNominationsDiscovery(t *testing.T) {
	t.Run("NB03_empty_nonmatching", func(t *testing.T) {
		f := makeFixture(t)
		f.names = []string{".", "..", "confctl-foo-x-link", "prefix-confctl-foo-1-link", "confctl-a-1-link-tail"}
		if got := f.load(); len(got) != 0 {
			t.Fatal(got)
		}
	})
	t.Run("NB04_current_not_newest", func(t *testing.T) {
		f := makeFixture(t)
		f.current("rack:node", "6")
		m := f.load()[0]
		if m.Current.ID.String() != "6" || m.Generations[0].ID.String() != "7" {
			t.Fatal(m)
		}
		b, err := renderMenu(nixosMenu, menuData{Config: f.c, Machine: m})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "LABEL node.test-7") || !strings.Contains(string(b), "LABEL node.test-6") {
			t.Fatal(string(b))
		}
	})
	t.Run("NB05_first_duplicate_target", func(t *testing.T) {
		f := makeFixture(t)
		p := filepath.Join(f.d.Profiles, "confctl-rack:node-99-link")
		f.link(f.stores["rack:node/7"], p)
		f.names = append([]string{filepath.Base(p)}, f.names...)
		m := f.load()[0]
		if m.Current.ID.String() != "99" || m.Generations[1].Current {
			t.Fatal("first enumeration match lost")
		}
	})
	t.Run("NB06_unmatched_current", func(t *testing.T) {
		f := makeFixture(t)
		p := filepath.Join(f.d.Profiles, "confctl-rack:node")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		f.link(f.root, p)
		_, err := LoadMachines(f.c, f.d)
		requireError(t, err, "Unable to find current")
	})
	t.Run("NB07_corrupt_metadata", func(t *testing.T) {
		f := makeFixture(t)
		f.write(filepath.Join(f.stores["rack:node/5"], "machine.json"), "{", 0644)
		_, err := LoadMachines(f.c, f.d)
		if err == nil {
			t.Fatal("corrupt older generation skipped")
		}
	})
	t.Run("NB08_missing_toplevel", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("5", func(m map[string]any) { delete(m, "toplevel") })
		_, err := LoadMachines(f.c, f.d)
		requireError(t, err, "missing toplevel")
	})
	t.Run("NB09_optional_absent", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("7", func(m map[string]any) {
			for _, k := range []string{"spin", "fqdn", "label", "revision", "version", "macs", "kernelParams"} {
				delete(m, k)
			}
		})
		m := f.load()[0]
		g := m.Current
		if m.Spin != "nixos" || m.FQDN != "rack:node" || m.Label != "rack:node" || g.Version != nil || g.Revision != nil || g.ShortRev != nil {
			t.Fatalf("%+v %+v", m, g)
		}
	})
	t.Run("NB25_regular_kernel_readlink", func(t *testing.T) {
		f := makeFixture(t)
		var top string
		f.metadata("7", func(m map[string]any) { top = m["toplevel"].(string) })
		p := filepath.Join(top, "kernel")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		f.write(p, "regular", 0644)
		_, err := LoadMachines(f.c, f.d)
		if !strings.Contains(fmt.Sprint(err), "invalid argument") {
			t.Fatal(err)
		}
	})
	t.Run("NB28_encounter_and_spin_sort", func(t *testing.T) {
		f := makeFixture(t)
		f.add("aaa-other", "1", map[string]any{"spin": "nixos", "fqdn": "zzz.test"})
		f.current("aaa-other", "1")
		ms := f.load()
		if ms[0].Name != "rack:node" || ms[1].Name != "aaa-other" {
			t.Fatal(ms)
		}
		root := t.TempDir()
		if err := BuildTFTP(f.c, root, ms, f.d); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(root, "pxeserver/nixos.cfg"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Index(string(b), "LABEL zzz.test") > strings.Index(string(b), "LABEL node.test") {
			t.Fatal("spin page not name-sorted")
		}
		b, err = pretty(object{{"machines", []any{machineRecord(ms[0]), machineRecord(ms[1])}}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Index(string(b), `"name": "rack:node"`) > strings.Index(string(b), `"name": "aaa-other"`) {
			t.Fatal("index reordered")
		}
	})
}

func TestNBNominationsParamsAndAssets(t *testing.T) {
	t.Run("NB10_vpsadminos", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("7", func(m map[string]any) { m["spin"] = "vpsadminos" })
		g := f.load()[0].Current
		if len(g.Variants) != 5 || g.Variants[0].Name != "default" || g.Variants[4].Name != "single" || !strings.HasSuffix(g.Params[0], "root.squashfs") {
			t.Fatal(g)
		}
	})
	t.Run("NB11_params_file_precedence", func(t *testing.T) {
		f := makeFixture(t)
		f.write(filepath.Join(f.stores["rack:node/7"], "kernel-params"), " \t\nhttproot=old console=one\vinit=explicit\fconsole=one\r\n\x00", 0644)
		g := f.load()[0].Current
		want := []string{"httproot=http://netboot.test/node.test/7/initrd", "console=one", "init=explicit", "console=one"}
		if !reflect.DeepEqual(g.Params, want) {
			t.Fatalf("%q", g.Params)
		}
	})
	t.Run("NB12_no_boot_files", func(t *testing.T) {
		f := makeFixture(t)
		for _, n := range []string{"bzImage", "initrd", "root.squashfs"} {
			if err := os.Remove(filepath.Join(f.stores["rack:node/7"], n)); err != nil {
				t.Fatal(err)
			}
		}
		m := f.load()[0]
		if len(m.Current.Boot) != 0 {
			t.Fatal(m.Current.Boot)
		}
		b, err := renderMenu(nixosAuto, menuData{Machine: m})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "INITRD boot/node.test/7/initrd") {
			t.Fatal(string(b))
		}
	})
	t.Run("NB13_bzImage_fallback", func(t *testing.T) {
		f := makeFixture(t)
		if err := os.Remove(filepath.Join(f.stores["rack:node/7"], "initrd")); err != nil {
			t.Fatal(err)
		}
		g := f.load()[0].Current
		if g.Params[0] != "httproot=http://netboot.test/node.test/7/bzImage" {
			t.Fatal(g.Params)
		}
	})
	t.Run("NB14_unknown_spin_partial", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("7", func(m map[string]any) { m["spin"] = "unknown" })
		err := Run(f.c, f.d)
		requireError(t, err, "no machine template")
		if _, err := os.Stat(f.c.TFTPRoot + ".abcdef/pxelinux.cfg/default"); err != nil {
			t.Fatal(err)
		}
		requireAbsent(t, f.c.HTTPRoot+".abcdef")
	})
	t.Run("NB15_duplicate_MAC", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("7", func(m map[string]any) { m["macs"] = []string{"aa:bb", "aa:bb"} })
		err := Run(f.c, f.d)
		if !os.IsExist(err) {
			t.Fatal(err)
		}
		requireLink(t, f.c.TFTPRoot+".abcdef/pxelinux.cfg/01-aa-bb", "../pxeserver/machines/node.test/auto.cfg")
		requireAbsent(t, f.c.HTTPRoot+".abcdef")
	})
	t.Run("NB16_MAC_case_unescaped_label", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("7", func(m map[string]any) { m["label"] = "A <>&\nLABEL injected" })
		if err := Run(f.c, f.d); err != nil {
			t.Fatal(err)
		}
		requireLink(t, f.c.TFTPRoot+".abcdef/pxelinux.cfg/01-AA-BB-CC-DD-EE-FF", "../pxeserver/machines/node.test/auto.cfg")
		b, err := os.ReadFile(f.c.TFTPRoot + ".abcdef/pxeserver/machines/node.test/menu.cfg")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "MENU TITLE A <>&\nLABEL injected\n") {
			t.Fatal(string(b))
		}
	})
	t.Run("NB17_memtest_ordered_ISO", func(t *testing.T) {
		f := makeFixture(t)
		f.c.Memtest = &Memtest{filepath.Join(f.root, "memtest"), []string{"one", "two"}}
		f.c.ISO = []ISOImage{{filepath.Join(f.root, "b.iso"), "B", "00-b.iso"}, {filepath.Join(f.root, "a.iso"), "a.iso", "01-a.iso"}}
		if err := Run(f.c, f.d); err != nil {
			t.Fatal(err)
		}
		requireLink(t, f.c.TFTPRoot+".abcdef/boot/memtest86/memtest.bin", f.c.Memtest.Package+"/memtest.bin")
		requireLink(t, f.c.TFTPRoot+".abcdef/boot/iso-images/00-b.iso", f.c.ISO[0].File)
		b, err := os.ReadFile(f.c.TFTPRoot + ".abcdef/pxeserver/iso-images.cfg")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Index(string(b), "00-b.iso") > strings.Index(string(b), "01-a.iso") {
			t.Fatal("ISO reordered")
		}
	})
}

func TestProfileMetadataTypesNumbersAndTimes(t *testing.T) {
	t.Run("label_trailing_dot_split", func(t *testing.T) {
		f := makeFixture(t)
		f.metadata("7", func(m map[string]any) { m["label"] = "one..." })
		m := f.load()[0]
		if m.ShortLabel != "one" {
			t.Fatalf("short label=%q", m.ShortLabel)
		}
	})
	for _, key := range []string{"toplevel", "spin", "fqdn", "label", "revision", "version", "kernelParams"} {
		for _, v := range []any{nil, json.Number("42")} {
			t.Run(fmt.Sprintf("%s_%v", key, v), func(t *testing.T) {
				f := makeFixture(t)
				f.metadata("7", func(m map[string]any) {
					m[key] = v
					if key == "version" {
						delete(m, "revision")
					}
				})
				_, err := LoadMachines(f.c, f.d)
				if (key == "label" || key == "revision" || key == "version") && v == nil {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("malformed value defaulted")
				}
			})
		}
	}
	t.Run("equal_huge_IDs", func(t *testing.T) {
		f := makeFixture(t)
		huge := "9007199254740993123456789"
		f.add("rack:node", huge, map[string]any{})
		p := filepath.Join(f.d.Profiles, "confctl-rack:node-0"+huge+"-link")
		f.link(f.stores["rack:node/"+huge], p)
		f.names = append([]string{filepath.Base(p)}, f.names...)
		f.current("rack:node", huge)
		m := f.load()[0]
		if m.Current.Link != p || m.Generations[0].Link != p || m.Generations[1].ID.String() != huge {
			t.Fatal("equal IDs reordered or narrowed")
		}
	})
	for _, tm := range []time.Time{time.Unix(-1, 750000000), time.Date(2024, 3, 31, 1, 0, 0, 0, time.UTC), time.Date(2024, 10, 27, 1, 0, 0, 0, time.UTC)} {
		t.Run(tm.String(), func(t *testing.T) {
			f := makeFixture(t)
			loc, err := time.LoadLocation("Europe/Amsterdam")
			if err != nil {
				t.Fatal(err)
			}
			orig := f.d.FS.Lstat
			f.d.FS.Lstat = func(p string) (os.FileInfo, error) {
				i, err := orig(p)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(p, "-link") {
					return timedInfo{i, tm}, nil
				}
				return i, nil
			}
			f.d.LocalTime = func(tm time.Time) string { return tm.In(loc).Format("2006-01-02 15:04:05") }
			g := f.load()[0].Current
			if g.Time.Unix() != tm.Unix() || g.TimeS != tm.In(loc).Format("2006-01-02 15:04:05") {
				t.Fatal(g)
			}
		})
	}
	t.Run("nonENOENT_boot_error", func(t *testing.T) {
		f := makeFixture(t)
		orig := f.d.FS.Realpath
		f.d.FS.Realpath = func(p string) (string, error) {
			if strings.HasSuffix(p, "/initrd") {
				return "", syscall.EACCES
			}
			return orig(p)
		}
		_, err := LoadMachines(f.c, f.d)
		if err != syscall.EACCES {
			t.Fatal(err)
		}
	})
	if !reflect.DeepEqual(pxe(&Generation{Toplevel: "/fixture/system"}), []string{"init=/fixture/system/init", "loglevel=7"}) {
		t.Fatal("pxe fallback changed")
	}
}

func TestNativeEnumerationUsesDirectoryOrder(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"z", "a", "m"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, len(entries))
	for i, e := range entries {
		want[i] = e.Name()
	}
	got, err := nativeEntries(root)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v err=%v", got, want, err)
	}
}

type timedInfo struct {
	os.FileInfo
	tm time.Time
}

func (i timedInfo) ModTime() time.Time { return i.tm }
