//go:build windows

package windows

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
)

var errPeer = errors.New("local peer authentication refused")

type ipc struct{ paths *paths }
type connection struct {
	handle                      win.Handle
	socket                      bool
	peer                        platform.ProcessRef
	process                     platform.Process
	mu                          sync.Mutex
	readMu, writeMu             sync.Mutex
	readDeadline, writeDeadline time.Time
	closed                      bool
	prefix                      []byte
	active                      sync.WaitGroup
}

func (c *connection) Peer() (platform.ProcessRef, error) {
	return c.peer, c.process.Pin(context.Background(), c.peer)
}
func (c *connection) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	return nil
}
func (c *connection) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	return nil
}
func (c *connection) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return nil
}
func (c *connection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	win.CancelIoEx(c.handle, nil)
	c.mu.Unlock()
	c.active.Wait()
	if c.socket {
		return win.Closesocket(c.handle)
	}
	return win.CloseHandle(c.handle)
}
func (c *connection) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(c.prefix) > 0 {
		if _, e := c.Peer(); e != nil {
			return 0, errPeer
		}
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.transfer(b, false)
}
func (c *connection) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.transfer(b, true)
}
func (c *connection) transfer(b []byte, write bool) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if _, e := c.Peer(); e != nil {
		return 0, errPeer
	}
	event, e := win.CreateEvent(nil, 1, 0, nil)
	if e != nil {
		return 0, errPeer
	}
	defer win.CloseHandle(event)
	ov := win.Overlapped{HEvent: event}
	var n uint32
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.active.Add(1)
	defer c.active.Done()
	if c.socket {
		buf := win.WSABuf{Len: uint32(len(b)), Buf: &b[0]}
		var flags uint32
		if write {
			e = win.WSASend(c.handle, &buf, 1, &n, 0, &ov, nil)
		} else {
			e = win.WSARecv(c.handle, &buf, 1, &n, &flags, &ov, nil)
		}
	} else if write {
		e = win.WriteFile(c.handle, b, &n, &ov)
	} else {
		e = win.ReadFile(c.handle, b, &n, &ov)
	}
	c.mu.Unlock()
	if e != nil && !errors.Is(e, win.ERROR_IO_PENDING) {
		if errors.Is(e, win.ERROR_BROKEN_PIPE) {
			return 0, io.EOF
		}
		return 0, errPeer
	}
	for {
		r, waitErr := win.WaitForSingleObject(event, 50)
		if waitErr != nil {
			win.CancelIoEx(c.handle, &ov)
			c.overlappedResult(&ov, &n, true)
			runtime.KeepAlive(b)
			return 0, errPeer
		}
		if r == win.WAIT_OBJECT_0 {
			break
		}
		c.mu.Lock()
		deadline := c.readDeadline
		if write {
			deadline = c.writeDeadline
		}
		closed := c.closed
		c.mu.Unlock()
		if closed || (!deadline.IsZero() && !time.Now().Before(deadline)) {
			win.CancelIoEx(c.handle, &ov)
			c.overlappedResult(&ov, &n, true)
			runtime.KeepAlive(b)
			return int(n), errors.New("local I/O deadline or closure")
		}
	}
	e = c.overlappedResult(&ov, &n, false)
	runtime.KeepAlive(b)
	if errors.Is(e, win.ERROR_BROKEN_PIPE) || (!write && e == nil && n == 0) {
		return int(n), io.EOF
	}
	if e != nil {
		return int(n), errPeer
	}
	return int(n), nil
}

