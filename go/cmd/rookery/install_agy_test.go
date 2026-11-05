//go:build linux || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"
)

type agyInstallExec struct {
	configured bool
	calls      [][]string
	unknown    bool
	listing    string
}

func (e *agyInstallExec) Exec(_ context.Context, s platform.Spec) (platform.ExecResult, error) {
	e.calls = append(e.calls, append([]string{}, s.Args...))
	if equalStrings(s.Args, []string{"mcp", "list"}) {
		listing := "rookery stdio enabled /synthetic/python-rookery\n" + e.listing
		if e.configured {
			listing += "rookery-go stdio enabled /synthetic/other-go-helper --state-dir /synthetic/other-state\n"
		}
		return platform.ExecResult{State: platform.Completed, Stdout: []byte(listing)}, nil
	}
	if len(s.Args) < 3 || !equalStrings(s.Args[:3], []string{"mcp", "add", "rookery-go"}) {
		return platform.ExecResult{}, errors.New("unexpected native command")
	}
	if e.unknown {
		return platform.ExecResult{State: platform.Uncertain}, errors.New("sensitive fake native details")
	}
	e.configured = true
	return platform.ExecResult{State: platform.Completed}, nil
}
func agyInstallFixture(t *testing.T) (platform.Host, string, *agyInstallExec, installEnvironment, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("AGY installer is Linux only")
	}
	h, root, _, env := installFixture(t)
	config := filepath.Join(t.TempDir(), "agy-config")
	t.Setenv("ROOKERY_AGY_CONFIG", config)
	exec := &agyInstallExec{}
	h.Exec = exec
	return h, root, exec, env, filepath.Join(config, "plugins", "rookery-go")
}

