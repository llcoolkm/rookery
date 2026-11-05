//go:build linux

package linux

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/llcoolkm/rookery/go/platform"
)

// controlSocket retains O_PATH descriptors so neither inode reuse nor a pathname
// substitution can change the object used by connect. Only the fixed Codex
// control capability can reach this exception to the no-symlink policy.
type controlSocket struct {
	path, target         string
	socket, link         int
	socketStat, linkStat syscall.Stat_t
}

func (s *controlSocket) close() {
	if s.socket >= 0 {
		syscall.Close(s.socket)
	}
	if s.link >= 0 {
		syscall.Close(s.link)
	}
}
func sameObject(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid
}

func controlLinkOwned(st *syscall.Stat_t) bool {
	return st.Mode&syscall.S_IFMT == syscall.S_IFLNK && st.Uid == uint32(os.Geteuid())
}
func openObject(path string, privateParent bool) (int, syscall.Stat_t, error) {
	var st syscall.Stat_t
	parent, base := filepath.Split(path)
	fd, err := dirOpen(filepath.Clean(parent), privateParent)
	if err != nil {
		return -1, st, platform.ErrControlEndpointUnsafe
	}
	defer syscall.Close(fd)
	if privateParent {
		var parentStat syscall.Stat_t
		if syscall.Fstat(fd, &parentStat) != nil || parentStat.Mode&0777 != 0700 {
			return -1, st, platform.ErrControlEndpointUnsafe
		}
	}
	object, err := syscall.Openat(fd, base, 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, st, platform.ErrControlEndpointUnsafe
	}
	if syscall.Fstat(object, &st) != nil {
		syscall.Close(object)
		return -1, st, platform.ErrControlEndpointUnsafe
	}
	return object, st, nil
}
func verifyControlSocket(path string) (*controlSocket, error) {
	s := &controlSocket{path: path, target: path, socket: -1, link: -1}
	fail := func() (*controlSocket, error) { s.close(); return nil, platform.ErrControlEndpointUnsafe }
	fd, st, err := openObject(path, false)
	if err != nil {
		return fail()
	}
	s.socket, s.socketStat = fd, st
	if st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
		s.link, s.linkStat, s.socket = fd, st, -1
		if !controlLinkOwned(&st) {
			return fail()
		}
		// Both CODEX_HOME and its control directory must be owner-only. Their
		// ancestors use StrictModes, including its root-owned sticky /tmp exception.
		for _, dir := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
			d, e := dirOpen(dir, true)
			if e != nil {
				return fail()
			}
			syscall.Close(d)
		}
		target, e := os.Readlink(path)
		if e != nil || !filepath.IsAbs(target) || filepath.Clean(target) != target {
			return fail()
		}
		s.target = target
		fd, st, err = openObject(target, true)
		if err != nil {
			return fail()
		}
		s.socket, s.socketStat = fd, st
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || !safeStat(&st, false, false) {
		return fail()
	}
	return s, nil
}
func (s *controlSocket) reverify() error {
	other, err := verifyControlSocket(s.path)
	if err != nil {
		return platform.ErrControlEndpointUnsafe
	}
	defer other.close()
	if s.target != other.target || (s.link >= 0) != (other.link >= 0) || !sameObject(s.socketStat, other.socketStat) || (s.link >= 0 && !sameObject(s.linkStat, other.linkStat)) {
		return platform.ErrControlEndpointUnsafe
	}
	return nil
}
func (i *ipc) dialControl(ctx context.Context, e endpoint, s *controlSocket) (platform.Connection, error) {
	// /proc addresses our retained socket descriptor, never an untrusted link.
	// This also avoids the sockaddr_un length limit for long advertised paths.
	dialer := net.Dialer{}
	c, err := dialer.DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(s.socket))
	if err != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	out, err := i.authenticate(ctx, c.(*net.UnixConn), e.expected)
	if err != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	if err = s.reverify(); err != nil {
		out.Close()
		return nil, err
	}
	return out, nil
}
