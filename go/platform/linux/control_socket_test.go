//go:build linux

package linux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

type controlFixture struct {
	host                          platform.Host
	root, home, link, target, dir string
	listener                      *net.UnixListener
}

func newControlFixture(t *testing.T) controlFixture {
	t.Helper()
	// A short synthetic target avoids sun_path limits independently of the test
	// runner's working directory or temporary-directory naming convention.
	root, err := os.MkdirTemp("/tmp", "rookery-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := controlFixture{root: root, home: filepath.Join(root, "codex"), dir: filepath.Join(root, "target")}
	f.link = filepath.Join(f.home, "app-server-control", "app-server-control.sock")
	f.target = filepath.Join(f.dir, "socket")
	for _, dir := range []string{filepath.Dir(f.link), f.dir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.host, err = New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	f.listener = controlListener(t, f.target)
	controlSymlink(t, f.target, f.link)
	t.Setenv("CODEX_HOME", f.home)
	return f
}

func controlListener(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	t.Cleanup(func() { l.Close() })
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return l
}

func controlSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func replaceControlLink(t *testing.T, f controlFixture, target string) {
	t.Helper()
	if err := os.Remove(f.link); err != nil {
		t.Fatal(err)
	}
	controlSymlink(t, target, f.link)
}

func TestControlSymlinkPrivateSocketAuthenticatedAndPinned(t *testing.T) {
	f := newControlFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p := f.host.Paths.(*paths)
	ep, err := p.ControlEndpoint(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.host.IPC.Dial(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peer, err := c.Peer()
	if err != nil || peer.PID != os.Getpid() || f.host.Process.Pin(ctx, peer) != nil {
		t.Fatal("control peer was not authenticated and pinned")
	}
	cached, err := p.get(ep)
	if err != nil || cached.expected == nil || *cached.expected != peer {
		t.Fatal("authenticated peer was not retained by the capability")
	}
	cached.expected.Start += "changed"
	p.mu.Lock()
	p.endpoints[ep] = cached
	p.mu.Unlock()
	if c, err := f.host.IPC.Dial(ctx, ep); err == nil {
		c.Close()
		t.Fatal("control connection accepted a changed ProcessRef")
	}
}

func TestControlSymlinkLongAdvertisedPath(t *testing.T) {
	f := newControlFixture(t)
	home := filepath.Join(f.root, strings.Repeat("synthetic", 16))
	if err := os.Rename(f.home, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	if len(filepath.Join(home, "app-server-control", "app-server-control.sock")) <= 108 {
		t.Fatal("fixture did not exceed sun_path limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ep, err := f.host.Paths.(*paths).ControlEndpoint(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.host.IPC.Dial(ctx, ep)
	if err != nil {
		t.Fatal("long advertised path did not connect through retained target", err)
	}
	c.Close()
}

func TestControlSymlinkRefusesAccessibleTargetParent(t *testing.T) {
	for _, mode := range []os.FileMode{0750, 0770, 0755, 0775, 0707, 0777} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newControlFixture(t)
			ep, err := f.host.Paths.(*paths).ControlEndpoint(context.Background(), "codex")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.dir, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := f.host.Paths.(*paths).ControlEndpoint(context.Background(), "codex"); !errors.Is(err, platform.ErrControlEndpointUnsafe) {
				t.Fatal("accepted group/other-accessible target parent")
			}
			if c, err := f.host.IPC.Dial(context.Background(), ep); !errors.Is(err, platform.ErrControlEndpointUnsafe) {
				if c != nil {
					c.Close()
				}
				t.Fatal("Dial accepted target parent changed after capability issuance")
			}
		})
	}
}

func TestControlSymlinkRefusesBroaderShapes(t *testing.T) {
	for _, shape := range []string{"two-links", "target-ancestor-link", "relative", "dotdot", "regular", "unsafe-target-ancestor", "public-home", "public-control-directory", "control-ancestor-link"} {
		t.Run(shape, func(t *testing.T) {
			f := newControlFixture(t)
			switch shape {
			case "two-links":
				second := filepath.Join(f.dir, "second")
				controlSymlink(t, f.target, second)
				replaceControlLink(t, f, second)
			case "target-ancestor-link":
				alias := filepath.Join(f.root, "alias")
				controlSymlink(t, f.dir, alias)
				replaceControlLink(t, f, filepath.Join(alias, "socket"))
			case "relative":
				replaceControlLink(t, f, "../../target/socket")
			case "dotdot":
				replaceControlLink(t, f, f.dir+"/../target/socket")
			case "regular":
				regular := filepath.Join(f.dir, "regular")
				if err := os.WriteFile(regular, []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
				replaceControlLink(t, f, regular)
			case "unsafe-target-ancestor":
				ancestor := filepath.Join(f.root, "unsafe")
				private := filepath.Join(ancestor, "private")
				if err := os.MkdirAll(private, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(ancestor, 0777); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(private, "socket")
				controlListener(t, target)
				replaceControlLink(t, f, target)
			case "public-home":
				if err := os.Chmod(f.home, 0755); err != nil {
					t.Fatal(err)
				}
			case "public-control-directory":
				if err := os.Chmod(filepath.Dir(f.link), 0755); err != nil {
					t.Fatal(err)
				}
			case "control-ancestor-link":
				real := f.home + "-real"
				if err := os.Rename(f.home, real); err != nil {
					t.Fatal(err)
				}
				controlSymlink(t, real, f.home)
			}
			if s, err := verifyControlSocket(f.link); !errors.Is(err, platform.ErrControlEndpointUnsafe) {
				if s != nil {
					s.close()
				}
				t.Fatal("accepted unsafe control symlink shape")
			}
		})
	}
}

func TestControlSymlinkTargetSwapAfterVerificationRefused(t *testing.T) {
	f := newControlFixture(t)
	s, err := verifyControlSocket(f.link)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := os.Rename(f.target, f.target+"-original"); err != nil {
		t.Fatal(err)
	}
	controlListener(t, f.target)
	var retained syscall.Stat_t
	if err := syscall.Fstat(s.socket, &retained); err != nil || !sameObject(retained, s.socketStat) {
		t.Fatal("verification did not retain the original socket inode")
	}
	if err := s.reverify(); !errors.Is(err, platform.ErrControlEndpointUnsafe) {
		t.Fatal("reverification accepted a replacement socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := f.host.IPC.(*ipc).dialControl(ctx, endpoint{path: f.link, control: true}, s)
	if !errors.Is(err, platform.ErrControlEndpointUnsafe) {
		if c != nil {
			c.Close()
		}
		t.Fatal("connect accepted a socket replaced after verification")
	}
	if err := f.listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	server, err := f.listener.AcceptUnix()
	if err != nil {
		t.Fatal("retained socket was not connected before reverification", err)
	}
	server.Close()
}

func TestControlSymlinkLinkSwapAfterVerificationRefused(t *testing.T) {
	f := newControlFixture(t)
	s, err := verifyControlSocket(f.link)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	replaceControlLink(t, f, f.target)
	if err := s.reverify(); !errors.Is(err, platform.ErrControlEndpointUnsafe) {
		t.Fatal("reverification accepted a replaced link to the same socket")
	}
}

func TestControlSymlinkForeignOwnerPolicy(t *testing.T) {
	uid := uint32(os.Geteuid())
	if !controlLinkOwned(&syscall.Stat_t{Mode: syscall.S_IFLNK | 0777, Uid: uid}) {
		t.Fatal("refused current user's link")
	}
	if controlLinkOwned(&syscall.Stat_t{Mode: syscall.S_IFLNK | 0777, Uid: uid + 1}) {
		t.Fatal("accepted foreign-owned link")
	}
	if safeStat(&syscall.Stat_t{Mode: syscall.S_IFSOCK | 0600, Uid: uid + 1}, false, false) {
		t.Fatal("accepted foreign-owned socket")
	}
}

func TestControlSymlinkForeignOwnerFilesystem(t *testing.T) {
	f := newControlFixture(t)
	if err := os.Lchown(f.link, os.Geteuid()+1, -1); err != nil {
		t.Skip("lchown unavailable; foreign ownership covered by policy test")
	}
	if _, err := verifyControlSocket(f.link); !errors.Is(err, platform.ErrControlEndpointUnsafe) {
		t.Fatal("accepted foreign-owned control symlink")
	}
}

func TestControlSymlinkExceptionDoesNotExtendToOtherCapabilities(t *testing.T) {
	f := newControlFixture(t)
	ctx := context.Background()
	ep, err := f.host.Paths.Endpoint("helper.sock")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := f.host.Paths.(*paths).get(ep)
	if err != nil {
		t.Fatal(err)
	}
	controlSymlink(t, f.target, entry.path)
	if c, err := f.host.IPC.Dial(ctx, ep); err == nil {
		c.Close()
		t.Fatal("helper capability followed a symlink")
	}
	if err := f.host.Files.AtomicWrite(ctx, f.home, "record.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	record, err := f.host.Files.OpenNoFollow(ctx, f.home, "record.json", false)
	if err != nil {
		t.Fatal(err)
	}
	defer record.Close()
	peer, err := f.host.Process.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Paths.HarnessEndpoint(ctx, "codex", record, f.link, peer); err == nil {
		t.Fatal("record-based harness capability followed a symlink")
	}
}
