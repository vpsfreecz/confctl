package hookcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func modelTools(t *testing.T, service bool) ToolSet {
	t.Helper()
	output := "/nix/store/" + strings.Repeat("a", 32) + "-hook-model"
	tool := func(n string) ToolIdentity {
		p := output + "/bin/" + n
		return ToolIdentity{output, p, p, sha256.Sum256([]byte(n))}
	}
	git := testGit(t)
	out := ToolSet{Checker: tool("confctl-hooks"), Git: ToolIdentity{filepath.Dir(filepath.Dir(git)), git, git, sha256.Sum256([]byte("git-model"))}, Lefthook: tool("lefthook"), Nixfmt: tool("nixfmt"), Gofmt: tool("gofmt"), ChildBinding: ChildBinding{Path: []string{output + "/bin"}, Locale: "C.UTF-8"}}
	if service {
		ruby := tool("rubocop")
		out.ServiceRuby = true
		out.RuboCop = &ruby
		out.ChildBinding.Service = &ServiceEnvironment{output + "/gems", output + "/gems", output + "/lib", output + "/Gemfile", output + "/bundle"}
	}
	return out
}
func trustFixture(t *testing.T, service bool) (TrustRequest, TrustIO, string) {
	t.Helper()
	root, env := newFixture(t)
	tools := modelTools(t, service)
	fixed, err := FixedConfig(tools.Checker.Program)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "lefthook.yml", string(fixed))
	put(t, root, ".rubocop.yml", "root policy\n")
	put(t, root, ".rubocop_todo.yml", "root todo\n")
	put(t, root, "file.nix", "staged nix\n")
	stage(t, root, env, ".")
	put(t, root, "untracked", "retained original bytes")
	put(t, root, "file.nix", "unstaged nix\n")
	io := LocalTrustIO()
	io.IdentifyTool = func(ctx context.Context, want ToolIdentity) (ToolIdentity, error) { return want, ctx.Err() }
	return TrustRequest{Capture: CaptureOptions{root, testGit(t), t.TempDir(), env, realInvoke}, Tools: tools}, io, root
}
func trustNow(t *testing.T, q TrustRequest, io TrustIO) {
	t.Helper()
	p, _, err := PrepareTrust(context.Background(), q, io)
	if err != nil {
		t.Fatal(err)
	}
	out, err := CommitTrust(context.Background(), q, p, io)
	if err != nil || !out.Published {
		t.Fatalf("explicit disposable trust: %#v %v", out, err)
	}
}
func assertRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var r *TrustRefusal
	if !errors.As(err, &r) || !errors.Is(err, ErrTrustRequired) || (reason != "" && r.Reason != reason) {
		t.Fatalf("wanted trust refusal %q: %v", reason, err)
	}
}

func TestTrustPrepareCommitFreshCandidateAndCopiedChanges(t *testing.T) {
	q, io, root := trustFixture(t, false)
	before := fixtureState(t, root)
	p, changes, err := PrepareTrust(context.Background(), q, io)
	if err != nil || p == nil || len(changes) == 0 {
		t.Fatalf("proposal: %#v %#v %v", p, changes, err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, root)) {
		t.Fatal("prepare wrote original state/receipt")
	}
	for i := range changes {
		for j := range changes[i].New {
			changes[i].New[j] = 'x'
		}
	}
	put(t, root, "flake.lock", "unrelated staged source")
	stage(t, root, q.Capture.Env, "flake.lock")
	out, err := CommitTrust(context.Background(), q, p, io)
	if err != nil || !out.Published {
		t.Fatalf("unrelated index/source staled proposal: %#v %v", out, err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".git", receiptName))
	if err != nil || !bytes.Equal(b, p.candidate) {
		t.Fatalf("published altered changes: %q %v", b, err)
	}
	info, _ := os.Stat(filepath.Join(root, ".git", receiptName))
	if info.Mode().Perm() != 0600 {
		t.Fatal("receipt mode")
	}
	p2, changes, err := PrepareTrust(context.Background(), q, io)
	if err != nil || len(changes) != 0 {
		t.Fatalf("matching trust proposal: %#v %v", changes, err)
	}
	out, err = CommitTrust(context.Background(), q, p2, io)
	if err != nil || out.Published {
		t.Fatalf("no-change rewrote receipt: %#v %v", out, err)
	}
	info2, _ := os.Stat(filepath.Join(root, ".git", receiptName))
	if !os.SameFile(info, info2) {
		t.Fatal("no-change replaced receipt")
	}
}

