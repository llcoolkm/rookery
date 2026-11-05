//go:build linux

package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	hostlinux "github.com/llcoolkm/rookery/go/platform/linux"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *core.LocalStore {
	t.Helper()
	ctx := context.Background()
	temp := t.TempDir()
	if e := os.Chmod(temp, 0700); e != nil {
		t.Fatal(e)
	}
	host, e := hostlinux.New(filepath.Join(temp, "state"))
	if e != nil {
		t.Fatal(e)
	}
	root, e := host.Paths.StateDir()
	if e != nil {
		t.Fatal(e)
	}
	store, e := core.NewLocalStore(ctx, host.Files, root)
	if e != nil {
		t.Fatal(e)
	}
	return store
}
func TestStoreRecoveryAndReadAdmission(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mid := "00000000-0000-4000-8000-000000000001"
	claim := "00000000-0000-4000-8000-000000000002"
	if e := s.StartAttempt(ctx, mid, claim); e != nil {
		t.Fatal(e)
	}
	if e := s.RecoverAttempts(ctx); e != nil {
		t.Fatal(e)
	}
	a, e := s.Reports(ctx)
	if len(a) == 1 {
		if created, e := core.RequestCreated(a[0].RequestID); e != nil || created == nil {
			t.Fatal("native report lacks durable UUIDv7")
		}
	}
	if e != nil || len(a) != 1 || a[0].Result.Status != "uncertain" {
		t.Fatalf("recovery %#v %v", a, e)
	}
	if s.StartAttempt(ctx, mid, "another") == nil {
		t.Fatal("replayed uncertain")
	}
	c := core.Client{Store: s, Config: core.Config{Node: "alpha"}, Actor: "codex:00000000-0000-4000-8000-000000000003"}
	actor, _ := c.Identity(ctx)
	m := core.Message{ID: mid, Recipient: actor, Status: core.Pending, Updated: 1, Expires: 1e12}
	b, _ := json.Marshal(m)
	if e = s.Apply(ctx, []core.Event{{Type: "message", Data: b}}, 7); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Invoke(ctx, "inbox", map[string]any{}); e != nil {
		t.Fatal(e)
	}
	reads, e := s.PendingReads(ctx)
	if e != nil || len(reads[actor]) != 1 {
		t.Fatal("read not durably admitted", e)
	}
	n, _ := s.Cursor(ctx)
	if n != 7 {
		t.Fatal("cursor not atomic")
	}
	if e = s.Prune(ctx, time.Now().Add(60*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	a, _ = s.Reports(ctx)
	if len(a) != 1 {
		t.Fatal("uncertain evidence pruned")
	}
}

type responseHub struct {
	err   error
	value json.RawMessage
}

func (h responseHub) Call(context.Context, core.RPCRequest) (json.RawMessage, error) {
	return h.value, h.err
}
func TestRetainAndDoNotOverwriteSync(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	c := core.Client{Store: s}
	r, e := c.Request(ctx, "ack", map[string]any{"message_id": "synthetic"}, 0)
	if e != nil {
		t.Fatal(e)
	}
	id := r["request_id"].(string)
	h := core.Helper{Store: s, Hub: responseHub{err: errors.New("unknown")}}
	if h.Requests(ctx) == nil {
		t.Fatal("expected error")
	}
	cmds, _ := s.Commands(ctx, 64)
	if len(cmds) != 1 || cmds[0].ID != id {
		t.Fatal("retry identity changed")
	}
	m := core.Message{ID: "synthetic", Status: core.Acknowledged}
	b, _ := json.Marshal(m)
	_ = s.Apply(ctx, []core.Event{{Type: "message", Data: b}}, 10)
	h.Hub = responseHub{value: json.RawMessage(`{"id":"synthetic","status":"pending"}`)}
	if e = h.Requests(ctx); e != nil {
		t.Fatal(e)
	}
	got, _, _ := s.Message(ctx, "synthetic")
	if got.Status != core.Acknowledged {
		t.Fatal("delayed response overwrote sync")
	}
	_, ok, e := s.Result(ctx, id)
	if !ok || e != nil {
		t.Fatal("result lost")
	}
}
func TestConcurrentAdmission(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _ := core.NewRequestID(time.Now())
			data, _ := json.Marshal(map[string]any{"_request_id": id, "number": i})
			if e := s.Admit(ctx, core.Command{ID: id, Op: "ack", Data: data}); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	commands, e := s.Commands(ctx, 100)
	if e != nil || len(commands) != 20 {
		t.Fatal(fmt.Sprint(len(commands), e))
	}
}
func TestPendingDeliveryNotHiddenByHistory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	events := []core.Event{}
	for i := 0; i < 101; i++ {
		m := core.Message{ID: fmt.Sprint(i), Recipient: "alpha/claude:synthetic", Status: core.Submitted, Updated: 100, Expires: 1e12}
		b, _ := json.Marshal(m)
		events = append(events, core.Event{Type: "message", Data: b})
	}
	for _, m := range []core.Message{{ID: "pending", Recipient: "alpha/claude:synthetic", Status: core.Pending, Updated: 1, Expires: 1e12}, {ID: "outgoing", Recipient: "beta/claude:synthetic", Status: core.Pending, Updated: 101, Expires: 1e12}} {
		b, _ := json.Marshal(m)
		events = append(events, core.Event{Type: "message", Data: b})
	}
	if e := s.Apply(ctx, events, 104); e != nil {
		t.Fatal(e)
	}
	rows, e := s.PendingMessages(ctx, "alpha", 1)
	if e != nil || len(rows) != 1 || rows[0].ID != "pending" {
		t.Fatal("incoming pending work hidden by message history")
	}
}

type observedFiles struct {
	platform.Files
	writes     int
	afterWrite func()
}

func (f *observedFiles) AtomicWrite(ctx context.Context, root, name string, data []byte) error {
	err := f.Files.AtomicWrite(ctx, root, name, data)
	if err == nil {
		f.writes++
		if f.afterWrite != nil {
			f.afterWrite()
		}
	}
	return err
}
func TestStoreSkipsUnchangedSnapshots(t *testing.T) {
	s := newStore(t)
	f := &observedFiles{Files: s.Files}
	s.Files = f
	ctx := context.Background()
	if err := s.Apply(ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatal("unchanged empty sync rewrote store")
	}
	if err := s.Apply(ctx, nil, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, nil, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverAttempts(ctx); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatal("unchanged snapshot rewrote store", f.writes)
	}
}
func TestClientCancellationAfterAdmissionRemainsQueued(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &observedFiles{Files: s.Files, afterWrite: cancel}
	s.Files = f
	c := core.Client{Store: s}
	r, err := c.Request(ctx, "ack", map[string]any{"message_id": messageID}, time.Second)
	if err != nil || r["status"] != "queued_local" {
		t.Fatal("admitted command not returned as queued", r, err)
	}
	commands, err := s.Commands(context.Background(), 10)
	if err != nil || len(commands) != 1 || commands[0].ID != r["request_id"] {
		t.Fatal("admitted command lost")
	}
}
func TestClientRejectsNonBooleanPeek(t *testing.T) {
	c := core.Client{Store: newStore(t), Config: core.Config{Node: "alpha"}, Actor: "claude:" + sessionID}
	for _, value := range []any{"false", 0, nil} {
		if _, err := c.Invoke(context.Background(), "inbox", map[string]any{"peek": value}); err == nil {
			t.Fatal("invalid peek accepted", value)
		}
	}
}

type undurableFiles struct{ platform.Files }

func (f undurableFiles) AtomicWrite(ctx context.Context, root, name string, data []byte) error {
	if err := f.Files.AtomicWrite(ctx, root, name, data); err != nil {
		return err
	}
	return platform.ErrCommittedNotDurable
}
func TestAdmissionAfterRenameFailureRemainsQueuedButClaimFailsClosed(t *testing.T) {
	s := newStore(t)
	s.Files = undurableFiles{s.Files}
	c := core.Client{Store: s}
	result, err := c.Request(context.Background(), "ack", map[string]any{"message_id": messageID}, time.Second)
	if err != nil || result["status"] != "queued_local" {
		t.Fatal("renamed command misreported as rejected", result, err)
	}
	if err := s.StartAttempt(context.Background(), messageID, claimID); !errors.Is(err, platform.ErrCommittedNotDurable) {
		t.Fatal("undurable native journal permitted submission", err)
	}
}
