//go:build linux

package linux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const maxWSLConfig = 1 << 20

func (s *service) wslTarget() string {
	if s.wslConfigPath != "" {
		return s.wslConfigPath
	}
	return "/etc/wsl.conf"
}
func (s *service) wslDirectory(write bool) (int, string, error) {
	if write && s.wslConfigPath == "" && os.Geteuid() != 0 {
		return -1, "", errors.New("WSL boot installation requires root; use sudo with explicit --user, --executable and --state-dir")
	}
	target := s.wslTarget()
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return -1, "", errUnsafe
	}
	fd, e := dirOpen(filepath.Dir(target), false)
	if e != nil {
		return -1, "", errUnsafe
	}
	return fd, filepath.Base(target), nil
}
func readWSLFile(fd int, name string) ([]byte, os.FileMode, error) {
	raw, e := syscall.Openat(fd, name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e == syscall.ENOENT {
		return nil, 0644, nil
	}
	if e != nil {
		return nil, 0, errUnsafe
	}
	file := os.NewFile(uintptr(raw), "WSL configuration")
	defer file.Close()
	var st syscall.Stat_t
	if syscall.Fstat(raw, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || st.Mode&0022 != 0 || (st.Uid != rootUID && st.Uid != uint32(os.Geteuid())) {
		return nil, 0, errUnsafe
	}
	b, e := io.ReadAll(io.LimitReader(file, maxWSLConfig+1))
	if e != nil || len(b) > maxWSLConfig {
		return nil, 0, errors.New("WSL configuration exceeds limit")
	}
	return b, os.FileMode(st.Mode & 0777), nil
}
func atomicWSLFile(fd int, name string, data []byte, mode os.FileMode, exclusive bool) error {
	var random [16]byte
	if _, e := rand.Read(random[:]); e != nil {
		return errors.New("WSL configuration unavailable")
	}
	temporary := ".rookery-go-" + hex.EncodeToString(random[:])
	raw, e := syscall.Openat(fd, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return errUnsafe
	}
	file := os.NewFile(uintptr(raw), "WSL replacement")
	defer file.Close()
	defer syscall.Unlinkat(fd, temporary)
	if _, e = file.Write(data); e != nil {
		return errors.New("WSL configuration unavailable")
	}
	if e = file.Chmod(mode); e != nil {
		return errUnsafe
	}
	if e = file.Sync(); e != nil {
		return errors.New("WSL configuration unavailable")
	}
	if exclusive {
		// linkat installs a first-write backup without ever overwriting it.
		if e = linkAt(fd, temporary, name); e != nil {
			return e
		}
	} else if e = syscall.Renameat(fd, temporary, fd, name); e != nil {
		return errors.New("WSL configuration unavailable")
	}
	if syscall.Fsync(fd) != nil {
		return errors.New("WSL configuration durability unavailable")
	}
	return nil
}

// linkAt uses the same descriptor-relative helper as secure private files.
func linkAt(fd int, old, new string) error {
	before, e := syscall.BytePtrFromString(old)
	if e != nil {
		return errUnsafe
	}
	after, e := syscall.BytePtrFromString(new)
	if e != nil {
		return errUnsafe
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, uintptr(fd), uintptr(unsafe.Pointer(before)), uintptr(fd), uintptr(unsafe.Pointer(after)), 0, 0)
	if errno != 0 {
		return errors.New("WSL backup unavailable")
	}
	return nil
}
func (s *service) mutateWSL(change func(string) string) error {
	fd, name, e := s.wslDirectory(true)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	lock, e := syscall.Openat(fd, ".rookery-go-wsl.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return errUnsafe
	}
	defer syscall.Close(lock)
	var st syscall.Stat_t
	if syscall.Fstat(lock, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 || st.Nlink != 1 {
		return errUnsafe
	}
	if syscall.Flock(lock, syscall.LOCK_EX) != nil {
		return errors.New("WSL configuration lock unavailable")
	}
	defer syscall.Flock(lock, syscall.LOCK_UN)
	original, mode, e := readWSLFile(fd, name)
	if e != nil {
		return e
	}
	updated := change(string(original))
	if updated == string(original) {
		return nil
	}
	backup := name + ".rookery-go.bak"
	existing, e := syscall.Openat(fd, backup, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e == nil {
		var info syscall.Stat_t
		e = syscall.Fstat(existing, &info)
		syscall.Close(existing)
		if e != nil || info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0 || info.Nlink != 1 {
			return errUnsafe
		}
	} else if e == syscall.ENOENT {
		if e = atomicWSLFile(fd, backup, original, 0600, true); e != nil {
			return e
		}
	} else {
		return errUnsafe
	}
	return atomicWSLFile(fd, name, []byte(updated), mode, false)
}
func (s *service) installWSL(command, previous string) error {
	return s.mutateWSL(func(original string) string { return MergeWSLConfig(original, command, previous) })
}
func removeWSLCommand(original, command string) string {
	lines := strings.SplitAfter(original, "\n")
	boot := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			boot = strings.EqualFold(trimmed, "[boot]")
			continue
		}
		if !boot {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "command") {
			continue
		}
		old := strings.TrimSpace(value)
		if old == command {
			lines[i] = ""
		} else if strings.HasSuffix(old, "; "+command) {
			lines[i] = "command=" + strings.TrimSuffix(old, "; "+command) + "\n"
		}
		break
	}
	return strings.Join(lines, "")
}
func (s *service) uninstallWSL(command string) error {
	if command == "" {
		return errors.New("WSL startup command unavailable")
	}
	return s.mutateWSL(func(original string) string { return removeWSLCommand(original, command) })
}
func (s *service) wslPresent(command string) bool {
	if command == "" {
		return false
	}
	fd, name, e := s.wslDirectory(false)
	if e != nil {
		return false
	}
	defer syscall.Close(fd)
	original, _, e := readWSLFile(fd, name)
	if e != nil {
		return false
	}
	return removeWSLCommand(string(original), command) != string(original)
}