func TestTrustProposalRejectsInterveningFiniteChanges(t *testing.T) {
	for _, change := range []string{"policy", "tools", "root", "receipt"} {
		t.Run(change, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			p, _, err := PrepareTrust(context.Background(), q, io)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "policy":
				put(t, root, ".rubocop.yml", "new reviewed policy needed")
				stage(t, root, q.Capture.Env, ".rubocop.yml")
			case "tools":
				q.Tools.Nixfmt.SHA256 = sha256.Sum256([]byte("changed installed bytes"))
			case "root":
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Rename(moved, root) })
				q.Capture.Dir = moved
			case "receipt":
				trustNow(t, q, io)
			}
			before := fixtureState(t, q.Capture.Dir)
			out, err := CommitTrust(context.Background(), q, p, io)
			assertRefusal(t, err, "stale-policy")
			if out.Published || !reflect.DeepEqual(before, fixtureState(t, q.Capture.Dir)) {
				t.Fatal("stale proposal published/mutated")
			}
		})
	}
}

func TestTrustPolicySlotsPartialStagingModesTypesAndExtras(t *testing.T) {
	for _, change := range []string{"staged-old", "working-old", "missing-optional", "mode", "symlink", "directory", "invalid-fixed", "gitlink"} {
		t.Run(change, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			switch change {
			case "staged-old":
				put(t, root, ".rubocop.yml", "working only")
			case "working-old":
				put(t, root, ".rubocop.yml", "new staged")
				stage(t, root, q.Capture.Env, ".rubocop.yml")
				put(t, root, ".rubocop.yml", "root policy\n")
			case "missing-optional":
				if err := os.Remove(filepath.Join(root, ".rubocop.yml")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(root, ".rubocop.yml"), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				os.Remove(filepath.Join(root, ".rubocop.yml"))
				if err := os.Symlink("file.nix", filepath.Join(root, ".rubocop.yml")); err != nil {
					t.Fatal(err)
				}
			case "directory":
				os.Remove(filepath.Join(root, ".rubocop.yml"))
				if err := os.Mkdir(filepath.Join(root, ".rubocop.yml"), 0700); err != nil {
					t.Fatal(err)
				}
			case "invalid-fixed":
				put(t, root, "lefthook.yml", "pre-commit: arbitrary command\n")
				stage(t, root, q.Capture.Env, "lefthook.yml")
			case "gitlink":
				fixtureGit(t, root, q.Capture.Env, nil, "update-index", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",.rubocop.yml")
			}
			before := fixtureState(t, root)
			p, _, err := PrepareTrust(context.Background(), q, io)
			if err == nil || p != nil {
				t.Fatal("unsupported/disagreeing policy approved")
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("refusal mutated policy/original")
			}
		})
	}
	for _, name := range extraNames() {
		for _, staged := range []bool{false, true} {
			t.Run(name+" staged="+map[bool]string{false: "false", true: "true"}[staged], func(t *testing.T) {
				q, io, root := trustFixture(t, false)
				put(t, root, name, "foreign runner config")
				if staged {
					stage(t, root, q.Capture.Env, name)
					os.Remove(filepath.Join(root, name))
				}
				before := fixtureState(t, root)
				p, _, err := PrepareTrust(context.Background(), q, io)
				assertRefusal(t, err, "unsupported-policy")
				if p != nil || !reflect.DeepEqual(before, fixtureState(t, root)) {
					t.Fatal("extra config ignored/mutated")
				}
			})
		}
	}
}

