//go:build windows

package windows

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type endpoint struct {
	path            string
	expected        *platform.ProcessRef
	helper, control bool
}
type paths struct {
	state     string
	files     *files
	process   platform.Process
	mu        sync.Mutex
	endpoints map[platform.Endpoint]endpoint
}

func newPaths(state string, f *files, proc platform.Process) (*paths, error) {
	p := &paths{state: state, files: f, process: proc, endpoints: map[platform.Endpoint]endpoint{}}
	var e error
	if p.state == "" {
		p.state, e = p.StateDir()
		if e != nil {
			return nil, e
		}
	}
	p.state, e = f.PrivateState(context.Background(), p.state)
	if e != nil {
		return nil, e
	}
	return p, nil
}
func (p *paths) StateDir() (string, error) {
	if p.state != "" {
		return p.state, nil
	}
	if root := os.Getenv("ROOKERY_GO_HOME"); root != "" {
		if !validPath(root) {
			return "", errUnsafe
		}
		return root, nil
	}
	base := os.Getenv("LOCALAPPDATA")
	if !validPath(base) {
		return "", errors.New("state root unavailable")
	}
	return filepath.Join(base, "rookery-go"), nil
}
func (p *paths) issue(e endpoint) (platform.Endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.issueLocked(e)
}
func (p *paths) issueLocked(e endpoint) (platform.Endpoint, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errUnsafe
	}
	id := platform.Endpoint(hex.EncodeToString(b[:]))
	p.endpoints[id] = e
	return id, nil
}
func (p *paths) get(id platform.Endpoint) (endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.endpoints[id]
	if !ok {
		return endpoint{}, errors.New("unissued endpoint")
	}
	return e, nil
}
func (p *paths) Revoke(id platform.Endpoint) { p.mu.Lock(); delete(p.endpoints, id); p.mu.Unlock() }
func (p *paths) Endpoint(name string) (platform.Endpoint, error) {
	if !validComponent(name) {
		return "", errUnsafe
	}
	sum := sha256.Sum256([]byte(p.files.sid.String() + "\x00" + strings.ToLower(p.state) + "\x00" + name))
	return p.issue(endpoint{path: `\\.\pipe\rookery-go-` + hex.EncodeToString(sum[:]), helper: true})
}
func (p *paths) ProviderData(provider string) (string, error) {
	var root, leaf string
	switch provider {
	case "codex":
		root = os.Getenv("CODEX_HOME")
		leaf = ".codex"
	case "claude":
		root = os.Getenv("CLAUDE_CONFIG_DIR")
		leaf = ".claude"
	default:
		return "", errors.New("unknown provider")
	}
	if root == "" {
		home := os.Getenv("USERPROFILE")
		if !validPath(home) {
			return "", errUnsafe
		}
		root = filepath.Join(home, leaf)
	}
	if !validPath(root) {
		return "", errUnsafe
	}
	// The path may not exist until an explicit install. Existing ancestors must
	// always pass the ownership, write-access and no-reparse traversal policy.
	candidate := root
	for {
		g, e := p.files.guardPath(candidate, false, false)
		if e == nil {
			if candidate == root {
				sd, err := win.GetSecurityInfo(g[len(g)-1], win.SE_FILE_OBJECT, win.OWNER_SECURITY_INFORMATION)
				if err != nil {
					closeGuards(g)
					return "", errUnsafe
				}
				owner, _, err := sd.Owner()
				if err != nil || !owner.Equals(p.files.sid) {
					closeGuards(g)
					return "", errUnsafe
				}
			}
			closeGuards(g)
			break
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", errUnsafe
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", errUnsafe
		}
		candidate = parent
	}
	return root, nil
}
func (p *paths) HarnessEndpoint(ctx context.Context, provider string, record platform.File, address string, server platform.ProcessRef) (platform.Endpoint, error) {
	real, ok := record.(*verifiedFile)
	if !ok || real.host != p.files {
		return "", errUnsafe
	}
	root, e := p.ProviderData(provider)
	if e != nil || !strings.EqualFold(root, real.root) || recordProvenance(real) != nil || p.process.Pin(ctx, server) != nil {
		return "", errUnsafe
	}
	if provider != "claude" || !strings.HasPrefix(address, `\\.\pipe\`) || !validComponent(strings.TrimPrefix(address, `\\.\pipe\`)) {
		return "", errUnsafe
	}
	return p.issue(endpoint{path: address, expected: &server})
}
func (p *paths) IsWSL() bool                            { return false }
func (p *paths) Executable(name string) (string, error) { return ResolveExecutable(name) }
