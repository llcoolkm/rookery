package main

import (
	"strings"
	"testing"
)

func TestSessionsTable(t *testing.T) {
	directory := map[string]any{"sessions": []any{
		map[string]any{"node": "server", "name": "idle-agent", "status": "idle", "online": true},
		map[string]any{"node": "server02", "name": "waiting\x1b-agent", "status": "waiting", "online": true,
			"waiting_for": "permission", "status_since": float64(97), "tmux": "main:4", "cwd": "/home/km/work"},
		map[string]any{"node": "offline-agent", "status": "unknown", "online": false},
	}}
	var output strings.Builder
	if err := writeSessions(&output, directory, 100); err != nil {
		t.Fatal(err)
	}
	table := output.String()
	if waiting, idle := strings.Index(table, "waiting?-agent"), strings.Index(table, "idle-agent"); waiting < 0 || idle < waiting {
		t.Fatalf("waiting session should precede idle session: %s", table)
	}
	for _, value := range []string{"permission", "3s", "server02 main:4", "/home/km/work"} {
		if !strings.Contains(table, value) {
			t.Fatalf("missing %q in %s", value, table)
		}
	}
	if strings.Contains(table, "offline-agent") || strings.Contains(table, "\x1b") {
		t.Fatalf("expected only sanitized live sessions: %s", table)
	}
}
