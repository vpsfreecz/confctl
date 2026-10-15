package gendata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// File and injected-error expectations here are source-derived. They exercise
// the actual Generator/opened-file helper, not an original Ruby observation.
func makeParents(t *testing.T, root, relative string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0755); err != nil {
		t.Fatal(err)
	}
}

func putFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, body string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("%s: body=%q err=%v, want=%q", path, got, err, body)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

func TestGendataFilesCompleteProjectionBeforeOpen(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("fixture show failed")
	machines := []MachineInput{
		{Name: "first", Container: some(ContainerRef{ID: some(json.Number("1"))})},
		{Name: "second", Container: some(ContainerRef{ID: some(json.Number("2"))})},
		{Name: "third", Container: some(ContainerRef{ID: some(json.Number("3"))})},
	}
	for _, test := range []struct {
		name      string
		failAt    int
		responses []VPS
		calls     int
	}{
		{"first-show", 1, nil, 1},
		{"second-show", 2, []VPS{resolvedVPS("1", "n", "n", "l", "d")}, 2},
		{"second-projection", 0, []VPS{resolvedVPS("1", "n", "n", "l", "d"), {}}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			makeParents(t, root, containersFile)
			path := filepath.Join(root, containersFile)
			putFile(t, path, "OLD\n", 0644)
			putFile(t, path+".new", "STALE\n", 0600)
			source := &recordingSource{ctx: ctx, failAt: test.failAt, failure: failure, responses: test.responses}
			renders := 0
			g := Generator{Root: root, Source: source, Literal: func(any) (string, error) { renders++; return "unreachable", nil }}
			err := g.Containers(ctx, machines)
			if err == nil || len(source.ids) != test.calls || renders != 0 {
				t.Fatalf("err=%v shows=%d renders=%d", err, len(source.ids), renders)
			}
			if test.failAt != 0 && !errors.Is(err, failure) {
				t.Fatalf("source cause lost: %v", err)
			}
			assertFile(t, path, "OLD\n")
			assertFile(t, path+".new", "STALE\n")
		})
	}
}

func TestGendataFilesOpenBeforeRenderAndNoMkdir(t *testing.T) {
	for _, blockedNew := range []bool{false, true} {
		root := t.TempDir()
		path := filepath.Join(root, containersFile)
		if blockedNew {
			makeParents(t, root, containersFile)
			putFile(t, path, "OLD\n", 0644)
			if err := os.Mkdir(path+".new", 0755); err != nil {
				t.Fatal(err)
			}
		}
		renders := 0
		g := Generator{Root: root, Literal: func(any) (string, error) { renders++; return "unreachable", nil }}
		err := g.Containers(context.Background(), nil)
		if err == nil || renders != 0 || !strings.Contains(err.Error(), path+".new: open:") {
			t.Fatalf("blockedNew=%t: err=%v renders=%d", blockedNew, err, renders)
		}
		if blockedNew {
			assertFile(t, path, "OLD\n")
			info, err := os.Lstat(path + ".new")
			if err != nil || !info.IsDir() {
				t.Fatalf("existing .new directory changed: %v", err)
			}
		} else {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("open cause lost: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("generator created a parent/extra file: entries=%v err=%v", entries, err)
			}
		}
	}
}

func TestGendataFilesRenderFailureTruncatesAndRetainsMode(t *testing.T) {
	root := t.TempDir()
	makeParents(t, root, containersFile)
	path := filepath.Join(root, containersFile)
	putFile(t, path, "OLD\n", 0644)
	putFile(t, path+".new", "STALE-LONGER-NEW\n", 0600)
	failure := errors.New("fixture formatter failure")
	renders := 0
	g := Generator{Root: root, Literal: func(value any) (string, error) {
		renders++
		if !reflect.DeepEqual(value, map[string]any{}) {
			t.Fatalf("wrong literal input: %#v", value)
		}
		info, err := os.Stat(path + ".new")
		if err != nil || info.Size() != 0 || info.Mode().Perm() != 0600 {
			t.Fatalf("literal invoked before truncate: info=%v err=%v", info, err)
		}
		return "not-written", failure
	}}
	err := g.Containers(context.Background(), nil)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), ".new: render:") || renders != 1 {
		t.Fatalf("err=%v renders=%d", err, renders)
	}
	assertFile(t, path, "OLD\n")
	assertFile(t, path+".new", "")
}

