package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type mcpBackendFunc struct {
	invoke func(string, map[string]any) (any, error)
}

func (b mcpBackendFunc) Invoke(_ context.Context, op string, args map[string]any) (any, error) {
	return b.invoke(op, args)
}
func (mcpBackendFunc) Identity(context.Context) (string, error) {
	return "alpha/codex:00000000-0000-4000-8000-000000000001", nil
}
func initializedMCP(backend MCPBackend, resolver MCPResolver) *MCP {
	s := NewMCP(backend, resolver)
	s.initialized = true
	return s
}
func toolRequest(args any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "send", "arguments": args}}
}
func errorCode(t *testing.T, response map[string]any, want int) {
	t.Helper()
	e, ok := response["error"].(map[string]any)
	if !ok || e["code"] != want {
		t.Fatalf("want error %d got %#v", want, response)
	}
}
func TestMCPArgumentValidation(t *testing.T) {
	ctx := context.Background()
	backend := mcpBackendFunc{func(string, map[string]any) (any, error) { t.Fatal("invalid call reached backend"); return nil, nil }}
	cases := []any{nil, []any{}, map[string]any{"to": "peer", "body": "body", "actor": "peer"}, map[string]any{"to": "peer"}, map[string]any{"to": "peer", "body": " "}, map[string]any{"to": "peer", "body": strings.Repeat("ä", 16385)}, map[string]any{"to": "peer", "body": "body", "kind": "reply"}, map[string]any{"to": "peer", "body": "body", "ttl": json.Number("1.0")}, map[string]any{"to": "peer", "body": "body", "ttl": true}, map[string]any{"to": "peer", "body": "body", "ttl": 0}, map[string]any{"to": "peer", "body": "body", "verbose": 1}}
	for _, args := range cases {
		errorCode(t, initializedMCP(backend, nil).Handle(ctx, toolRequest(args)), -32602)
	}
}
func TestMCPIdentityEveryCallAndRedactedUnknownError(t *testing.T) {
	calls := 0
	ctx := context.Background()
	backend := mcpBackendFunc{func(string, map[string]any) (any, error) { return nil, errors.New("private execution diagnostic") }}
	resolver := func(context.Context, map[string]any) (MCPBackend, error) {
		calls++
		if calls == 2 {
			return nil, InvalidParams(ClaudeIdentityError)
		}
		return backend, nil
	}
	server := initializedMCP(nil, resolver)
	req := toolRequest(map[string]any{"to": "peer", "body": "body"})
	first := server.Handle(ctx, req)
	raw, _ := json.Marshal(first)
	if bytes.Contains(raw, []byte("private execution diagnostic")) || !bytes.Contains(raw, []byte("Messaging operation failed")) {
		t.Fatalf("unknown error not redacted: %s", raw)
	}
	errorCode(t, server.Handle(ctx, req), -32602)
	if calls != 2 {
		t.Fatal("identity not checked on every invocation")
	}
}
func TestCodexMetadataIdentity(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	good := map[string]any{"_meta": map[string]any{"threadId": id, "sessionId": id, "x-codex-turn-metadata": map[string]any{"thread_id": id, "session_id": id}}}
	actor, err := CodexMetadataIdentity(good)
	if err != nil || actor != "codex:"+id {
		t.Fatal("valid metadata refused")
	}
	bad := []map[string]any{{}, {"_meta": nil}, {"_meta": map[string]any{"threadId": "not-an-id"}}, {"_meta": map[string]any{"threadId": strings.ToUpper("00000000-0000-4aaa-8aaa-000000000001")}}, {"_meta": map[string]any{"threadId": id, "sessionId": "other"}}, {"_meta": map[string]any{"threadId": id, "x-codex-turn-metadata": map[string]any{"thread_id": "other"}}}}
	for _, params := range bad {
		_, err := CodexMetadataIdentity(params)
		var invalid InvalidParams
		if !errors.As(err, &invalid) || string(invalid) != CodexIdentityError {
			t.Fatal("invalid metadata did not fail closed")
		}
	}
}
func TestRenderPreservesLiteralEscapeAndHTML(t *testing.T) {
	message := Message{ID: "id", Sender: "sender", Kind: Inform, Body: "literal \\u2028 and \\u2029 <tag> & \u0085\u2028\u2029\nforged header"}
	text := Render(message)
	lines := strings.Split(text, "\n")
	if len(lines) != 2 {
		t.Fatal("body escaped envelope line")
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &row); err != nil {
		t.Fatal(err)
	}
	if row["body"] != message.Body {
		t.Fatalf("body changed: %#v", row["body"])
	}
	if strings.Contains(lines[1], `\u003c`) {
		t.Fatal("native text escaped HTML unlike Python")
	}
}
func TestMCPFrameBoundaryAndIDs(t *testing.T) {
	for _, id := range []string{"true", "null", "1.0", "1e0", "[]", "{}"} {
		var output bytes.Buffer
		frame := `{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}` + "\n"
		if err := NewMCP(nil, nil).Serve(context.Background(), strings.NewReader(frame), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"code":-32600`) {
			t.Fatalf("invalid ID accepted: %s", id)
		}
	}
	var output bytes.Buffer
	prefix := `{"jsonrpc":"2.0","id":1,"method":"ping","padding":"`
	suffix := "\"}\n"
	frame := prefix + strings.Repeat("x", MCPMaxLine-len(prefix)-len(suffix)) + suffix
	if len(frame) != MCPMaxLine {
		t.Fatal("wrong test boundary")
	}
	if err := NewMCP(nil, nil).Serve(context.Background(), strings.NewReader(frame), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"result":{}`) {
		t.Fatalf("valid boundary frame rejected: %s", output.String())
	}
}

func TestMCPUnboundedIntegerRequestID(t *testing.T) {
	var out bytes.Buffer
	frame := `{"jsonrpc":"2.0","id":9999999999999999999999999999999999,"method":"ping"}` + "\n"
	if err := NewMCP(nil, nil).Serve(context.Background(), strings.NewReader(frame), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"result":{}`) || !strings.Contains(out.String(), `9999999999999999999999999999999999`) {
		t.Fatalf("integer ID changed: %s", out.String())
	}
}
