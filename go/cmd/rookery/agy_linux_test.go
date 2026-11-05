//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
	"github.com/llcoolkm/rookery/go/provider/agy"
)

const agyTestID = "00000000-0000-4000-8000-000000000071"
const agyOtherID = "00000000-0000-4000-8000-000000000072"

type agyTestProcess struct {
	platform.Process
	owner platform.ProcessRef
	dead  bool
}

func (p *agyTestProcess) Lookup(ctx context.Context, pid int) (platform.ProcessRef, error) {
	if pid == p.owner.PID {
		if p.dead {
			return platform.ProcessRef{}, errors.New("exited")
		}
		return p.owner, nil
	}
	return p.Process.Lookup(ctx, pid)
}
func (p *agyTestProcess) Pin(ctx context.Context, ref platform.ProcessRef) error {
	if ref.PID == p.owner.PID {
		if p.dead || ref != p.owner {
			return errors.New("changed")
		}
		return nil
	}
	return p.Process.Pin(ctx, ref)
}
func (p *agyTestProcess) CommandLine(context.Context, platform.ProcessRef) ([]string, error) {
	panic("AGY must not inspect argv")
}
func agyFixture(t *testing.T) (platform.Host, string, agyEnvironment) {
	t.Helper()
	parent, err := os.MkdirTemp("", "rk4-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	root := filepath.Join(parent, "s")
	h, err := newHost(root)
	if err != nil {
		t.Fatal(err)
	}
	self, err := h.Process.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := self
	owner.PID = 1000000000
	owner.Start = "synthetic-start"
	h.Process = &agyTestProcess{Process: h.Process, owner: owner}
	env := agyEnvironment{getenv: func(key string) string {
		if key != "ANTIGRAVITY_CONVERSATION_ID" {
			t.Fatal("unexpected environment inspection", key)
		}
		return agyTestID
	}, self: func() (string, error) { return "/synthetic/rookery-go", nil }, ancestor: func(context.Context) (platform.ProcessRef, error) { return owner, nil }, classify: func(context.Context, string, string) (string, error) { return "top-level", nil }, spawn: func(context.Context, string, string, string) (any, error) {
		return map[string]any{"spawned": true}, nil
	}}
	return h, root, env
}
func agyArgs(command, id string, flags ...string) arguments {
	a := arguments{command: command, positionals: []string{"agy"}, options: map[string]string{}}
	if id != "" {
		a.options["conversation"] = id
	}
	for _, f := range flags {
		a.options[f] = "true"
	}
	return a
}
func startFakeAGY(t *testing.T, h platform.Host, root, id string, env agyEnvironment) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan any, 1)
	done := make(chan error, 1)
	go func() {
		_, err := serveAGY(ctx, h, root, id, "synthetic", mustAGYOwner(env), func(v any) error { ready <- v; return nil })
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal("endpoint failed", err)
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint readiness timeout")
	}
	return cancel, done
}
func mustAGYOwner(env agyEnvironment) platform.ProcessRef {
	r, _ := env.ancestor(context.Background())
	return r
}
func TestAGYAttachRejectsMismatchedShellAndUnknownConversation(t *testing.T) {
	h, root, env := agyFixture(t)
	for _, id := range []string{agyOtherID, "00000000-0000-4000-8000-0000000000AB", "../unsafe"} {
		if id == agyTestID {
			continue
		}
		if _, err := agyCommandWith(context.Background(), h, root, agyArgs("attach", id), env); err == nil {
			t.Fatal("accepted mismatched or invalid id", id)
		}
	}
	env.classify = func(context.Context, string, string) (string, error) { return "unknown", nil }
	if _, err := agyCommandWith(context.Background(), h, root, agyArgs("attach", ""), env); err == nil {
		t.Fatal("unknown conversation attached")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("rejected attach wrote state")
	}
}
func TestAGYHeldEndpointLifecycleAndIdempotentAttach(t *testing.T) {
	h, root, env := agyFixture(t)
	cancel, done := startFakeAGY(t, h, root, agyTestID, env)
	p, _ := agy.New(provider.Config{}, h)
	rows, err := p.Discover(context.Background())
	if err != nil || len(rows) != 1 || p.Admission(rows[0]) != provider.AdmitBusy {
		t.Fatal("not discoverable", rows, err)
	}
	result, err := p.Deliver(context.Background(), rows[0], core.Message{Body: "body-canary"})
	if err != nil || result.Status != "held" {
		t.Fatal(result, err)
	}
	value, err := agyCommandWith(context.Background(), h, root, agyArgs("attach", ""), env)
	if err != nil || value.(map[string]any)["already"] != true {
		t.Fatal("not idempotent", value, err)
	}
	ep, _ := h.Paths.Endpoint(agy.SocketName(agyTestID))
	c, err := h.IPC.Dial(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(c, "{\"op\":\"submit\",\"request_id\":\"synthetic\",\"text\":\"body-canary\"}\n")
	var response map[string]any
	if json.NewDecoder(c).Decode(&response) != nil || response["status"] != "held" {
		t.Fatal(response)
	}
	c.Close()
	files, _ := os.ReadDir(root)
	for _, file := range files {
		if !file.Type().IsRegular() {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(root, file.Name()))
		if strings.Contains(string(b), "body-canary") {
			t.Fatal("peer text persisted", file.Name())
		}
	}
	_, err = agyCommandWith(context.Background(), h, root, agyArgs("detach", agyTestID), env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Admission(rows[0]) != provider.AdmitNone {
		t.Fatal("detached still admitted")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detach did not stop endpoint")
	}
	cancel()
	if !agyMarker(context.Background(), h, root, agyTestID, "detached") {
		t.Fatal("sticky detach missing")
	}
	// Explicit reattach clears sticky detach and can reuse the removed socket.
	cancel, done = startFakeAGY(t, h, root, agyTestID, env)
	cancel()
	<-done
}
func TestAGYHookRefusalDetachResumeAndSubagent(t *testing.T) {
	h, root, env := agyFixture(t)
	ctx := context.Background()
	hook := func(event string) map[string]any {
		v, err := agyHookWith(ctx, h, root, event, strings.NewReader(`{"conversationId":"`+agyTestID+`"}`), env)
		if err != nil {
			t.Fatal(err)
		}
		return v.(map[string]any)
	}
	first := hook("session-start")
	b, _ := json.Marshal(first)
	if !strings.Contains(string(b), "delivery is held") || !strings.Contains(string(b), "attach agy") {
		t.Fatal(first)
	}
	if len(hook("pre-invocation")) != 0 {
		t.Fatal("permission refusal repeated")
	}
	owner := mustAGYOwner(env)
	owner.PID++
	owner.Start = "next-start"
	env.ancestor = func(context.Context) (platform.ProcessRef, error) { return owner, nil }
	if len(hook("session-start")) != 0 {
		t.Fatal("refusal repeated after process change")
	}
	h.Files.AtomicWrite(ctx, root, agy.MarkerName(agyTestID, "attached"), []byte("{}"))
	if len(hook("session-start")) == 0 || len(hook("pre-invocation")) != 0 {
		t.Fatal("resume reminder policy")
	}
	agyCommandWith(ctx, h, root, agyArgs("detach", agyTestID), env)
	if len(hook("session-start")) != 0 {
		t.Fatal("explicit detach ignored")
	}
	env.classify = func(context.Context, string, string) (string, error) { return "subagent", nil }
	if len(hook("session-start")) != 0 {
		t.Fatal("subagent hook injected")
	}
	for _, payload := range []string{"not-json", `{"conversationId":42}`, strings.Repeat(" ", 65537)} {
		v, err := agyHookWith(ctx, h, root, "session-start", strings.NewReader(payload), env)
		if err != nil || len(v.(map[string]any)) != 0 {
			t.Fatal("malformed hook", v, err)
		}
	}
}
func TestAGYSameProcessSwitchFencesOldEndpoint(t *testing.T) {
	h, root, env := agyFixture(t)
	_, done := startFakeAGY(t, h, root, agyTestID, env)
	ctx := context.Background()
	if err := agySupersede(ctx, h, root, agyOtherID, mustAGYOwner(env)); err != nil {
		t.Fatal(err)
	}
	p, _ := agy.New(provider.Config{}, h)
	rows, _ := p.Discover(ctx)
	if len(rows) != 0 {
		t.Fatal("superseded endpoint visible")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("superseded endpoint did not stop")
	}
	if agyMarker(ctx, h, root, agyTestID, "detached") {
		t.Fatal("switch created sticky detach")
	}
}
func TestAGYUnconfirmedHookStopsNothing(t *testing.T) {
	h, root, env := agyFixture(t)
	ctx := context.Background()
	calls := 0
	env.classify = func(context.Context, string, string) (string, error) {
		calls++
		if calls == 1 {
			return "top-level", nil
		}
		return "subagent", nil
	}
	v, err := agyHookWith(ctx, h, root, "session-start", strings.NewReader(`{"conversationId":"`+agyTestID+`"}`), env)
	if err != nil || len(v.(map[string]any)) != 0 {
		t.Fatal(v, err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("unconfirmed hook changed state")
	}
}
