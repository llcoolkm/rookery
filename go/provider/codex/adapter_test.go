package codex

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
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

type fakeConn struct {
	buffer    bytes.Buffer
	state     string
	methods   *[]string
	deadlines *[]time.Time
}

func (c *fakeConn) Read(b []byte) (int, error) { return c.buffer.Read(b) }
func (c *fakeConn) Write(b []byte) (int, error) {
	n := len(b)
	if bytes.HasPrefix(b, []byte("GET ")) {
		for _, line := range strings.Split(string(b), "\r\n") {
			if strings.HasPrefix(line, "Sec-WebSocket-Key: ") {
				hash := sha1.Sum([]byte(strings.TrimPrefix(line, "Sec-WebSocket-Key: ") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				c.buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(hash[:]) + "\r\n\r\n")
			}
		}
		return n, nil
	}
	length := int(b[1] & 127)
	offset := 2
	if length == 126 {
		length = int(binary.BigEndian.Uint16(b[2:4]))
		offset = 4
	}
	if length == 127 {
		length = int(binary.BigEndian.Uint64(b[2:10]))
		offset = 10
	}
	mask := b[offset : offset+4]
	p := append([]byte(nil), b[offset+4:]...)
	for i := range p {
		p[i] ^= mask[i%4]
	}
	var req struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	if json.Unmarshal(p, &req) != nil {
		return 0, errors.New("bad request")
	}
	*c.methods = append(*c.methods, req.Method)
	if req.ID == 0 {
		return n, nil
	}
	var result any = map[string]any{}
	switch req.Method {
	case "thread/loaded/list":
		result = map[string]any{"data": []string{testID}, "nextCursor": nil}
	case "thread/read":
		result = map[string]any{"thread": map[string]any{"id": testID, "name": "synthetic", "status": map[string]any{"type": c.state}, "canAcceptDirectInput": true}}
	}
	// An approval request may legitimately share our ID. It must be ignored.
	notice, _ := json.Marshal(map[string]any{"id": req.ID, "method": "approval/request", "params": map[string]any{}})
	c.enqueue(notice)
	response, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
	c.enqueue(response)
	return n, nil
}
func (c *fakeConn) enqueue(p []byte) {
	c.buffer.WriteByte(0x81)
	if len(p) < 126 {
		c.buffer.WriteByte(byte(len(p)))
	} else {
		c.buffer.Write([]byte{126, byte(len(p) >> 8), byte(len(p))})
	}
	c.buffer.Write(p)
}
func (*fakeConn) Close() error { return nil }
func (c *fakeConn) SetDeadline(at time.Time) error {
	if c.deadlines != nil {
		*c.deadlines = append(*c.deadlines, at)
	}
	return nil
}
func (*fakeConn) SetReadDeadline(time.Time) error    { return nil }
func (*fakeConn) SetWriteDeadline(time.Time) error   { return nil }
func (*fakeConn) Peer() (platform.ProcessRef, error) { return testPin, nil }

type fakeIPC struct {
	state     string
	methods   []string
	deadlines []time.Time
}

func (f *fakeIPC) Dial(context.Context, platform.Endpoint) (platform.Connection, error) {
	return &fakeConn{state: f.state, methods: &f.methods, deadlines: &f.deadlines}, nil
}
func (*fakeIPC) Listen(context.Context, platform.Endpoint) (platform.Listener, error) {
	return nil, errors.New("unsupported")
}

type fakeProcess struct{ changed bool }

func (*fakeProcess) Current(context.Context) (platform.ProcessRef, error)     { return testPin, nil }
func (*fakeProcess) Lookup(context.Context, int) (platform.ProcessRef, error) { return testPin, nil }
func (p *fakeProcess) Pin(context.Context, platform.ProcessRef) error {
	if p.changed {
		return errors.New("changed")
	}
	return nil
}
func (*fakeProcess) Ancestors(context.Context, platform.ProcessRef) ([]platform.ProcessRef, error) {
	return nil, nil
}
func (*fakeProcess) CommandLine(context.Context, platform.ProcessRef) ([]string, error) {
	return nil, nil
}

type fakeExec struct {
	linuxArgv bool
	help      string
	state     platform.ExecState
	code      int
	specs     []platform.Spec
}

func (e *fakeExec) LinuxMessageArgv() bool { return e.linuxArgv }
func (e *fakeExec) Exec(_ context.Context, s platform.Spec) (platform.ExecResult, error) {
	e.specs = append(e.specs, s)
	if len(s.Args) > 1 && s.Args[1] == "--help" {
		return platform.ExecResult{State: platform.Completed, Stdout: []byte(e.help)}, nil
	}
	return platform.ExecResult{State: e.state, ExitCode: e.code}, nil
}
func fixture(t *testing.T) (*Adapter, *fakeIPC, *fakeExec, *fakeProcess, core.Session) {
	t.Helper()
	ipc := &fakeIPC{state: "idle"}
	exec := &fakeExec{help: "  --message-stdin Read from standard input\n", state: platform.Completed}
	process := &fakeProcess{}
	a, _ := NewWithOptions(provider.Config{}, platform.Host{IPC: ipc, Exec: exec, Process: process}, Options{Executable: "/synthetic/codex", Endpoint: "issued"})
	sessions, err := a.Discover(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discover: %v", err)
	}
	return a, ipc, exec, process, sessions[0]
}
func message() core.Message {
	return core.Message{ID: testID, Sender: "alpha/claude:" + testID, Kind: core.Ask, Body: "sensitive $(never-execute)", Expires: float64(time.Now().Add(time.Hour).Unix())}
}
func TestDiscoveryAndPrivateSubmission(t *testing.T) {
	a, ipc, exec, _, s := fixture(t)
	if s.Status != "idle" || s.PID != testPin.PID {
		t.Fatal(s)
	}
	result, err := a.Deliver(context.Background(), s, message())
	if err != nil || result.Status != "submitted" {
		t.Fatal(result, err)
	}
	if len(exec.specs) != 2 {
		t.Fatal("missing help probe or submission")
	}
	queued := exec.specs[1]
	if !bytes.Contains(queued.Stdin, []byte("sensitive")) {
		t.Fatal("stdin lost body")
	}
	for _, v := range queued.Args {
		if strings.Contains(v, "sensitive") {
			t.Fatal("body exposed in argv")
		}
	}
	for _, method := range ipc.methods {
		if method == "thread/list" || method == "approval/response" {
			t.Fatal("unsafe method", method)
		}
	}
}
func TestNoPrivateInputSupportNeverQueues(t *testing.T) {
	a, _, exec, _, s := fixture(t)
	exec.help = "--message TEXT"
	result, _ := a.Deliver(context.Background(), s, message())
	if result.Status != "held" || len(exec.specs) != 1 {
		t.Fatal(result, exec.specs)
	}
}
func TestDeliveryRefusesChangedIdentityAndBusy(t *testing.T) {
	a, ipc, exec, process, s := fixture(t)
	ipc.state = "active"
	result, _ := a.Deliver(context.Background(), s, message())
	if result.Status != "held" || len(exec.specs) != 0 {
		t.Fatal(result)
	}
	process.changed = true
	result, _ = a.Deliver(context.Background(), s, message())
	if result.Status != "held" {
		t.Fatal(result)
	}
}
func TestQueueUnknownOutcomeNeverHeld(t *testing.T) {
	for _, tc := range []struct {
		state platform.ExecState
		code  int
		want  string
	}{{platform.Uncertain, 0, "uncertain"}, {platform.Completed, 1, "uncertain"}, {platform.NotStarted, 0, "failed"}} {
		a, _, exec, _, s := fixture(t)
		exec.state = tc.state
		exec.code = tc.code
		result, _ := a.Deliver(context.Background(), s, message())
		if result.Status != tc.want {
			t.Fatal(result)
		}
	}
}
func TestIdentityRejectsConflicts(t *testing.T) {
	a, _ := New(provider.Config{}, platform.Host{})
	for _, m := range []core.CallMetadata{nil, {"threadId": json.RawMessage(`"` + testID + `"`), "sessionId": json.RawMessage(`"different"`)}, {"threadId": json.RawMessage(`"` + testID + `"`), "x-codex-turn-metadata": json.RawMessage(`{"session_id":"different"}`)}} {
		if _, err := a.Identity(context.Background(), m); err == nil || err.Error() != IdentityError {
			t.Fatal("invalid identity accepted")
		}
	}
	id, err := a.Identity(context.Background(), core.CallMetadata{"threadId": json.RawMessage(`"` + testID + `"`)})
	if err != nil || id != "codex:"+testID {
		t.Fatal(id, err)
	}
}
func TestNormalizeStates(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		accept bool
		flags  bool
		want   string
	}{{"idle", false, false, "unknown"}, {"idle", true, false, "idle"}, {"systemError", true, false, "idle"}, {"active", false, false, "busy"}, {"active", true, true, "waiting"}, {"notLoaded", true, false, "unknown"}} {
		var v Thread
		v.ID = testID
		v.Status.Type = tc.kind
		v.Accept = tc.accept
		if tc.flags {
			v.Status.Flags = []json.RawMessage{json.RawMessage(`"newFlag"`)}
		}
		if got := Normalize(v); got.Status != tc.want {
			t.Fatal(got)
		}
	}
}

