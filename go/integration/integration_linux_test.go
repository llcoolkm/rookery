//go:build linux

package integration_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	linuxhost "github.com/llcoolkm/rookery/go/platform/linux"
)

const goSession = "00000000-0000-4000-8000-000000000001"
const pythonSession = "00000000-0000-4000-8000-000000000002"
const goActor = "alpha/claude:" + goSession

type peer struct {
	in  io.WriteCloser
	out *bufio.Reader
}

func (p *peer) call(t *testing.T, op string, args map[string]any) any {
	t.Helper()
	request := map[string]any{"op": op}
	for k, v := range args {
		request[k] = v
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal("cannot encode isolated peer request")
	}
	if _, err = p.in.Write(append(raw, '\n')); err != nil {
		t.Fatal("cannot write isolated Python peer request")
	}
	type response struct {
		OK     bool
		Result any
	}
	line, err := p.out.ReadBytes('\n')
	if err != nil {
		t.Fatal("isolated Python peer exited")
	}
	var r response
	if json.Unmarshal(line, &r) != nil || !r.OK {
		t.Fatal("isolated Python peer rejected operation")
	}
	return r.Result
}
func (p *peer) invoke(t *testing.T, method string, args map[string]any) any {
	return p.call(t, "invoke", map[string]any{"method": method, "args": args})
}
func reserveAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal("cannot reserve local test port")
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func syntheticToken(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		t.Fatal("cannot generate test credential")
	}
	return hex.EncodeToString(b[:])
}
func checked(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatalf("integration operation failed: %v", e)
	}
}
func privateDir(t *testing.T, path string) {
	t.Helper()
	checked(t, os.MkdirAll(path, 0700))
	checked(t, os.Chmod(path, 0700))
}
func object(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatal("expected result object")
	}
	return m
}
func invoke(t *testing.T, ctx context.Context, c *core.Client, op string, args map[string]any) map[string]any {
	t.Helper()
	v, e := c.Invoke(ctx, op, args)
	checked(t, e)
	return object(t, v)
}
func message(t *testing.T, ctx context.Context, c *core.Client, id string) map[string]any {
	t.Helper()
	return invoke(t, ctx, c, "status", map[string]any{"message_id": id})
}
func pending(t *testing.T, v any) string {
	t.Helper()
	m := object(t, v)
	id, ok := m["id"].(string)
	if !ok || id == "" {
		t.Fatal("queued message missing identity")
	}
	return id
}

type loseResponse struct {
	inner core.HubClient
	lost  bool
}

func (l *loseResponse) Call(ctx context.Context, r core.RPCRequest) (json.RawMessage, error) {
	out, e := l.inner.Call(ctx, r)
	if e == nil && r.Op == "send" && !l.lost {
		l.lost = true
		return nil, errors.New("synthetic lost response")
	}
	return out, e
}

