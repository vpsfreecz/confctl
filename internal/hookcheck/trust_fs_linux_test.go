package hookcheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func publicationFixture(t *testing.T) (*localTrustDirectory, []byte, string) {
	t.Helper()
	q, io, root := trustFixture(t, false)
	p, _, err := PrepareTrust(context.Background(), q, io)
	if err != nil {
		t.Fatal(err)
	}
	i, err := localOpen(root, filepath.Join(root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	d := i.(*localTrustDirectory)
	t.Cleanup(func() { d.Close() })
	return d, bytes.Clone(p.candidate), root
}

func TestTrustFilesystemAtomicPublicationAndUnchangedSentinels(t *testing.T) {
	d, data, root := publicationFixture(t)
	put(t, root, ".git/config-sentinel", "common sentinel")
	put(t, root, ".git/hooks/sentinel", "hook sentinel")
	before := fixtureState(t, root)
	out, err := d.Publish(context.Background(), ReceiptImage{}, data)
	if err != nil || !out.Published {
		t.Fatalf("publication: %#v %v", out, err)
	}
	image, err := d.ReadReceipt()
	if err != nil || !image.Present || !bytes.Equal(image.Data, data) || image.File.Mode&07777 != 0600 || image.File.Links != 1 {
		t.Fatalf("receipt: %#v %v", image, err)
	}
	after := fixtureState(t, root)
	delete(after, filepath.Join(".git", receiptName))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("publication changed other data/index/config/hooks")
	}
	if err := os.Remove(filepath.Join(root, ".git", receiptName)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("publication left temporary/lock data")
	}
}

func TestTrustFilesystemRefusesReceiptShapeAndExistingLock(t *testing.T) {
	for _, shape := range []string{"symlink", "directory", "mode", "hardlink", "invalid", "existing-lock"} {
		t.Run(shape, func(t *testing.T) {
			d, data, root := publicationFixture(t)
			path := filepath.Join(root, ".git", receiptName)
			switch shape {
			case "symlink":
				put(t, root, "outside-receipt", string(data))
				if err := os.Symlink("../outside-receipt", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, filepath.Join(root, "hardlink")); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "existing-lock":
				if err := os.WriteFile(filepath.Join(root, ".git", trustLockName), []byte("held"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := fixtureState(t, root)
			if shape != "existing-lock" {
				if _, err := d.ReadReceipt(); err == nil {
					t.Fatal("unsafe receipt read")
				}
			}
			out, err := d.Publish(context.Background(), ReceiptImage{}, data)
			if err == nil || out.Published {
				t.Fatalf("unsafe publication: %#v %v", out, err)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("refusal repaired/changed receipt or lock")
			}
		})
	}
}

func TestTrustPublicationFaultsTruthfulPartialStateAndPrimary(t *testing.T) {
	for _, point := range []string{"lock-create", "temp-create", "write", "file-sync", "file-close", "pre-rename", "rename", "dir-sync", "cleanup-lock"} {
		t.Run(point, func(t *testing.T) {
			d, data, root := publicationFixture(t)
			primary := errors.New("injected " + point)
			d.fault = func(p string) error {
				if p == point {
					return primary
				}
				return nil
			}
			before := fixtureState(t, root)
			out, err := d.Publish(context.Background(), ReceiptImage{}, data)
			published := point == "dir-sync" || point == "cleanup-lock"
			if !errors.Is(err, primary) || out.Published != published {
				t.Fatalf("partial state: %#v %v", out, err)
			}
			after := fixtureState(t, root)
			if published {
				if after[filepath.Join(".git", receiptName)].Data != string(data) {
					t.Fatal("published receipt missing")
				}
				delete(after, filepath.Join(".git", receiptName))
			}
			if point == "cleanup-lock" {
				if _, ok := after[filepath.Join(".git", trustLockName)]; !ok {
					t.Fatal("failed cleanup hidden")
				}
				delete(after, filepath.Join(".git", trustLockName))
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("fault left unexpected changes")
			}
		})
	}
	d, data, root := publicationFixture(t)
	primary, cleanup := errors.New("write error"), errors.New("cleanup error")
	d.fault = func(p string) error {
		switch p {
		case "write":
			return primary
		case "cleanup-temp":
			return cleanup
		}
		return nil
	}
	out, err := d.Publish(context.Background(), ReceiptImage{}, data)
	if out.Published || !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("joined errors: %#v %v", out, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, ".git"))
	temps := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "confctl-hooks-trust.tmp-") {
			temps++
		}
		if e.Name() == trustLockName {
			t.Fatal("other owned cleanup was not attempted")
		}
	}
	if temps != 1 {
		t.Fatal("failed temporary cleanup not reported/preserved")
	}
}

func TestTrustPublicationCancellationBeforeAndAfterRename(t *testing.T) {
	for _, point := range []string{"before", "pre-rename", "rename", "dir-sync"} {
		t.Run(point, func(t *testing.T) {
			d, data, root := publicationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if point == "before" {
				cancel()
			} else {
				d.fault = func(p string) error {
					if p == point {
						cancel()
					}
					return nil
				}
			}
			before := fixtureState(t, root)
			out, err := d.Publish(ctx, ReceiptImage{}, data)
			if !errors.Is(err, context.Canceled) || out.Published != (point == "dir-sync") {
				t.Fatalf("cancel publication: %#v %v", out, err)
			}
			after := fixtureState(t, root)
			if out.Published {
				delete(after, filepath.Join(".git", receiptName))
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("cancellation changed nonreceipt state")
			}
		})
	}
}

func TestTrustPublicationChangedPreimageLockAndDirectory(t *testing.T) {
	for _, change := range []string{"receipt", "lock", "admin", "root"} {
		t.Run(change, func(t *testing.T) {
			d, data, root := publicationFixture(t)
			d.fault = func(p string) error {
				if p != "pre-rename" {
					return nil
				}
				switch change {
				case "receipt":
					return os.WriteFile(filepath.Join(root, ".git", receiptName), data, 0600)
				case "lock":
					path := filepath.Join(root, ".git", trustLockName)
					if err := os.Rename(path, path+"-old"); err != nil {
						return err
					}
					return os.WriteFile(path, []byte("foreign lock"), 0600)
				case "admin":
					path := filepath.Join(root, ".git")
					if err := os.Rename(path, path+"-moved"); err != nil {
						return err
					}
					if err := os.Mkdir(path, 0700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(path, "foreign"), []byte("retained"), 0600)
				case "root":
					if err := os.Rename(root, root+"-moved"); err != nil {
						return err
					}
					if err := os.Mkdir(root, 0700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(root, "foreign"), []byte("retained"), 0600)
				}
				return nil
			}
			out, err := d.Publish(context.Background(), ReceiptImage{}, data)
			if out.Published || err == nil {
				t.Fatalf("changed preimage accepted: %#v %v", out, err)
			}
			switch change {
			case "receipt":
				b, _ := os.ReadFile(filepath.Join(root, ".git", receiptName))
				if !bytes.Equal(b, data) {
					t.Fatal("concurrent receipt overwritten")
				}
			case "lock":
				b, _ := os.ReadFile(filepath.Join(root, ".git", trustLockName))
				if string(b) != "foreign lock" {
					t.Fatal("foreign lock deleted")
				}
			case "admin":
				b, _ := os.ReadFile(filepath.Join(root, ".git/foreign"))
				if string(b) != "retained" {
					t.Fatal("replacement admin changed")
				}
			case "root":
				b, _ := os.ReadFile(filepath.Join(root, "foreign"))
				if string(b) != "retained" {
					t.Fatal("replacement root changed")
				}
				t.Cleanup(func() { os.RemoveAll(root + "-moved") })
			}
		})
	}
}

func TestTrustFilesystemNofollowWorkingPolicyAndDirectory(t *testing.T) {
	d, _, root := publicationFixture(t)
	if err := os.Chmod(filepath.Join(root, ".rubocop.yml"), 0664); err != nil {
		t.Fatal(err)
	}
	p, err := d.WorkingPolicy(".rubocop.yml")
	if err != nil || p.Mode != 0100644 {
		t.Fatalf("umask mode incorrectly stales policy: %#v %v", p, err)
	}
	if err := os.Symlink(root, root+"-link"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(root + "-link") })
	if i, err := localOpen(root+"-link", filepath.Join(root, ".git")); err == nil {
		i.Close()
		t.Fatal("symlink directory accepted")
	}
	if _, err := identifyLocalTool(context.Background(), ToolIdentity{Program: filepath.Join(root, "not-a-store-tool")}); err == nil {
		t.Fatal("local tool certified as production immutable tool")
	}
}

