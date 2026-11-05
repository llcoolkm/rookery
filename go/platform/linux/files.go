//go:build linux

package linux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
)

var errUnsafe = errors.New("unsafe local object")

// The mounted filesystem root is the trust anchor (it may be mapped in a user namespace).
var rootUID = func() uint32 {
	var st syscall.Stat_t
	if syscall.Stat("/", &st) != nil {
		return 0
	}
	return st.Uid
}()

// files uses directory descriptors for every traversal and mutation, so a
// concurrent rename or symlink substitution cannot escape the selected root.
type files struct {
	mu     sync.Mutex
	opened map[*verifiedFile]bool
}
type verifiedFile struct {
	*os.File
	host     *files
	root     string
	relative string
}

func (f *verifiedFile) Close() error {
	f.host.mu.Lock()
	delete(f.host.opened, f)
	f.host.mu.Unlock()
	return f.File.Close()
}
func safeStat(st *syscall.Stat_t, private bool, directory bool) bool {
	uid := uint32(os.Geteuid())
	if st.Uid != uid && (!directory || st.Uid != rootUID) {
		return false
	}
	if private {
		return st.Uid == uid && st.Mode&0077 == 0
	}
	return st.Mode&0022 == 0 || (directory && st.Uid == rootUID && st.Mode&syscall.S_ISVTX != 0)
}
func dirOpen(path string, private bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, errUnsafe
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, errUnsafe
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if e != nil {
			return -1, errUnsafe
		}
		fd = next
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil || !safeStat(&st, false, true) {
			syscall.Close(fd)
			return -1, errUnsafe
		}
	}
	if private {
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil || !safeStat(&st, true, true) {
			syscall.Close(fd)
			return -1, errUnsafe
		}
	}
	return fd, nil
}
func validRelative(name string) bool {
	return name != "" && !filepath.IsAbs(name) && filepath.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}
