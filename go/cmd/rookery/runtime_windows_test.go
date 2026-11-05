//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWindowsCLIPrivateOfflineState(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	h, err := newHost(root)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic key remains in a private fixture file and never reaches a hub.
	if err = h.Files.AtomicWrite(ctx, root, "test-bearer", bytes.Repeat([]byte{'x'}, 32)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROOKERY_SESSION", "")
	run := func(args ...string) any {
		t.Helper()
		v, err := runPlatform(ctx, append([]string{"--state-dir", root}, args...))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	configured := run("configure", "--node", "alpha", "--url", "https://hub.example.test", "--token-file", filepath.Join(root, "test-bearer")).(map[string]any)
	if configured["node"] != "alpha" || configured["token_sha256"] != fmt.Sprintf("%x", sha256.Sum256(bytes.Repeat([]byte{'x'}, 32))) || configured["next_steps"] != "Give this hash to the hub operator; they add it with `rookery hub node add NODE HASH` (or deploy/kubernetes/hub-node.sh for Kubernetes). Then check `rookery health`." {
		t.Fatal("configure did not explain hub registration", configured)
	}
	encoded, err := json.Marshal(configured)
	if err != nil || bytes.Contains(encoded, bytes.Repeat([]byte{'x'}, 32)) {
		t.Fatal("configure output disclosed bearer token")
	}
	run("register-polling", "codex:00000000-0000-4000-8000-000000000001", "--pid", strconv.Itoa(os.Getpid()))
	run("health")
	run("inbox", "--as", "codex:00000000-0000-4000-8000-000000000001", "--peek")
	run("unregister-polling", "codex:00000000-0000-4000-8000-000000000001")
	plan := run("service", "install", "--dry-run").(map[string]any)
	if plan["backend"] != "scheduled-task" || plan["started"] != false {
		t.Fatal("invalid service plan")
	}
	if err = h.Files.AtomicWrite(ctx, root, "helper.sqlite3", []byte("synthetic Python marker")); err != nil {
		t.Fatal(err)
	}
	if _, err = runPlatform(ctx, []string{"--state-dir", root, "service", "status"}); err == nil || !strings.Contains(err.Error(), "Python helper state") {
		t.Fatal("Python state not refused before service access")
	}
}