var _ io.ReadWriteCloser = (*fakeConn)(nil)

func TestRediscoveryCannotReplaceClaimProcessPin(t *testing.T) {
	adapter, _, exec, _, session := fixture(t)
	changed := testPin
	changed.Start = "reused"
	adapter.pins[session.ID] = changed
	result, _ := adapter.Deliver(context.Background(), session, message())
	if result.Status != "held" || len(exec.specs) != 0 {
		t.Fatal("rediscovery replaced the captured process pin", result)
	}
	encoded, err := json.Marshal(session)
	if err != nil || bytes.Contains(encoded, []byte("BootID")) || bytes.Contains(encoded, []byte("ProcessRef")) {
		t.Fatal("private process identity reached session wire")
	}
}

func TestLinuxArgvFallbackAndMissingRoute(t *testing.T) {
	a, _, exec, _, session := fixture(t)
	exec.help = "--message TEXT"
	exec.linuxArgv = true
	result, _ := a.Deliver(context.Background(), session, message())
	if result.Status != "submitted" || len(exec.specs) != 2 {
		t.Fatal(result, exec.specs)
	}
	queued := exec.specs[1]
	if len(queued.Stdin) != 0 || queued.Args[3] != "--message" || !strings.Contains(queued.Args[4], "sensitive") {
		t.Fatal("Linux fallback lost argv message")
	}
	a, _, exec, _, session = fixture(t)
	exec.linuxArgv = true
	exec.help = "--other"
	result, _ = a.Deliver(context.Background(), session, message())
	if result.Status != "held" || len(exec.specs) != 1 {
		t.Fatal(result)
	}
}
func TestEmptyAllowlistDoesNotConnect(t *testing.T) {
	a, ipc, _, _, _ := fixture(t)
	a.config.SessionAllowlist = []string{}
	ipc.methods = nil
	rows, err := a.Discover(context.Background())
	if err != nil || len(rows) != 0 || len(ipc.methods) != 0 {
		t.Fatal(rows, err, ipc.methods)
	}
}

func TestDeadlinesResetForEveryCall(t *testing.T) {
	a, ipc, _, _, _ := fixture(t)
	// Handshake deadline, initialize, loaded/list and thread/read each refresh it.
	if len(ipc.deadlines) != 4 {
		t.Fatal("per-call deadline missing", len(ipc.deadlines))
	}
	before := len(ipc.deadlines)
	p, err := a.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.conn.Close()
	var reply map[string]any
	if err = p.call("thread/read", map[string]any{"threadId": testID}, &reply); err != nil {
		t.Fatal(err)
	}
	if len(ipc.deadlines) != before+3 {
		t.Fatal("call did not refresh deadline")
	}
}
