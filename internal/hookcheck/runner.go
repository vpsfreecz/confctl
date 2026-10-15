package hookcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

type preparedEntry struct {
	Path     []byte
	ObjectID string
	Mode     uint32
}
type authorEntry struct {
	Name  string
	Value []byte
}
type preparedPolicy struct {
	Path    string
	Present bool
	Mode    uint32
	Data    []byte
	File    FileIdentity
}
type preparedContext struct {
	Format                 string
	Phase                  HookPhase
	Trust                  trustRecord
	TrustSHA256            string
	Snapshot               []byte
	SnapshotID             directoryRecord
	Entries                []preparedEntry
	Selected, HEAD         [][]byte
	Authors                []authorEntry
	UserName, UserEmail    []byte
	Policies               []preparedPolicy
	Control                []byte
	ControlID              directoryRecord
	Message, CommentPrefix []byte
}

func phaseValid(p HookPhase) bool { return p == PreCommit || p == CommitMsg }
func authorKey(k string) bool {
	return k == "GIT_AUTHOR_NAME" || k == "GIT_AUTHOR_EMAIL" || k == "GIT_COMMITTER_NAME" || k == "GIT_COMMITTER_EMAIL"
}
func directoryProjection(path string, id FileIdentity) directoryRecord {
	return directoryRecord{[]byte(path), []byte(path), decimal(id.Device), decimal(id.Inode), decimal(uint64(id.UID))}
}
func checkDirectoryRecord(r directoryRecord) error {
	if !absoluteBytes(r.Original) || !bytes.Equal(r.Original, r.Resolved) || !validDecimal(r.Device) || !validDecimal(r.Inode) || r.UID != decimal(uint64(os.Geteuid())) {
		return refuse("stale-identity", string(r.Original))
	}
	f, err := openDirectory(string(r.Original))
	if err != nil {
		return err
	}
	id, e := fileIdentity(f)
	err = errors.Join(e, f.Close())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(directoryProjection(string(r.Original), id), r) {
		return refuse("stale-identity", string(r.Original))
	}
	if id.Mode&07777 != 0700 {
		return refuse("stale-identity", string(r.Original))
	}
	return nil
}
func snapshotIdentity(s *Snapshot) (FileIdentity, error) {
	f, err := openDirectory(s.dir)
	if err != nil {
		return FileIdentity{}, err
	}
	id, e := fileIdentity(f)
	err = errors.Join(e, f.Close())
	if err != nil {
		return id, err
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return id, err
	}
	if !os.SameFile(s.owned, info) {
		return id, refuse("stale-identity", s.dir)
	}
	return id, nil
}
func (c *trustCollection) recheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := snapshotIdentity(c.s); err != nil {
		return err
	}
	if !reflect.DeepEqual(c.d.Identity(), c.repository) {
		return refuse("stale-identity", c.s.origin)
	}
	receipt, err := c.d.ReadReceipt()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(receipt, c.old) {
		return refuse("stale-identity", receiptName)
	}
	for _, r := range toolRoles(c.tools) {
		want := toolsFromSingle(r)
		got, err := c.io.IdentifyTool(ctx, want)
		if err != nil {
			return err
		}
		if got != want {
			return refuse("stale-identity", want.Program)
		}
	}
	extra, err := c.d.ExtraRunnerConfigs()
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return refuse("unsupported-policy", extra[0])
	}
	for i, n := range policySlots {
		staged, err := stagedPolicy(c.s, n)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(staged, c.policies[i]) {
			return refuse("stale-policy", n)
		}
		working, err := c.d.WorkingPolicy(n)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(working, c.working[i]) {
			return refuse("stale-policy", n)
		}
	}
	return ctx.Err()
}

type controlDirectory struct {
	path  string
	id    FileIdentity
	known map[string]FileIdentity
}

