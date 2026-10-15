package hookcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type HookPhase string

const (
	PreCommit HookPhase = "pre-commit"
	CommitMsg HookPhase = "commit-msg"
)

type ToolIdentity struct {
	Output, Program, Resolved string
	SHA256                    [32]byte
}

type ServiceEnvironment struct {
	GemHome, GemPath, RubyLib, BundleGemfile, BundleAppConfig string
}

type ChildBinding struct {
	Path        []string
	Locale      string
	RunnerShell *ToolIdentity
	Service     *ServiceEnvironment
}

type ToolSet struct {
	Checker, Git, Lefthook, Nixfmt, Gofmt ToolIdentity
	ServiceRuby                           bool
	RuboCop                               *ToolIdentity
	ChildBinding                          ChildBinding
}

func copyTools(t ToolSet) ToolSet {
	t.ChildBinding.Path = append([]string(nil), t.ChildBinding.Path...)
	if t.RuboCop != nil {
		tool := *t.RuboCop
		t.RuboCop = &tool
	}
	if t.ChildBinding.RunnerShell != nil {
		tool := *t.ChildBinding.RunnerShell
		t.ChildBinding.RunnerShell = &tool
	}
	if t.ChildBinding.Service != nil {
		service := *t.ChildBinding.Service
		t.ChildBinding.Service = &service
	}
	return t
}

type PreparedInput struct {
	ContextPath string
	SHA256      [32]byte
	Phase       HookPhase
	MessagePath string
}

type TrustRequest struct {
	Capture CaptureOptions
	Tools   ToolSet
}

type HookRequest struct {
	TrustRequest
	Phase         HookPhase
	MessagePath   string
	CommentPrefix string
}

var ErrTrustRequired = errors.New("explicit local hook trust required")

// TrustRefusal contains only a finite reason and a literal relevant path.
type TrustRefusal struct{ Reason, Path string }

func (e *TrustRefusal) Error() string  { return fmt.Sprintf("hook trust %s: %q", e.Reason, e.Path) }
func (e *TrustRefusal) Unwrap() error  { return ErrTrustRequired }
func refuse(reason, path string) error { return &TrustRefusal{reason, path} }

// FileIdentity is an observation, not a lifetime lock against same-user writers.
type FileIdentity struct {
	Device, Inode, Links uint64
	UID, Mode            uint32
	Size, MTime, CTime   int64
}

type RepositoryIdentity struct {
	Root, Admin, ResolvedRoot, ResolvedAdmin       []byte
	RootDevice, RootInode, AdminDevice, AdminInode uint64
	UID                                            uint32
}

type PolicyImage struct {
	Present bool
	Mode    uint32
	Data    []byte
	File    FileIdentity
}

type ReceiptImage struct {
	Present bool
	Data    []byte
	File    FileIdentity
}

type TrustIO struct {
	Open         func(root, ownGitDir string) (TrustDirectory, error)
	IdentifyTool func(context.Context, ToolIdentity) (ToolIdentity, error)
}

type TrustDirectory interface {
	Identity() RepositoryIdentity
	WorkingPolicy(string) (PolicyImage, error)
	ExtraRunnerConfigs() ([]string, error)
	ReadReceipt() (ReceiptImage, error)
	Publish(context.Context, ReceiptImage, []byte) (Publication, error)
	Close() error
}

type Publication struct{ Published bool }
type HookOutcome struct {
	Started bool
	Result  ToolResult
}

// A proposal owns the reviewed bytes/preimage. RunHook never consumes a proposal.
type TrustProposal struct {
	candidate []byte
	old       ReceiptImage
}
type TrustChange struct {
	Slot     string
	Old, New []byte
}

var policySlots = [...]string{"lefthook.yml", ".rubocop.yml", ".rubocop_todo.yml"}
var storeProgram = regexp.MustCompile(`^/nix/store/[a-z0-9]{32}-[A-Za-z0-9+._-]+/[A-Za-z0-9+._/-]+$`)

// FixedConfig is deliberately a byte template, not a checkout YAML interpreter.
func FixedConfig(checker string) ([]byte, error) {
	if !storeProgram.MatchString(checker) || filepath.Clean(checker) != checker {
		return nil, refuse("unsupported-policy", checker)
	}
	return []byte(fmt.Sprintf("pre-commit:\n  commands:\n    confctl:\n      run: \"%s check-prepared pre-commit\"\ncommit-msg:\n  commands:\n    confctl:\n      run: \"%s check-prepared commit-msg\"\n", checker, checker)), nil
}

