//go:build windows

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	host "github.com/llcoolkm/rookery/go/platform/windows"
)

type isolatedHub struct {
	lost  bool
	calls []core.RPCRequest
}

func (h *isolatedHub) Call(_ context.Context, r core.RPCRequest) (json.RawMessage, error) {
	h.calls = append(h.calls, r)
	if !h.lost {
		h.lost = true
		return nil, errors.New("synthetic lost response")
	}
	return json.RawMessage(`{"status":"acknowledged"}`), nil
}

// This integration composes the native Windows platform, durable core, CLI
// client engine and helper request engine without opening a live hub or reading
// a provider session. Python's isolated hub integration remains Linux-only.
func TestWindowsDurableHelperWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, e := host.New(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	current, e := h.Process.Current(ctx)
	if e != nil || h.Process.Pin(ctx, current) != nil {
		t.Fatal("current process identity unavailable")
	}
	root, e := h.Paths.StateDir()
	if e != nil {
		t.Fatal(e)
	}
	store, e := core.NewLocalStore(ctx, h.Files, root)
	if e != nil {
		t.Fatal(e)
	}
	client := core.Client{Store: store}
	result, e := client.Request(ctx, "ack", map[string]any{"message_id": "00000000-0000-4000-8000-000000000001"}, 0)
	if e != nil || result["status"] != "queued_local" {
		t.Fatal("offline admission failed", e)
	}
	remote := &isolatedHub{}
	helper := core.Helper{Store: store, Hub: remote}
	if helper.Requests(ctx) == nil {
		t.Fatal("lost-response simulation failed")
	}
	reopened, e := core.NewLocalStore(ctx, h.Files, root)
	if e != nil {
		t.Fatal(e)
	}
	commands, e := reopened.Commands(ctx, 100)
	if e != nil || len(commands) != 1 || commands[0].ID != result["request_id"] {
		t.Fatal("durable identity lost", e)
	}
	helper.Store = reopened
	if e = helper.Requests(ctx); e != nil {
		t.Fatal(e)
	}
	if len(remote.calls) != 2 || !reflect.DeepEqual(remote.calls[0], remote.calls[1]) {
		t.Fatal("retry changed request")
	}
	if _, ok, e := reopened.Result(ctx, commands[0].ID); e != nil || !ok {
		t.Fatal("completion not persisted", e)
	}
	const mid = "00000000-0000-4000-8000-000000000002"
	const claim = "00000000-0000-4000-8000-000000000003"
	if e = reopened.StartAttempt(ctx, mid, claim); e != nil {
		t.Fatal(e)
	}
	recovered, e := core.NewLocalStore(ctx, h.Files, root)
	if e != nil {
		t.Fatal(e)
	}
	if e = recovered.RecoverAttempts(ctx); e != nil {
		t.Fatal(e)
	}
	reports, e := recovered.Reports(ctx)
	if e != nil || len(reports) != 1 || reports[0].Result.Status != "uncertain" {
		t.Fatal("crash did not preserve uncertainty", e)
	}
	if eligible, e := recovered.DeliveryEligible(ctx, mid); e != nil || eligible {
		t.Fatal("uncertain attempt can replay", e)
	}
}