func TestTrustCanonicalReceiptBytePathsAndInvalidEncodings(t *testing.T) {
	q, io, _ := trustFixture(t, false)
	p, _, err := PrepareTrust(context.Background(), q, io)
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseReceipt(p.candidate)
	if err != nil {
		t.Fatal(err)
	}
	r.Root.Original = []byte("/literal \n'$\xff/root")
	r.Root.Resolved = bytes.Clone(r.Root.Original)
	data, _ := canonical(r)
	round, err := parseReceipt(data)
	if err != nil || !bytes.Equal(round.Root.Original, r.Root.Original) {
		t.Fatalf("opaque path bytes lost: %#v %v", round, err)
	}
	for _, kind := range []string{"unknown", "duplicate", "trailing", "noncanonical", "format", "slots", "roles", "base64", "digest", "decimal", "relative"} {
		t.Run(kind, func(t *testing.T) {
			changed := bytes.Clone(data)
			copyR := round
			switch kind {
			case "unknown":
				changed = bytes.Replace(changed, []byte(`{"Format"`), []byte(`{"Unknown":true,"Format"`), 1)
			case "duplicate":
				changed = bytes.Replace(changed, []byte(`{"Format"`), []byte(`{"Format":"confctl-hooks-trust-v1","Format"`), 1)
			case "trailing":
				changed = append(changed, []byte("{}\n")...)
			case "noncanonical":
				changed = append([]byte(" "), changed...)
			case "format":
				copyR.Format = "unknown"
				changed, _ = canonical(copyR)
			case "slots":
				copyR.Policies = copyR.Policies[:2]
				changed, _ = canonical(copyR)
			case "roles":
				copyR.Tools = append([]toolRecord(nil), copyR.Tools...)
				copyR.Tools[0].Role = "other"
				changed, _ = canonical(copyR)
			case "base64":
				changed = bytes.Replace(changed, []byte(`"Original":"`), []byte(`"Original":"!`), 1)
			case "digest":
				copyR.ChildSHA256 = strings.Repeat("A", 64)
				changed, _ = canonical(copyR)
			case "decimal":
				copyR.Root.Inode = "01"
				changed, _ = canonical(copyR)
			case "relative":
				copyR.Root.Original = []byte("relative")
				changed, _ = canonical(copyR)
			}
			_, err := parseReceipt(changed)
			assertRefusal(t, err, "invalid-receipt")
		})
	}
	for _, bad := range []string{"/tmp/checker", q.Tools.Checker.Program + ";echo", q.Tools.Checker.Program + "/../tool"} {
		if _, err := FixedConfig(bad); err == nil {
			t.Fatal("shell-unsafe checker path accepted")
		}
	}
}

func TestTrustExplicitOptionalAbsenceAndAmbientControls(t *testing.T) {
	q, io, root := trustFixture(t, false)
	for _, n := range policySlots[1:] {
		os.Remove(filepath.Join(root, n))
		fixtureGit(t, root, q.Capture.Env, nil, "update-index", "--force-remove", "--", n)
	}
	p, _, err := PrepareTrust(context.Background(), q, io)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := parseReceipt(p.candidate)
	if r.Policies[1].Present || r.Policies[1].SHA256 != "" || r.Policies[1].Length != "0" {
		t.Fatal("optional absence was not explicit")
	}
	for _, control := range []string{"LEFTHOOK=", "LEFTHOOK_SKIP=0", "SKIP=", "SKIP_HOOKS=0", "CONFCTL_HOOK_CONTEXT=ignored"} {
		copyQ := q
		copyQ.Capture.Env = append(append([]string(nil), q.Capture.Env...), control)
		p, _, err := PrepareTrust(context.Background(), copyQ, io)
		assertRefusal(t, err, "forbidden-control")
		if p != nil {
			t.Fatal("control approved")
		}
	}
}

