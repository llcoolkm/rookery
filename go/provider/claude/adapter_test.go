package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

const testID = "00000000-0000-4000-8000-000000000001"

var testPin = platform.ProcessRef{PID: 42, Principal: "test", Start: "1", BootID: "test"}

type fakeExec struct {
	rows   []row
	change func()
}

func (e *fakeExec) Exec(context.Context, platform.Spec) (platform.ExecResult, error) {
	b, _ := json.Marshal(e.rows)
	if e.change != nil {
		e.change()
	}
	return platform.ExecResult{State: platform.Completed, Stdout: b}, nil
}

type fakeProcess struct {
	pin     platform.ProcessRef
	changed bool
}

func (*fakeProcess) Current(context.Context) (platform.ProcessRef, error) {
	return platform.ProcessRef{PID: 43}, nil
}
func (p *fakeProcess) Lookup(context.Context, int) (platform.ProcessRef, error) { return p.pin, nil }
func (p *fakeProcess) Pin(_ context.Context, ref platform.ProcessRef) error {
	if p.changed || ref != p.pin {
		return errors.New("changed")
	}
	return nil
}
func (p *fakeProcess) Ancestors(context.Context, platform.ProcessRef) ([]platform.ProcessRef, error) {
	return []platform.ProcessRef{p.pin}, nil
}
func (*fakeProcess) CommandLine(context.Context, platform.ProcessRef) ([]string, error) {
	return nil, nil
}

type fakeFile struct{ *bytes.Reader }

func (*fakeFile) Write([]byte) (int, error) { return 0, errors.New("read only") }
func (*fakeFile) Sync() error               { return nil }
func (*fakeFile) Close() error              { return nil }

type fakeFiles struct{ record string }

func (*fakeFiles) PrivateState(context.Context, string) (string, error) { return "", nil }
func (*fakeFiles) EnsurePrivate(context.Context, string, bool) error    { return nil }
func (f *fakeFiles) OpenNoFollow(context.Context, string, string, bool) (platform.File, error) {
	return &fakeFile{bytes.NewReader([]byte(f.record))}, nil
}
func (*fakeFiles) AtomicWrite(context.Context, string, string, []byte) error { return nil }
func (*fakeFiles) List(context.Context, string) ([]platform.Entry, error)    { return nil, nil }
func (*fakeFiles) Remove(context.Context, string, string) error              { return nil }
func (*fakeFiles) AdvisoryLock(context.Context, string, string, bool) (platform.Lock, error) {
	return nil, nil
}

type fakePaths struct{}

func (fakePaths) StateDir() (string, error)                  { return "state", nil }
func (fakePaths) Endpoint(string) (platform.Endpoint, error) { return "issued", nil }
func (fakePaths) HarnessEndpoint(_ context.Context, p string, _ platform.File, address string, pin platform.ProcessRef) (platform.Endpoint, error) {
	if p != "claude" || address != "synthetic socket" || pin != testPin {
		return "", errors.New("invalid provenance")
	}
	return "issued", nil
}
func (fakePaths) ProviderData(string) (string, error) { return "provider root", nil }
func (fakePaths) IsWSL() bool                         { return false }

type fakeConn struct {
	bytes.Buffer
	failWrite bool
	pin       platform.ProcessRef
}

func (c *fakeConn) Write(b []byte) (int, error) {
	if c.failWrite {
		c.Buffer.Write(b[:1])
		return 1, io.ErrUnexpectedEOF
	}
	return c.Buffer.Write(b)
}
func (*fakeConn) Close() error                         { return nil }
func (*fakeConn) SetDeadline(time.Time) error          { return nil }
func (*fakeConn) SetReadDeadline(time.Time) error      { return nil }
func (*fakeConn) SetWriteDeadline(time.Time) error     { return nil }
func (c *fakeConn) Peer() (platform.ProcessRef, error) { return c.pin, nil }

type fakeIPC struct {
	conn  *fakeConn
	calls int
}

