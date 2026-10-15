package docgen

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type faultFile struct {
	io.WriteCloser
	events   *[]string
	phase    string
	sentinel error
}

func (f faultFile) Write(data []byte) (int, error) {
	*f.events = append(*f.events, "write")
	if f.phase == "write" || f.phase == "short write" {
		n, err := f.WriteCloser.Write(data[:len(data)/2])
		if err != nil {
			return n, err
		}
		if f.phase == "write" {
			return n, f.sentinel
		}
		return n, nil
	}
	return f.WriteCloser.Write(data)
}

func (f faultFile) Close() error {
	*f.events = append(*f.events, "close")
	err := f.WriteCloser.Close()
	if f.phase == "close" {
		return f.sentinel
	}
	return err
}

func TestFixedSiblingFailureOrderAndResidue(t *testing.T) {
	for _, test := range []struct {
		phase, wantSibling string
		events             []string
	}{
		{"render", "prior sibling", []string{"render"}},
		{"open", "prior sibling", []string{"render", "open"}},
		{"write", "new", []string{"render", "open", "write", "close"}},
		{"short write", "new", []string{"render", "open", "write", "close"}},
		{"close", "new doc", []string{"render", "open", "write", "close"}},
		{"rename", "new doc", []string{"render", "open", "write", "close", "rename"}},
		{"success", "", []string{"render", "open", "write", "close", "rename"}},
	} {
		t.Run(test.phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manual.md")
			if err := os.WriteFile(path, []byte("old doc"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".new", []byte("prior sibling"), 0600); err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("fixture " + test.phase)
			var events []string
			render := func() ([]byte, error) {
				events = append(events, "render")
				if test.phase == "render" {
					return nil, sentinel
				}
				return []byte("new doc"), nil
			}
			files := fileOps{
				open: func(name string) (io.WriteCloser, error) {
					events = append(events, "open")
					if name != path+".new" {
						t.Fatalf("non-source temporary path: %s", name)
					}
					if test.phase == "open" {
						return nil, sentinel
					}
					file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
					if err != nil {
						return nil, err
					}
					return faultFile{file, &events, test.phase, sentinel}, nil
				},
				rename: func(old, new string) error {
					events = append(events, "rename")
					if old != path+".new" || new != path {
						t.Fatalf("unexpected rename: %s -> %s", old, new)
					}
					if test.phase == "rename" {
						return sentinel
					}
					return os.Rename(old, new)
				},
			}
			err := writeFile(path, render, files)
			if test.phase == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if test.phase == "short write" {
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("lost short write: %v", err)
				}
			} else if !errors.Is(err, sentinel) {
				t.Fatalf("lost original failure: %v", err)
			}
			if !reflect.DeepEqual(events, test.events) {
				t.Fatalf("phase order %v, want %v", events, test.events)
			}
			wantDestination := "old doc"
			if test.phase == "success" {
				wantDestination = "new doc"
			}
			if content, err := os.ReadFile(path); err != nil || string(content) != wantDestination {
				t.Fatalf("destination changed incorrectly: %q, %v", content, err)
			}
			content, err := os.ReadFile(path + ".new")
			if test.phase == "success" {
				if !os.IsNotExist(err) {
					t.Fatalf("successful rename left sibling: %v", err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("existing sibling permissions changed: %v, %v", info, err)
				}
			} else if err != nil || string(content) != test.wantSibling {
				t.Fatalf("failure residue %q, want %q: %v", content, test.wantSibling, err)
			}
		})
	}
}

func TestWriteFileRealEffects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manual.md")
	want := []byte("source\ntext\x00\n")
	if err := WriteFile(path, func() ([]byte, error) { return want, nil }); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(content, want) {
		t.Fatalf("output bytes changed: %q, %v", content, err)
	}
	if err := WriteFile(filepath.Join(root, "missing", "manual.md"), func() ([]byte, error) { return want, nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent unexpectedly created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatal("writer created a parent directory")
	}
	// A real rename refusal leaves the complete sibling for inspection.
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(directory, func() ([]byte, error) { return want, nil }); err == nil {
		t.Fatal("rename over directory succeeded")
	}
	if content, err := os.ReadFile(directory + ".new"); err != nil || !reflect.DeepEqual(content, want) {
		t.Fatalf("rename failure residue lost: %q, %v", content, err)
	}
}

func TestExistingSiblingSymlinkKeepsOrdinaryWriteEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manual.md")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path+".new"); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("fixture rename refusal")
	err := writeFile(path, func() ([]byte, error) { return []byte("written through sibling"), nil }, fileOps{
		open: func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		},
		rename: func(string, string) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rename refusal lost: %v", err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "written through sibling" {
		t.Fatalf("ordinary symlink write effect changed: %q, %v", content, err)
	}
	if target, err := os.Readlink(path + ".new"); err != nil || target != path {
		t.Fatalf("sibling failure residue changed: %q, %v", target, err)
	}
}