func TestGendataFilesSuccessCreateReplaceAndExistingNewMode(t *testing.T) {
	for _, existingNew := range []bool{false, true} {
		root := t.TempDir()
		makeParents(t, root, containersFile)
		path := filepath.Join(root, containersFile)
		var wantMode os.FileMode
		if existingNew {
			putFile(t, path, "OLD\n", 0644)
			putFile(t, path+".new", "STALE-LONGER-NEW\n", 0600)
			wantMode = 0600
		} else {
			// A control file captures this process's inherited umask without
			// changing global umask or assuming the test runner's privileges.
			control := filepath.Join(root, "umask-control")
			putFile(t, control, "", 0666)
			info, err := os.Stat(control)
			if err != nil {
				t.Fatal(err)
			}
			wantMode = info.Mode().Perm()
		}
		renders := 0
		g := Generator{Root: root, Literal: func(any) (string, error) {
			renders++
			assertFile(t, path+".new", "")
			return "{\n}", nil
		}}
		if err := g.Containers(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		assertFile(t, path, "{\n}\n")
		assertAbsent(t, path+".new")
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != wantMode || renders != 1 {
			t.Fatalf("existingNew=%t info=%v err=%v renders=%d, mode want=%o", existingNew, info, err, renders, wantMode)
		}
	}
}

func TestGendataFilesRenameFailureRetainsCompleteNew(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, containersFile)
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	putFile(t, path+".new", "STALE\n", 0600)
	g := Generator{Root: root, Literal: func(any) (string, error) { return "{\n}", nil }}
	err := g.Containers(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "rename:") {
		t.Fatalf("missing rename error: %v", err)
	}
	var pathErr *os.LinkError
	if !errors.As(err, &pathErr) {
		t.Fatalf("rename cause lost: %v", err)
	}
	assertFile(t, path+".new", "{\n}\n")
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("target directory changed: %v", err)
	}
}

type scriptedWriteCloser struct {
	body     bytes.Buffer
	limit    int // -1 accepts all bytes
	writeErr error
	closeErr error
	closes   int
	events   *[]string
}

func (f *scriptedWriteCloser) Write(body []byte) (int, error) {
	*f.events = append(*f.events, "write")
	count := len(body)
	if f.limit >= 0 && f.limit < count {
		count = f.limit
	}
	f.body.Write(body[:count])
	return count, f.writeErr
}

func (f *scriptedWriteCloser) Close() error {
	*f.events = append(*f.events, "close")
	f.closes++
	return f.closeErr
}

func TestGendataWriteAndCloseFailuresAndPrimaryError(t *testing.T) {
	renderFailure := errors.New("render failure")
	writeFailure := errors.New("write failure")
	closeFailure := errors.New("close failure")
	for _, test := range []struct {
		name      string
		nilRender bool
		renderErr error
		writeErr  error
		closeErr  error
		limit     int
		wantErr   error
		wantPhase string
		wantBody  string
		wantCalls []string
	}{
		{"success", false, nil, nil, nil, -1, nil, "", "{\n}\n", []string{"render", "write", "close"}},
		{"render-and-close", false, renderFailure, nil, closeFailure, -1, renderFailure, "render:", "", []string{"render", "close"}},
		{"missing-formatter", true, nil, nil, nil, -1, nil, "render:", "", []string{"close"}},
		{"partial-write-and-close", false, nil, writeFailure, closeFailure, 2, writeFailure, "write:", "{\n", []string{"render", "write", "close"}},
		{"short-write", false, nil, nil, nil, 2, io.ErrShortWrite, "write:", "{\n", []string{"render", "write", "close"}},
		{"zero-write", false, nil, nil, nil, 0, io.ErrShortWrite, "write:", "", []string{"render", "write", "close"}},
		{"close-only", false, nil, nil, closeFailure, -1, closeFailure, "close:", "{\n}\n", []string{"render", "write", "close"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			file := &scriptedWriteCloser{limit: test.limit, writeErr: test.writeErr, closeErr: test.closeErr, events: &events}
			var literal func(any) (string, error)
			if !test.nilRender {
				literal = func(value any) (string, error) {
					events = append(events, "render")
					if !reflect.DeepEqual(value, map[string]any{}) {
						t.Fatalf("wrong literal input: %#v", value)
					}
					return "{\n}", test.renderErr
				}
			}
			err := writeAndClose(file, map[string]any{}, literal)
			if test.wantPhase == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantPhase) || test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("err=%v, want cause=%v phase=%s", err, test.wantErr, test.wantPhase)
			}
			if test.wantErr != closeFailure && test.closeErr != nil && errors.Is(err, closeFailure) {
				t.Fatalf("close replaced primary failure: %v", err)
			}
			if file.closes != 1 || file.body.String() != test.wantBody || !reflect.DeepEqual(events, test.wantCalls) {
				t.Fatalf("closes=%d body=%q events=%v", file.closes, file.body.String(), events)
			}
		})
	}
}