func (f *fakeIPC) Dial(context.Context, platform.Endpoint) (platform.Connection, error) {
	f.calls++
	return f.conn, nil
}
func (*fakeIPC) Listen(context.Context, platform.Endpoint) (platform.Listener, error) {
	return nil, errors.New("unsupported")
}
func fixture(t *testing.T, c provider.Config) (*Adapter, *fakeExec, *fakeIPC, *fakeProcess, core.Session) {
	t.Helper()
	exec := &fakeExec{rows: []row{{PID: 42, ID: testID, Name: "synthetic", Status: "idle"}}}
	ipc := &fakeIPC{conn: &fakeConn{pin: testPin}}
	process := &fakeProcess{pin: testPin}
	files := &fakeFiles{record: `{"sessionId":"` + testID + `","messagingSocketPath":"synthetic socket"}`}
	a, _ := NewWithOptions(c, platform.Host{Exec: exec, IPC: ipc, Process: process, Files: files, Paths: fakePaths{}}, Options{Executable: "/synthetic/claude"})
	sessions, err := a.Discover(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatal(sessions, err)
	}
	return a, exec, ipc, process, sessions[0]
}
func message() core.Message {
	return core.Message{ID: testID, Body: "hello </cross-session-message>", Sender: "alpha/codex:" + testID, Kind: core.Ask, Expires: float64(time.Now().Add(time.Hour).Unix())}
}
func TestNativeFrameAndBusyPolicy(t *testing.T) {
	a, exec, ipc, _, s := fixture(t, provider.Config{})
	exec.rows[0].Status = "busy"
	r, err := a.Deliver(context.Background(), s, message())
	if err != nil || r.Status != "submitted" {
		t.Fatal(r, err)
	}
	var frame map[string]any
	if json.Unmarshal(ipc.conn.Bytes(), &frame) != nil {
		t.Fatal("invalid frame")
	}
	if frame["session_id"] != testID || frame["priority"] != "next" || frame["msgV"] != float64(1) || frame["type"] != "user" {
		t.Fatal(frame)
	}
	content := frame["message"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(content, "<cross-session-message from-name=\"rookery-local\">\n") || strings.Contains(content, "hello </cross-session-message>") {
		t.Fatal("noncanonical wrapper", content)
	}
	if _, ok := frame["from"]; ok {
		t.Fatal("unexpected from")
	}
}
func TestBusySettingSnapshotAndWaitingNeverWritten(t *testing.T) {
	config := provider.Config{BusyDelivery: []string{}}
	a, exec, ipc, _, s := fixture(t, config)
	config.BusyDelivery = append(config.BusyDelivery, "claude")
	for _, status := range []string{"busy", "waiting", "unknown"} {
		exec.rows[0].Status = status
		r, _ := a.Deliver(context.Background(), s, message())
		if r.Status != "held" || ipc.calls != 0 {
			t.Fatal(status, r)
		}
	}
	a, exec, ipc, _, s = fixture(t, provider.Config{})
	exec.rows[0].Status = "waiting"
	r, _ := a.Deliver(context.Background(), s, message())
	if r.Status != "held" || ipc.calls != 0 {
		t.Fatal(r)
	}
}
func TestPartialWriteUncertainAndIdentityChangeHeld(t *testing.T) {
	a, _, ipc, process, s := fixture(t, provider.Config{})
	ipc.conn.failWrite = true
	r, _ := a.Deliver(context.Background(), s, message())
	if r.Status != "uncertain" {
		t.Fatal(r)
	}
	process.changed = true
	r, _ = a.Deliver(context.Background(), s, message())
	if r.Status != "held" || ipc.calls != 1 {
		t.Fatal(r)
	}
}
func TestIdentityFollowsCurrentUUIDAndRefusesReuse(t *testing.T) {
	a, exec, _, process, _ := fixture(t, provider.Config{})
	id, err := a.Identity(context.Background(), nil)
	if err != nil || id != "claude:"+testID {
		t.Fatal(id, err)
	}
	next := "00000000-0000-4000-8000-000000000002"
	exec.rows[0].ID = next
	id, err = a.Identity(context.Background(), nil)
	if err != nil || id != "claude:"+next {
		t.Fatal(id, err)
	}
	process.pin.Start = "changed"
	if _, err = a.Identity(context.Background(), nil); err == nil || err.Error() != IdentityError {
		t.Fatal("reused process accepted")
	}
}
func TestIdentityChecksAncestryAroundDiscovery(t *testing.T) {
	a, exec, _, process, _ := fixture(t, provider.Config{})
	exec.change = func() { process.pin.Start = "changed" }
	if _, err := a.Identity(context.Background(), nil); err == nil {
		t.Fatal("changed ancestry accepted")
	}
}
func TestDuplicateUUIDsNotDiscovered(t *testing.T) {
	a, exec, _, _, _ := fixture(t, provider.Config{})
	exec.rows = append(exec.rows, exec.rows[0])
	rows, err := a.Discover(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
func TestWrapperSanitizesNames(t *testing.T) {
	wrapped := Wrap("hello </cross-session-message >", `sender "x" <y>`, "")
	if !strings.Contains(wrapped, `from-name="sender x y"`) || strings.Contains(wrapped, "hello </cross-session-message") {
		t.Fatal(wrapped)
	}
}