func createControl(parent string) (_ *controlDirectory, err error) {
	if parent == "" {
		parent = os.TempDir()
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp(parent, "confctl-hook-control-")
	if err != nil {
		return nil, err
	}
	c := &controlDirectory{path: path, known: make(map[string]FileIdentity)}
	f, err := openDirectory(path)
	if err != nil {
		return nil, errors.Join(err, os.Remove(path))
	}
	c.id, err = fileIdentity(f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, c.close())
		}
	}()
	for _, n := range []string{"home", "tmp", "config", "cache", "gocache", "gopath", "runner"} {
		if err = os.Mkdir(filepath.Join(path, n), 0700); err != nil {
			return nil, err
		}
		f, e := openDirectory(filepath.Join(path, n))
		if e != nil {
			return nil, e
		}
		id, e := fileIdentity(f)
		e = errors.Join(e, f.Close())
		if e != nil {
			return nil, e
		}
		c.known[n] = id
	}
	return c, nil
}

func (c *controlDirectory) runnerDirectory() (*os.File, error) {
	path := filepath.Join(c.path, "runner")
	f, err := openDirectory(path)
	if err != nil {
		return nil, err
	}
	id, err := fileIdentity(f)
	if err != nil || !sameDirectory(id, c.known["runner"]) || id.Mode&07777 != 0700 {
		return nil, errors.Join(err, refuse("stale-identity", path), f.Close())
	}
	return f, nil
}

func (c *controlDirectory) writeRunnerPolicy(policy []byte) (err error) {
	d, err := c.runnerDirectory()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	f, err := openAt(d, "lefthook.yml", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0400)
	if err != nil {
		return err
	}
	_, err = f.Write(policy)
	return errors.Join(err, f.Close())
}

