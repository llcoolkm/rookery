package core

import "testing"

func TestSendsDefaultToTwelveHoursAndRepliesToADay(t *testing.T) {
	if SendTTL != 43200 || ReplyTTL != 86400 {
		t.Fatalf("defaults changed: send %d reply %d", SendTTL, ReplyTTL)
	}
	for _, tool := range MCPTools() {
		if tool["name"] != "send" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if got := props["ttl"].(map[string]any)["default"]; got != SendTTL {
			t.Fatalf("send tool ttl default %v", got)
		}
		return
	}
	t.Fatal("send tool missing")
}
