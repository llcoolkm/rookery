//go:build linux

package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/llcoolkm/rookery/go/core"
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
