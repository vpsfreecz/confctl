package inputs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

type SetDeps struct {
	Run       func(context.Context, string, []string) (Output, error)
	Argument  func(any) string
	Changelog Changelog
	Commit    func(context.Context, string, string, bool) error
}

type SetPhase uint8

const (
	FirstRead SetPhase = iota
	SnapshotRead
	Validate
	TempName
	TempRead
	Restore
	Format
	TempWrite
	Rename
	FinalRead
	Diff
)

// SetError retains the actual file operands and cause without changing its
// presentation. Process, output-guard and context failures keep their types.
type SetError struct {
	Phase        SetPhase
	Path, Target string
	Err          error
}

func (e *SetError) Error() string { return e.Err.Error() }
func (e *SetError) Unwrap() error { return e.Err }

type setOps struct {
	Stat      func(string) (os.FileInfo, error)
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error
	Rename    func(string, string) error
	Unlink    func(string) error
	PID       func() int
	Random    func() ([6]byte, error)
}

func defaultSetOps() setOps {
	return setOps{
		Stat: os.Stat, ReadFile: os.ReadFile,
		WriteFile: func(path string, data []byte) error { return os.WriteFile(path, data, 0666) },
		Rename:    os.Rename, Unlink: syscall.Unlink, PID: os.Getpid,
		Random: func() (bytes [6]byte, err error) { _, err = rand.Read(bytes[:]); return },
	}
}

func Set(ctx context.Context, root string, names []any, rev string, opts UpdateOptions, deps SetDeps) (UpdateResult, error) {
	return setWithOps(ctx, root, names, rev, opts, deps, defaultSetOps())
}

// File.join retains internal separators and supplies the slash for an empty
// root. Literal operands must not pass through filepath.Clean.
func setPath(root, name string) string { return strings.TrimRight(root, "/") + "/" + name }

func setPathValid(path string) error {
	if strings.IndexByte(path, 0) >= 0 {
		return errors.New("path name contains null byte")
	}
	return nil
}

func setFailure(ctx context.Context, phase SetPhase, path, target string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	return &SetError{Phase: phase, Path: path, Target: target, Err: err}
}

func setReadDocument(ctx context.Context, path string, phase SetPhase, ops setOps) (*orderedLockDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bytes, err := ops.ReadFile(path)
	if failure := setFailure(ctx, phase, path, "", err); failure != nil {
		return nil, failure
	}
	doc, err := parseOrderedLock(bytes)
	return doc, setFailure(ctx, phase, path, "", err)
}

func setTruthy(v *orderedLockValue) bool {
	return v != nil && v.kind != orderedNull && (v.kind != orderedBool || v.boolean)
}

func setObject() *orderedLockValue { return &orderedLockValue{kind: orderedObject} }
func setOrObject(v *orderedLockValue) *orderedLockValue {
	if !setTruthy(v) {
		return setObject()
	}
	return v
}

// dig stops at intermediate null, while false and other scalar containers fail
// at that consumer. A non-string key remains a non-string lookup, not to_s.
func setDig(v *orderedLockValue, keys ...any) (*orderedLockValue, error) {
	for i, key := range keys {
		if i > 0 && (v == nil || v.kind == orderedNull) {
			return nil, nil
		}
		if v == nil || v.kind != orderedObject {
			return nil, errors.New("expected input object for dig")
		}
		name, ok := key.(string)
		if !ok {
			return nil, nil
		}
		v, _ = v.lookup(name)
	}
	return v, nil
}

func setMember(v *orderedLockValue, name string) (*orderedLockValue, error) {
	if v == nil || v.kind != orderedObject {
		return nil, errors.New("expected input object")
	}
	value, _ := v.lookup(name)
	return value, nil
}

func setResolveNodeID(doc *orderedLockDocument, name any, argument func(any) string) (string, error) {
	ref, err := setDig(doc.root, "nodes", "root", "inputs", name)
	if err != nil {
		return "", err
	}
	if ref != nil && ref.kind == orderedArray && len(ref.elements) > 0 {
		ref = ref.elements[0]
	}
	if ref != nil && ref.kind == orderedString {
		return ref.text, nil
	}
	return "", fmt.Errorf("unknown input '%s'", argument(name))
}

type setInfo struct {
	info             Info
	locked, original *orderedLockValue
}

