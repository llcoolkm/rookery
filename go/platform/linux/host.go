//go:build linux

package linux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type endpoint struct {
	path     string
	expected *platform.ProcessRef
	helper   bool
}
type paths struct {
	state     string
	files     *files
	process   *processes
	mu        sync.Mutex
	endpoints map[platform.Endpoint]endpoint
}

func New(stateDir string) (platform.Host, error) {
	p := &paths{state: stateDir, files: &files{opened: make(map[*verifiedFile]bool)}, process: &processes{}, endpoints: make(map[platform.Endpoint]endpoint)}
	if stateDir == "" {
		var e error
		p.state, e = p.StateDir()
		if e != nil {
			return platform.Host{}, e
		}
	}
	if !filepath.IsAbs(p.state) {
		return platform.Host{}, errUnsafe
	}
	root, e := p.files.PrivateState(context.Background(), filepath.Clean(p.state))
	if e != nil {
		return platform.Host{}, e
	}
	p.state = root
	return platform.Host{Paths: p, Files: p.files, Process: p.process, Exec: executor{}, IPC: &ipc{paths: p}, Service: &service{paths: p}}, nil
}
func (p *paths) StateDir() (string, error) {
	if p.state != "" {
		return p.state, nil
	}
	if v := os.Getenv("ROOKERY_GO_HOME"); v != "" {
		if !filepath.IsAbs(v) {
			return "", errUnsafe
		}
		return filepath.Clean(v), nil
	}
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", errors.New("state root unavailable")
		}
		root = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(root) {
		return "", errUnsafe
	}
	return filepath.Join(root, "rookery-go"), nil
}
func (p *paths) issue(e endpoint) (platform.Endpoint, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("endpoint unavailable")
	}
	handle := platform.Endpoint(hex.EncodeToString(b[:]))
	p.mu.Lock()
	p.endpoints[handle] = e
	p.mu.Unlock()
	return handle, nil
}
func (p *paths) get(h platform.Endpoint) (endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.endpoints[h]
	if !ok {
		return endpoint{}, errors.New("unissued endpoint")
	}
	return e, nil
}
func (p *paths) Endpoint(name string) (platform.Endpoint, error) {
	if name == "" || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return "", errUnsafe
	}
	path := filepath.Join(p.state, name)
	if len(path) > 100 {
		return "", errors.New("endpoint path too long")
	}
	return p.issue(endpoint{path: path, helper: true})
}
func (p *paths) ProviderData(provider string) (string, error) {
	var root string
	switch provider {
	case "codex":
		root = os.Getenv("CODEX_HOME")
		if root == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return "", errUnsafe
			}
			root = filepath.Join(home, ".codex")
		}
	case "claude":
		root = os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return "", errUnsafe
			}
			root = filepath.Join(home, ".claude")
		}
	default:
		return "", errors.New("unknown provider")
	}
	if !filepath.IsAbs(root) {
		return "", errUnsafe
	}
	return filepath.Clean(root), nil
}
func (p *paths) HarnessEndpoint(ctx context.Context, provider string, record platform.File, address string, server platform.ProcessRef) (platform.Endpoint, error) {
	real, ok := record.(*verifiedFile)
	if !ok || real.host != p.files {
		return "", errUnsafe
	}
	p.files.mu.Lock()
	live := p.files.opened[real]
	p.files.mu.Unlock()
	root, e := p.ProviderData(provider)
	if e != nil || !live || real.root != root {
		return "", errUnsafe
	}
	var st syscall.Stat_t
	if syscall.Fstat(int(real.Fd()), &st) != nil || !safeStat(&st, true, false) || st.Nlink != 1 {
		return "", errUnsafe
	}
	if e = p.process.Pin(ctx, server); e != nil {
		return "", e
	}
	if e = socketSafe(address); e != nil {
		return "", e
	}
	return p.issue(endpoint{path: address, expected: &server})
}
func (p *paths) IsWSL() bool {
	data, e := os.ReadFile("/proc/sys/kernel/osrelease")
	return e == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}

// Executable resolves a native executable once without shell/PATHEXT fallback.
func (p *paths) Executable(name string) (string, error) { return ResolveExecutable(name) }

// ControlEndpoint grants a capability for the platform's fixed Codex control
// socket. Unlike record-based HarnessEndpoint it accepts no caller path. The
// resolver authenticates and pins its server now; Dial must match that identity.
func (p *paths) ControlEndpoint(ctx context.Context, provider string) (platform.Endpoint, error) {
	if provider != "codex" {
		return "", errors.New("provider has no fixed control endpoint")
	}
	root, e := p.ProviderData(provider)
	if e != nil {
		return "", e
	}
	path := filepath.Join(root, "app-server-control", "app-server-control.sock")
	if e = socketSafe(path); e != nil {
		return "", e
	}
	dialer := net.Dialer{}
	raw, e := dialer.DialContext(ctx, "unix", path)
	if e != nil {
		return "", errors.New("local endpoint unavailable")
	}
	facility := &ipc{paths: p}
	conn, e := facility.authenticate(ctx, raw.(*net.UnixConn), nil)
	if e != nil {
		return "", e
	}
	defer conn.Close()
	ref, e := conn.Peer()
	if e != nil {
		return "", e
	}
	if e = socketSafe(path); e != nil {
		return "", e
	}
	return p.issue(endpoint{path: path, expected: &ref})
}
