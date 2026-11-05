//go:build linux

package contract_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/llcoolkm/rookery/go/core"
	hostlinux "github.com/llcoolkm/rookery/go/platform/linux"
)

type retryVectorHub struct {
	calls []core.RPCRequest
	lose  bool
}

func (h *retryVectorHub) Call(_ context.Context, r core.RPCRequest) (json.RawMessage, error) {
	h.calls = append(h.calls, r)
	if h.lose {
		h.lose = false
		return nil, errors.New("hub response interrupted; original request retained")
	}
	return json.RawMessage(`{"status":"pending"}`), nil
}
func vectorJSON(t *testing.T, value any) any {
	t.Helper()
	raw, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("invalid vector JSON")
	}
	return decoded
}

// The Python spool's queued command is admitted unchanged into the Go journal.
// Reopening that journal and retrying after a lost response must preserve the
// original UUIDv7 identity, message binding and absolute expiration deadline.
func TestLocalCommandAdmissionAndReopenVectors(t *testing.T) {
	raw, e := os.ReadFile("testdata/contract.json")
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		Local []struct {
			Name              string
			Request, Imported core.Command
			Response          map[string]any
			SpoolRemoved      bool `json:"spool_removed"`
		}
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Local) == 0 {
		t.Fatal("missing local vectors")
	}
	for _, v := range fixture.Local {
		t.Run(v.Name, func(t *testing.T) {
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
			if e = store.Admit(ctx, v.Request); e != nil {
				t.Fatal(e)
			}
			// Repeated import is idempotent; the Python fixture's imported form is the
			// normative command, independently of the chosen local storage format.
			if e = store.Admit(ctx, v.Imported); e != nil {
				t.Fatal(e)
			}
			commands, e := store.Commands(ctx, 0)
			if e != nil || len(commands) != 1 {
				t.Fatal("duplicate import or lost command", e)
			}
			if !reflect.DeepEqual(vectorJSON(t, commands[0]), vectorJSON(t, v.Imported)) {
				t.Fatal("admission changed contract command")
			}
			hub := &retryVectorHub{lose: true}
			helper := core.Helper{Store: store, Hub: hub}
			if helper.Requests(ctx) == nil {
				t.Fatal("lost-response simulation did not fail")
			}
			reopened, e := core.NewLocalStore(ctx, host.Files, root)
			if e != nil {
				t.Fatal(e)
			}
			commands, e = reopened.Commands(ctx, 0)
			if e != nil || len(commands) != 1 {
				t.Fatal("lost response removed durable command", e)
			}
			if !reflect.DeepEqual(vectorJSON(t, commands[0]), vectorJSON(t, v.Imported)) {
				t.Fatal("reopen refreshed identity or deadline")
			}
			helper.Store = reopened
			if e = helper.Requests(ctx); e != nil {
				t.Fatal(e)
			}
			if len(hub.calls) != 2 || !reflect.DeepEqual(vectorJSON(t, hub.calls[0]), vectorJSON(t, hub.calls[1])) {
				t.Fatal("retry changed hub request")
			}
			if !reflect.DeepEqual(vectorJSON(t, hub.calls[0]), vectorJSON(t, core.RPCRequest{Op: v.Request.Op, Data: v.Request.Data})) {
				t.Fatal("wire request differs from admitted vector")
			}
			result, found, e := reopened.Result(ctx, v.Request.ID)
			if e != nil || !found || !result.OK {
				t.Fatal("completion result not durable")
			}
			commands, e = reopened.Commands(ctx, 0)
			if e != nil || len(commands) != 0 {
				t.Fatal("completed command remains queued")
			}
			var data map[string]any
			if json.Unmarshal(v.Request.Data, &data) != nil || data["expires"] == nil || data["_request_id"] != v.Response["request_id"] || data["id"] != v.Response["id"] || v.Response["status"] != "queued_local" || !v.SpoolRemoved {
				t.Fatal("inconsistent queue-admission vector")
			}
		})
	}
}
