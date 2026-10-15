package hookcheck

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadPreparedToolsBorrowedDispatch(t *testing.T) {
	for _, phase := range []HookPhase{PreCommit, CommitMsg} {
		t.Run(string(phase), func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			message := filepath.Join(root, "message")
			put(t, root, "message", "Subject.\n")
			trustNow(t, q, io)
			before := fixtureState(t, root)
			request := HookRequest{TrustRequest: q, Phase: phase}
			if phase == CommitMsg {
				request.MessagePath = message
				request.CommentPrefix = "#"
			}
			_, err := runFixtureHook(t, context.Background(), request, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				input := preparedInput(t, req, phase)
				tools, err := loadPreparedTools(ctx, input, func() (string, error) { return q.Tools.Checker.Resolved, nil }, io.IdentifyTool)
				if err != nil || !reflect.DeepEqual(tools, q.Tools) {
					t.Fatalf("load: %#v %v", tools, err)
				}
				invoke := func(context.Context, Invocation) (ToolResult, error) { return ToolResult{}, nil }
				got, err := CheckPrepared(ctx, input, tools, invoke, nil)
				want, originalErr := CheckPrepared(ctx, input, q.Tools, invoke, nil)
				if err != nil || originalErr != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("dispatch differs: %#v %v / %#v %v", got, err, want, originalErr)
				}
				if _, err := os.Stat(fixtureSnapshot(t, req)); err != nil {
					t.Fatal("loader closed parent snapshot:", err)
				}
				return ToolResult{}, nil
			})
			if err != nil || !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("loader changed parent resources/trust:", err)
			}
		})
	}
}

func TestLoadPreparedToolsRejectsMalformedAndDriftedContexts(t *testing.T) {
	for _, change := range []string{"empty-tools", "truncated-tools", "extra-tool", "wrong-hash", "phase", "message", "policy", "snapshot", "replace-after-load", "cancel"} {
		t.Run(change, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			trustNow(t, q, io)
			before := fixtureState(t, root)
			_, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				input := preparedInput(t, req, PreCommit)
				original, err := os.ReadFile(input.ContextPath)
				if err != nil {
					t.Fatal(err)
				}
				var p preparedContext
				if err := decodeCanonical(original, &p); err != nil {
					t.Fatal(err)
				}
				restore := func() { os.WriteFile(input.ContextPath, original, 0600) }
				switch change {
				case "empty-tools":
					p.Trust.Tools = nil
				case "truncated-tools":
					p.Trust.Tools = p.Trust.Tools[:1]
				case "extra-tool":
					p.Trust.Tools = append(p.Trust.Tools, p.Trust.Tools[0])
				case "wrong-hash":
					input.SHA256 = sha256.Sum256([]byte("not the context"))
				case "phase":
					input.Phase = CommitMsg
				case "message":
					input.MessagePath = filepath.Join(root, "not-a-message")
				case "policy":
					path := filepath.Join(fixtureSnapshot(t, req), "lefthook.yml")
					b, _ := os.ReadFile(path)
					if err := os.WriteFile(path, []byte("different policy"), 0644); err != nil {
						t.Fatal(err)
					}
					restore = func() { os.WriteFile(path, b, 0644) }
				case "snapshot":
					if err := os.Chmod(fixtureSnapshot(t, req), 0755); err != nil {
						t.Fatal(err)
					}
					restore = func() { os.Chmod(fixtureSnapshot(t, req), 0700) }
				case "cancel":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				defer restore()
				if change == "empty-tools" || change == "truncated-tools" || change == "extra-tool" {
					b, _ := canonical(p)
					if err := os.WriteFile(input.ContextPath, b, 0600); err != nil {
						t.Fatal(err)
					}
					input.SHA256 = sha256.Sum256(b)
				}
				tools, loadErr := loadPreparedTools(ctx, input, func() (string, error) { return q.Tools.Checker.Resolved, nil }, io.IdentifyTool)
				calls := 0
				if change == "replace-after-load" {
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					if err := os.WriteFile(input.ContextPath, []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
					_, loadErr = CheckPrepared(ctx, input, tools, func(context.Context, Invocation) (ToolResult, error) { calls++; return ToolResult{}, nil }, nil)
				}
				if loadErr == nil || calls != 0 {
					t.Fatalf("%s accepted: %v, tool calls %d", change, loadErr, calls)
				}
				if change == "cancel" && !errors.Is(loadErr, context.Canceled) {
					t.Fatal(loadErr)
				}
				return ToolResult{}, nil
			})
			if err != nil || !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("refusal changed parent state:", err)
			}
		})
	}
}

func TestLoadPreparedToolsRequiresActualChecker(t *testing.T) {
	for _, change := range []string{"path", "digest"} {
		t.Run(change, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			actual := q.Tools.Checker
			if change == "path" {
				q.Tools.Checker.Resolved += "-other"
			} else {
				q.Tools.Checker.SHA256 = sha256.Sum256([]byte("another checker"))
			}
			trustNow(t, q, io)
			before := fixtureState(t, root)
			_, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io, func(ctx context.Context, req Invocation) (ToolResult, error) {
				input := preparedInput(t, req, PreCommit)
				_, err := loadPreparedTools(ctx, input, func() (string, error) { return actual.Resolved, nil },
					func(context.Context, ToolIdentity) (ToolIdentity, error) { return actual, nil })
				assertRefusal(t, err, "stale-identity")
				return ToolResult{}, nil
			})
			if err != nil || !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal(err)
			}
		})
	}
}
