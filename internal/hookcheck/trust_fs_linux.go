package hookcheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

const receiptName = "confctl-hooks-trust.json"
const trustLockName = "confctl-hooks-trust.lock"
const linuxPath = 0x200000 // O_PATH, identical on the declared Linux platforms.

func identity(st syscall.Stat_t) FileIdentity {
	return FileIdentity{uint64(st.Dev), st.Ino, uint64(st.Nlink), st.Uid, st.Mode, st.Size,
		st.Mtim.Sec*1e9 + st.Mtim.Nsec, st.Ctim.Sec*1e9 + st.Ctim.Nsec}
}
func fileIdentity(f *os.File) (FileIdentity, error) {
	var st syscall.Stat_t
	err := syscall.Fstat(int(f.Fd()), &st)
	return identity(st), err
}
func sameDirectory(a, b FileIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode && a.UID == b.UID && a.Mode&syscall.S_IFMT == syscall.S_IFDIR && b.Mode&syscall.S_IFMT == syscall.S_IFDIR
}

// All directory components are opened without following symlinks. The handles
// remain anchored even if another writer moves a pathname; rechecks detect that.
func openDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || filepath.Clean(path) != path {
		return nil, refuse("stale-identity", path)
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		closeErr := syscall.Close(fd)
		if e != nil {
			return nil, errors.Join(e, closeErr)
		}
		if closeErr != nil {
			return nil, errors.Join(closeErr, syscall.Close(next))
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func openAt(d *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Openat(int(d.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func atIdentity(d *os.File, name string) (id FileIdentity, err error) {
	f, err := openAt(d, name, linuxPath, 0)
	if err != nil {
		return id, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return fileIdentity(f)
}
func readAt(d *os.File, name string, private bool) (data []byte, id FileIdentity, err error) {
	f, err := openAt(d, name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, id, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	id, err = fileIdentity(f)
	if err != nil {
		return nil, id, err
	}
	if id.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, id, refuse("unsupported-policy", name)
	}
	if private && (id.Mode&07777 != 0600 || id.UID != uint32(os.Geteuid()) || id.Links != 1) {
		return nil, id, refuse("invalid-receipt", name)
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, id, err
	}
	after, err := fileIdentity(f)
	if err != nil {
		return nil, id, err
	}
	current, err := atIdentity(d, name)
	if err != nil {
		return nil, id, err
	}
	if id != after || id != current {
		return nil, id, refuse("stale-identity", name)
	}
	return data, id, nil
}
func readOwnedRegular(path string, private bool) (b []byte, id FileIdentity, err error) {
	d, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return nil, id, err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	return readAt(d, filepath.Base(path), private)
}

type localTrustDirectory struct {
	root, admin     *os.File
	rootID, adminID FileIdentity
	id              RepositoryIdentity
	// Named syscall boundary faults are package-private and only used by tests.
	fault func(string) error
}

func (d *localTrustDirectory) checkpoint(name string) error {
	if d.fault != nil {
		return d.fault(name)
	}
	return nil
}
func (d *localTrustDirectory) Identity() RepositoryIdentity {
	i := d.id
	i.Root = bytes.Clone(i.Root)
	i.Admin = bytes.Clone(i.Admin)
	i.ResolvedRoot = bytes.Clone(i.ResolvedRoot)
	i.ResolvedAdmin = bytes.Clone(i.ResolvedAdmin)
	return i
}
func (d *localTrustDirectory) recheck() (err error) {
	for _, pair := range []struct {
		path []byte
		id   FileIdentity
	}{{d.id.Root, d.rootID}, {d.id.Admin, d.adminID}} {
		f, e := openDirectory(string(pair.path))
		if e != nil {
			return e
		}
		now, e := fileIdentity(f)
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		if !sameDirectory(pair.id, now) {
			return refuse("stale-identity", string(pair.path))
		}
	}
	return nil
}
func (d *localTrustDirectory) Close() error {
	var err error
	if d.root != nil {
		err = errors.Join(err, d.root.Close())
		d.root = nil
	}
	if d.admin != nil {
		err = errors.Join(err, d.admin.Close())
		d.admin = nil
	}
	return err
}
func localOpen(root, admin string) (_ TrustDirectory, err error) {
	r, err := openDirectory(root)
	if err != nil {
		return nil, err
	}
	d := &localTrustDirectory{root: r}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.Close())
		}
	}()
	d.admin, err = openDirectory(admin)
	if err != nil {
		return nil, err
	}
	d.rootID, err = fileIdentity(r)
	if err != nil {
		return nil, err
	}
	d.adminID, err = fileIdentity(d.admin)
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Geteuid())
	if d.rootID.UID != uid || d.adminID.UID != uid {
		return nil, refuse("stale-identity", admin)
	}
	d.id = RepositoryIdentity{[]byte(root), []byte(admin), []byte(root), []byte(admin), d.rootID.Device, d.rootID.Inode, d.adminID.Device, d.adminID.Inode, uid}
	return d, nil
}
func (d *localTrustDirectory) WorkingPolicy(name string) (PolicyImage, error) {
	ok := false
	for _, n := range policySlots {
		if n == name {
			ok = true
		}
	}
	if !ok {
		return PolicyImage{}, refuse("unsupported-policy", name)
	}
	if err := d.recheck(); err != nil {
		return PolicyImage{}, err
	}
	b, id, err := readAt(d.root, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return PolicyImage{}, nil
	}
	if err != nil {
		return PolicyImage{}, err
	}
	mode := policyMode(id.Mode)
	return PolicyImage{true, mode, b, id}, nil
}
func (d *localTrustDirectory) ExtraRunnerConfigs() ([]string, error) {
	if err := d.recheck(); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range extraNames() {
		_, err := atIdentity(d.root, n)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
func (d *localTrustDirectory) ReadReceipt() (ReceiptImage, error) {
	if err := d.recheck(); err != nil {
		return ReceiptImage{}, err
	}
	b, id, err := readAt(d.admin, receiptName, true)
	if errors.Is(err, os.ErrNotExist) {
		return ReceiptImage{}, nil
	}
	if err != nil {
		return ReceiptImage{}, err
	}
	if _, err = parseReceipt(b); err != nil {
		return ReceiptImage{}, err
	}
	return ReceiptImage{true, b, id}, nil
}
func (d *localTrustDirectory) removeOwned(name string, id FileIdentity) error {
	now, err := atIdentity(d.admin, name)
	if err != nil {
		return err
	}
	if now != id {
		return refuse("stale-identity", name)
	}
	return syscall.Unlinkat(int(d.admin.Fd()), name)
}

// Publish never repairs a stale lock or receipt. Published records the rename
// point even when later durability/cleanup/cancellation reports an error.
func (d *localTrustDirectory) Publish(ctx context.Context, old ReceiptImage, data []byte) (out Publication, err error) {
	if _, err = parseReceipt(data); err != nil {
		return out, err
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = d.recheck(); err != nil {
		return out, err
	}
	if err = d.checkpoint("lock-create"); err != nil {
		return out, err
	}
	lock, err := openAt(d.admin, trustLockName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		return out, err
	}
	lockID, err := fileIdentity(lock)
	if err != nil {
		return out, errors.Join(err, lock.Close())
	}
	var tempName string
	var tempID FileIdentity
	defer func() {
		if tempName != "" {
			e := d.checkpoint("cleanup-temp")
			if e == nil {
				e = d.removeOwned(tempName, tempID)
			}
			err = errors.Join(err, e)
		}
		err = errors.Join(err, lock.Close())
		e := d.checkpoint("cleanup-lock")
		if e == nil {
			e = d.removeOwned(trustLockName, lockID)
		}
		err = errors.Join(err, e)
	}()
	preimage := func() error {
		if e := d.recheck(); e != nil {
			return e
		}
		now, e := d.ReadReceipt()
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(now, old) {
			return refuse("stale-identity", receiptName)
		}
		lockNow, e := atIdentity(d.admin, trustLockName)
		if e != nil {
			return e
		}
		if lockNow != lockID {
			return refuse("stale-identity", trustLockName)
		}
		return ctx.Err()
	}
	if err = preimage(); err != nil {
		return out, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return out, err
	}
	name := "confctl-hooks-trust.tmp-" + hex.EncodeToString(nonce[:])
	if err = d.checkpoint("temp-create"); err != nil {
		return out, err
	}
	f, err := openAt(d.admin, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		return out, err
	}
	tempID, err = fileIdentity(f)
	if err != nil {
		return out, errors.Join(err, f.Close())
	}
	tempName = name
	writeErr := d.checkpoint("write")
	if writeErr == nil {
		var n int
		n, writeErr = f.Write(data)
		if writeErr == nil && n != len(data) {
			writeErr = io.ErrShortWrite
		}
	}
	if writeErr == nil {
		writeErr = d.checkpoint("file-sync")
		if writeErr == nil {
			writeErr = f.Sync()
		}
	}
	afterWrite, idErr := fileIdentity(f)
	if idErr == nil {
		tempID = afterWrite
	}
	closeErr := errors.Join(d.checkpoint("file-close"), f.Close())
	if err = errors.Join(writeErr, idErr, closeErr); err != nil {
		return out, err
	}
	if err = d.checkpoint("pre-rename"); err != nil {
		return out, err
	}
	if err = preimage(); err != nil {
		return out, err
	}
	current, e := atIdentity(d.admin, name)
	if e != nil {
		return out, e
	}
	if current != tempID || current.Links != 1 {
		return out, refuse("stale-identity", name)
	}
	if err = d.checkpoint("rename"); err != nil {
		return out, err
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = syscall.Renameat(int(d.admin.Fd()), name, int(d.admin.Fd()), receiptName); err != nil {
		return out, err
	}
	out.Published = true
	tempName = ""
	err = d.checkpoint("dir-sync")
	if err == nil {
		err = d.admin.Sync()
	}
	err = errors.Join(err, ctx.Err())
	return out, err
}

func identifyLocalTool(ctx context.Context, t ToolIdentity) (out ToolIdentity, err error) {
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if !storeProgram.MatchString(t.Program) || !storeProgram.MatchString(t.Resolved) || filepath.Clean(t.Output) != t.Output || !strings.HasPrefix(t.Output, "/nix/store/") {
		return out, refuse("stale-identity", t.Program)
	}
	output, err := openDirectory(t.Output)
	if err != nil {
		return out, err
	}
	if err = output.Close(); err != nil {
		return out, err
	}
	rel, err := filepath.Rel(t.Output, t.Program)
	if err != nil || !validPath(rel) {
		return out, refuse("stale-identity", t.Program)
	}
	// Reuse the existing component-wise confined resolver: every symlink target
	// must stay inside the declared output, including physical symlink/.. paths.
	resolved, err := (&Snapshot{dir: t.Output}).confinedPath(rel)
	if err != nil {
		return out, err
	}
	if resolved != t.Resolved || !strings.HasPrefix(resolved, t.Output+"/") {
		return out, refuse("stale-identity", t.Program)
	}
	b, id, err := readOwnedRegular(resolved, false)
	if err != nil {
		return out, err
	}
	if id.Mode&0111 == 0 || sha256.Sum256(b) != t.SHA256 {
		return out, refuse("stale-identity", t.Program)
	}
	return t, nil
}

// LocalTrustIO selects Linux receipt and immutable-tool operations. Executable
// dispatch and child lifetime remain the caller's responsibility.
func LocalTrustIO() TrustIO { return TrustIO{Open: localOpen, IdentifyTool: identifyLocalTool} }

var _ TrustDirectory = (*localTrustDirectory)(nil)