type directoryRecord struct {
	Original, Resolved []byte
	Device, Inode, UID string
}
type toolRecord struct {
	Role                      string
	Output, Program, Resolved []byte
	SHA256                    string
}
type policyRecord struct {
	Path    string
	Present bool
	Mode    uint32
	Length  string
	SHA256  string
}
type childRecord struct {
	Path    [][]byte
	Locale  string
	Shell   *toolRecord
	Service *serviceRecord
}
type serviceRecord struct{ GemHome, GemPath, RubyLib, BundleGemfile, BundleAppConfig []byte }
type trustRecord struct {
	Format      string
	Root, Admin directoryRecord
	Tools       []toolRecord
	ServiceRuby bool
	Child       childRecord
	ChildSHA256 string
	Policies    []policyRecord
}

func canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	return append(b, '\n'), err
}

func decodeCanonical(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	b, err := canonical(v)
	if err != nil || !bytes.Equal(b, data) {
		return errors.New("noncanonical JSON")
	}
	return nil
}
func digest(b []byte) string  { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func decimal(n uint64) string { return strconv.FormatUint(n, 10) }
func validDecimal(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && decimal(n) == s
}
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func absoluteBytes(b []byte) bool {
	return len(b) > 0 && filepath.IsAbs(string(b)) && !bytes.ContainsRune(b, 0)
}
func recordTool(role string, t ToolIdentity) toolRecord {
	return toolRecord{role, []byte(t.Output), []byte(t.Program), []byte(t.Resolved), hex.EncodeToString(t.SHA256[:])}
}
func toolRoles(t ToolSet) []toolRecord {
	out := []toolRecord{recordTool("checker", t.Checker), recordTool("git", t.Git), recordTool("lefthook", t.Lefthook), recordTool("nixfmt", t.Nixfmt), recordTool("gofmt", t.Gofmt)}
	if t.ServiceRuby {
		out = append(out, recordTool("rubocop", *t.RuboCop))
	}
	if t.ChildBinding.RunnerShell != nil {
		out = append(out, recordTool("runner-shell", *t.ChildBinding.RunnerShell))
	}
	return out
}
func childIdentity(c ChildBinding) childRecord {
	out := childRecord{Locale: c.Locale}
	for _, p := range c.Path {
		out.Path = append(out.Path, []byte(p))
	}
	if c.RunnerShell != nil {
		r := recordTool("runner-shell", *c.RunnerShell)
		out.Shell = &r
	}
	if c.Service != nil {
		s := c.Service
		out.Service = &serviceRecord{[]byte(s.GemHome), []byte(s.GemPath), []byte(s.RubyLib), []byte(s.BundleGemfile), []byte(s.BundleAppConfig)}
	}
	return out
}
func toolsFromRecord(r trustRecord) ToolSet {
	decode := func(t toolRecord) ToolIdentity {
		var h [32]byte
		b, _ := hex.DecodeString(t.SHA256)
		copy(h[:], b)
		return ToolIdentity{string(t.Output), string(t.Program), string(t.Resolved), h}
	}
	t := ToolSet{Checker: decode(r.Tools[0]), Git: decode(r.Tools[1]), Lefthook: decode(r.Tools[2]), Nixfmt: decode(r.Tools[3]), Gofmt: decode(r.Tools[4]), ServiceRuby: r.ServiceRuby}
	i := 5
	if t.ServiceRuby {
		v := decode(r.Tools[i])
		t.RuboCop = &v
		i++
	}
	if r.Child.Shell != nil {
		v := decode(r.Tools[i])
		t.ChildBinding.RunnerShell = &v
	}
	t.ChildBinding.Locale = r.Child.Locale
	for _, p := range r.Child.Path {
		t.ChildBinding.Path = append(t.ChildBinding.Path, string(p))
	}
	if r.Child.Service != nil {
		s := r.Child.Service
		t.ChildBinding.Service = &ServiceEnvironment{string(s.GemHome), string(s.GemPath), string(s.RubyLib), string(s.BundleGemfile), string(s.BundleAppConfig)}
	}
	return t
}

func validateTools(t ToolSet) error {
	if t.ServiceRuby != (t.RuboCop != nil) || t.ServiceRuby != (t.ChildBinding.Service != nil) {
		return refuse("unsupported-policy", "service binding")
	}
	if _, err := FixedConfig(t.Checker.Program); err != nil {
		return err
	}
	for _, r := range toolRoles(t) {
		if !absoluteBytes(r.Output) || !absoluteBytes(r.Program) || !absoluteBytes(r.Resolved) || !validDigest(r.SHA256) {
			return refuse("stale-identity", string(r.Program))
		}
		for _, p := range [][]byte{r.Program, r.Resolved} {
			rel, err := filepath.Rel(string(r.Output), string(p))
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
				return refuse("stale-identity", string(p))
			}
		}
	}
	if len(t.ChildBinding.Path) == 0 || (t.ChildBinding.Locale != "C.UTF-8" && t.ChildBinding.Locale != "C.utf8") {
		return refuse("unsupported-policy", "child binding")
	}
	for _, p := range t.ChildBinding.Path {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, ":\x00") {
			return refuse("unsupported-policy", p)
		}
	}
	if t.ChildBinding.Service != nil {
		s := t.ChildBinding.Service
		for _, p := range []string{s.GemHome, s.GemPath, s.RubyLib, s.BundleGemfile, s.BundleAppConfig} {
			if !storeProgram.MatchString(p) || filepath.Clean(p) != p {
				return refuse("unsupported-policy", p)
			}
		}
	}
	return nil
}
func validateRecord(r trustRecord) error {
	if r.Format != "confctl-hooks-trust-v1" {
		return errors.New("unknown receipt format")
	}
	for _, d := range []directoryRecord{r.Root, r.Admin} {
		if !absoluteBytes(d.Original) || !absoluteBytes(d.Resolved) || !validDecimal(d.Device) || !validDecimal(d.Inode) || !validDecimal(d.UID) {
			return errors.New("invalid directory identity")
		}
	}
	if r.Root.UID != r.Admin.UID {
		return errors.New("different directory owners")
	}
	roles := []string{"checker", "git", "lefthook", "nixfmt", "gofmt"}
	if r.ServiceRuby {
		roles = append(roles, "rubocop")
	}
	if r.Child.Shell != nil {
		roles = append(roles, "runner-shell")
	}
	if len(r.Tools) != len(roles) || len(r.Policies) != 3 {
		return errors.New("invalid fixed roles/slots")
	}
	for i, t := range r.Tools {
		if t.Role != roles[i] || !validDigest(t.SHA256) {
			return errors.New("invalid tool role")
		}
	}
	if err := validateTools(toolsFromRecord(r)); err != nil {
		return err
	}
	if !reflect.DeepEqual(childIdentity(toolsFromRecord(r).ChildBinding), r.Child) {
		return errors.New("inconsistent child binding")
	}
	b, _ := canonical(r.Child)
	if r.ChildSHA256 != digest(b) {
		return errors.New("invalid child digest")
	}
	for i, p := range r.Policies {
		if p.Path != policySlots[i] || !validDecimal(p.Length) {
			return errors.New("invalid policy slot")
		}
		if p.Present {
			if (p.Mode != 0100644 && p.Mode != 0100755) || !validDigest(p.SHA256) {
				return errors.New("invalid policy identity")
			}
		} else if p.Mode != 0 || p.Length != "0" || p.SHA256 != "" {
			return errors.New("invalid absent policy")
		}
	}
	if !r.Policies[0].Present {
		return errors.New("missing runner policy")
	}
	return nil
}
func parseReceipt(data []byte) (trustRecord, error) {
	var r trustRecord
	if err := decodeCanonical(data, &r); err != nil {
		return r, refuse("invalid-receipt", "confctl-hooks-trust.json")
	}
	if err := validateRecord(r); err != nil {
		return r, refuse("invalid-receipt", "confctl-hooks-trust.json")
	}
	return r, nil
}
func policySummary(name string, p PolicyImage) policyRecord {
	if !p.Present {
		return policyRecord{Path: name, Length: "0"}
	}
	return policyRecord{name, true, p.Mode, decimal(uint64(len(p.Data))), digest(p.Data)}
}