func TestAGYInstallerPlansWithoutWritingAndOnlyRegistersOwnMCP(t *testing.T) {
	h, root, exec, env, plugin := agyInstallFixture(t)
	plan := installPlan(t, h, root, installerArgs("agy"), env)
	if plan["applied"] != false || plan["configured"] != false || len(plan["actions"].([]any)) != 4 || len(exec.calls) != 1 {
		t.Fatal(plan, exec.calls)
	}
	if _, err := os.Lstat(filepath.Dir(filepath.Dir(plugin))); !os.IsNotExist(err) {
		t.Fatal("plan created provider directory", err)
	}
	plan = installPlan(t, h, root, installerArgs("agy", "apply"), env)
	if plan["applied"] != true || len(exec.calls) != 3 {
		t.Fatal(plan, exec.calls)
	}
	want := []string{"mcp", "add", "rookery-go", "--", "/synthetic/rookery", "--state-dir", root, "mcp", "--provider", "agy"}
	if !equalStrings(exec.calls[2], want) {
		t.Fatal(exec.calls[2])
	}
	for _, name := range []string{"plugin.json", "hooks.json", "rules/AGENTS.md"} {
		if err := h.Files.EnsurePrivate(context.Background(), filepath.Join(plugin, filepath.FromSlash(name)), false); err != nil {
			t.Fatal(name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(plugin, "mcp_config.json")); !os.IsNotExist(err) {
		t.Fatal("duplicate plugin MCP generated")
	}
	manifest, _ := os.ReadFile(filepath.Join(plugin, "plugin.json"))
	var object map[string]string
	if json.Unmarshal(manifest, &object) != nil || object["name"] != "rookery-go" {
		t.Fatal(string(manifest))
	}
	hooks, _ := os.ReadFile(filepath.Join(plugin, "hooks.json"))
	for _, want := range []string{"rookery-go", "SessionStart", "PreInvocation", "agy-hook session-start", "agy-hook pre-invocation"} {
		if !bytes.Contains(hooks, []byte(want)) {
			t.Fatal("missing hook", want)
		}
	}
	for _, forbidden := range []string{"permissionOverrides", "PreToolUse", "\"allow\"", "ANTIGRAVITY_CSRF_TOKEN", "ANTIGRAVITY_LS_ADDRESS", "agentapi send-message"} {
		if bytes.Contains(hooks, []byte(forbidden)) {
			t.Fatal("unsafe generated hooks", forbidden)
		}
	}
	plan = installPlan(t, h, root, installerArgs("agy", "apply"), env)
	if len(plan["actions"].([]any)) != 0 || len(plan["done"].([]any)) != 0 || len(exec.calls) != 4 {
		t.Fatal("install not idempotent", plan, exec.calls)
	}
}

func TestAGYInstallerPreservesPythonAndExistingGoRegistrationAndBacksUp(t *testing.T) {
	h, root, exec, env, plugin := agyInstallFixture(t)
	python := filepath.Join(filepath.Dir(plugin), "rookery")
	if err := os.MkdirAll(python, 0700); err != nil {
		t.Fatal(err)
	}
	pythonFile := filepath.Join(python, "hooks.json")
	if err := os.WriteFile(pythonFile, []byte("synthetic Python plugin bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	exec.configured = true
	installPlan(t, h, root, installerArgs("agy", "apply"), env)
	original := []byte("synthetic edited plugin bytes\n")
	if err := os.WriteFile(filepath.Join(plugin, "plugin.json"), original, 0600); err != nil {
		t.Fatal(err)
	}
	oldMCP := filepath.Join(plugin, "mcp_config.json")
	if err := os.WriteFile(oldMCP, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, h, root, installerArgs("agy", "apply"), env)
	var backups []string
	for _, action := range plan["done"].([]any) {
		if name, ok := action.(map[string]any)["backup"].(string); ok {
			backups = append(backups, name)
		}
	}
	if len(backups) != 1 {
		t.Fatal("backup missing", plan)
	}
	saved, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(saved, original) {
		t.Fatal("original backup changed", err)
	}
	if err := h.Files.EnsurePrivate(context.Background(), backups[0], false); err != nil {
		t.Fatal("backup not private", err)
	}
	if now, _ := os.ReadFile(pythonFile); string(now) != "synthetic Python plugin bytes" {
		t.Fatal("Python plugin changed")
	}
	if now, _ := os.ReadFile(oldMCP); string(now) != "{}" {
		t.Fatal("stale MCP config changed")
	}
	for _, call := range exec.calls {
		if !equalStrings(call, []string{"mcp", "list"}) {
			t.Fatal("existing Go MCP replaced", exec.calls)
		}
	}
	if plan["matches"] != nil || plan["configured"] != true {
		t.Fatal("lossy list falsely proved argv", plan)
	}
	text, _ := json.Marshal(plan["notes"])
	if !bytes.Contains(text, []byte("mcp_config.json")) || !bytes.Contains(text, []byte("cannot prove exact arguments")) {
		t.Fatal("preservation note missing", string(text))
	}
}

func TestAGYInstallerRefusesUnsafeFilesAndUnknownRegistration(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		h, root, exec, env, plugin := agyInstallFixture(t)
		if err := os.MkdirAll(plugin, 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "unrelated")
		if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(plugin, "plugin.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := installWith(context.Background(), h, root, installerArgs("agy", "apply"), env); err == nil {
			t.Fatal("symlink accepted")
		}
		if len(exec.calls) != 1 {
			t.Fatal("registered despite unsafe plugin")
		}
		if now, _ := os.ReadFile(target); string(now) != "unchanged" {
			t.Fatal("symlink target changed")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		h, root, exec, env, plugin := agyInstallFixture(t)
		exec.unknown = true
		if _, err := installWith(context.Background(), h, root, installerArgs("agy", "apply"), env); err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatal("unredacted unknown result", err)
		}
		if _, err := os.Lstat(plugin); !os.IsNotExist(err) {
			t.Fatal("plugin written after unknown registration")
		}
	})
	t.Run("inbound", func(t *testing.T) {
		h, root, exec, env, _ := agyInstallFixture(t)
		if _, err := installWith(context.Background(), h, root, installerArgs("agy", "accept-inbound"), env); err == nil || len(exec.calls) != 0 {
			t.Fatal("wrong-provider inbound flag reached native CLI")
		}
	})
}

func TestAGYInstallerWindowsRefusesBeforeAnyNativeAccess(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows refusal")
	}
	env := installEnvironment{executable: func(string) (string, error) { t.Fatal("resolved Windows AGY executable"); return "", nil }, self: func() (string, error) { t.Fatal("resolved helper for unsupported install"); return "", nil }}
	if _, err := installWith(context.Background(), platform.Host{}, "", installerArgs("agy", "apply"), env); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal("Windows AGY install accepted", err)
	}
}

func TestAGYPluginCommandQuotesShellMetacharacters(t *testing.T) {
	files := agyPluginFiles("/synthetic/helper with ' quote", "/synthetic/state;$(literal)")
	var hooks map[string]map[string][]map[string]any
	if json.Unmarshal(files[1].body, &hooks) != nil {
		t.Fatal("invalid hook JSON")
	}
	command := hooks["rookery-go"]["SessionStart"][0]["command"].(string)
	if !strings.Contains(command, "'\"'\"'") || !strings.Contains(command, "'/synthetic/state;$(literal)'") {
		t.Fatal("command not shell quoted", command)
	}
}