func parentOpen(root, name string) (int, string, error) {
	if !validRelative(name) {
		return -1, "", errUnsafe
	}
	fd, err := dirOpen(root, false)
	if err != nil {
		return -1, "", err
	}
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if e != nil {
			return -1, "", errUnsafe
		}
		fd = next
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil || !safeStat(&st, false, true) {
			syscall.Close(fd)
			return -1, "", errUnsafe
		}
	}
	return fd, parts[len(parts)-1], nil
}
func (f *files) PrivateState(ctx context.Context, path string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", errUnsafe
	}
	// Build missing directories one component at a time without following links.
	fd, e := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return "", errUnsafe
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, p := range parts {
		if p == "" {
			continue
		}
		next, e := syscall.Openat(fd, p, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if e == syscall.ENOENT {
			if e = syscall.Mkdirat(fd, p, 0700); e == nil {
				e = syscall.Fsync(fd)
			}
			if e == nil {
				next, e = syscall.Openat(fd, p, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			}
		}
		syscall.Close(fd)
		if e != nil {
			return "", errUnsafe
		}
		fd = next
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil || !safeStat(&st, false, true) {
			syscall.Close(fd)
			return "", errUnsafe
		}
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || !safeStat(&st, true, true) {
		return "", errUnsafe
	}
	return path, nil
}
func (f *files) EnsurePrivate(ctx context.Context, path string, directory bool) error {
	if directory {
		fd, e := dirOpen(path, true)
		if e == nil {
			syscall.Close(fd)
		}
		return e
	}
	root, name := filepath.Split(path)
	v, e := f.OpenNoFollow(ctx, filepath.Clean(root), name, false)
	if e != nil {
		return e
	}
	return v.Close()
}
func (f *files) OpenNoFollow(ctx context.Context, root, name string, create bool) (platform.File, error) {
	return f.open(ctx, root, name, create, true)
}

// OpenProviderRecord permits provider-readable records, but never writable-by-others records.
func (f *files) OpenProviderRecord(ctx context.Context, root, name string) (platform.File, error) {
	return f.open(ctx, root, name, false, false)
}
func (f *files) open(ctx context.Context, root, name string, create, private bool) (platform.File, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	fd, base, e := parentOpen(root, name)
	if e != nil {
		return nil, e
	}
	defer syscall.Close(fd)
	if create {
		var directory syscall.Stat_t
		if syscall.Fstat(fd, &directory) != nil || !safeStat(&directory, true, true) {
			return nil, errUnsafe
		}
	}
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= syscall.O_CREAT | syscall.O_RDWR
	}
	n, e := syscall.Openat(fd, base, flags, 0600)
	if e == syscall.ENOENT {
		return nil, fs.ErrNotExist
	}
	if e != nil {
		return nil, errUnsafe
	}
	var st syscall.Stat_t
	if syscall.Fstat(n, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || !safeStat(&st, private, false) || st.Nlink != 1 {
		syscall.Close(n)
		return nil, errUnsafe
	}
	v := &verifiedFile{File: os.NewFile(uintptr(n), base), host: f, root: root, relative: name}
	f.mu.Lock()
	f.opened[v] = true
	f.mu.Unlock()
	return v, nil
}
func (f *files) AtomicWrite(ctx context.Context, root, name string, data []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, e := dirOpenChecked(root, true); e != nil {
		return e
	}
	fd, base, e := parentOpen(root, name)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	var directory syscall.Stat_t
	if syscall.Fstat(fd, &directory) != nil || !safeStat(&directory, true, true) {
		return errUnsafe
	}
	// Refuse an existing unsafe destination before replacement.
	old, e := syscall.Openat(fd, base, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if e == nil {
		var st syscall.Stat_t
		e = syscall.Fstat(old, &st)
		syscall.Close(old)
		if e != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || !safeStat(&st, true, false) || st.Nlink != 1 {
			return errUnsafe
		}
	} else if e != syscall.ENOENT {
		return errUnsafe
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		return errors.New("local storage unavailable")
	}
	tmp := ".write-" + hex.EncodeToString(random[:])
	n, e := syscall.Openat(fd, tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return errUnsafe
	}
	defer syscall.Unlinkat(fd, tmp)
	out := os.NewFile(uintptr(n), tmp)
	_, e = out.Write(data)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return errors.New("local storage unavailable")
	}
	if e = syscall.Renameat(fd, tmp, fd, base); e != nil {
		return errors.New("local storage unavailable")
	}
	// Rename committed the record. A directory durability failure must not invite
	// replay of an operation that is already visible locally.
	if syscall.Fsync(fd) != nil {
		return platform.ErrCommittedNotDurable
	}
	return nil
}
func dirOpenChecked(root string, private bool) (bool, error) {
	fd, e := dirOpen(root, private)
	if e == nil {
		syscall.Close(fd)
	}
	return e == nil, e
}
func (f *files) List(ctx context.Context, root string) ([]platform.Entry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	fd, e := dirOpen(root, false)
	if e != nil {
		return nil, e
	}
	d := os.NewFile(uintptr(fd), "directory")
	defer d.Close()
	entries, e := d.ReadDir(-1)
	if e != nil {
		return nil, errUnsafe
	}
	out := make([]platform.Entry, 0, len(entries))
	for _, v := range entries {
		child, e := syscall.Openat(fd, v.Name(), 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if e != nil {
			return nil, errUnsafe
		}
		var st syscall.Stat_t
		e = syscall.Fstat(child, &st)
		syscall.Close(child)
		if e != nil {
			return nil, errUnsafe
		}
		if st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			continue
		}
		out = append(out, platform.Entry{Name: v.Name(), Size: st.Size, Directory: st.Mode&syscall.S_IFMT == syscall.S_IFDIR})
	}
	return out, nil
}
func (f *files) Remove(ctx context.Context, root, name string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	v, e := f.OpenNoFollow(ctx, root, name, false)
	if e != nil {
		return e
	}
	v.Close()
	fd, base, e := parentOpen(root, name)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	if e = syscall.Unlinkat(fd, base); e == nil {
		e = syscall.Fsync(fd)
	}
	return e
}

type lock struct{ platform.File }

func (f *files) AdvisoryLock(ctx context.Context, root, name string, wait bool) (platform.Lock, error) {
	v, e := f.OpenNoFollow(ctx, root, name, true)
	if e != nil {
		return nil, e
	}
	real := v.(*verifiedFile)
	flags := syscall.LOCK_EX | syscall.LOCK_NB
	for {
		e = syscall.Flock(int(real.Fd()), flags)
		if e == nil {
			return &lock{v}, nil
		}
		if !wait || e != syscall.EWOULDBLOCK {
			v.Close()
			return nil, errors.New("local lock unavailable")
		}
		select {
		case <-ctx.Done():
			v.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

var _ io.ReadWriteSeeker = (*verifiedFile)(nil)

// AtomicWriteExclusive commits a complete private file only if the destination
// does not exist. linkat is the atomic exclusive commit: an earlier backup is
// never replaced, including when another process races backup creation.
func (f *files) AtomicWriteExclusive(ctx context.Context, root, name string, data []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fd, base, e := parentOpen(root, name)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	var directory syscall.Stat_t
	if syscall.Fstat(fd, &directory) != nil || !safeStat(&directory, true, true) {
		return errUnsafe
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		return errors.New("local storage unavailable")
	}
	tmp := ".write-" + hex.EncodeToString(random[:])
	n, e := syscall.Openat(fd, tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return errUnsafe
	}
	defer syscall.Unlinkat(fd, tmp)
	out := os.NewFile(uintptr(n), tmp)
	_, e = out.Write(data)
	if e == nil {
		e = out.Sync()
	}
	closeError := out.Close()
	if e == nil {
		e = closeError
	}
	if e != nil {
		return errors.New("local storage unavailable")
	}
	source, e := syscall.BytePtrFromString(tmp)
	if e != nil {
		return errUnsafe
	}
	target, e := syscall.BytePtrFromString(base)
	if e != nil {
		return errUnsafe
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, uintptr(fd), uintptr(unsafe.Pointer(source)), uintptr(fd), uintptr(unsafe.Pointer(target)), 0, 0)
	if errno != 0 {
		if errno == syscall.EEXIST {
			return fs.ErrExist
		}
		return errors.New("local storage unavailable")
	}
	if syscall.Unlinkat(fd, tmp) != nil || syscall.Fsync(fd) != nil {
		return errors.New("local storage unavailable")
	}
	return nil
}

// recordProvenance revalidates the handle's original pathname by descriptor,
// rejecting renamed, replaced or symlinked records and changed ancestry.
func recordProvenance(record *verifiedFile) error {
	fd, base, e := parentOpen(record.root, record.relative)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	pathFD, e := syscall.Openat(fd, base, 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return errUnsafe
	}
	defer syscall.Close(pathFD)
	var original, current syscall.Stat_t
	if syscall.Fstat(int(record.Fd()), &original) != nil || syscall.Fstat(pathFD, &current) != nil || original.Dev != current.Dev || original.Ino != current.Ino || current.Mode&syscall.S_IFMT != syscall.S_IFREG || !safeStat(&current, false, false) || current.Nlink != 1 {
		return errUnsafe
	}
	return nil
}