type networkCallbackSource struct {
	list func() ([]Network, error)
}

func (*networkCallbackSource) ShowVPS(context.Context, Field[json.Number]) (VPS, error) {
	return VPS{}, errors.New("unexpected VPS show")
}

func (s *networkCallbackSource) ListNetworks(context.Context) ([]Network, error) { return s.list() }

func TestGendataAllSecondStageFailureKeepsContainers(t *testing.T) {
	for _, stage := range []string{"list", "projection", "open", "render", "rename"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			makeParents(t, root, containersFile)
			containerPath := filepath.Join(root, containersFile)
			networkPath := filepath.Join(root, networksFile)
			putFile(t, containerPath, "OLD-C\n", 0644)
			if stage != "open" {
				makeParents(t, root, networksFile)
				if stage == "rename" {
					if err := os.Mkdir(networkPath, 0755); err != nil {
						t.Fatal(err)
					}
				} else {
					putFile(t, networkPath, "OLD-N\n", 0644)
				}
				putFile(t, networkPath+".new", "STALE-N\n", 0600)
			}
			failure := errors.New("fixture second-stage failure")
			lists := 0
			source := &networkCallbackSource{list: func() ([]Network, error) {
				lists++
				assertFile(t, containerPath, "{\n}\n")
				assertAbsent(t, containerPath+".new")
				if stage == "list" {
					return nil, failure
				}
				if stage == "projection" {
					return []Network{{IPVersion: some[any](json.Number("4")), Prefix: some(json.Number("invalid"))}}, nil
				}
				return nil, nil
			}}
			renders := 0
			g := Generator{Root: root, Source: source, Literal: func(value any) (string, error) {
				renders++
				if _, network := value.(map[string]any)["ipv4"]; network {
					assertFile(t, networkPath+".new", "")
					if stage == "render" {
						return "not-written", failure
					}
					return "NETWORK", nil
				}
				return "{\n}", nil
			}}
			err := g.All(context.Background(), nil)
			if err == nil || lists != 1 {
				t.Fatalf("err=%v lists=%d renders=%d", err, lists, renders)
			}
			if (stage == "list" || stage == "render") && !errors.Is(err, failure) {
				t.Fatalf("primary cause lost: %v", err)
			}
			assertFile(t, containerPath, "{\n}\n")
			assertAbsent(t, containerPath+".new")
			switch stage {
			case "list", "projection":
				assertFile(t, networkPath, "OLD-N\n")
				assertFile(t, networkPath+".new", "STALE-N\n")
				if renders != 1 {
					t.Fatalf("render before list/projection success: %d", renders)
				}
			case "open":
				assertAbsent(t, filepath.Dir(networkPath))
				if renders != 1 {
					t.Fatalf("render before successful network open: %d", renders)
				}
			case "render":
				assertFile(t, networkPath, "OLD-N\n")
				assertFile(t, networkPath+".new", "")
			case "rename":
				assertFile(t, networkPath+".new", "NETWORK\n")
				info, err := os.Stat(networkPath)
				if err != nil || !info.IsDir() {
					t.Fatalf("network target directory changed: %v", err)
				}
			}
		})
	}
}

func TestGendataAllContainersFailurePreventsNetwork(t *testing.T) {
	root := t.TempDir()
	containerPath := filepath.Join(root, containersFile)
	if err := os.MkdirAll(containerPath, 0755); err != nil {
		t.Fatal(err)
	}
	makeParents(t, root, networksFile)
	networkPath := filepath.Join(root, networksFile)
	putFile(t, networkPath, "OLD-N\n", 0644)
	putFile(t, networkPath+".new", "STALE-N\n", 0600)
	lists := 0
	source := &networkCallbackSource{list: func() ([]Network, error) { lists++; return nil, nil }}
	g := Generator{Root: root, Source: source, Literal: func(any) (string, error) { return "{\n}", nil }}
	if err := g.All(context.Background(), nil); err == nil || lists != 0 {
		t.Fatalf("err=%v lists=%d", err, lists)
	}
	assertFile(t, containerPath+".new", "{\n}\n")
	assertFile(t, networkPath, "OLD-N\n")
	assertFile(t, networkPath+".new", "STALE-N\n")
}
