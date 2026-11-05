//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/platform/linux"
	"github.com/llcoolkm/rookery/go/provider"
)

const testID = "00000000-0000-4000-8000-000000000001"

type fakeProvider struct {
	name   string
	rows   []core.Session
	err    error
	policy provider.Admission
}

func (p fakeProvider) Name() string { return p.name }
func (fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Topology: "host"}
}
func (p fakeProvider) Admission(core.Session) provider.Admission { return p.policy }
func (fakeProvider) Identity(context.Context, core.CallMetadata) (string, error) {
	return "", errors.New("unavailable")
}
func (p fakeProvider) Discover(context.Context) ([]core.Session, error) { return p.rows, p.err }
func (fakeProvider) Deliver(context.Context, core.Session, core.Message) (core.DeliveryResult, error) {
	return core.DeliveryResult{Status: "submitted"}, nil
}
func (fakeProvider) Busy(context.Context, core.Session) (provider.BusyState, error) {
	return provider.Idle, nil
}
func host(t *testing.T) platform.Host {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h, err := linux.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func TestPollingPinValidationAndRemoval(t *testing.T) {
	ctx := context.Background()
	h := host(t)
	root, _ := h.Paths.StateDir()
	pin, err := h.Process.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := RegisterPolling(ctx, h, root, "codex", testID, pin.PID, "synthetic polling")
	if err != nil || r.(map[string]any)["delivery"] != "polling" {
		t.Fatal(r, err)
	}
	rows, err := DiscoverPolling(ctx, h, root)
	if err != nil || len(rows) != 1 || rows[0].Admission != "none" || rows[0].Status != "waiting" {
		t.Fatal(rows, err)
	}
	pin.Start = "changed"
	b, _ := json.Marshal(pollingRecord{Provider: "codex", ID: testID, Name: "synthetic", Pin: pin})
	if err = h.Files.AtomicWrite(ctx, root, pollingName("codex", testID), b); err != nil {
		t.Fatal(err)
	}
	rows, err = DiscoverPolling(ctx, h, root)
	if err != nil || len(rows) != 0 {
		t.Fatal("reused PID accepted", rows, err)
	}
	if _, err = UnregisterPolling(ctx, h, root, "codex", testID); err != nil {
		t.Fatal(err)
	}
	if _, err = UnregisterPolling(ctx, h, root, "codex", testID); err != nil {
		t.Fatal("removal is not idempotent", err)
	}
}
func TestPollingValidation(t *testing.T) {
	h := host(t)
	root, _ := h.Paths.StateDir()
	for _, tc := range []struct {
		provider, id string
		pid          int
		name         string
	}{{"unknown", testID, 1, "x"}, {"codex", "invalid", 1, "x"}, {"codex", testID, -1, "x"}, {"codex", testID, 1, " "}, {"codex", testID, 1, strings.Repeat("x", 257)}} {
		if _, err := RegisterPolling(context.Background(), h, root, tc.provider, tc.id, tc.pid, tc.name); err == nil {
			t.Fatal("invalid record accepted", tc)
		}
	}
}
func TestRegistryIsolatesErrorsAndDuplicateIdentities(t *testing.T) {
	h := host(t)
	s := core.Session{Provider: "codex", ID: testID, PID: 42, Status: "idle"}
	r := &Registry{Host: h, Providers: map[string]provider.Provider{"codex": fakeProvider{name: "codex", rows: []core.Session{s, s}, policy: provider.AdmitIdle}, "claude": fakeProvider{name: "claude", err: errors.New("sensitive raw diagnostic")}}}
	rows, problems := r.Discover(context.Background())
	if len(rows) != 0 || problems["codex"] != "ambiguous live session identity" || problems["claude"] != "provider discovery unavailable" {
		t.Fatal(rows, problems)
	}
	r.Providers["codex"] = fakeProvider{name: "codex", rows: []core.Session{s}, policy: "invalid"}
	rows, _ = r.Discover(context.Background())
	if len(rows) != 1 || rows[0].Admission != "none" || r.Ready(rows[0]) {
		t.Fatal(rows)
	}
}
func TestPollingOverridesNativeAndAttachedOverridesPolling(t *testing.T) {
	ctx := context.Background()
	h := host(t)
	root, _ := h.Paths.StateDir()
	pin, _ := h.Process.Current(ctx)
	if _, err := RegisterPolling(ctx, h, root, "codex", testID, pin.PID, "polling"); err != nil {
		t.Fatal(err)
	}
	native := core.Session{Provider: "codex", ID: testID, PID: pin.PID, Status: "idle"}
	r := &Registry{Host: h, Providers: map[string]provider.Provider{"codex": fakeProvider{name: "codex", rows: []core.Session{native}, policy: provider.AdmitIdle}}}
	rows, _ := r.Discover(ctx)
	if len(rows) != 1 || rows[0].Delivery != "polling" || r.Ready(rows[0]) {
		t.Fatal(rows)
	}
	native.Delivery = "attached"
	r.Providers["codex"] = fakeProvider{name: "codex", rows: []core.Session{native}, policy: provider.AdmitBusy}
	rows, _ = r.Discover(ctx)
	if len(rows) != 1 || rows[0].Delivery != "attached" || !r.Ready(rows[0]) {
		t.Fatal(rows)
	}
}

func TestRestrictedDiscoveryDefaultsToNoSessions(t *testing.T) {
	h := host(t)
	r := NewRegistry(provider.Config{SessionAllowlist: []string{}}, h)
	r.Providers = map[string]provider.Provider{"codex": fakeProvider{name: "codex", rows: []core.Session{{Provider: "codex", ID: testID}}}}
	rows, problems := r.Discover(context.Background())
	if len(rows) != 0 || len(problems) != 0 {
		t.Fatal(rows, problems)
	}
	r.Config.SessionAllowlist = []string{"codex:" + testID}
	rows, problems = r.Discover(context.Background())
	if len(rows) != 1 || len(problems) != 0 {
		t.Fatal(rows, problems)
	}
}
