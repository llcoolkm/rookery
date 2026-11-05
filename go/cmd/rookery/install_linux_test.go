//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallerRejectsSymlinkAndPublicSettings(t *testing.T) {
	h, root, _, env := installFixture(t)
	target := filepath.Join(root, "unrelated")
	if err := os.WriteFile(target, []byte(`{"keep":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := installWith(context.Background(), h, root, installerArgs("claude", "accept-inbound", "apply"), env); err == nil {
		t.Fatal("symlinked settings accepted")
	}
	now, _ := os.ReadFile(target)
	if string(now) != `{"keep":true}` {
		t.Fatal("unrelated file changed")
	}
	if err := os.Remove(filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := installWith(context.Background(), h, root, installerArgs("claude", "accept-inbound", "apply"), env); err == nil {
		t.Fatal("nonprivate settings accepted")
	}
}
