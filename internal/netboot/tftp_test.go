package netboot

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOriginalCompleteMenuBytes(t *testing.T) {
	c, ms := originalModel(t)
	d := OSDependencies()
	root := t.TempDir()
	// Only asset copies are injected; real private writes/links render every
	// template and preserve literal absolute link text without dereferencing it.
	d.FS.Copy = func(_, dst string) error { return os.WriteFile(dst, []byte("asset"), 0755) }
	// MAC collisions are tested separately. Keep this model's identical source
	// MAC values in JSON while not creating their duplicated routing link here.
	symlink := d.FS.Symlink
	d.FS.Symlink = func(src, dst string) error {
		if strings.Contains(dst, "/01-") {
			return nil
		}
		return symlink(src, dst)
	}
	if err := BuildTFTP(c, root, ms, d); err != nil {
		t.Fatal(err)
	}
	var names []string
	err := filepath.WalkDir("testdata/original/menus", func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("testdata/original/menus", path)
		if err != nil {
			return err
		}
		names = append(names, rel)
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		assertGolden(t, "menus/"+rel, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if (strings.HasSuffix(path, ".cfg") || path == filepath.Join(root, "pxelinux.cfg/default")) && !e.IsDir() {
			rel, _ := filepath.Rel(root, path)
			actual = append(actual, rel)
		}
		return nil
	})
	if !reflect.DeepEqual(names, actual) {
		t.Fatalf("menu set got %v want %v", actual, names)
	}
	menu, err := os.ReadFile(filepath.Join(root, "pxeserver/machines/node.test/menu.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(menu), "LABEL node.test-7") || !strings.Contains(string(menu), "LABEL node.test-6") {
		t.Fatal("source generations[1..] quirk lost")
	}
}
