package inputs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const updateOld = `{"nodes":{"root":{"inputs":{"a":"a","b":"b"}},"a":{"locked":{"rev":"1111111111","url":"https://repo"}},"b":{"locked":{"rev":"1111111111","url":"https://repo/"}}},"unknown":[null,false,9007199254740993]}`

func updateNext() string { return strings.ReplaceAll(updateOld, "1111111111", "2222222222") }
func updateLoad(path string, optional bool) (*Lock, error) {
	if optional {
		return LoadOptionalLock(path)
	}
	return LoadLock(path)
}
func updateArgument(v any) string {
	if v == nil {
		return ""
	}
	if v == false {
		return "false"
	}
	return v.(string)
}

func TestUpdateRetryBoundaryAndPartialWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  []error
		stderr    string
		calls     int
		wantError bool
	}{
		{"retry", []error{&ExitError{Command: "nix flake update a", Code: 1, Output: Output{Stderr: "EXPERIMENTAL flakes"}}}, "", 2, false},
		{"second failure", []error{&ExitError{Code: 1, Output: Output{Stderr: "experimental nix-command"}}, &ExitError{Code: 9, Output: Output{Stderr: "experimental flakes"}}}, "", 2, true},
		{"ordinary exit", []error{&ExitError{Code: 73, Output: Output{Stderr: "refused"}}}, "", 1, true},
		{"spawn cannot retry", []error{errors.New("experimental flakes spawn failure")}, "", 1, true},
		{"cached warning", nil, "unable to download 'https://api.github.com/repos/x': unavailable; using cached version", 1, true},
		{"rate alone is harmless", nil, "warning: rate limit exceeded", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "flake.lock")
			if err := os.WriteFile(path, []byte(updateOld), 0600); err != nil {
				t.Fatal(err)
			}
			var calls [][]string
			commits := 0
			deps := UpdateDeps{Load: updateLoad, Argument: updateArgument, Run: func(ctx context.Context, dir string, argv []string) (Output, error) {
				if dir != root {
					t.Fatal(dir)
				}
				calls = append(calls, append([]string{}, argv...))
				if err := os.WriteFile(path, []byte(updateNext()), 0600); err != nil {
					t.Fatal(err)
				}
				if len(calls) <= len(tc.failures) {
					return Output{}, tc.failures[len(calls)-1]
				}
				return Output{Stderr: tc.stderr}, nil
			}, Commit: func(context.Context, string, string, bool) error { commits++; return nil }}
			result, err := Update(context.Background(), root, []any{"a"}, UpdateOptions{}, deps)
			if (err != nil) != tc.wantError || len(calls) != tc.calls || commits != 0 {
				t.Fatal(result, err, calls, commits)
			}
			want := []string{"nix", "flake", "update", "a"}
			if !reflect.DeepEqual(calls[0], want) {
				t.Fatal(calls)
			}
			if len(calls) == 2 && !reflect.DeepEqual(calls[1], []string{"nix", "--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes", "flake", "update", "a"}) {
				t.Fatal(calls)
			}
			bytes, _ := os.ReadFile(path)
			if string(bytes) != updateNext() {
				t.Fatal("failed update rolled back or serialized lock", string(bytes))
			}
		})
	}
}

