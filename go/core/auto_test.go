//go:build linux || windows

package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/llcoolkm/rookery/go/core"
)

func TestAutoEnrollEnrollsNewSessionsButNeverDisabledOnes(t *testing.T) {
	ctx := context.Background()
	fresh, disabled, done := helperSession(), helperSession(), helperSession()
	fresh.ID, disabled.ID, done.ID = "00000000-0000-4000-8000-000000000021", "00000000-0000-4000-8000-000000000022", "00000000-0000-4000-8000-000000000023"
	sessions := []core.Session{fresh, disabled, done}
	on := false
	var enrolled []string
	h := &core.Helper{Store: newStore(t), Node: "alpha", AutoEnroll: func(context.Context) bool { return on }}
	h.Native = core.NativeHooks{Discover: func(context.Context) ([]core.Session, map[string]string) { return sessions, nil }}
	h.Hub = helperHub{call: func(_ context.Context, r core.RPCRequest) (json.RawMessage, error) {
		switch r.Op {
		case "publish":
			return json.RawMessage(`{}`), nil
		case "list":
			return json.Marshal(map[string]any{"sessions": []map[string]any{
				{"key": "alpha/claude:" + fresh.ID, "enrolled": false},
				{"key": "alpha/claude:" + disabled.ID, "enrolled": false, "disabled": true},
				{"key": "alpha/claude:" + done.ID, "enrolled": true},
			}})
		case "enroll":
			var d struct{ Target string }
			_ = json.Unmarshal(r.Data, &d)
			enrolled = append(enrolled, d.Target)
			return json.RawMessage(`{}`), nil
		}
		t.Fatalf("unexpected op %s", r.Op)
		return nil, nil
	}}
	if e := h.Publish(ctx); e != nil || len(enrolled) != 0 {
		t.Fatalf("off must not enroll: %v %v", e, enrolled)
	}
	on = true
	if e := h.Publish(ctx); e != nil {
		t.Fatal(e)
	}
	if len(enrolled) != 1 || enrolled[0] != "alpha/claude:"+fresh.ID {
		t.Fatalf("want only the new session enrolled, got %v", enrolled)
	}
}
