//go:build linux

package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/platform/linux"
	"github.com/llcoolkm/rookery/go/provider"
)

// TestMessagingSocket exercises the documented frame against a real AF_UNIX
// listener with kernel peer credentials; discovery and all records are synthetic.
func TestMessagingSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	host, err := linux.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	address := filepath.Join(root, "inbox.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{"sessionId": testID, "messagingSocketPath": address})
	if err = host.Files.AtomicWrite(ctx, root, fmt.Sprintf("sessions/%d.json", os.Getpid()), record); err != nil {
		t.Fatal(err)
	}
	host.Exec = &fakeExec{rows: []row{{PID: os.Getpid(), ID: testID, Name: "synthetic", Status: "idle"}}}
	adapter, _ := NewWithOptions(provider.Config{}, host, Options{Executable: "/synthetic/claude"})
	sessions, err := adapter.Discover(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatal(sessions, err)
	}
	received := make(chan []byte, 1)
	go func() {
		conn, e := listener.AcceptUnix()
		if e != nil {
			received <- nil
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		b, _ := bufio.NewReader(conn).ReadBytes('\n')
		received <- b
	}()
	result, err := adapter.Deliver(ctx, sessions[0], message())
	if err != nil || result.Status != "submitted" {
		t.Fatal(result, err)
	}
	select {
	case b := <-received:
		var frame map[string]any
		if json.Unmarshal(b, &frame) != nil || frame["session_id"] != testID || frame["priority"] != "next" {
			t.Fatal("invalid native frame")
		}
	case <-ctx.Done():
		t.Fatal("fake socket did not receive frame")
	}
}
