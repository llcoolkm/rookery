package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"
)

type panesProcess struct {
	platform.Process
	calls int
}

func (p *panesProcess) TmuxPanes(context.Context) map[int]string {
	p.calls++
	return map[int]string{9: "work bench:3", 12: "direct:0"}
}

func (*panesProcess) Ancestors(_ context.Context, ref platform.ProcessRef) ([]platform.ProcessRef, error) {
	if ref.PID == 11 {
		return []platform.ProcessRef{ref, {PID: 10}, {PID: 9}}, nil
	}
	return []platform.ProcessRef{ref, {PID: 1}}, nil
}

type publishRecorder struct{ req RPCRequest }

func (h *publishRecorder) Call(_ context.Context, req RPCRequest) (json.RawMessage, error) {
	h.req = req
	return json.RawMessage(`{}`), nil
}

func TestPublishSessionMetadataMapsTmuxAncestors(t *testing.T) {
	process := new(panesProcess)
	hub := new(publishRecorder)
	h := Helper{Process: process, Hub: hub}
	h.Native.Discover = func(context.Context) ([]Session, map[string]string) {
		return []Session{
			{Provider: "claude", ID: "grandchild", Status: "waiting", WaitingFor: "permission", Cwd: "/workspace", PID: 11, ProcessRef: &platform.ProcessRef{PID: 11}},
			{Provider: "claude", ID: "direct", Status: "idle", WaitingFor: "out of date", Cwd: "     ", PID: 12, ProcessRef: &platform.ProcessRef{PID: 12}},
			{Provider: "claude", ID: "outside", Status: "waiting", WaitingFor: "invalid\x1b", Cwd: strings.Repeat("å", 257), PID: 19, ProcessRef: &platform.ProcessRef{PID: 19}},
			{Provider: "codex", ID: "shared-daemon", Status: "waiting", WaitingFor: "approval", PID: 12, ProcessRef: &platform.ProcessRef{PID: 12}},
		}, nil
	}
	if err := h.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	var published struct{ Sessions []map[string]any }
	if err := json.Unmarshal(hub.req.Data, &published); hub.req.Op != "publish" || err != nil || len(published.Sessions) != 4 || process.calls != 1 {
		t.Fatalf("unexpected publish: %#v, %v, tmux runs: %d", hub.req, err, process.calls)
	}
	grandchild, direct, unrelated, codex := published.Sessions[0], published.Sessions[1], published.Sessions[2], published.Sessions[3]
	if grandchild["tmux"] != "work bench:3" || grandchild["waiting_for"] != "permission" || grandchild["cwd"] != "/workspace" || direct["tmux"] != "direct:0" {
		t.Fatalf("missing session metadata: %v", published.Sessions)
	}
	if direct["waiting_for"] != nil || direct["cwd"] != nil || unrelated["tmux"] != nil || unrelated["waiting_for"] != nil || unrelated["cwd"] != nil || codex["tmux"] != nil || codex["waiting_for"] != "approval" {
		t.Fatalf("published unsupported or malformed metadata: %v", published.Sessions)
	}
	for _, row := range published.Sessions {
		if _, ok := row["status_since"]; ok {
			t.Fatalf("helper published hub-owned status transition time: %v", row)
		}
	}
}

func TestSessionPublishRequestSizesEncodedOptionalMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, tmux, reason   string
		dropTmux, dropReason bool
	}{
		{name: "cwd", tmux: "main:3", reason: "approval"},
		{name: "tmux", tmux: strings.Repeat("<", 126) + ":3", reason: "approval", dropTmux: true},
		{name: "waiting_for", tmux: strings.Repeat("<", 126) + ":3", reason: strings.Repeat(">", 128), dropTmux: true, dropReason: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([]map[string]any, 128)
			for i := range rows {
				// The wire encoder expands angle brackets to six bytes each.
				rows[i] = map[string]any{"name": strings.Repeat("<", 256), "cwd": strings.Repeat("<", 512), "tmux": tc.tmux, "waiting_for": tc.reason}
			}
			request, err := sessionPublishRequest(rows, nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(request)
			if err != nil || len(body) > maxRequestBytes {
				t.Fatalf("publish frame does not fit: %d bytes, %v", len(body), err)
			}
			var payload struct{ Sessions []map[string]any }
			if err := json.Unmarshal(request.Data, &payload); err != nil || len(payload.Sessions) != 128 {
				t.Fatalf("publish batch changed: %d rows, %v", len(payload.Sessions), err)
			}
			for _, row := range payload.Sessions {
				if row["cwd"] != nil || (row["tmux"] == nil) != tc.dropTmux || (row["waiting_for"] == nil) != tc.dropReason {
					t.Fatalf("unexpected optional metadata shedding: %v", row)
				}
			}
		})
	}
}

func TestPublishKeepsLatestLocalSessionsWhenOverLimit(t *testing.T) {
	h := Helper{Sessions: []Session{{ID: "old"}}}
	h.Native.Discover = func(context.Context) ([]Session, map[string]string) {
		sessions := make([]Session, 129)
		sessions[0].ID = "new"
		return sessions, map[string]string{"claude": "discovery error"}
	}
	if err := h.Publish(context.Background()); err == nil {
		t.Fatal("oversized discovery should fail publication")
	}
	if len(h.Sessions) != 129 || h.Sessions[0].ID != "new" || h.ProviderErrors["claude"] == "" {
		t.Fatal("oversized discovery left local discovery state stale")
	}
}