func setInputInfo(doc *orderedLockDocument, name any, argument func(any) string) (setInfo, error) {
	var result setInfo
	ref, err := setDig(doc.root, "nodes", "root", "inputs", name)
	if err != nil {
		return result, err
	}
	if ref != nil && ref.kind == orderedArray {
		if len(ref.elements) > 0 && ref.elements[0].kind == orderedString {
			ref = ref.elements[0]
		} else {
			ref = nil
		}
	}
	var node *orderedLockValue
	if ref != nil && ref.kind == orderedString {
		node, err = setDig(doc.root, "nodes", ref.text)
		if err != nil {
			return result, err
		}
	}
	node = setOrObject(node)
	locked, err := setMember(node, "locked")
	if err != nil {
		return result, err
	}
	original, err := setMember(node, "original")
	if err != nil {
		return result, err
	}
	result.locked, result.original = setOrObject(locked), setOrObject(original)
	field := func(container *orderedLockValue, key string) any {
		if err != nil {
			return nil
		}
		var v *orderedLockValue
		v, err = setMember(container, key)
		if v == nil {
			return nil
		}
		return v.view()
	}
	l, o := result.locked, result.original
	preferred := func(a, b *orderedLockValue, key string) any {
		value := field(a, key)
		if value == nil || value == false {
			value = field(b, key)
		}
		return value
	}
	// Keep both consumer order and Ruby's short-circuit field accesses.
	result.info.Type = first(preferred(l, o, "type"), "-")
	result.info.Rev = field(l, "rev")
	if err != nil {
		return result, err
	}
	if setRevision := result.info.Rev; setRevision != nil && setRevision != false {
		revision, ok := setRevision.(string)
		if !ok {
			return result, errors.New("expected input revision string")
		}
		runes := []rune(revision)
		if len(runes) > 8 {
			runes = runes[:8]
		}
		result.info.ShortRev = string(runes)
	}
	result.info.Ref = preferred(o, l, "ref")
	result.info.URL = preferred(l, o, "url")
	if url, ok := result.info.URL.(string); ok {
		result.info.URL = strings.TrimPrefix(url, "git+")
	}
	if field(l, "type") == "github" || field(o, "type") == "github" {
		owner, repo := preferred(l, o, "owner"), preferred(l, o, "repo")
		if owner != nil && owner != false && repo != nil && repo != false {
			result.info.URL = "https://github.com/" + argument(owner) + "/" + argument(repo)
		}
	}
	return result, err
}

func setOverrideURL(info setInfo, rev string, argument func(any) string) (string, error) {
	field := func(key string) (any, error) {
		v, err := setMember(info.locked, key)
		if err != nil {
			return nil, err
		}
		if !setTruthy(v) {
			v, err = setMember(info.original, key)
		}
		if v == nil {
			return nil, err
		}
		return v.view(), err
	}
	switch info.info.Type {
	case "github":
		owner, err := field("owner")
		if err != nil {
			return "", err
		}
		repo, err := field("repo")
		if err != nil {
			return "", err
		}
		dir, err := field("dir")
		if err != nil {
			return "", err
		}
		if owner == nil || owner == false || repo == nil || repo == false {
			return "", errors.New("missing github owner or repo")
		}
		url := "github:" + argument(owner) + "/" + argument(repo) + "/" + rev
		if dir != nil && dir != false {
			url += "?dir=" + argument(dir)
		}
		return url, nil
	case "git":
		value, err := field("url")
		if err != nil {
			return "", err
		}
		if value == nil || value == false {
			return "", errors.New("missing git url")
		}
		url, ok := value.(string)
		if !ok {
			return "", errors.New("expected git URL string")
		}
		url = strings.TrimPrefix(url, "git+")
		separator := "?"
		if strings.Contains(url, "?") {
			separator = "&"
		}
		return "git+" + url + separator + "rev=" + rev, nil
	}
	return "", errors.New("unsupported input type")
}

type setPlan struct {
	name     any
	original *orderedLockValue
	url      string
}

// Like lib/confctl/inputs/setter.rb, every successful operation restores all
// selected originals, resolving aliases afresh in supplied order. Later
// selected names win when the new lock aliases their nodes.
func setRestoreOriginals(doc *orderedLockDocument, plans []setPlan, argument func(any) string) error {
	for _, plan := range plans {
		id, err := setResolveNodeID(doc, plan.name, argument)
		if err != nil {
			return err
		}
		value, err := setDig(doc.root, "nodes", id)
		if err != nil {
			return err
		}
		node := setOrObject(value)
		if node.kind != orderedObject {
			return errors.New("expected input object")
		}
		if plan.original == nil || plan.original.kind == orderedNull {
			node.delete("original")
		} else {
			node.set("original", plan.original)
		}
		nodes, err := setDig(doc.root, "nodes")
		if err != nil || nodes == nil || nodes.kind != orderedObject {
			return errors.New("expected input object")
		}
		nodes.set(id, node)
	}
	return nil
}

// Ruby setter.rb uses File.exist? followed by FileUtils.rm_f: Stat follows
// links and every literal-path Stat syscall failure means false. Forced
// nonrecursive Unlink suppresses errors, retaining dangling links and
// directories without replacing the primary operation error.
func setRemoveTemp(path string, ops setOps) {
	if _, err := ops.Stat(path); err == nil {
		_ = ops.Unlink(path)
	}
}

