// Package hookcheck contains private staged-content checks. It installs no hooks.
package hookcheck

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Invocation is data passed to the caller's fixed executable adapter, never shell text.
type Invocation struct {
	Program string
	Args    []string
	Dir     string
	Env     []string
	Stdin   []byte
}

// ToolResult distinguishes an ordinary tool status from a spawn/wait/cancellation error.
type ToolResult struct {
	Stdout, Stderr []byte
	ExitCode       int
}

type Invoke func(context.Context, Invocation) (ToolResult, error)

type CaptureOptions struct {
	Dir, Git, TempParent string
	Env                  []string
	Invoke               Invoke
}

type Entry struct {
	Path, ObjectID string
	Mode           uint32
}

// Snapshot holds one staged revision. Its private metadata never aliases caller slices.
type Snapshot struct {
	dir, origin, gitDir, indexFile string
	entries                        []Entry
	selected, headPaths            []string
	env                            []string
	config                         map[string]string
	owned                          os.FileInfo
	remove                         func(string) error
}

func (s *Snapshot) Directory() string    { return s.dir }
func (s *Snapshot) Origin() string       { return s.origin }
func (s *Snapshot) GitDirectory() string { return s.gitDir }
func (s *Snapshot) IndexFile() string    { return s.indexFile }
func (s *Snapshot) Entries() []Entry     { return append([]Entry(nil), s.entries...) }
func (s *Snapshot) Selected() []string   { return append([]string(nil), s.selected...) }

// Close touches only the directory created by Capture, without following child symlinks.
// If cleanup also fails, errors.Is still identifies the primary error.
func (s *Snapshot) Close(primary error) error {
	if s.dir == "" {
		return primary
	}
	info, err := os.Lstat(s.dir)
	if err == nil && (!info.IsDir() || !os.SameFile(info, s.owned)) {
		err = fmt.Errorf("snapshot directory identity changed: %q", s.dir)
	}
	if err == nil {
		err = s.remove(s.dir)
	}
	if err != nil {
		return errors.Join(primary, fmt.Errorf("snapshot cleanup: %w", err))
	}
	s.dir = ""
	return primary
}

type indexImage struct {
	info os.FileInfo
	data []byte
}

func readIndex(path string) (indexImage, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return indexImage{}, nil
	}
	if err != nil {
		return indexImage{}, err
	}
	if !info.Mode().IsRegular() {
		return indexImage{}, fmt.Errorf("index is not a regular file: %q", path)
	}
	data, err := os.ReadFile(path)
	return indexImage{info, data}, err
}

func (a indexImage) equal(b indexImage) bool {
	if a.info == nil || b.info == nil {
		return a.info == nil && b.info == nil
	}
	return os.SameFile(a.info, b.info) && a.info.Mode() == b.info.Mode() && bytes.Equal(a.data, b.data)
}

func envSet(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, pair := range env {
		if !strings.HasPrefix(pair, key+"=") {
			out = append(out, pair)
		}
	}
	return append(out, key+"="+value)
}

