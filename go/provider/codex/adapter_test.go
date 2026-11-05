package codex

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

const testID = "00000000-0000-4000-8000-000000000001"

var testPin = platform.ProcessRef{PID: 42, Principal: "test", Start: "1", BootID: "test"}

type rpcRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type fakeConn struct {
	buffer      bytes.Buffer
	state       string
	queueMode   string
	requests    *[]rpcRequest
	deadlines   *[]time.Time
	onRead      func()
	initialized bool
	ready       bool
	closed      bool
}

func (c *fakeConn) Read(b []byte) (int, error) { return c.buffer.Read(b) }
func (c *fakeConn) Write(b []byte) (int, error) {
	if c.closed {
		return 0, io.ErrClosedPipe
	}
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
		offset = 4
	}
	if length == 127 {
		offset = 10
	}
	mask := b[offset : offset+4]
	p := append([]byte(nil), b[offset+4:]...)
	for i := range p {
		p[i] ^= mask[i%4]
	}
	var req rpcRequest
	if json.Unmarshal(p, &req) != nil {
		return 0, errors.New("bad request")
	}
	*c.requests = append(*c.requests, req)
	var result any = map[string]any{}
	switch req.Method {
	case "initialize":
		var params struct {
			Capabilities struct {
				ExperimentalAPI bool `json:"experimentalApi"`
			} `json:"capabilities"`
		}
		if json.Unmarshal(req.Params, &params) != nil || !params.Capabilities.ExperimentalAPI {
			return 0, errors.New("experimental opt-in required")
		}
		c.initialized = true
	case "initialized":
		if !c.initialized || req.ID != 0 {
			return 0, errors.New("initialization order invalid")
		}
		c.ready = true
		return n, nil
	case "thread/loaded/list":
		if !c.ready {
			return 0, errors.New("not initialized")
		}
		result = map[string]any{"data": []string{testID}, "nextCursor": nil}
	case "thread/read":
		if !c.ready {
			return 0, errors.New("not initialized")
		}
		result = map[string]any{"thread": map[string]any{"id": testID, "name": "synthetic", "status": map[string]any{"type": c.state}, "canAcceptDirectInput": true}}
		if c.onRead != nil {
			c.onRead()
		}
	case "thread/queue/add":
		if !c.ready {
			return 0, errors.New("not initialized")
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(req.Params, &params) != nil {
			return 0, errors.New("invalid queue parameters")
		}
		switch c.queueMode {
		case "lost":
			return n, nil
		case "partial-write":
			return n / 2, io.ErrClosedPipe
		case "error", "unsupported", "experimental", "opt-in-rejected":
			code := -32603
			if c.queueMode == "unsupported" {
				code = -32601
			}
			if c.queueMode == "experimental" || c.queueMode == "opt-in-rejected" {
				code = -32600
			}
			remoteMessage := "synthetic private remote detail"
			if c.queueMode == "opt-in-rejected" {
				remoteMessage = "thread/queue/add requires experimentalApi capability"
			}
			response, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": code, "message": remoteMessage, "data": map[string]any{"synthetic": true}}})
			c.enqueue(response)
			return n, nil
		case "malformed":
			result = map[string]any{"queuedSubmission": map[string]any{"id": "synthetic-queue"}}
		default:
			clientID := params["clientUserMessageId"]
			if c.queueMode == "mismatched" {
				clientID = json.RawMessage(`"different"`)
			}
			result = map[string]any{"queuedSubmission": map[string]any{"id": "synthetic-queue", "input": params["input"], "clientUserMessageId": clientID}}
		}
	default:
		return 0, errors.New("unexpected method")
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
func (c *fakeConn) Close() error { c.closed = true; return nil }
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
	queueMode string
	requests  []rpcRequest
	deadlines []time.Time
	onRead    func()
}

func (f *fakeIPC) Dial(context.Context, platform.Endpoint) (platform.Connection, error) {
	return &fakeConn{state: f.state, queueMode: f.queueMode, requests: &f.requests, deadlines: &f.deadlines, onRead: f.onRead}, nil
}
func (*fakeIPC) Listen(context.Context, platform.Endpoint) (platform.Listener, error) {
	return nil, errors.New("unsupported")
}
func (f *fakeIPC) queueCount() int {
	n := 0
	for _, req := range f.requests {
		if req.Method == "thread/queue/add" {
			n++
		}
	}
	return n
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

func fixture(t *testing.T) (*Adapter, *fakeIPC, *fakeProcess, core.Session) {
	t.Helper()
	ipc := &fakeIPC{state: "idle"}
	process := &fakeProcess{}
	a, _ := NewWithOptions(provider.Config{}, platform.Host{IPC: ipc, Process: process}, Options{Endpoint: "issued"})
	sessions, err := a.Discover(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discover: %v", err)
	}
	return a, ipc, process, sessions[0]
}
func message() core.Message {
	return core.Message{ID: testID, Sender: "alpha/claude:" + testID, Kind: core.Ask, Body: "sensitive $(never-execute)", Expires: float64(time.Now().Add(time.Hour).Unix())}
}
func TestDiscoveryAndRPCSubmission(t *testing.T) {
	a, ipc, _, s := fixture(t)
	if s.Status != "idle" || s.PID != testPin.PID {
		t.Fatal(s)
	}
	m := message()
	result, err := a.Deliver(context.Background(), s, m)
	if err != nil || result.Status != "submitted" {
		t.Fatal(result, err)
	}
	wantMethods := []string{"initialize", "initialized", "thread/loaded/list", "thread/read", "initialize", "initialized", "thread/read", "thread/queue/add"}
	if len(ipc.requests) != len(wantMethods) {
		t.Fatal(ipc.requests)
	}
	for i, want := range wantMethods {
		if ipc.requests[i].Method != want {
			t.Fatal(ipc.requests)
		}
	}
	var got map[string]any
	if json.Unmarshal(ipc.requests[len(ipc.requests)-1].Params, &got) != nil {
		t.Fatal("invalid queue params")
	}
	want := map[string]any{"threadId": testID, "clientUserMessageId": m.ID, "input": []any{map[string]any{"type": "text", "text": core.Render(m), "text_elements": []any{}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong queue parameters: %#v", got)
	}
}

func TestDeliveryRefusesChangedIdentityAndNonIdle(t *testing.T) {
	for _, state := range []string{"active", "notLoaded", "unknown"} {
		t.Run(state, func(t *testing.T) {
			a, ipc, _, s := fixture(t)
			ipc.state = state
			result, _ := a.Deliver(context.Background(), s, message())
			if result.Status != "held" || ipc.queueCount() != 0 {
				t.Fatal(result, ipc.requests)
			}
		})
	}
	a, ipc, process, s := fixture(t)
	process.changed = true
	result, _ := a.Deliver(context.Background(), s, message())
	if result.Status != "held" || ipc.queueCount() != 0 {
		t.Fatal(result)
	}
}

func TestFinalPinRevalidation(t *testing.T) {
	a, ipc, process, s := fixture(t)
	ipc.onRead = func() { process.changed = true }
	result, _ := a.Deliver(context.Background(), s, message())
	if result.Status != "held" || ipc.queueCount() != 0 {
		t.Fatal(result, ipc.requests)
	}
}

func TestExpiredAndPollingNeverSubmit(t *testing.T) {
	for _, polling := range []bool{false, true} {
		a, ipc, _, s := fixture(t)
		m := message()
		if polling {
			s.Delivery = "polling"
		} else {
			m.Expires = 0
		}
		result, _ := a.Deliver(context.Background(), s, m)
		if result.Status != "held" || ipc.queueCount() != 0 {
			t.Fatal(result)
		}
	}
}

func TestQueueUnknownOutcomeNeverRetries(t *testing.T) {
	for _, mode := range []string{"error", "experimental", "lost", "partial-write", "malformed", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			a, ipc, _, s := fixture(t)
			ipc.queueMode = mode
			result, err := a.Deliver(context.Background(), s, message())
			if err != nil || result.Status != "uncertain" || ipc.queueCount() != 1 {
				t.Fatal(result, err, ipc.requests)
			}
			if strings.Contains(result.Detail, "private remote detail") {
				t.Fatal("untrusted remote error leaked")
			}
		})
	}
}

func TestUnsupportedQueueHeld(t *testing.T) {
	for _, mode := range []string{"unsupported", "opt-in-rejected"} {
		a, ipc, _, s := fixture(t)
		ipc.queueMode = mode
		result, err := a.Deliver(context.Background(), s, message())
		if err != nil || result.Status != "held" || ipc.queueCount() != 1 {
			t.Fatal(result, err, ipc.requests)
		}
	}
}

func TestCancelledBeforeSubmissionHeld(t *testing.T) {
	a, ipc, _, s := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ipc.onRead = cancel
	result, err := a.Deliver(ctx, s, message())
	if err != nil || result.Status != "held" || ipc.queueCount() != 0 {
		t.Fatal(result, err, ipc.requests)
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
	adapter, ipc, _, session := fixture(t)
	changed := testPin
	changed.Start = "reused"
	adapter.pins[session.ID] = changed
	result, _ := adapter.Deliver(context.Background(), session, message())
	if result.Status != "held" || ipc.queueCount() != 0 {
		t.Fatal("rediscovery replaced the captured process pin", result)
	}
	encoded, err := json.Marshal(session)
	if err != nil || bytes.Contains(encoded, []byte("BootID")) || bytes.Contains(encoded, []byte("ProcessRef")) {
		t.Fatal("private process identity reached session wire")
	}
}

func TestEmptyAllowlistDoesNotConnect(t *testing.T) {
	a, ipc, _, _ := fixture(t)
	a.config.SessionAllowlist = []string{}
	ipc.requests = nil
	rows, err := a.Discover(context.Background())
	if err != nil || len(rows) != 0 || len(ipc.requests) != 0 {
		t.Fatal(rows, err, ipc.requests)
	}
}

func TestDeadlinesResetForEveryCall(t *testing.T) {
	a, ipc, _, _ := fixture(t)
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