func setApplyInput(ctx context.Context, root, path, temp string, plan setPlan, plans []setPlan, deps SetDeps, ops setOps) error {
	defer setRemoveTemp(temp, ops)
	for extra := false; ; extra = true {
		if err := ctx.Err(); err != nil {
			return err
		}
		argv := []string{"nix"}
		if extra {
			argv = append(argv, "--extra-experimental-features", "nix-command", "--extra-experimental-features", "flakes")
		}
		input := deps.Argument(plan.name)
		argv = append(argv, "flake", "lock", "--update-input", input, "--override-input", input, plan.url, "--output-lock-file", temp)
		out, err := deps.Run(ctx, root, argv)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			var exit *ExitError
			if !extra && errors.As(err, &exit) {
				message := strings.ToLower(exit.Error())
				if strings.Contains(message, "experimental") && (strings.Contains(message, "nix-command") || strings.Contains(message, "flakes")) {
					continue
				}
			}
			return err
		}
		if err := CheckUpdateOutput(out); err != nil {
			return err
		}
		break
	}
	doc, err := setReadDocument(ctx, temp, TempRead, ops)
	if err != nil {
		return err
	}
	if err := setFailure(ctx, Restore, temp, "", setRestoreOriginals(doc, plans, deps.Argument)); err != nil {
		return err
	}
	bytes, err := prettyOrderedLock(doc)
	if err := setFailure(ctx, Format, temp, "", err); err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	if err := setFailure(ctx, TempWrite, temp, "", ops.WriteFile(temp, bytes)); err != nil {
		return err
	}
	return setFailure(ctx, Rename, temp, path, ops.Rename(temp, path))
}

func setDiff(old, next *orderedLockDocument, names []any, argument func(any) string) ([]Change, error) {
	changes := []Change{}
	for _, name := range names {
		if _, err := setInputInfo(old, name, argument); err != nil {
			return nil, err
		}
		if _, err := setInputInfo(next, name, argument); err != nil {
			return nil, err
		}
		before, err := old.lockView()
		if err != nil {
			return nil, err
		}
		after, err := next.lockView()
		if err != nil {
			return nil, err
		}
		one, err := DiffLocks(before, after, []any{name})
		if err != nil {
			return nil, err
		}
		changes = append(changes, one...)
	}
	return changes, nil
}

// Ruby inputs/setter.rb replaces locks sequentially: completed replacements
// survive any later operation or error. There is no rollback.
func setWithOps(ctx context.Context, root string, names []any, rev string, opts UpdateOptions, deps SetDeps, ops setOps) (UpdateResult, error) {
	var result UpdateResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(names) == 0 {
		return result, errors.New("inputs empty")
	}
	path := setPath(root, "flake.lock")
	if err := setPathValid(path); err != nil {
		return result, setFailure(ctx, FirstRead, path, "", err)
	}
	_, err := ops.Stat(path)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, errors.New("flake.lock missing; run confctl inputs update first")
	}
	old, err := setReadDocument(ctx, path, FirstRead, ops)
	if err != nil {
		return result, err
	}
	snapshot, err := setReadDocument(ctx, path, SnapshotRead, ops)
	if err != nil {
		return result, err
	}
	plans := make([]setPlan, 0, len(names))
	for _, name := range names {
		id, err := setResolveNodeID(snapshot, name, deps.Argument)
		if err != nil {
			return result, setFailure(ctx, Validate, path, "", err)
		}
		original, err := setDig(snapshot.root, "nodes", id, "original")
		if err != nil {
			return result, setFailure(ctx, Validate, path, "", err)
		}
		info, err := setInputInfo(old, name, deps.Argument)
		if err != nil {
			return result, setFailure(ctx, Validate, path, "", err)
		}
		if info.info.Type != "github" && info.info.Type != "git" {
			err = fmt.Errorf("input '%s' has unsupported type '%s' for set", deps.Argument(name), deps.Argument(info.info.Type))
			return result, setFailure(ctx, Validate, path, "", err)
		}
		url, err := setOverrideURL(info, rev, deps.Argument)
		if err != nil {
			return result, setFailure(ctx, Validate, path, "", err)
		}
		plans = append(plans, setPlan{name, original.clone(), url})
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Source obtains the PID before SecureRandom.hex; neither reserves TEMP.
		pid := ops.PID()
		bytes, err := ops.Random()
		if err != nil || ctx.Err() != nil {
			return result, setFailure(ctx, TempName, path, "", err)
		}
		temp := setPath(root, fmt.Sprintf("flake.lock.tmp-%d-%x", pid, bytes))
		if err := setApplyInput(ctx, root, path, temp, plan, plans, deps, ops); err != nil {
			return result, err
		}
	}
	next, err := setReadDocument(ctx, path, FinalRead, ops)
	if err != nil {
		return result, err
	}
	result.Changes, err = setDiff(old, next, names, deps.Argument)
	if err = setFailure(ctx, Diff, path, "", err); err != nil || len(result.Changes) == 0 {
		return result, err
	}
	if opts.Commit {
		message, err := buildCommitMessage(ctx, result.Changes, opts.Changelog, opts.Downgrade, deps.Changelog, SetAction)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err != nil {
			return result, err
		}
		err = deps.Commit(ctx, root, message, opts.Editor)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
	return result, nil
}