// Capture reads Git in the original CWD/environment, including literal alternate-index
// context. It never writes a tree, index, ref, hook, stash, or checkout.
func Capture(ctx context.Context, o CaptureOptions) (_ *Snapshot, err error) {
	if o.Git == "" || o.Invoke == nil {
		return nil, errors.New("Git executable and invocation adapter are required")
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	env := envSet(envSet(o.Env, "GIT_OPTIONAL_LOCKS", "0"), "GIT_LITERAL_PATHSPECS", "1")
	// Index object IDs identify raw staged bytes, even with ambient replace refs.
	env = envSet(env, "GIT_NO_REPLACE_OBJECTS", "1")
	run := func(args ...string) (ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		return o.Invoke(ctx, Invocation{o.Git, args, dir, append([]string(nil), env...), nil})
	}
	read := func(args ...string) ([]byte, error) {
		r, err := run(args...)
		if err != nil {
			return nil, err
		}
		if r.ExitCode != 0 {
			return nil, fmt.Errorf("git %s exited %d: %s", args[0], r.ExitCode, r.Stderr)
		}
		return r.Stdout, nil
	}
	path := func(args ...string) (string, error) {
		b, err := read(args...)
		return strings.TrimSuffix(string(b), "\n"), err
	}
	root, err := path("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	gitDir, err := path("rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	index, err := path("rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(gitDir) || !filepath.IsAbs(index) {
		return nil, errors.New("Git returned a non-absolute repository/index identity")
	}
	before, err := readIndex(index)
	if err != nil {
		return nil, err
	}
	head, err := run("rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil || (head.ExitCode != 0 && head.ExitCode != 1) {
		return nil, errors.Join(err, fmt.Errorf("cannot identify HEAD (status %d): %s", head.ExitCode, head.Stderr))
	}
	// Keep the caller's CWD/index binding while enumerating the entire root in
	// root-relative coordinates. The absolute directory pathspec stays literal.
	raw, err := read("ls-files", "--full-name", "--stage", "-z", "--", root)
	if err != nil {
		return nil, err
	}
	entries, err := parseEntries(raw)
	if err != nil {
		return nil, err
	}
	diff, err := read("diff", "--cached", "--no-relative", "--name-status", "-z", "--diff-filter=ACMRT", "--no-ext-diff", "--no-textconv", "-M", "-C", "--", root)
	if err != nil {
		return nil, err
	}
	selected, err := parseDestinations(diff, entries)
	if err != nil {
		return nil, err
	}
	var headPaths []string
	if head.ExitCode == 0 {
		raw, err := read("ls-tree", "-rz", "--full-tree", strings.TrimSuffix(string(head.Stdout), "\n"))
		if err != nil {
			return nil, err
		}
		headPaths, err = parseHeadPaths(raw)
		if err != nil {
			return nil, err
		}
	}
	config := make(map[string]string)
	for _, key := range []string{"user.email", "user.name"} {
		r, err := run("config", "--get", key)
		if err != nil || (r.ExitCode != 0 && r.ExitCode != 1) {
			return nil, errors.Join(err, fmt.Errorf("cannot read %s: %s", key, r.Stderr))
		}
		if r.ExitCode == 0 {
			config[key] = strings.TrimSuffix(strings.TrimSuffix(string(r.Stdout), "\n"), "\r")
		}
	}
	var input bytes.Buffer
	for _, e := range entries {
		if e.Mode != 0160000 {
			fmt.Fprintln(&input, e.ObjectID)
		}
	}
	r, err := o.Invoke(ctx, Invocation{o.Git, []string{"cat-file", "--batch"}, dir, append([]string(nil), env...), input.Bytes()})
	if err != nil || r.ExitCode != 0 {
		return nil, errors.Join(err, fmt.Errorf("cannot read staged blobs: %s", r.Stderr))
	}
	blobs, err := parseBlobs(r.Stdout, entries)
	if err != nil {
		return nil, err
	}
	tempParent := o.TempParent
	if tempParent == "" {
		tempParent = os.TempDir()
	}
	tempParent, err = filepath.Abs(tempParent)
	if err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp(tempParent, "confctl-hookcheck-")
	if err != nil {
		return nil, err
	}
	owned, err := os.Lstat(temp)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(temp))
	}
	s := &Snapshot{dir: temp, origin: root, gitDir: gitDir, indexFile: index,
		entries: entries, selected: selected, headPaths: headPaths,
		env: append([]string(nil), o.Env...), config: config, owned: owned, remove: os.RemoveAll}
	defer func() {
		if err != nil {
			err = s.Close(err)
		}
	}()
	// Parent collision validation precedes creation, so no staged symlink is followed.
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if e.Mode == 0160000 {
			continue
		}
		p := filepath.Join(temp, e.Path)
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return nil, err
		}
		if e.Mode == 0120000 {
			err = os.Symlink(string(blobs[e.Path]), p)
		} else {
			mode := fs.FileMode(0644)
			if e.Mode == 0100755 {
				mode = 0755
			}
			err = os.WriteFile(p, blobs[e.Path], mode)
			if err == nil {
				err = os.Chmod(p, mode)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	after, err := readIndex(index)
	if err != nil {
		return nil, err
	}
	if !before.equal(after) {
		return nil, errors.New("Git index changed while collecting staged snapshot")
	}
	headAfter, err := run("rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return nil, err
	}
	if headAfter.ExitCode != head.ExitCode || !bytes.Equal(headAfter.Stdout, head.Stdout) {
		return nil, errors.New("HEAD changed while collecting staged snapshot")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func nulRecords(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, errors.New("unterminated Git NUL records")
	}
	return strings.Split(string(raw[:len(raw)-1]), "\x00"), nil
}

func validPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func parseEntries(raw []byte) ([]Entry, error) {
	records, err := nulRecords(raw)
	if err != nil {
		return nil, err
	}
	var out []Entry
	seen := make(map[string]bool)
	for _, record := range records {
		meta, name, ok := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 3 || !validPath(name) {
			return nil, fmt.Errorf("invalid staged entry: %q", record)
		}
		if parts[2] != "0" {
			return nil, fmt.Errorf("unmerged index entry: %q", name)
		}
		mode, err := strconv.ParseUint(parts[0], 8, 32)
		if err != nil || (mode != 0100644 && mode != 0100755 && mode != 0120000 && mode != 0160000) {
			return nil, fmt.Errorf("unsupported staged mode: %q", parts[0])
		}
		if _, err := hex.DecodeString(parts[1]); err != nil || (len(parts[1]) != 40 && len(parts[1]) != 64) {
			return nil, errors.New("invalid staged object ID")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate index path: %q", name)
		}
		seen[name] = true
		out = append(out, Entry{name, parts[1], uint32(mode)})
	}
	for name := range seen {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if seen[parent] {
				return nil, fmt.Errorf("staged parent is a file or symlink: %q", parent)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func parseDestinations(raw []byte, entries []Entry) ([]string, error) {
	records, err := nulRecords(raw)
	if err != nil {
		return nil, err
	}
	staged := make(map[string]Entry)
	for _, e := range entries {
		staged[e.Path] = e
	}
	selected := make(map[string]bool)
	for i := 0; i < len(records); {
		status := records[i]
		i++
		if status == "" || !strings.ContainsRune("ACMRT", rune(status[0])) {
			return nil, fmt.Errorf("unexpected cached status: %q", status)
		}
		count := 1
		if status[0] == 'R' || status[0] == 'C' {
			count = 2
		}
		if i+count > len(records) {
			return nil, errors.New("truncated cached destination")
		}
		name := records[i+count-1]
		i += count
		e, ok := staged[name]
		if !ok {
			return nil, fmt.Errorf("cached destination missing from index: %q", name)
		}
		// Only regular module manifests need a new type-change destination:
		// HEAD stores paths without old modes, so symlink -> regular must reach
		// dependency preflight. Other type changes stay out of formatter input.
		if status[0] == 'T' && (!moduleManifest(name) || e.Mode != 0100644 && e.Mode != 0100755) {
			continue
		}
		if e.Mode != 0160000 {
			selected[name] = true
		}
	}
	var out []string
	for name := range selected {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func parseHeadPaths(raw []byte) ([]string, error) {
	records, err := nulRecords(raw)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, record := range records {
		meta, name, ok := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 3 || !validPath(name) {
			return nil, errors.New("invalid HEAD tree entry")
		}
		if parts[1] == "blob" {
			out = append(out, name)
		}
	}
	return out, nil
}

func parseBlobs(raw []byte, entries []Entry) (map[string][]byte, error) {
	out := make(map[string][]byte)
	for _, e := range entries {
		if e.Mode == 0160000 {
			continue
		}
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			return nil, errors.New("truncated blob header")
		}
		parts := strings.Fields(string(raw[:end]))
		if len(parts) != 3 || parts[0] != e.ObjectID || parts[1] != "blob" {
			return nil, fmt.Errorf("invalid blob response for %q", e.Path)
		}
		n, err := strconv.Atoi(parts[2])
		raw = raw[end+1:]
		if err != nil || n < 0 || n >= len(raw) || raw[n] != '\n' {
			return nil, fmt.Errorf("truncated blob for %q", e.Path)
		}
		out[e.Path] = bytes.Clone(raw[:n])
		raw = raw[n+1:]
	}
	if len(raw) != 0 {
		return nil, errors.New("extra blob response bytes")
	}
	return out, nil
}

// confinedPath follows staged symlinks component by component. It rejects an
// escape before touching an outside component; symlink/.. traversal stays physical.
func (s *Snapshot) confinedPath(name string) (string, error) {
	if !validPath(name) {
		return "", fmt.Errorf("unsafe selected path: %q", name)
	}
	pending, stack, links := strings.Split(name, "/"), []string{}, 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(stack) == 0 {
				return "", fmt.Errorf("selected symlink escapes snapshot: %q", name)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		p := filepath.Join(append([]string{s.dir}, append(stack, part)...)...)
		info, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", fmt.Errorf("symlink loop: %q", name)
			}
			target, err := os.Readlink(p)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				return "", fmt.Errorf("absolute selected symlink: %q", name)
			}
			pending = append(strings.Split(target, "/"), pending...)
		} else {
			if len(pending) > 0 && !info.IsDir() {
				return "", fmt.Errorf("selected path component is not a directory: %q", name)
			}
			stack = append(stack, part)
		}
	}
	return filepath.Join(append([]string{s.dir}, stack...)...), nil
}