func TestPolicyModeWorkingAndStagedUseOwnerExecute(t *testing.T) {
	for _, mode := range policyModeCases {
		t.Run(mode.name, func(t *testing.T) {
			root, admin := t.TempDir(), t.TempDir()
			const data = "unchanged policy bytes\n"
			put(t, root, ".rubocop.yml", data)
			if err := os.Chmod(filepath.Join(root, ".rubocop.yml"), mode.physical); err != nil {
				t.Fatal(err)
			}
			before := fixtureState(t, root)
			d, err := localOpen(root, admin)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := d.Close(); err != nil {
					t.Error(err)
				}
			}()
			t.Run("working", func(t *testing.T) {
				p, err := d.WorkingPolicy(".rubocop.yml")
				if err != nil || p.Mode != mode.gitMode || string(p.Data) != data || p.File.Mode&0777 != uint32(mode.physical) {
					t.Fatalf("working physical mode projected incorrectly: %#v %v", p, err)
				}
			})
			for _, staged := range []struct {
				name string
				mode uint32
			}{{"100644", 0100644}, {"100755", 0100755}} {
				t.Run("staged-"+staged.name, func(t *testing.T) {
					// Exercise the staged owner on an actual materialized regular file
					// with each physical mode, independently of the working reader.
					s := &Snapshot{dir: root, entries: []Entry{{Path: ".rubocop.yml", Mode: staged.mode}}}
					p, err := stagedPolicy(s, ".rubocop.yml")
					if staged.mode == mode.gitMode {
						if err != nil || p.Mode != staged.mode || string(p.Data) != data {
							t.Fatalf("matching staged projection: %#v %v", p, err)
						}
					} else {
						assertRefusal(t, err, "stale-policy")
					}
				})
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("read projections changed policy bytes/full modes")
			}
		})
	}
}