// TestPythonGoHelpers starts the real Python hub CLI and real helper engines.
// Both helper roots and the entire child-process HOME/XDG environment are private
// temporary directories. Discovery is synthetic and no native provider is run.
func TestPythonGoHelpers(t *testing.T) {
	if _, e := exec.LookPath("uv"); e != nil {
		t.Fatal("Linux integration requires uv and the Python hub extra")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	checked(t, os.Chmod(root, 0700))
	home := filepath.Join(root, "home")
	privateDir(t, home)
	project, e := filepath.Abs("../..")
	checked(t, e)
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "ROOKERY_HOME", "ROOKERY_GO_HOME", "ROOKERY_SESSION", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "UV_CACHE_DIR", "UV_PROJECT_ENVIRONMENT", "PYTHONPATH", "PYTHONHOME":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_CACHE_HOME="+filepath.Join(root, "cache"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "CODEX_HOME="+filepath.Join(root, "codex"), "CLAUDE_CONFIG_DIR="+filepath.Join(root, "claude"), "UV_CACHE_DIR="+filepath.Join(root, "uv-cache"), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, "venv"), "PYTHONNOUSERSITE=1")
	alpha, beta := syntheticToken(t), syntheticToken(t)
	ah, bh := sha256.Sum256([]byte(alpha)), sha256.Sum256([]byte(beta))
	hashes, _ := json.Marshal(map[string]string{"alpha": hex.EncodeToString(ah[:]), "beta": hex.EncodeToString(bh[:])})
	credentials := filepath.Join(root, "credentials.json")
	checked(t, os.WriteFile(credentials, hashes, 0600))
	address, metrics := reserveAddress(t), reserveAddress(t)
	for metrics == address {
		metrics = reserveAddress(t)
	}
	url := "http://" + address
	hub := exec.CommandContext(ctx, "uv", "run", "--frozen", "--extra", "hub", "rookery", "hub", "serve", "--db", filepath.Join(root, "hub.sqlite3"), "--tokens", credentials, "--listen", address, "--metrics-listen", metrics)
	hub.Dir = project
	hub.Env = env
	hub.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	checked(t, hub.Start())
	defer func() { _ = syscall.Kill(-hub.Process.Pid, syscall.SIGKILL); _ = hub.Wait() }()
	remote, e := core.NewHTTPHub(url, alpha, true)
	checked(t, e)
	ready := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		var value any
		if core.RPC(ctx, remote, "list", map[string]any{}, &value) == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("local Python hub startup timed out")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatal("local Python hub did not become ready")
	}
	py := exec.CommandContext(ctx, "uv", "run", "--frozen", "--extra", "hub", "python", "scripts/go_integration_peer.py")
	py.Dir = project
	py.Env = env
	py.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, e := py.StdinPipe()
	checked(t, e)
	output, e := py.StdoutPipe()
	checked(t, e)
	checked(t, py.Start())
	defer func() { _ = input.Close(); _ = syscall.Kill(-py.Process.Pid, syscall.SIGKILL); _ = py.Wait() }()
	python := &peer{input, bufio.NewReader(output)}
	python.call(t, "setup", map[string]any{"root": filepath.Join(root, "python-helper"), "url": url, "token": beta})
	host, e := linuxhost.New(filepath.Join(root, "go-helper"))
	checked(t, e)
	store, e := core.NewLocalStore(ctx, host.Files, filepath.Join(root, "go-helper"))
	checked(t, e)
	client := &core.Client{Store: store, Config: core.Config{Node: "alpha", URL: url, Token: alpha, AllowHTTP: true}, Actor: "claude:" + goSession, Timeout: 0}
	nativeEnabled := false
	nativeSubmissions := 0
	helper := &core.Helper{Store: store, Hub: remote, Node: "alpha", Native: core.NativeHooks{Discover: func(context.Context) ([]core.Session, map[string]string) {
		s := core.Session{Provider: "claude", ID: goSession, Name: "synthetic polling peer", Status: "idle", Delivery: "polling", Admission: "none", PID: os.Getpid()}
		if nativeEnabled {
			s.Delivery = ""
			s.Admission = "idle"
		}
		return []core.Session{s}, map[string]string{}
	}, Ready: func(s core.Session) bool { return s.Delivery != "polling" && s.Status == "idle" }, Submit: func(context.Context, core.Session, core.Message) (core.DeliveryResult, error) {
		nativeSubmissions++
		return core.DeliveryResult{Status: "uncertain"}, nil
	}}}
	cycle := func() { checked(t, helper.Cycle(ctx)); python.call(t, "cycle", nil); checked(t, helper.Cycle(ctx)) }
	cycle()
	invoke(t, ctx, client, "enroll", map[string]any{"target": "claude:" + goSession, "alias": "alpha"})
	python.invoke(t, "enroll", map[string]any{"target": "codex:" + pythonSession, "alias": "beta"})
	cycle()

	t.Run("cross_delivery_and_read_receipts", func(t *testing.T) {
		sent := invoke(t, ctx, client, "send", map[string]any{"to": "beta", "body": "Go to Python: åäö", "kind": "ask"})
		id := pending(t, sent)
		cycle()
		rows := python.invoke(t, "inbox", map[string]any{"peek": true}).([]any)
		found := false
		for _, r := range rows {
			m := object(t, r)
			if m["id"] == id {
				found = true
				if m["body"] != "Go to Python: åäö" || m["status"] != "pending" {
					t.Fatal("cross delivery changed message")
				}
			}
		}
		if !found {
			t.Fatal("Go message missing from Python inbox")
		}
		// Peek neither reads nor dispatches a polling session.
		before := message(t, ctx, client, id)
		if before["read_at"] != nil {
			t.Fatal("peek marked message read")
		}
		if before["received_by_host"] != true {
			t.Fatal("Python host did not report durable ingestion")
		}
		python.invoke(t, "inbox", nil)
		cycle()
		after := message(t, ctx, client, id)
		if after["read_at"] == nil || after["status"] != "read" {
			t.Fatal("Python inbox did not emit read receipt")
		}
		replyID := pending(t, python.invoke(t, "reply", map[string]any{"message_id": id, "body": "Python reply"}))
		cycle()
		got := message(t, ctx, client, replyID)
		if got["recipient"] != goActor || got["kind"] != "reply" || got["body"] != "Python reply" {
			t.Fatal("Python reply missing in Go helper")
		}
		_, e := client.Invoke(ctx, "inbox", map[string]any{})
		checked(t, e)
		cycle()
		invoke(t, ctx, client, "ack", map[string]any{"message_id": replyID})
		cycle()
		status := object(t, python.invoke(t, "status", map[string]any{"message_id": replyID}))
		if status["status"] != "acknowledged" || status["read_at"] == nil {
			t.Fatal("Go read/ack receipt missing at Python sender")
		}
		reverseID := pending(t, python.invoke(t, "send", map[string]any{"to": "alpha", "body": "Python to Go", "kind": "inform"}))
		cycle()
		if message(t, ctx, client, reverseID)["body"] != "Python to Go" {
			t.Fatal("Python send missing in Go inbox")
		}
	})

	t.Run("lost_response_retains_identity_and_retry_is_idempotent", func(t *testing.T) {
		sent := invoke(t, ctx, client, "send", map[string]any{"to": "beta", "body": "exactly one durable message"})
		id := pending(t, sent)
		loss := &loseResponse{inner: remote}
		helper.Hub = loss
		if helper.Requests(ctx) == nil {
			t.Fatal("lost response was treated as confirmed")
		}
		commands, e := store.Commands(ctx, 0)
		checked(t, e)
		if len(commands) != 1 {
			t.Fatal("unknown outcome command was not retained")
		}
		requestID := commands[0].ID
		checked(t, helper.Requests(ctx))
		remaining, e := store.Commands(ctx, 0)
		checked(t, e)
		if len(remaining) != 0 {
			t.Fatal("confirmed retry stayed queued")
		}
		result, ok, e := store.Result(ctx, requestID)
		checked(t, e)
		if !ok || !result.OK {
			t.Fatal("retry did not preserve original request result")
		}
		helper.Hub = remote
		cycle()
		var page struct{ Messages []core.Message }
		checked(t, core.RPC(ctx, remote, "status", map[string]any{}, &page))
		n := 0
		for _, m := range page.Messages {
			if m.ID == id {
				n++
			}
		}
		if n != 1 {
			t.Fatal("retry duplicated a message")
		}
	})

	t.Run("expiry_deadline_and_polling_no_claim", func(t *testing.T) {
		id := pending(t, invoke(t, ctx, client, "send", map[string]any{"to": "beta", "body": "expiry probe", "ttl": 1}))
		cycle()
		betaHub, e := core.NewHTTPHub(url, beta, true)
		checked(t, e)
		var claim struct{ Delivery any }
		checked(t, core.RPC(ctx, betaHub, "claim", map[string]any{"message_id": id}, &claim))
		if claim.Delivery != nil {
			t.Fatal("polling session was claimed for push")
		}
		time.Sleep(1200 * time.Millisecond)
		cycle()
		status := message(t, ctx, client, id)
		if status["status"] != "expired" {
			t.Fatal("unread pending message did not expire")
		}
		rows := python.invoke(t, "inbox", map[string]any{"peek": true}).([]any)
		found := false
		for _, r := range rows {
			if object(t, r)["id"] == id {
				found = true
			}
		}
		if !found {
			t.Fatal("expired unread mail vanished")
		}
	})

	t.Run("six_message_hop_limit", func(t *testing.T) {
		id := pending(t, invoke(t, ctx, client, "send", map[string]any{"to": "beta", "body": "bounded thread"}))
		cycle()
		for hop := 1; hop <= 5; hop++ {
			if hop%2 == 1 {
				id = pending(t, python.invoke(t, "reply", map[string]any{"message_id": id, "body": "continue"}))
			} else {
				id = pending(t, invoke(t, ctx, client, "reply", map[string]any{"message_id": id, "body": "continue"}))
			}
			cycle()
		}
		sixth := invoke(t, ctx, client, "reply", map[string]any{"message_id": id, "body": "must reject"})
		requestID, _ := sixth["request_id"].(string)
		cycle()
		result, ok, e := store.Result(ctx, requestID)
		checked(t, e)
		if !ok || result.OK || result.Error != "thread reached the 6-message limit; send a new message instead" {
			t.Fatal("sixth thread edge did not return exact refusal")
		}
	})
	t.Run("crashed_claim_recovers_uncertain_without_native_replay", func(t *testing.T) {
		// Consume earlier polling test messages before enabling this synthetic push
		// route, so the only eligible native message is the crash probe.
		_, e := client.Invoke(ctx, "inbox", map[string]any{})
		checked(t, e)
		checked(t, helper.Requests(ctx))
		nativeEnabled = true
		checked(t, helper.Publish(ctx))
		id := pending(t, python.invoke(t, "send", map[string]any{"to": "alpha", "body": "crash recovery probe"}))
		python.call(t, "cycle", nil)
		checked(t, helper.Sync(ctx, 0))
		var claim struct {
			Delivery *struct {
				ClaimID string `json:"claim_id"`
			}
		}
		checked(t, core.RPC(ctx, remote, "claim", map[string]any{"message_id": id}, &claim))
		if claim.Delivery == nil {
			t.Fatal("synthetic native message was not claimed")
		}
		checked(t, store.StartAttempt(ctx, id, claim.Delivery.ClaimID))
		// Reopen the durable store exactly as a restarted helper would, preserving
		// the unfinished attempt. Recovery reports uncertain and never submits.
		reopened, e := core.NewLocalStore(ctx, host.Files, store.Root)
		checked(t, e)
		checked(t, reopened.RecoverAttempts(ctx))
		helper.Store = reopened
		checked(t, helper.Report(ctx))
		cycle()
		got := message(t, ctx, client, id)
		if got["status"] != "uncertain" || nativeSubmissions != 0 {
			t.Fatal("unfinished claim was replayed or lost")
		}
		checked(t, helper.Cycle(ctx))
		if nativeSubmissions != 0 {
			t.Fatal("uncertain outcome was automatically retried")
		}
		nativeEnabled = false
		checked(t, helper.Publish(ctx))
	})

}
