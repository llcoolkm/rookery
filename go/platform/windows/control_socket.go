//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
	"path/filepath"
	"sync"
	"unsafe"
)

// Windows binds the fixed advertised endpoint directly, without a Unix symlink.
// openai/codex rust-v0.166.0 app-server-transport/src/transport/unix_socket.rs:106-115.
func (p *paths) ControlEndpoint(ctx context.Context, provider string) (platform.Endpoint, error) {
	if provider != "codex" || ctx.Err() != nil {
		return "", platform.ErrControlEndpointUnsafe
	}
	root, e := p.ProviderData(provider)
	if e != nil {
		return "", platform.ErrControlEndpointUnsafe
	}
	path := filepath.Join(root, "app-server-control", "app-server-control.sock")
	guard, e := p.files.guardPath(filepath.Dir(path), true, false)
	if e != nil {
		return "", platform.ErrControlEndpointUnsafe
	}
	defer closeGuards(guard)
	if e = verifyAFUnixSocket(path, p.files.sid); e != nil {
		return "", e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for handle, entry := range p.endpoints {
		if entry.control && entry.path == path {
			if entry.expected == nil || p.process.Pin(ctx, *entry.expected) == nil {
				return handle, nil
			}
			delete(p.endpoints, handle)
		}
	}
	return p.issueLocked(endpoint{path: path, control: true})
}

// AF_UNIX's kernel socket tag does not redirect path traversal. Refuse every
// other reparse tag. All directory components remain strictly non-reparse.
type socketFileID struct{ volume, high, low uint32 }

func verifyAFUnixSocket(path string, sid *win.SID) error {
	h, _, e := openAFUnixSocket(path, sid)
	if e == nil {
		win.CloseHandle(h)
	}
	return e
}
func openAFUnixSocket(path string, sid *win.SID) (win.Handle, socketFileID, error) {
	var id socketFileID
	name, e := win.UTF16PtrFromString(path)
	if e != nil {
		return 0, id, platform.ErrControlEndpointUnsafe
	}
	h, e := win.CreateFile(name, win.READ_CONTROL|win.FILE_READ_ATTRIBUTES, win.FILE_SHARE_READ|win.FILE_SHARE_WRITE, nil, win.OPEN_EXISTING, win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return 0, id, platform.ErrControlEndpointUnsafe
	}
	fail := func() (win.Handle, socketFileID, error) {
		win.CloseHandle(h)
		return 0, id, platform.ErrControlEndpointUnsafe
	}
	var tag struct{ Attributes, Tag uint32 }
	if win.GetFileInformationByHandleEx(h, 9, (*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))) != nil {
		return fail()
	}
	if tag.Attributes&win.FILE_ATTRIBUTE_DIRECTORY != 0 || tag.Attributes&win.FILE_ATTRIBUTE_REPARSE_POINT == 0 || tag.Tag != 0x80000023 {
		return fail()
	}
	sd, e := win.GetSecurityInfo(h, win.SE_FILE_OBJECT, win.OWNER_SECURITY_INFORMATION)
	if e != nil {
		return fail()
	}
	owner, _, e := sd.Owner()
	if e != nil || !win.EqualSid(owner, sid) {
		win.CloseHandle(h)
		return 0, id, fmt.Errorf("control socket owner mismatch: %w", platform.ErrControlEndpointUnsafe)
	}
	var info win.ByHandleFileInformation
	if win.GetFileInformationByHandle(h, &info) != nil {
		return fail()
	}
	id = socketFileID{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}
	return h, id, nil
}

var socketStartup sync.Once
var socketStartupError error

func startSockets() error {
	socketStartup.Do(func() { var data win.WSAData; socketStartupError = win.WSAStartup(0x202, &data) })
	return socketStartupError
}
func (i *ipc) dialControl(ctx context.Context, e endpoint) (platform.Connection, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if startSockets() != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	guard, err := i.paths.files.guardPath(filepath.Dir(e.path), true, false)
	if err != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	defer closeGuards(guard)
	socketGuard, identity, err := openAFUnixSocket(e.path, i.paths.files.sid)
	if err != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	defer win.CloseHandle(socketGuard)

	if e.expected != nil && i.paths.process.Pin(ctx, *e.expected) != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	h, err := win.Socket(win.AF_UNIX, win.SOCK_STREAM, 0)
	if err != nil {
		return nil, platform.ErrControlEndpointUnsafe
	}
	if win.Connect(h, &win.SockaddrUnix{Name: e.path}) != nil {
		win.Closesocket(h)
		return nil, platform.ErrControlEndpointUnsafe
	}
	// windows_peer.rs: SIO_AF_UNIX_GETPEERPID, same SID, BOTH tokens non-elevated.
	out, err := i.authenticate(ctx, h, true, true, e.expected)
	if err != nil {
		win.Closesocket(h)
		if errors.Is(err, platform.ErrControlEndpointElevated) {
			return nil, platform.ErrControlEndpointElevated
		}
		return nil, platform.ErrControlEndpointUnsafe
	}
	second, after, checkErr := openAFUnixSocket(e.path, i.paths.files.sid)
	if checkErr == nil {
		win.CloseHandle(second)
	}
	if checkErr != nil || identity != after {
		out.Close()
		return nil, platform.ErrControlEndpointUnsafe
	}

	if e.expected == nil {
		pin, pinErr := out.Peer()
		if pinErr != nil {
			out.Close()
			return nil, platform.ErrControlEndpointUnsafe
		}
		i.paths.mu.Lock()
		for handle, entry := range i.paths.endpoints {
			if entry.control && entry.path == e.path {
				if entry.expected != nil && *entry.expected != pin {
					i.paths.mu.Unlock()
					out.Close()
					return nil, platform.ErrControlEndpointUnsafe
				}
				entry.expected = &pin
				i.paths.endpoints[handle] = entry
			}
		}
		i.paths.mu.Unlock()
	}
	return out, nil
}
