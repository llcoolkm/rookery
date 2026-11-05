//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCLIIsolatedPollingAndConfiguration(t *testing.T) {
	ctx := context.Background()
	temp := t.TempDir()
	if e := os.Chmod(temp, 0700); e != nil {
		t.Fatal(e)
	}
	home := filepath.Join(temp, "home")
	if e := os.Mkdir(home, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("PATH", home)
	t.Setenv("ROOKERY_SESSION", "")
	state := filepath.Join(temp, "state")
	token := filepath.Join(temp, "test-bearer")
	if e := os.WriteFile(token, bytes.Repeat([]byte{'x'}, 32), 0600); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Op string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := map[string]any{}
		switch req.Op {
		case "publish":
			out["published"] = 1
		case "sync":
			out = map[string]any{"events": []any{}, "cursor": 0}
		case "list":
			out["sessions"] = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
	}))
	defer server.Close()
	run := func(args ...string) any {
		t.Helper()
		v, e := runPlatform(ctx, append([]string{"--state-dir", state}, args...))
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	run("configure", "--node", "alpha", "--url", server.URL, "--token-file", token, "--allow-http-loopback")
	run("register-polling", "codex:00000000-0000-4000-8000-000000000001", "--pid", strconv.Itoa(os.Getpid()))
	run("daemon", "--once")
	v := run("health").(map[string]any)
	if v["running"] != false {
		t.Fatal("once helper marked running")
	}
	run("inbox", "--as", "codex:00000000-0000-4000-8000-000000000001", "--peek")
	run("unregister-polling", "codex:00000000-0000-4000-8000-000000000001")
	run("busy-delivery", "claude", "off")
	var config map[string]any
	b, e := os.ReadFile(filepath.Join(state, "config.json"))
	if e != nil || json.Unmarshal(b, &config) != nil {
		t.Fatal("configuration unavailable")
	}
	if a, ok := config["busy_delivery"].([]any); !ok || len(a) != 0 {
		t.Fatal("empty busy policy lost")
	}
	if _, e = runPlatform(ctx, []string{"--state-dir", state, "mcp"}); e == nil || !strings.Contains(e.Error(), "MCP requires") {
		t.Fatal("unbound MCP accepted")
	}
	if _, e = runPlatform(ctx, []string{"--state-dir", state, "--as", "codex:00000000-0000-4000-8000-000000000001", "mcp", "--provider", "codex"}); e == nil {
		t.Fatal("conflicting MCP binding accepted")
	}
	v = run("service", "install", "--dry-run", "--backend", "manual").(map[string]any)
	if v["backend"] != "manual" || v["started"] != false {
		t.Fatal("invalid service plan")
	}
}
func TestParserReorderedFlags(t *testing.T) {
	a, e := parse([]string{"--state-dir", "synthetic", "reply", "synthetic-id", "--body", "body", "--ttl=17"})
	if e != nil || a.options["ttl"] != "17" || a.pos(0) != "synthetic-id" {
		t.Fatal("flag parsing")
	}
	for _, args := range [][]string{{"send", "--bad"}, {"send", "--body"}, {"inbox", "--peek=false"}} {
		if _, e := parse(args); e == nil {
			t.Fatal("invalid flags accepted")
		}
	}
}

func TestCLIRefusesPythonStateBeforeConfigurationOrService(t *testing.T) {
	root := t.TempDir()
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(root, "helper.sqlite3")
	if e := os.WriteFile(marker, []byte("synthetic marker; never open"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, cmd := range []string{"configure", "service", "mcp"} {
		if _, e := runPlatform(context.Background(), []string{"--state-dir", root, cmd}); e == nil || !strings.Contains(e.Error(), "Python helper state") {
			t.Fatal("Python helper root accepted")
		}
	}
	entries, e := os.ReadDir(root)
	if e != nil || len(entries) != 1 || entries[0].Name() != "helper.sqlite3" {
		t.Fatal("refused Python root was modified")
	}
}