type observedTrustDirectory struct {
	TrustDirectory
	read       func(ReceiptImage) (ReceiptImage, error)
	policy     func(string, PolicyImage) (PolicyImage, error)
	closeError error
}

func (d *observedTrustDirectory) ReadReceipt() (ReceiptImage, error) {
	p, err := d.TrustDirectory.ReadReceipt()
	if err != nil {
		return p, err
	}
	if d.read != nil {
		return d.read(p)
	}
	return p, nil
}
func (d *observedTrustDirectory) WorkingPolicy(name string) (PolicyImage, error) {
	p, err := d.TrustDirectory.WorkingPolicy(name)
	if err != nil {
		return p, err
	}
	if d.policy != nil {
		return d.policy(name, p)
	}
	return p, nil
}
func (d *observedTrustDirectory) Close() error {
	return errors.Join(d.TrustDirectory.Close(), d.closeError)
}

func TestTrustInjectedIOFailuresAndCleanupRejectProposal(t *testing.T) {
	for _, failure := range []string{"read", "policy", "tool", "owner", "close"} {
		t.Run(failure, func(t *testing.T) {
			q, io, root := trustFixture(t, false)
			trustNow(t, q, io)
			before := fixtureState(t, root)
			primary := errors.New("injected " + failure)
			baseOpen := io.Open
			io.Open = func(root, admin string) (TrustDirectory, error) {
				d, err := baseOpen(root, admin)
				if err != nil {
					return nil, err
				}
				wrapped := &observedTrustDirectory{TrustDirectory: d}
				if failure == "read" {
					wrapped.read = func(p ReceiptImage) (ReceiptImage, error) { return p, primary }
				}
				if failure == "policy" {
					wrapped.policy = func(n string, p PolicyImage) (PolicyImage, error) { return p, primary }
				}
				if failure == "owner" {
					wrapped.read = func(p ReceiptImage) (ReceiptImage, error) { p.File.UID++; return p, nil }
				}
				if failure == "close" {
					wrapped.closeError = primary
				}
				return wrapped, nil
			}
			if failure == "tool" {
				io.IdentifyTool = func(context.Context, ToolIdentity) (ToolIdentity, error) { return ToolIdentity{}, primary }
			}
			p, changes, err := PrepareTrust(context.Background(), q, io)
			if p != nil || changes != nil || err == nil {
				t.Fatal("I/O or cleanup error returned an approved proposal")
			}
			if failure == "owner" {
				assertRefusal(t, err, "invalid-receipt")
			} else if !errors.Is(err, primary) {
				t.Fatalf("primary error masked: %v", err)
			}
			if !reflect.DeepEqual(before, fixtureState(t, root)) {
				t.Fatal("I/O failure changed original state")
			}
		})
	}
}

func TestTrustCommitCleanupErrorKeepsPublishedState(t *testing.T) {
	q, io, root := trustFixture(t, false)
	p, _, err := PrepareTrust(context.Background(), q, io)
	if err != nil {
		t.Fatal(err)
	}
	open := io.Open
	io.Open = func(root, admin string) (TrustDirectory, error) {
		d, err := open(root, admin)
		if err != nil {
			return nil, err
		}
		return &observedTrustDirectory{TrustDirectory: d, closeError: os.ErrPermission}, nil
	}
	out, err := CommitTrust(context.Background(), q, p, io)
	if !out.Published || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("lost postpublication cleanup state: %#v %v", out, err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".git", receiptName))
	if err != nil || !bytes.Equal(b, p.candidate) {
		t.Fatal("published receipt rolled back after cleanup error")
	}
	entries, _ := os.ReadDir(q.Capture.TempParent)
	if len(entries) != 0 {
		t.Fatal("cleanup error bypassed snapshot owner")
	}
}

