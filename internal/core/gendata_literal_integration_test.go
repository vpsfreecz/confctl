package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vpsfreecz/confctl/internal/gendata"
)

func TestGendataLiteralErrorsAfterOpenAndAliasResidue(t *testing.T) {
	for _, invalid := range []struct {
		name  string
		key   string
		value string
	}{
		{"NUL-key", "ct\x00", "node"},
		{"NUL-value", "ct", "node\x00"},
		{"invalid-UTF8-key", string([]byte{0xff}), "node"},
		{"invalid-UTF8-value", "ct", string([]byte{0xff})},
	} {
		for _, alias := range []bool{false, true} {
			t.Run(invalid.name+map[bool]string{false: "/regular", true: "/alias"}[alias], func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "data/vpsadmin/containers.nix")
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("OLD\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if alias {
					if err := os.Symlink(path, path+".new"); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path+".new", []byte("STALE\n"), 0600); err != nil {
					t.Fatal(err)
				}
				vps := gendata.VPS{Node: gdField(gendata.Node{
					Name: gdField(invalid.value), Location: gdField(gendata.Location{Environment: gdField(gendata.Environment{})}),
				})}
				source := &genDataSource{shows: []gendata.VPS{vps}}
				calls := 0
				g := gendata.Generator{Root: root, Source: source, Literal: func(value any) (string, error) {
					calls++
					body, err := os.ReadFile(path + ".new")
					if err != nil || len(body) != 0 {
						t.Fatalf("real formatter reached before open/truncate: body=%q err=%v", body, err)
					}
					return genDataNixLiteral(value)
				}}
				err := g.Containers(context.Background(), []gendata.MachineInput{{Container: gdField(gendata.ContainerRef{}), HostName: gdField(invalid.key)}})
				if err == nil || !strings.Contains(err.Error(), ".new: render:") || calls != 1 || len(source.showIDs) != 1 {
					t.Fatalf("error=%v literal=%d requests=%d", err, calls, len(source.showIDs))
				}
				body, readErr := os.ReadFile(path + ".new")
				if readErr != nil || len(body) != 0 {
					t.Fatalf("missing/modified failure sibling: %q %v", body, readErr)
				}
				info, statErr := os.Lstat(path + ".new")
				if statErr != nil || (info.Mode()&os.ModeSymlink != 0) != alias {
					t.Fatalf("sibling was renamed/replaced: %v %v", info, statErr)
				}
				body, readErr = os.ReadFile(path)
				want := "OLD\n"
				if alias {
					want = "" // Open already followed the .new alias and truncated the target.
				}
				if readErr != nil || string(body) != want {
					t.Fatalf("destination residue=%q err=%v want=%q", body, readErr, want)
				}
			})
		}
	}
}

func TestGendataAllRealLiteralFailureKeepsFirstFile(t *testing.T) {
	for _, alias := range []bool{false, true} {
		root := t.TempDir()
		container := filepath.Join(root, "data/vpsadmin/containers.nix")
		network := filepath.Join(root, "data/vpsadmin/networks/containers.nix")
		if err := os.MkdirAll(filepath.Dir(network), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(network, []byte("OLD-N\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if alias {
			if err := os.Symlink(network, network+".new"); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(network+".new", []byte("STALE-N\n"), 0600); err != nil {
			t.Fatal(err)
		}
		lists := 0
		source := &genDataSource{list: func() ([]gendata.Network, error) {
			lists++
			body, err := os.ReadFile(container)
			if err != nil || string(body) != "{\n}\n" {
				t.Fatalf("network work began before first replacement: %q %v", body, err)
			}
			return []gendata.Network{{IPVersion: gdField[any](json.Number("4")), Address: gdField("\x00")}}, nil
		}}
		g := gendata.Generator{Root: root, Source: source, Literal: genDataNixLiteral}
		if err := g.All(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "NUL") || lists != 1 {
			t.Fatalf("error=%v lists=%d", err, lists)
		}
		body, err := os.ReadFile(container)
		if err != nil || string(body) != "{\n}\n" {
			t.Fatalf("first file rolled back: %q %v", body, err)
		}
		if _, err := os.Lstat(container + ".new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("first .new remained after success: %v", err)
		}
		body, err = os.ReadFile(network + ".new")
		if err != nil || len(body) != 0 {
			t.Fatalf("network sibling residue=%q err=%v", body, err)
		}
		body, err = os.ReadFile(network)
		want := "OLD-N\n"
		if alias {
			want = ""
		}
		if err != nil || string(body) != want {
			t.Fatalf("network target residue=%q err=%v want=%q", body, err, want)
		}
	}
}