func TestUpdateCreationMetadataDiffAndCancellation(t *testing.T) {
	for _, mode := range []string{"create", "metadata", "cancel-run", "cancel-mirror", "commit-failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "flake.lock")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode != "create" {
				if err := os.WriteFile(path, []byte(updateOld), 0600); err != nil {
					t.Fatal(err)
				}
			}
			commits := 0
			deps := UpdateDeps{Load: updateLoad, Argument: updateArgument, Run: func(context.Context, string, []string) (Output, error) {
				text := updateNext()
				if mode == "metadata" {
					text = strings.ReplaceAll(updateOld, "https://repo", "https://new")
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "cancel-run" {
					cancel()
				}
				return Output{}, nil
			}, Changelog: func(context.Context, string, string, string) (string, error) {
				cancel()
				return "", errors.New("ordinary mirror text")
			}, Commit: func(context.Context, string, string, bool) error {
				commits++
				if mode == "commit-failure" {
					return errors.New("git commit failed")
				}
				return nil
			}}
			opts := UpdateOptions{Commit: true, Changelog: mode == "cancel-mirror"}
			result, err := Update(ctx, root, []any{"b", "a", "b", nil, false}, opts, deps)
			if strings.HasPrefix(mode, "cancel-") {
				if !errors.Is(err, context.Canceled) || commits != 0 {
					t.Fatal(result, err, commits)
				}
			} else if mode == "metadata" {
				if err != nil || len(result.Changes) != 0 || commits != 0 {
					t.Fatal(result, err, commits)
				}
			} else {
				if len(result.Changes) != 3 || result.Changes[0].Name != "b" || result.Changes[1].Name != "a" || result.Changes[2].Name != "b" || commits != 1 {
					t.Fatal(result, err, commits)
				}
				if (err != nil) != (mode == "commit-failure") {
					t.Fatal(err)
				}
			}
			bytes, _ := os.ReadFile(path)
			if len(bytes) == 0 || mode != "metadata" && string(bytes) != updateNext() {
				t.Fatal("partial writes lost", string(bytes))
			}
		})
	}
}

func TestUpdateWarningAndExitPresentation(t *testing.T) {
	warning := "unable to download 'https://api.github.com/repos/x': rate limit exceeded; using cached version"
	err := CheckUpdateOutput(Output{Stderr: warning})
	if err == nil || err.Error() != "Nix could not refresh GitHub input metadata because the GitHub API rate limit was exceeded.\nIt used a cached version instead, so the selected input may not be the latest revision.\nConfigure a GitHub token in Nix access-tokens or retry later." {
		t.Fatal(err)
	}
	if err := CheckUpdateOutput(Output{Stdout: "unrelated warning", Stderr: "using cached version"}); err != nil {
		t.Fatal(err)
	}
	exit := &ExitError{Command: "nix flake update 'a b'", Code: 73, Output: Output{Stdout: " \n", Stderr: " refused\n"}}
	if exit.Error() != "Running `nix flake update 'a b'` failed with\n  exit status: 73\n  stdout: Nothing written\n  stderr: refused\n" {
		t.Fatal(exit.Error())
	}
}

func TestUpdateReadFailuresAndExactSelectionValues(t *testing.T) {
	for _, mode := range []string{"old-corrupt", "new-missing", "new-corrupt"} {
		root := t.TempDir()
		path := filepath.Join(root, "flake.lock")
		if mode == "old-corrupt" {
			if err := os.WriteFile(path, []byte("bad json"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		deps := UpdateDeps{Load: updateLoad, Argument: updateArgument, Run: func(context.Context, string, []string) (Output, error) {
			calls++
			if mode == "new-corrupt" {
				if err := os.WriteFile(path, []byte("bad json"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return Output{}, nil
		}}
		_, err := Update(context.Background(), root, []any{"a"}, UpdateOptions{}, deps)
		if err == nil || (mode == "old-corrupt" && calls != 0) || (mode != "old-corrupt" && calls != 1) {
			t.Fatal(mode, calls, err)
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, "flake.lock")
	var got []string
	deps := UpdateDeps{Load: updateLoad, Argument: updateArgument, Run: func(_ context.Context, _ string, argv []string) (Output, error) {
		got = append([]string{}, argv...)
		err := os.WriteFile(path, []byte(updateNext()), 0600)
		return Output{}, err
	}}
	result, err := Update(context.Background(), root, []any{nil, false, "a"}, UpdateOptions{}, deps)
	if err != nil || len(result.Changes) != 1 || result.Changes[0].Name != "a" || !reflect.DeepEqual(got, []string{"nix", "flake", "update", "", "false", "a"}) {
		t.Fatal(result, err, got)
	}
}