var policyModeCases = []struct {
	name     string
	physical os.FileMode
	gitMode  uint32
}{
	{"0644", 0644, 0100644},
	{"0664", 0664, 0100644},
	{"0654-group-only", 0654, 0100644},
	{"0645-other-only", 0645, 0100644},
	{"0655-group-and-other", 0655, 0100644},
	{"0755", 0755, 0100755},
	{"0744-owner-only", 0744, 0100755},
}

func TestTrustPolicyModeAdmissionUsesOwnerExecute(t *testing.T) {
	for _, mode := range policyModeCases {
		for _, staged := range []struct {
			name     string
			physical os.FileMode
			gitMode  uint32
		}{
			{"100644", 0644, 0100644}, {"100755", 0755, 0100755},
		} {
			t.Run(mode.name+"/staged-"+staged.name, func(t *testing.T) {
				q, io, root := trustFixture(t, false)
				path := filepath.Join(root, ".rubocop.yml")
				if err := os.Chmod(path, staged.physical); err != nil {
					t.Fatal(err)
				}
				stage(t, root, q.Capture.Env, ".rubocop.yml")
				entries, err := parseEntries(fixtureGit(t, root, q.Capture.Env, nil,
					"ls-files", "--stage", "-z", "--", ".rubocop.yml"))
				if err != nil || len(entries) != 1 || entries[0].Mode != staged.gitMode {
					t.Fatalf("actual staged fixture mode: %#v %v", entries, err)
				}
				// Establish trust with matching modes, then change only the working
				// permissions. Policy bytes and the trusted staged revision stay exact.
				trustNow(t, q, io)
				if err := os.Chmod(path, mode.physical); err != nil {
					t.Fatal(err)
				}
				before := fixtureState(t, root)
				defer func() {
					if !reflect.DeepEqual(before, fixtureState(t, root)) {
						t.Error("policy admission changed original index/ref/config/receipt/files/modes")
					}
				}()
				match := mode.gitMode == staged.gitMode
				proposal, changes, err := PrepareTrust(context.Background(), q, io)
				if match {
					if err != nil || proposal == nil || len(changes) != 0 {
						t.Errorf("matching Git policy mode refused by PrepareTrust: %v", err)
					}
				} else {
					var refusal *TrustRefusal
					if proposal != nil || !errors.As(err, &refusal) || refusal.Reason != "policy-disagreement" {
						t.Errorf("mismatching owner execute admitted by PrepareTrust: %v", err)
					}
				}
				runnerCalls, toolCalls := 0, 0
				out, err := runFixtureHook(t, context.Background(), HookRequest{TrustRequest: q, Phase: PreCommit}, io,
					func(ctx context.Context, req Invocation) (ToolResult, error) {
						runnerCalls++
						findings, err := CheckPrepared(ctx, preparedInput(t, req, PreCommit), q.Tools,
							func(ctx context.Context, req Invocation) (ToolResult, error) {
								toolCalls++
								if req.Program != q.Tools.Nixfmt.Program {
									t.Fatal("unexpected modeled tool")
								}
								return ToolResult{}, nil
							}, nil)
						if err != nil || HasErrors(findings) {
							t.Errorf("materialized policy projection: %#v %v", findings, err)
						}
						return ToolResult{}, err
					})
				if match {
					if err != nil || !out.Started || runnerCalls != 1 || toolCalls != 1 {
						t.Errorf("matching mode RunHook admission: %#v %v runner=%d tool=%d", out, err, runnerCalls, toolCalls)
					}
				} else {
					var refusal *TrustRefusal
					if !errors.As(err, &refusal) || refusal.Reason != "policy-disagreement" || out.Started || runnerCalls != 0 || toolCalls != 0 {
						t.Errorf("mode disagreement reached executable callback: %#v %v runner=%d tool=%d", out, err, runnerCalls, toolCalls)
					}
				}
				if entries, err := os.ReadDir(q.Capture.TempParent); err != nil || len(entries) != 0 {
					t.Errorf("admission cleanup: %v %#v", err, entries)
				}
			})
		}
	}
}
