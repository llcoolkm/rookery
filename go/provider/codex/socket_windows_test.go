//go:build windows

package codex

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	win "golang.org/x/sys/windows"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/platform/windows"
	"github.com/llcoolkm/rookery/go/provider"
)

func serveFake(conn platform.Connection, fake *fakeConn) error {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	var upgrade []byte
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		upgrade = append(upgrade, line...)
		if string(line) == "\r\n" {
			break
		}
	}
	if _, err := fake.Write(upgrade); err != nil {
		return err
	}
	if _, err := io.Copy(conn, &fake.buffer); err != nil {
		return err
	}
	for {
		var header [2]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		frame := append([]byte(nil), header[:]...)
		n := int(header[1] & 127)
		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(reader, b[:]); err != nil {
				return err
			}
			frame = append(frame, b[:]...)
			n = int(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			return io.ErrUnexpectedEOF
		}
		rest := make([]byte, n+4)
		if _, err := io.ReadFull(reader, rest); err != nil {
			return err
		}
		frame = append(frame, rest...)
		if _, err := fake.Write(frame); err != nil {
			return err
		}
		if _, err := io.Copy(conn, &fake.buffer); err != nil {
			return err
		}
		if fake.queueMode == "lost" && len(*fake.requests) > 0 && (*fake.requests)[len(*fake.requests)-1].Method == "thread/queue/add" {
			return nil
		}
	}
}

// TestControlSocket runs HTTP Upgrade, masked RFC6455 requests, notifications,
// loaded-thread discovery and revalidation against a real authenticated socket.
func TestControlSocket(t *testing.T) {
	for _, tc := range []struct {
		name, state, mode, want string
		queues                  int
	}{
		{"accepted", "idle", "", "submitted", 1},
		{"rpc-error", "idle", "error", "uncertain", 1},
		{"lost-after-write", "idle", "lost", "uncertain", 1},
		{"not-loaded", "notLoaded", "", "held", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			root := filepath.Join(t.TempDir(), "state")
			host, err := windows.New(root)
			if err != nil {
				t.Fatal(err)
			}
			shortRoot, e := os.MkdirTemp(os.TempDir(), "r-")
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { os.RemoveAll(shortRoot) })
			home := filepath.Join(shortRoot, "h")
			if _, err = host.Files.PrivateState(ctx, home); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(home, "app-server-control")
			if _, err = host.Files.PrivateState(ctx, dir); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", home)
			socketPath := filepath.Join(dir, "app-server-control.sock")
			if len([]byte(socketPath)) >= 108 {
				t.Fatal("fixture socket path exceeds Windows AF_UNIX limit")
			}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "app-server-control.sock"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			listener.SetDeadline(time.Now().Add(4 * time.Second))
			token, err := win.OpenCurrentProcessToken()
			if err != nil {
				t.Fatal(err)
			}
			elevated := token.IsElevated()
			token.Close()
			if elevated {
				// Ownership verification succeeds on this fixture. The token check
				// at Dial must refuse either elevated endpoint before protocol I/O.
				resolver := host.Paths.(interface {
					ControlEndpoint(context.Context, string) (platform.Endpoint, error)
				})
				ep, err := resolver.ControlEndpoint(ctx, "codex")
				if err != nil || ep == "" {
					t.Fatal("control_endpoint_resolution_failed")
				}
				conn, err := host.IPC.Dial(ctx, ep)
				if conn != nil {
					conn.Close()
					t.Fatal("elevated_connection_admitted")
				}
				if !errors.Is(err, platform.ErrControlEndpointElevated) || err.Error() != platform.ErrControlEndpointElevated.Error() {
					t.Fatal("expected control_endpoint_elevated at Dial")
				}
				adapter, _ := New(provider.Config{}, host)
				rows, err := adapter.Discover(ctx)
				var failure *discoveryError
				if rows != nil || !errors.As(err, &failure) || failure.reason != "control_endpoint_elevated" || err.Error() != errDiscovery.Error() {
					t.Fatal("expected redacted control_endpoint_elevated at Discover")
				}
				return
			}
			requests := []rpcRequest{}
			done := make(chan error, 1)
			go func() {
				for i := range 2 {
					raw, e := listener.AcceptUnix()
					conn := &fakeWindowsConnection{Conn: raw}
					if e != nil {
						done <- e
						return
					}
					state := "idle"
					if i == 1 {
						state = tc.state
					}
					if e = serveFake(conn, &fakeConn{state: state, queueMode: tc.mode, requests: &requests}); e != nil {
						done <- e
						return
					}
				}
				done <- nil
			}()
			adapter, _ := New(provider.Config{}, host)
			sessions, err := adapter.Discover(ctx)
			if err != nil || len(sessions) != 1 || sessions[0].PID != os.Getpid() {
				t.Fatal(sessions, err)
			}
			result, err := adapter.Deliver(ctx, sessions[0], message())
			if err != nil || result.Status != tc.want {
				t.Fatal(result, err)
			}
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("fake control server did not complete")
			}
			queues := 0
			for _, req := range requests {
				if req.Method == "thread/queue/add" {
					queues++
				}
			}
			if queues != tc.queues {
				t.Fatal("unexpected queue request count", queues)
			}
		})
	}
}

type fakeWindowsConnection struct{ net.Conn }

func (c *fakeWindowsConnection) Peer() (platform.ProcessRef, error) {
	return platform.ProcessRef{PID: os.Getpid()}, nil
}
