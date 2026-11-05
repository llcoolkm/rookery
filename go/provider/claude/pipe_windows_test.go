//go:build windows

package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
	hostwindows "github.com/llcoolkm/rookery/go/platform/windows"
	"github.com/llcoolkm/rookery/go/provider"
	win "golang.org/x/sys/windows"
)

// This capability exists only in the synthetic fixture. Production Windows
// refuses the UNVERIFIED token record format; this test must not enable it.
type syntheticRecordAuth struct{ platform.Paths }

func (syntheticRecordAuth) ClaudeAuthToken(context.Context, platform.File) (string, error) {
	return "synthetic-test-key", nil
}

func TestWindowsPipeHandshakeAndUnverifiedRecordRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := filepath.Join(t.TempDir(), "state")
	h, err := hostwindows.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	if _, err = h.Files.PrivateState(ctx, filepath.Join(root, "sessions")); err != nil {
		t.Fatal(err)
	}
	user, err := win.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sd, err := win.SecurityDescriptorFromString("O:" + sid + "D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	sa := win.SecurityAttributes{Length: uint32(unsafe.Sizeof(win.SecurityAttributes{})), SecurityDescriptor: sd}
	address := fmt.Sprintf(`\\.\pipe\rookery-go-claude-test-%d`, os.Getpid())
	wide, _ := win.UTF16PtrFromString(address)
	pipe, err := win.CreateNamedPipe(wide, win.PIPE_ACCESS_DUPLEX|win.FILE_FLAG_FIRST_PIPE_INSTANCE, win.PIPE_TYPE_BYTE|win.PIPE_READMODE_BYTE|win.PIPE_WAIT|win.PIPE_REJECT_REMOTE_CLIENTS, 1, 65536, 65536, 0, &sa)
	if err != nil {
		t.Fatal(err)
	}
	defer win.CloseHandle(pipe)
	record, _ := json.Marshal(map[string]any{"sessionId": testID, "messagingSocketPath": address})
	if err = h.Files.AtomicWrite(ctx, root, fmt.Sprintf("sessions/%d.json", os.Getpid()), record); err != nil {
		t.Fatal(err)
	}
	h.Exec = &fakeExec{rows: []row{{PID: os.Getpid(), ID: testID, Status: "idle"}}}
	production, _ := NewWithOptions(provider.Config{}, h, Options{Executable: `C:\synthetic\claude.exe`})
	sessions, err := production.Discover(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatal(err)
	}
	result, err := production.Deliver(ctx, sessions[0], message())
	if err != nil || result.Status != "held" || result.LocalReason != "auth_unverified" {
		t.Fatal("production must refuse undocumented token source", result, err)
	}
	received := make(chan error, 1)
	go func() {
		err := win.ConnectNamedPipe(pipe, nil)
		if err != nil && err != win.ERROR_PIPE_CONNECTED {
			received <- err
			return
		}
		reader := bufio.NewReader(pipeFixtureReader{pipe})
		first, err := reader.ReadBytes('\n')
		if err != nil {
			received <- err
			return
		}
		var auth map[string]string
		if json.Unmarshal(first, &auth) != nil || auth["type"] != "auth" || auth["token"] != "synthetic-test-key" {
			received <- fmt.Errorf("invalid auth first line")
			return
		}
		second, err := reader.ReadBytes('\n')
		if err != nil {
			received <- err
			return
		}
		var frame map[string]any
		if json.Unmarshal(second, &frame) != nil || frame["session_id"] != testID {
			received <- fmt.Errorf("invalid native frame")
			return
		}
		received <- nil
	}()
	h.Paths = syntheticRecordAuth{h.Paths}
	adapter, _ := NewWithOptions(provider.Config{}, h, Options{Executable: `C:\synthetic\claude.exe`})
	sessions, err = adapter.Discover(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatal(err)
	}
	result, err = adapter.Deliver(ctx, sessions[0], message())
	if err != nil || result.Status != "submitted" {
		t.Fatal(result, err)
	}
	select {
	case err = <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("pipe fixture timed out")
	}
}

// Read directly so only the outer fixture owns the pipe handle.
type pipeFixtureReader struct{ handle win.Handle }

func (p pipeFixtureReader) Read(b []byte) (int, error) {
	var n uint32
	err := win.ReadFile(p.handle, b, &n, nil)
	return int(n), err
}
