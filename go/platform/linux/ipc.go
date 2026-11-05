//go:build linux

package linux

import (
	"context"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type ipc struct{ paths *paths }
type connection struct {
	*net.UnixConn
	peer    platform.ProcessRef
	process *processes
}

func (c *connection) Peer() (platform.ProcessRef, error) {
	return c.peer, c.process.Pin(context.Background(), c.peer)
}
func (c *connection) Read(b []byte) (int, error) {
	if e := c.process.Pin(context.Background(), c.peer); e != nil {
		return 0, e
	}
	return c.UnixConn.Read(b)
}
func (c *connection) Write(b []byte) (int, error) {
	if e := c.process.Pin(context.Background(), c.peer); e != nil {
		return 0, e
	}
	return c.UnixConn.Write(b)
}
func socketSafe(path string) error {
	root, base := filepath.Split(path)
	if base == "" {
		return errUnsafe
	}
	fd, e := dirOpen(filepath.Clean(root), false)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	n, e := syscall.Openat(fd, base, 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return errUnsafe
	}
	defer syscall.Close(n)
	var st syscall.Stat_t
	if syscall.Fstat(n, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || !safeStat(&st, false, false) {
		return errUnsafe
	}
	return nil
}
func (i *ipc) authenticate(ctx context.Context, c *net.UnixConn, expected *platform.ProcessRef) (platform.Connection, error) {
	raw, e := c.SyscallConn()
	if e != nil {
		c.Close()
		return nil, errUnsafe
	}
	var cred *syscall.Ucred
	var inner error
	e = raw.Control(func(fd uintptr) {
		cred, inner = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if e != nil || inner != nil || cred == nil || cred.Uid != uint32(os.Geteuid()) {
		c.Close()
		return nil, errUnsafe
	}
	ref, e := i.paths.process.Lookup(ctx, int(cred.Pid))
	if e != nil || (expected != nil && ref != *expected) {
		c.Close()
		return nil, errors.New("peer identity changed")
	}
	return &connection{c, ref, i.paths.process}, nil
}
func (i *ipc) Dial(ctx context.Context, handle platform.Endpoint) (platform.Connection, error) {
	e, err := i.paths.get(handle)
	if err != nil {
		return nil, err
	}
	var out platform.Connection
	if e.control {
		verified, verifyErr := verifyControlSocket(e.path)
		if verifyErr != nil {
			return nil, verifyErr
		}
		defer verified.close()
		out, err = i.dialControl(ctx, e, verified)
	} else {
		checkSocket := socketSafe
		if e.helper {
			checkSocket = socketPrivate
		}
		if err = checkSocket(e.path); err != nil {
			return nil, err
		}
		if e.expected != nil {
			if err = i.paths.process.Pin(ctx, *e.expected); err != nil {
				return nil, err
			}
		}
		dialer := net.Dialer{}
		c, dialErr := dialer.DialContext(ctx, "unix", e.path)
		if dialErr != nil {
			return nil, errors.New("local endpoint unavailable")
		}
		out, err = i.authenticate(ctx, c.(*net.UnixConn), e.expected)
		if err == nil {
			err = checkSocket(e.path)
		}
	}
	if err != nil {
		if out != nil {
			out.Close()
		}
		return nil, err
	}
	// The first authenticated control connection pins the cached capability.
	if e.expected == nil && !e.helper {
		ref, pinErr := out.Peer()
		if pinErr != nil {
			out.Close()
			return nil, pinErr
		}
		i.paths.mu.Lock()
		current, live := i.paths.endpoints[handle]
		if !live || current.expected != nil && *current.expected != ref {
			i.paths.mu.Unlock()
			out.Close()
			return nil, errors.New("peer identity changed")
		}
		current.expected = &ref
		i.paths.endpoints[handle] = current
		i.paths.mu.Unlock()
	}
	return out, nil
}

type listener struct {
	*net.UnixListener
	ipc *ipc
}

func (l *listener) Accept(ctx context.Context) (platform.Connection, error) {
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		l.SetDeadline(deadline)
		c, e := l.AcceptUnix()
		if n, ok := e.(net.Error); ok && n.Timeout() {
			continue
		}
		if e != nil {
			return nil, errors.New("local listener unavailable")
		}
		out, e := l.ipc.authenticate(ctx, c, nil)
		if e != nil {
			continue
		}
		return out, nil
	}
}
func (i *ipc) Listen(ctx context.Context, handle platform.Endpoint) (platform.Listener, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	e, err := i.paths.get(handle)
	if err != nil || !e.helper {
		return nil, errUnsafe
	}
	if _, err = dirOpenChecked(i.paths.state, true); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(e.path); !os.IsNotExist(err) {
		return nil, errors.New("endpoint already exists")
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: e.path, Net: "unix"})
	if err != nil {
		return nil, errors.New("local listener unavailable")
	}
	if err = os.Chmod(e.path, 0600); err != nil {
		l.Close()
		return nil, errUnsafe
	}
	if err = socketPrivate(e.path); err != nil {
		l.Close()
		return nil, err
	}
	return &listener{l, i}, nil
}

// Rookery listeners retain owner-only permissions even when provider endpoints
// permit private-group writes.
func socketPrivate(path string) error {
	if err := socketSafe(path); err != nil {
		return err
	}
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil || !safeStat(&st, true, false) {
		return errUnsafe
	}
	return nil
}