func tokenDWORD(t win.Token, class uint32) (uint32, error) {
	var v, n uint32
	e := win.GetTokenInformation(t, class, (*byte)(unsafe.Pointer(&v)), 4, &n)
	return v, e
}
func tokenIntegrity(t win.Token) (uint32, error) {
	var n uint32
	win.GetTokenInformation(t, win.TokenIntegrityLevel, nil, 0, &n)
	if n == 0 {
		return 0, errPeer
	}
	b := make([]byte, n)
	if win.GetTokenInformation(t, win.TokenIntegrityLevel, &b[0], n, &n) != nil {
		return 0, errPeer
	}
	label := (*win.Tokenmandatorylabel)(unsafe.Pointer(&b[0]))
	sid := label.Label.Sid
	if sid == nil || sid.SubAuthorityCount() == 0 {
		return 0, errPeer
	}
	return sid.SubAuthority(uint32(sid.SubAuthorityCount() - 1)), nil
}
func tokenAllowed(peer, current win.Token, nonElevated bool) error {
	p, e := peer.GetTokenUser()
	if e != nil {
		return errPeer
	}
	u, e := current.GetTokenUser()
	if e != nil || !win.EqualSid(p.User.Sid, u.User.Sid) {
		return errPeer
	}
	pi, e := tokenIntegrity(peer)
	if e != nil {
		return errPeer
	}
	ci, e := tokenIntegrity(current)
	if e != nil || pi < ci {
		return errPeer
	}
	app, e := tokenDWORD(peer, 29)
	if e != nil || app != 0 {
		return errPeer
	}
	if nonElevated {
		for _, t := range []win.Token{peer, current} {
			v, e := tokenDWORD(t, win.TokenElevation)
			if e != nil || v != 0 {
				return errPeer
			}
		}
	}
	return nil
}
func (i *ipc) authenticate(ctx context.Context, h win.Handle, socket, server bool, expected *platform.ProcessRef) (platform.Connection, error) {
	var pid uint32
	if socket {
		var returned uint32
		if win.WSAIoctl(h, 0x58000100, nil, 0, (*byte)(unsafe.Pointer(&pid)), 4, &returned, nil, 0) != nil {
			return nil, errPeer
		}
	} else if server {
		if win.GetNamedPipeServerProcessId(h, &pid) != nil {
			return nil, errPeer
		}
	} else {
		if win.GetNamedPipeClientProcessId(h, &pid) != nil {
			return nil, errPeer
		}
	}
	if pid == 0 {
		return nil, errPeer
	}
	ref, e := i.paths.process.Lookup(ctx, int(pid))
	if e != nil || (expected != nil && ref != *expected) {
		return nil, errPeer
	}
	ph, e := win.OpenProcess(win.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if e != nil {
		return nil, errPeer
	}
	defer win.CloseHandle(ph)
	var pt, ct win.Token
	if win.OpenProcessToken(ph, win.TOKEN_QUERY, &pt) != nil {
		return nil, errPeer
	}
	defer pt.Close()
	if win.OpenProcessToken(win.CurrentProcess(), win.TOKEN_QUERY, &ct) != nil {
		return nil, errPeer
	}
	defer ct.Close()
	if tokenAllowed(pt, ct, socket) != nil {
		return nil, errPeer
	}
	c := &connection{handle: h, socket: socket, peer: ref, process: i.paths.process}
	if !socket && !server { // Impersonation identifies the token of the last read message.
		deadline := time.Now().Add(5 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		c.SetReadDeadline(deadline)
		var first [1]byte
		if n, e := c.transfer(first[:], false); e != nil || n != 1 {
			return nil, errPeer
		}
		c.prefix = append(c.prefix, first[0])
		c.SetReadDeadline(time.Time{})
		if connectedTokenAllowed(h, ct) != nil {
			return nil, errPeer
		}
	}
	if i.paths.process.Pin(ctx, ref) != nil {
		return nil, errPeer
	}
	return c, nil
}

var impersonatePipe = win.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

func connectedTokenAllowed(h win.Handle, current win.Token) error {
	runtime.LockOSThread()
	r, _, _ := impersonatePipe.Call(uintptr(h))
	if r == 0 {
		runtime.UnlockOSThread()
		return errPeer
	}
	defer func() {
		// A failed reversion must never return an impersonated thread to Go's pool.
		if win.RevertToSelf() != nil {
			panic("local peer impersonation cleanup failed")
		}
		runtime.UnlockOSThread()
	}()
	var token win.Token
	if win.OpenThreadToken(win.CurrentThread(), win.TOKEN_QUERY, true, &token) != nil {
		return errPeer
	}
	defer token.Close()
	return tokenAllowed(token, current, false)
}

func pipeSafe(h win.Handle, sid *win.SID) error {
	sd, e := win.GetSecurityInfo(h, win.SE_KERNEL_OBJECT, win.OWNER_SECURITY_INFORMATION|win.DACL_SECURITY_INFORMATION)
	if e != nil {
		return errPeer
	}
	owner, _, e := sd.Owner()
	if e != nil || !win.EqualSid(owner, sid) {
		return errPeer
	}
	acl, _, e := sd.DACL()
	if e != nil || acl == nil || acl.AceCount == 0 {
		return errPeer
	}
	control, _, e := sd.Control()
	if e != nil || control&win.SE_DACL_PROTECTED == 0 {
		return errPeer
	}
	// Every allow ACE must name the owning account. Unknown ACE forms fail closed.
	for n := uint32(0); n < uint32(acl.AceCount); n++ {
		var ace *win.ACCESS_ALLOWED_ACE
		if win.GetAce(acl, n, &ace) != nil {
			return errPeer
		}
		if ace.Header.AceType != win.ACCESS_ALLOWED_ACE_TYPE {
			return errPeer
		}
		if !win.EqualSid((*win.SID)(unsafe.Pointer(&ace.SidStart)), sid) {
			return errPeer
		}
	}
	return nil
}
func (i *ipc) Dial(ctx context.Context, handle platform.Endpoint) (platform.Connection, error) {
	e, err := i.paths.get(handle)
	if err != nil {
		return nil, errPeer
	}
	if e.control {
		return i.dialControl(ctx, e)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if e.expected != nil && i.paths.process.Pin(ctx, *e.expected) != nil {
		return nil, errPeer
	}
	name, err := win.UTF16PtrFromString(e.path)
	if err != nil {
		return nil, errPeer
	}
	h, err := win.CreateFile(name, win.GENERIC_READ|win.GENERIC_WRITE, 0, nil, win.OPEN_EXISTING, win.FILE_FLAG_OVERLAPPED|win.SECURITY_SQOS_PRESENT|win.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, errPeer
	}
	if pipeSafe(h, i.paths.files.sid) != nil {
		win.CloseHandle(h)
		return nil, errPeer
	}
	out, err := i.authenticate(ctx, h, false, true, e.expected)
	if err != nil {
		win.CloseHandle(h)
	}
	return out, err
}

type listener struct {
	ipc        *ipc
	mu         sync.Mutex
	pending    win.Handle
	name       string
	closed     bool
	acceptMu   sync.Mutex
	descriptor *win.SECURITY_DESCRIPTOR
}

func (l *listener) create(first bool) (win.Handle, error) {
	name, _ := win.UTF16PtrFromString(l.name)
	flags := uint32(win.PIPE_ACCESS_DUPLEX | win.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= win.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	return win.CreateNamedPipe(name, flags, win.PIPE_TYPE_BYTE|win.PIPE_READMODE_BYTE|win.PIPE_WAIT|win.PIPE_REJECT_REMOTE_CLIENTS, win.PIPE_UNLIMITED_INSTANCES, 65536, 65536, 0, &win.SecurityAttributes{Length: uint32(unsafe.Sizeof(win.SecurityAttributes{})), SecurityDescriptor: l.descriptor})
}
func (i *ipc) Listen(ctx context.Context, handle platform.Endpoint) (platform.Listener, error) {
	e, err := i.paths.get(handle)
	if err != nil || !e.helper || ctx.Err() != nil {
		return nil, errPeer
	}
	var ct win.Token
	if win.OpenProcessToken(win.CurrentProcess(), win.TOKEN_QUERY, &ct) != nil {
		return nil, errPeer
	}
	defer ct.Close()
	il, err := tokenIntegrity(ct)
	if err != nil {
		return nil, errPeer
	}
	sd, err := win.SecurityDescriptorFromString("O:" + i.paths.files.sid.String() + "D:P(A;;GA;;;" + i.paths.files.sid.String() + ")S:(ML;;NW;;;S-1-16-" + fmtUint(il) + ")")
	if err != nil {
		return nil, errPeer
	}
	l := &listener{ipc: i, name: e.path, descriptor: sd}
	l.pending, err = l.create(true)
	if err != nil {
		return nil, errPeer
	}
	return l, nil
}
func fmtUint(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b [10]byte
	p := len(b)
	for n > 0 {
		p--
		b[p] = byte('0' + n%10)
		n /= 10
	}
	return string(b[p:])
}
func (l *listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	win.CancelIoEx(l.pending, nil)
	return win.CloseHandle(l.pending)
}
func (l *listener) Accept(ctx context.Context) (platform.Connection, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, io.ErrClosedPipe
		}
		h := l.pending
		l.mu.Unlock()
		event, err := win.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			return nil, errPeer
		}
		ov := win.Overlapped{HEvent: event}
		err = win.ConnectNamedPipe(h, &ov)
		if err == nil || errors.Is(err, win.ERROR_PIPE_CONNECTED) {
			win.SetEvent(event)
		} else if !errors.Is(err, win.ERROR_IO_PENDING) {
			win.CloseHandle(event)
			return nil, errPeer
		}
		for {
			r, e := win.WaitForSingleObject(event, 50)
			if e != nil {
				err = errPeer
				break
			}
			if r == win.WAIT_OBJECT_0 {
				break
			}
			if ctx.Err() != nil {
				win.CancelIoEx(h, &ov)
				var n uint32
				win.GetOverlappedResult(h, &ov, &n, true)
				err = ctx.Err()
				break
			}
		}
		win.CloseHandle(event)
		if err != nil && !errors.Is(err, win.ERROR_IO_PENDING) && !errors.Is(err, win.ERROR_PIPE_CONNECTED) {
			return nil, err
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, io.ErrClosedPipe
		}
		next, e := l.create(false)
		if e != nil {
			l.mu.Unlock()
			return nil, errPeer
		}
		l.pending = next
		l.mu.Unlock()
		out, e := l.ipc.authenticate(ctx, h, false, false, nil)
		if e != nil {
			win.DisconnectNamedPipe(h)
			win.CloseHandle(h)
			continue
		}
		return out, nil
	}
}

func (c *connection) overlappedResult(ov *win.Overlapped, n *uint32, wait bool) error {
	if c.socket {
		var flags uint32
		return win.WSAGetOverlappedResult(c.handle, ov, n, wait, &flags)
	}
	return win.GetOverlappedResult(c.handle, ov, n, wait)
}
