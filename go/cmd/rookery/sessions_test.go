package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

func TestSessionsTableDistinguishesDirectoryStates(t *testing.T) {
	for _, tc := range []struct {
		directory map[string]any
		want      string
	}{
		{map[string]any{"detail": "helper has not connected yet"}, "Helper has not connected yet."},
		{map[string]any{"stale": true}, "Peer data is stale."},
		{map[string]any{}, "No live peer sessions."},
	} {
		var output strings.Builder
		if err := writeSessions(&output, tc.directory, 100); err != nil || !strings.Contains(output.String(), tc.want) {
			t.Fatalf("wanted %q: %q, %v", tc.want, output.String(), err)
		}
	}
}

func TestSessionsWatchRecoversFromReadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	broken := errors.New("cache unavailable\x1b\u202e\n")
	calls := 0
	peers := func(context.Context) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return nil, broken
		case 2:
			return map[string]any{}, nil
		default:
			cancel()
			return nil, nil
		}
	}
	var output strings.Builder
	if err := readSessions(ctx, &output, 0, false, peers); !errors.Is(err, broken) || output.Len() != 0 {
		t.Fatalf("one-shot did not surface read error: %q, %v", output.String(), err)
	}
	calls = 0
	if err := readSessions(ctx, &output, time.Millisecond, false, peers); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || !strings.Contains(output.String(), "Could not read peer sessions: cache unavailable???\n") || !strings.Contains(output.String(), "No live peer sessions.") {
		t.Fatalf("watch did not recover on next read: %d reads, %q", calls, output.String())
	}
}