type trustCollection struct {
	s          *Snapshot
	d          TrustDirectory
	old        ReceiptImage
	record     trustRecord
	bytes      []byte
	policies   [3]PolicyImage
	working    [3]PolicyImage
	repository RepositoryIdentity
	tools      ToolSet
	io         TrustIO
}

func (c *trustCollection) close(primary error) error {
	if c.d != nil {
		primary = errors.Join(primary, c.d.Close())
		c.d = nil
	}
	if c.s != nil {
		primary = c.s.Close(primary)
		c.s = nil
	}
	return primary
}
func forbiddenControls(env []string) error {
	for _, pair := range env {
		k, _, _ := strings.Cut(pair, "=")
		if k == "LEFTHOOK" || strings.HasPrefix(k, "LEFTHOOK_") || k == "SKIP" || strings.HasPrefix(k, "SKIP_") || strings.HasPrefix(k, "CONFCTL_HOOK_") {
			return refuse("forbidden-control", k)
		}
	}
	return nil
}

// Git projects regular-file policy modes using only the owner's execute bit.
func policyMode(mode uint32) uint32 {
	if mode&0100 != 0 {
		return 0100755
	}
	return 0100644
}

func stagedPolicy(s *Snapshot, name string) (PolicyImage, error) {
	for _, e := range s.entries {
		if e.Path == name {
			if e.Mode != 0100644 && e.Mode != 0100755 {
				return PolicyImage{}, refuse("unsupported-policy", name)
			}
			b, identity, err := readOwnedRegular(filepath.Join(s.dir, name), false)
			if err != nil {
				return PolicyImage{}, err
			}
			mode := policyMode(identity.Mode)
			if mode != e.Mode {
				return PolicyImage{}, refuse("stale-policy", name)
			}
			return PolicyImage{true, e.Mode, b, identity}, nil
		}
	}
	return PolicyImage{}, nil
}
func extraNames() []string {
	names := []string{".lefthook", "lefthook-local", ".lefthook-local"}
	for _, base := range []string{"lefthook", ".lefthook", "lefthook-local", ".lefthook-local"} {
		for _, ext := range []string{"yml", "yaml", "json", "toml"} {
			name := base + "." + ext
			if name != "lefthook.yml" {
				names = append(names, name)
			}
		}
	}
	return names
}
func capturedExtras(s *Snapshot) []string {
	var out []string
	for _, n := range extraNames() {
		for _, e := range s.entries {
			if e.Path == n || strings.HasPrefix(e.Path, n+"/") {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
func collectTrust(ctx context.Context, q TrustRequest, fs TrustIO) (_ *trustCollection, err error) {
	q.Tools = copyTools(q.Tools)
	q.Capture.Env = append([]string(nil), q.Capture.Env...)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = validateTools(q.Tools); err != nil {
		return nil, err
	}
	if q.Capture.Git != q.Tools.Git.Program || fs.Open == nil || fs.IdentifyTool == nil {
		return nil, refuse("stale-identity", q.Capture.Git)
	}
	if err = forbiddenControls(q.Capture.Env); err != nil {
		return nil, err
	}
	s, err := Capture(ctx, q.Capture)
	if err != nil {
		return nil, err
	}
	c := &trustCollection{s: s, tools: q.Tools, io: fs}
	defer func() {
		if err != nil {
			err = c.close(err)
		}
	}()
	c.d, err = fs.Open(s.origin, s.gitDir)
	if err != nil {
		return nil, err
	}
	c.old, err = c.d.ReadReceipt()
	if err != nil {
		return nil, err
	}
	c.old.Data = bytes.Clone(c.old.Data)
	if c.old.Present {
		if c.old.File.Mode&0170000 != 0100000 || c.old.File.Mode&07777 != 0600 || c.old.File.UID != uint32(os.Geteuid()) || c.old.File.Links != 1 {
			return nil, refuse("invalid-receipt", receiptName)
		}
		if _, err = parseReceipt(c.old.Data); err != nil {
			return nil, err
		}
	}
	for _, t := range toolRoles(q.Tools) {
		want := toolsFromSingle(t)
		got, e := fs.IdentifyTool(ctx, want)
		if e != nil {
			return nil, e
		}
		if got != want {
			return nil, refuse("stale-identity", want.Program)
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	extra, err := c.d.ExtraRunnerConfigs()
	if err != nil {
		return nil, err
	}
	extra = append(extra, capturedExtras(s)...)
	if len(extra) > 0 {
		return nil, refuse("unsupported-policy", extra[0])
	}
	for i, n := range policySlots {
		staged, e := stagedPolicy(s, n)
		if e != nil {
			return nil, e
		}
		working, e := c.d.WorkingPolicy(n)
		if e != nil {
			return nil, e
		}
		if staged.Present != working.Present || staged.Mode != working.Mode || !bytes.Equal(staged.Data, working.Data) {
			return nil, refuse("policy-disagreement", n)
		}
		c.policies[i] = staged
		working.Data = bytes.Clone(working.Data)
		c.working[i] = working
	}
	fixed, _ := FixedConfig(q.Tools.Checker.Program)
	if !c.policies[0].Present || !bytes.Equal(c.policies[0].Data, fixed) {
		return nil, refuse("unsupported-policy", "lefthook.yml")
	}
	if q.Tools.ServiceRuby {
		if !c.policies[1].Present || !c.policies[2].Present {
			return nil, refuse("unsupported-policy", "root RuboCop pair")
		}
		for _, e := range s.entries {
			if filepath.Base(e.Path) == ".rubocop.yml" && e.Path != ".rubocop.yml" {
				return nil, refuse("unsupported-policy", e.Path)
			}
		}
		if err = serviceAncestors(s); err != nil {
			return nil, err
		}
	}
	id := c.d.Identity()
	id.Root = bytes.Clone(id.Root)
	id.Admin = bytes.Clone(id.Admin)
	id.ResolvedRoot = bytes.Clone(id.ResolvedRoot)
	id.ResolvedAdmin = bytes.Clone(id.ResolvedAdmin)
	c.repository = id
	dirRecord := func(original, resolved []byte, dev, ino uint64) directoryRecord {
		return directoryRecord{bytes.Clone(original), bytes.Clone(resolved), decimal(dev), decimal(ino), decimal(uint64(id.UID))}
	}
	c.record = trustRecord{Format: "confctl-hooks-trust-v1", Root: dirRecord(id.Root, id.ResolvedRoot, id.RootDevice, id.RootInode), Admin: dirRecord(id.Admin, id.ResolvedAdmin, id.AdminDevice, id.AdminInode), Tools: toolRoles(q.Tools), ServiceRuby: q.Tools.ServiceRuby, Child: childIdentity(q.Tools.ChildBinding)}
	b, _ := canonical(c.record.Child)
	c.record.ChildSHA256 = digest(b)
	for i, n := range policySlots {
		c.record.Policies = append(c.record.Policies, policySummary(n, c.policies[i]))
	}
	if err = validateRecord(c.record); err != nil {
		return nil, err
	}
	c.bytes, err = canonical(c.record)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return c, nil
}
func toolsFromSingle(r toolRecord) ToolIdentity {
	var h [32]byte
	b, _ := hex.DecodeString(r.SHA256)
	copy(h[:], b)
	return ToolIdentity{string(r.Output), string(r.Program), string(r.Resolved), h}
}

// PrepareTrust has no publication side effect. Returned changes do not alias it.
func PrepareTrust(ctx context.Context, q TrustRequest, fs TrustIO) (proposal *TrustProposal, changes []TrustChange, err error) {
	c, err := collectTrust(ctx, q, fs)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		err = c.close(err)
		if err != nil {
			proposal = nil
			changes = nil
		}
	}()
	var old trustRecord
	if c.old.Present {
		old, _ = parseReceipt(c.old.Data)
	}
	fields := []struct {
		name string
		a, b any
	}{{"root", old.Root, c.record.Root}, {"admin", old.Admin, c.record.Admin}, {"tools", old.Tools, c.record.Tools}, {"child", old.Child, c.record.Child}, {"service", old.ServiceRuby, c.record.ServiceRuby}}
	for i, n := range policySlots {
		var prev policyRecord
		if len(old.Policies) == 3 {
			prev = old.Policies[i]
		}
		fields = append(fields, struct {
			name string
			a, b any
		}{n, prev, c.record.Policies[i]})
	}
	for _, f := range fields {
		a, _ := canonical(f.a)
		b, _ := canonical(f.b)
		if !bytes.Equal(a, b) {
			changes = append(changes, TrustChange{f.name, bytes.Clone(a), bytes.Clone(b)})
		}
	}
	proposal = &TrustProposal{bytes.Clone(c.bytes), ReceiptImage{c.old.Present, bytes.Clone(c.old.Data), c.old.File}}
	return proposal, changes, nil
}

// CommitTrust freshly collects the same finite candidate and old receipt. It
// publishes reviewed bytes only; intervening unrelated source changes are allowed.
// An already matching receipt is a no-change result (Published=false).
func CommitTrust(ctx context.Context, q TrustRequest, p *TrustProposal, fs TrustIO) (out Publication, err error) {
	if p == nil {
		return out, refuse("missing-receipt", "proposal")
	}
	c, err := collectTrust(ctx, q, fs)
	if err != nil {
		return out, err
	}
	defer func() { err = c.close(err) }()
	if !bytes.Equal(c.bytes, p.candidate) || !reflect.DeepEqual(c.old, p.old) {
		return out, refuse("stale-policy", "reviewed proposal")
	}
	if c.old.Present && bytes.Equal(c.old.Data, p.candidate) {
		return out, nil
	}
	return c.d.Publish(ctx, c.old, bytes.Clone(p.candidate))
}