func (c *controlDirectory) checkRunner(policy []byte) (err error) {
	d, err := c.runnerDirectory()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	git, err := openAt(d, ".git", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	id, err := fileIdentity(git)
	err = errors.Join(err, git.Close())
	if err != nil {
		return err
	}
	if id.UID != uint32(os.Geteuid()) {
		return refuse("stale-identity", filepath.Join(d.Name(), ".git"))
	}
	data, id, err := readAt(d, "lefthook.yml", false)
	if err != nil {
		return err
	}
	if id.UID != uint32(os.Geteuid()) || id.Links != 1 || id.Mode&07777 != 0400 || !bytes.Equal(data, policy) {
		return refuse("stale-policy", filepath.Join(d.Name(), "lefthook.yml"))
	}
	entries, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return refuse("unsupported-policy", d.Name())
	}
	return nil
}
func (c *controlDirectory) write(data []byte) (input PreparedInput, err error) {
	path := filepath.Join(c.path, "context.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return input, err
	}
	id, err := fileIdentity(f)
	if err != nil {
		return input, errors.Join(err, f.Close())
	}
	c.known["context.json"] = id
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return input, err
	}
	input.ContextPath = path
	input.SHA256 = sha256.Sum256(data)
	return input, nil
}
func (c *controlDirectory) close() error {
	f, err := openDirectory(c.path)
	if err != nil {
		return err
	}
	now, e := fileIdentity(f)
	err = errors.Join(e, f.Close())
	if err != nil {
		return err
	}
	if !sameDirectory(now, c.id) {
		return refuse("stale-identity", c.path)
	}
	var joined error
	// Refuse changed known resources; the independent snapshot is never a child
	// of this directory, so this cleanup cannot bypass Snapshot.Close's guard.
	for name, id := range c.known {
		p := filepath.Join(c.path, name)
		info, e := os.Lstat(p)
		if e != nil {
			joined = errors.Join(joined, e)
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(st.Dev) != id.Device || st.Ino != id.Inode || st.Uid != id.UID || st.Mode != id.Mode || (!info.IsDir() && uint64(st.Nlink) != id.Links) {
			joined = errors.Join(joined, refuse("stale-identity", p))
			continue
		}
		if info.IsDir() {
			e = removeControlContents(p, id)
		} else {
			e = os.Remove(p)
		}
		joined = errors.Join(joined, e)
	}
	// Unknown root entries are foreign and leave the directory nonempty.
	return errors.Join(joined, os.Remove(c.path))
}
func removeControlContents(path string, id FileIdentity) error {
	f, err := openDirectory(path)
	if err != nil {
		return err
	}
	now, e := fileIdentity(f)
	err = errors.Join(e, f.Close())
	if err != nil {
		return err
	}
	if !sameDirectory(now, id) {
		return refuse("stale-identity", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	var joined error
	for _, entry := range entries {
		p := filepath.Join(path, entry.Name())
		info, e := os.Lstat(p)
		if e != nil {
			joined = errors.Join(joined, e)
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Geteuid()) {
			joined = errors.Join(joined, refuse("stale-identity", p))
			continue
		}
		if info.IsDir() {
			e = removeControlContents(p, identity(*st))
		} else {
			e = os.Remove(p)
		}
		joined = errors.Join(joined, e)
	}
	return errors.Join(joined, os.Remove(path))
}
func controlledEnv(c ChildBinding, control, snapshot string, authors []authorEntry) []string {
	out := []string{"PATH=" + strings.Join(c.Path, ":"), "LANG=" + c.Locale, "LC_ALL=" + c.Locale, "TZ=UTC",
		"HOME=" + filepath.Join(control, "home"), "TMPDIR=" + filepath.Join(control, "tmp"), "XDG_CONFIG_HOME=" + filepath.Join(control, "config"), "XDG_CACHE_HOME=" + filepath.Join(control, "cache"),
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOENV=off", "GOFLAGS=", "GOCACHE=" + filepath.Join(control, "gocache"), "GOPATH=" + filepath.Join(control, "gopath"),
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(snapshot), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	for _, a := range authors {
		out = append(out, a.Name+"="+string(a.Value))
	}
	if c.RunnerShell != nil {
		out = append(out, "SHELL="+c.RunnerShell.Program)
	}
	if c.Service != nil {
		s := c.Service
		out = append(out, "GEM_HOME="+s.GemHome, "GEM_PATH="+s.GemPath, "RUBYLIB="+s.RubyLib, "BUNDLE_GEMFILE="+s.BundleGemfile, "BUNDLE_APP_CONFIG="+s.BundleAppConfig)
	}
	return out
}
func serviceAncestors(s *Snapshot) error {
	for _, n := range s.selected {
		if !rubyPath(n) {
			continue
		}
		for parent := filepath.Dir(n); parent != "."; parent = filepath.Dir(parent) {
			name := filepath.Join(parent, ".rubocop.yml")
			_, _, err := readOwnedRegular(filepath.Join(s.origin, name), false)
			if err == nil {
				return refuse("unsupported-policy", name)
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// RunHook owns one Capture and no trust writes. Its Invoke must join owned child
// activity before returning; injected status0 is not actual Lefthook acceptance.
func RunHook(ctx context.Context, q HookRequest, io TrustIO, invoke Invoke) (out HookOutcome, err error) {
	q.Tools = copyTools(q.Tools)
	q.Capture.Env = append([]string(nil), q.Capture.Env...)
	if !phaseValid(q.Phase) || invoke == nil {
		return out, refuse("unsupported-policy", string(q.Phase))
	}
	c, err := collectTrust(ctx, q.TrustRequest, io)
	if err != nil {
		return out, err
	}
	var control *controlDirectory
	defer func() {
		if control != nil {
			err = errors.Join(err, control.close())
		}
		err = c.close(err)
	}()
	if !c.old.Present {
		return out, refuse("missing-receipt", receiptName)
	}
	old, _ := parseReceipt(c.old.Data)
	if !reflect.DeepEqual(old.Root, c.record.Root) || !reflect.DeepEqual(old.Admin, c.record.Admin) || !reflect.DeepEqual(old.Tools, c.record.Tools) || !reflect.DeepEqual(old.Child, c.record.Child) || old.ServiceRuby != c.record.ServiceRuby {
		return out, refuse("stale-identity", receiptName)
	}
	if !bytes.Equal(c.old.Data, c.bytes) {
		return out, refuse("stale-policy", receiptName)
	}
	if q.Tools.ServiceRuby {
		if err = serviceAncestors(c.s); err != nil {
			return out, err
		}
	}
	message := ""
	if q.Phase == CommitMsg {
		message = q.MessagePath
		if !filepath.IsAbs(message) {
			cwd, e := filepath.Abs(q.Capture.Dir)
			if e != nil {
				return out, e
			}
			message = cwd + string(os.PathSeparator) + message
		}
		if !absoluteBytes([]byte(message)) {
			return out, refuse("unsupported-policy", message)
		}
	}
	control, err = createControl(q.Capture.TempParent)
	if err != nil {
		return out, err
	}
	// Git's discovery ceiling is a colon-separated path list. Refuse a private
	// temp parent it cannot encode as one literal directory, before any child.
	if strings.ContainsRune(filepath.Dir(c.s.dir), ':') {
		return out, refuse("unsupported-policy", filepath.Dir(c.s.dir))
	}
	sid, err := snapshotIdentity(c.s)
	if err != nil {
		return out, err
	}
	prepared := preparedContext{Format: "confctl-hooks-prepared-v1", Phase: q.Phase, Trust: c.record, TrustSHA256: digest(c.bytes), Snapshot: []byte(c.s.dir), SnapshotID: directoryProjection(c.s.dir, sid), Control: []byte(control.path), ControlID: directoryProjection(control.path, control.id), UserName: []byte(c.s.config["user.name"]), UserEmail: []byte(c.s.config["user.email"]), Message: []byte(message), CommentPrefix: []byte(q.CommentPrefix)}
	for _, e := range c.s.entries {
		prepared.Entries = append(prepared.Entries, preparedEntry{[]byte(e.Path), e.ObjectID, e.Mode})
	}
	for _, n := range c.s.selected {
		prepared.Selected = append(prepared.Selected, []byte(n))
	}
	for _, n := range c.s.headPaths {
		prepared.HEAD = append(prepared.HEAD, []byte(n))
	}
	for _, pair := range c.s.env {
		k, v, ok := strings.Cut(pair, "=")
		if ok && authorKey(k) {
			prepared.Authors = append(prepared.Authors, authorEntry{k, []byte(v)})
		}
	}
	for i, n := range policySlots {
		p := c.policies[i]
		prepared.Policies = append(prepared.Policies, preparedPolicy{n, p.Present, p.Mode, bytes.Clone(p.Data), p.File})
	}
	data, err := canonical(prepared)
	if err != nil {
		return out, err
	}
	input, err := control.write(data)
	if err != nil {
		return out, err
	}
	input.Phase = q.Phase
	input.MessagePath = message
	if err = c.recheck(ctx); err != nil {
		return out, err
	}
	// Lefthook needs Git startup metadata, but it must never acquire the
	// original repository or turn the captured staged snapshot into a repo.
	runner := filepath.Join(control.path, "runner")
	env := controlledEnv(q.Tools.ChildBinding, control.path, runner, prepared.Authors)
	initialized, initErr := invoke(ctx, Invocation{Program: q.Tools.Git.Program, Args: []string{"init", "--quiet", "--template=", "--initial-branch=confctl-hook-runner", "."}, Dir: runner, Env: env})
	if initErr = errors.Join(initErr, ctx.Err()); initErr != nil || initialized.ExitCode != 0 {
		return out, errors.Join(initErr, fmt.Errorf("initialize hook runner: status %d: %s", initialized.ExitCode, initialized.Stderr))
	}
	if err = control.writeRunnerPolicy(c.policies[0].Data); err != nil {
		return out, err
	}
	if err = c.recheck(ctx); err != nil {
		return out, err
	}
	if err = control.checkRunner(c.policies[0].Data); err != nil {
		return out, err
	}
	env = append(env, "CONFCTL_HOOK_CONTEXT="+input.ContextPath, "CONFCTL_HOOK_CONTEXT_SHA256="+hex.EncodeToString(input.SHA256[:]), "CONFCTL_HOOK_MESSAGE="+message)
	// Recheck the context itself before passing its fixed data binding.
	b, _, err := readOwnedRegular(input.ContextPath, true)
	if err != nil {
		return out, err
	}
	if !bytes.Equal(b, data) {
		return out, refuse("stale-identity", input.ContextPath)
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	out.Started = true
	out.Result, err = invoke(ctx, Invocation{Program: q.Tools.Lefthook.Program, Args: []string{"run", "--force", "--no-auto-install", "--no-stage-fixed", "--skip-lfs", string(q.Phase)}, Dir: runner, Env: env})
	err = errors.Join(err, ctx.Err())
	return out, err
}

func validatePrepared(p preparedContext, input PreparedInput, tools ToolSet) error {
	if p.Format != "confctl-hooks-prepared-v1" || !phaseValid(p.Phase) || p.Phase != input.Phase {
		return refuse("invalid-receipt", input.ContextPath)
	}
	if err := validateRecord(p.Trust); err != nil {
		return refuse("invalid-receipt", input.ContextPath)
	}
	if err := validateTools(tools); err != nil {
		return err
	}
	if !reflect.DeepEqual(toolRoles(tools), p.Trust.Tools) || !reflect.DeepEqual(childIdentity(tools.ChildBinding), p.Trust.Child) || tools.ServiceRuby != p.Trust.ServiceRuby {
		return refuse("stale-identity", "prepared tools")
	}
	b, _ := canonical(p.Trust)
	if digest(b) != p.TrustSHA256 {
		return refuse("invalid-receipt", input.ContextPath)
	}
	if !bytes.Equal(p.Snapshot, p.SnapshotID.Original) || !bytes.Equal(p.Control, p.ControlID.Original) || filepath.Join(string(p.Control), "context.json") != input.ContextPath {
		return refuse("stale-identity", input.ContextPath)
	}
	if filepath.Dir(string(p.Snapshot)) != filepath.Dir(string(p.Control)) || string(p.Snapshot) == string(p.Control) {
		return refuse("stale-identity", input.ContextPath)
	}
	if err := checkDirectoryRecord(p.ControlID); err != nil {
		return err
	}
	if err := checkDirectoryRecord(p.SnapshotID); err != nil {
		return err
	}
	if p.Phase == CommitMsg {
		if !absoluteBytes(p.Message) || string(p.Message) != input.MessagePath {
			return refuse("stale-identity", "message data")
		}
	} else if len(p.Message) != 0 || input.MessagePath != "" {
		return refuse("stale-identity", "message data")
	}
	seen := map[string]uint32{}
	previous := ""
	for i, e := range p.Entries {
		name := string(e.Path)
		if !validPath(name) || (i > 0 && name <= previous) {
			return refuse("invalid-receipt", name)
		}
		previous = name
		if e.Mode != 0100644 && e.Mode != 0100755 && e.Mode != 0120000 && e.Mode != 0160000 {
			return refuse("invalid-receipt", name)
		}
		oid, e2 := hex.DecodeString(e.ObjectID)
		if e2 != nil || (len(oid) != 20 && len(oid) != 32) {
			return refuse("invalid-receipt", name)
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if _, ok := seen[parent]; ok {
				return refuse("invalid-receipt", name)
			}
		}
		seen[name] = e.Mode
	}
	previous = ""
	for i, n := range p.Selected {
		name := string(n)
		mode, ok := seen[name]
		if !ok || mode == 0160000 || (i > 0 && name <= previous) {
			return refuse("invalid-receipt", name)
		}
		previous = name
	}
	for _, n := range p.HEAD {
		if !validPath(string(n)) {
			return refuse("invalid-receipt", string(n))
		}
	}
	for _, a := range p.Authors {
		if !authorKey(a.Name) || bytes.ContainsRune(a.Value, 0) {
			return refuse("invalid-receipt", a.Name)
		}
	}
	if len(p.Policies) != 3 {
		return refuse("invalid-receipt", "prepared policy slots")
	}
	s := &Snapshot{dir: string(p.Snapshot)}
	for _, e := range p.Entries {
		s.entries = append(s.entries, Entry{string(e.Path), e.ObjectID, e.Mode})
	}
	for i, n := range policySlots {
		want := p.Policies[i]
		if want.Path != n {
			return refuse("invalid-receipt", n)
		}
		image := PolicyImage{want.Present, want.Mode, want.Data, want.File}
		if !reflect.DeepEqual(policySummary(n, image), p.Trust.Policies[i]) {
			return refuse("stale-policy", n)
		}
		actual, err := stagedPolicy(s, n)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(image, actual) {
			return refuse("stale-policy", n)
		}
	}
	fixed, _ := FixedConfig(tools.Checker.Program)
	if !bytes.Equal(p.Policies[0].Data, fixed) {
		return refuse("unsupported-policy", "lefthook.yml")
	}
	if len(capturedExtras(s)) > 0 {
		return refuse("unsupported-policy", capturedExtras(s)[0])
	}
	return nil
}

// GoCheck borrows the validated snapshot and controlled environment for one
// synchronous semantic check. It must not retain or close those parent resources.
type GoCheck func(context.Context, Invocation) error

func moduleManifest(name string) bool {
	return name == "go.mod" || name == "go.sum" || name == "extensions/go.mod" || name == "extensions/go.sum"
}

func preparedGoCheckReached(p preparedContext) bool {
	for _, name := range p.Selected {
		if strings.HasSuffix(string(name), ".go") || moduleManifest(string(name)) {
			return true
		}
	}
	// Formatter selection excludes deletions, rename sources and nonregular
	// type changes. The complete captured HEAD/index sets still
	// identify a formerly tracked manifest that is absent or no longer regular.
	missing := map[string]bool{}
	for _, name := range p.HEAD {
		if moduleManifest(string(name)) {
			missing[string(name)] = true
		}
	}
	for _, entry := range p.Entries {
		if entry.Mode == 0100644 || entry.Mode == 0100755 {
			delete(missing, string(entry.Path))
		}
	}
	return len(missing) != 0
}

// CheckPrepared borrows the parent's snapshot, never captures Git or closes it.
// Its Go callback borrows validated context; only fixed formatters cross Invoke.
func CheckPrepared(ctx context.Context, input PreparedInput, tools ToolSet, invoke Invoke, checkGo GoCheck) ([]Finding, error) {
	tools = copyTools(tools)
	p, err := readPrepared(ctx, input)
	if err != nil {
		return nil, err
	}
	if err = validatePrepared(p, input, tools); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if p.Phase == CommitMsg {
		return CheckMessage(string(p.Message), string(p.CommentPrefix))
	}
	s := &Snapshot{dir: string(p.Snapshot), config: map[string]string{"user.name": string(p.UserName), "user.email": string(p.UserEmail)}}
	for _, n := range p.Selected {
		s.selected = append(s.selected, string(n))
	}
	for _, n := range p.HEAD {
		s.headPaths = append(s.headPaths, string(n))
	}
	for _, a := range p.Authors {
		s.env = append(s.env, a.Name+"="+string(a.Value))
	}
	narrow := func(ctx context.Context, req Invocation) (ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		allowed := req.Program == tools.Nixfmt.Program || req.Program == tools.Gofmt.Program
		if tools.ServiceRuby && tools.RuboCop != nil {
			allowed = allowed || req.Program == tools.RuboCop.Program
		}
		if !allowed || req.Dir != s.dir || invoke == nil {
			return ToolResult{}, refuse("unsupported-policy", req.Program)
		}
		req.Env = controlledEnv(tools.ChildBinding, string(p.Control), s.dir, p.Authors)
		return invoke(ctx, req)
	}
	o := CheckOptions{Nixfmt: tools.Nixfmt.Program, Gofmt: tools.Gofmt.Program, ServiceRuby: tools.ServiceRuby, Invoke: narrow}
	if tools.RuboCop != nil {
		o.RuboCop = tools.RuboCop.Program
	}
	findings, err := CheckFiles(ctx, s, o)
	if err != nil || HasErrors(findings) {
		return findings, err
	}
	if err := ctx.Err(); err != nil {
		return findings, err
	}
	if preparedGoCheckReached(p) {
		if checkGo == nil {
			return findings, errors.New("selected Go changes require a semantic check callback")
		}
		return findings, checkGo(ctx, Invocation{Dir: s.dir, Env: controlledEnv(tools.ChildBinding, string(p.Control), s.dir, p.Authors)})
	}
	return findings, nil
}
