//go:build linux || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/llcoolkm/rookery/go/platform"
)

type installExec struct {
	configured bool
	current    []byte
	calls      [][]string
	duringAdd  func()
	unknown    bool
}

func (e *installExec) Exec(_ context.Context, s platform.Spec) (platform.ExecResult, error) {
	e.calls = append(e.calls, append([]string(nil), s.Args...))
	for _, arg := range s.Args {
		if arg == "rookery" {
			return platform.ExecResult{}, errors.New("production MCP entry touched")
		}
	}
	if s.Args[1] == "get" {
		code := 1
		if e.configured {
			code = 0
		}
		return platform.ExecResult{State: platform.Completed, ExitCode: code, Stdout: e.current}, nil
	}
	if e.duringAdd != nil {
		e.duringAdd()
	}
	if e.unknown {
		return platform.ExecResult{State: platform.Uncertain}, errors.New("sensitive native stderr")
	}
	e.configured = true
	return platform.ExecResult{State: platform.Completed}, nil
}
func installFixture(t *testing.T) (platform.Host, string, *installExec, installEnvironment) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	root := filepath.Join(t.TempDir(), "provider")
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	t.Setenv("CODEX_HOME", root)
	h, err := newHost(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Files.PrivateState(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	exec := &installExec{}
	h.Exec = exec
	env := installEnvironment{executable: func(name string) (string, error) { return "/synthetic/" + name, nil }, self: func() (string, error) { return "/synthetic/rookery", nil }, managed: func() bool { return false }}
	return h, root, exec, env
}
func installerArgs(provider string, flags ...string) arguments {
	a := arguments{command: "install", positionals: []string{provider}, options: map[string]string{}}
	for _, flag := range flags {
		a.options[flag] = "true"
	}
	return a
}
func installPlan(t *testing.T, h platform.Host, root string, a arguments, env installEnvironment) map[string]any {
	t.Helper()
	value, err := installWith(context.Background(), h, root, a, env)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}
func TestInstallerPlansThenUsesOnlyProviderCLI(t *testing.T) {
	for _, name := range []string{"codex", "claude"} {
		t.Run(name, func(t *testing.T) {
			h, root, exec, env := installFixture(t)
			plan := installPlan(t, h, root, installerArgs(name), env)
			if plan["applied"] != false || plan["configured"] != false || len(plan["actions"].([]any)) != 1 || len(exec.calls) != 1 {
				t.Fatal(plan, exec.calls)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("planning wrote provider files")
			}
			plan = installPlan(t, h, root, installerArgs(name, "apply"), env)
			if plan["applied"] != true || len(exec.calls) != 3 || exec.calls[2][1] != "add" {
				t.Fatal(plan, exec.calls)
			}
			expected := []string{"mcp", "add"}
			if name == "claude" {
				expected = append(expected, "-s", "user")
			}
			expected = append(expected, "rookery-go", "--", "/synthetic/rookery", "--state-dir", root, "mcp", "--provider", name)
			if !equalStrings(exec.calls[2], expected) {
				t.Fatal(exec.calls[2])
			}
			plan = installPlan(t, h, root, installerArgs(name, "apply"), env)
			if len(plan["actions"].([]any)) != 0 || len(plan["done"].([]any)) != 0 || len(exec.calls) != 4 {
				t.Fatal("existing entry was replaced", plan, exec.calls)
			}
		})
	}
}
func TestInstallerManagedClaudeNeverRegisters(t *testing.T) {
	h, root, exec, env := installFixture(t)
	env.managed = func() bool { return true }
	plan := installPlan(t, h, root, installerArgs("claude", "apply"), env)
	if plan["managed"] != true || len(plan["actions"].([]any)) != 0 || len(exec.calls) != 1 {
		t.Fatal(plan, exec.calls)
	}
}
func TestInstallerMatchingUsesLosslessArguments(t *testing.T) {
	want := []string{"--state-dir", "/synthetic/state with spaces", "mcp", "--provider", "codex"}
	b, _ := json.Marshal(map[string]any{"transport": map[string]any{"command": "/synthetic/rookery", "args": want}})
	if got := installMatches("codex", b, "/synthetic/rookery", want); got != true {
		t.Fatal(got)
	}
	other := append([]string(nil), want...)
	other[1] = "/synthetic/other"
	if got := installMatches("codex", b, "/synthetic/rookery", other); got != false {
		t.Fatal(got)
	}
	if got := installMatches("codex", []byte(`{"transport":{"command":"/synthetic/rookery"}}`), "/synthetic/rookery", want); got != nil {
		t.Fatal(got)
	}
	text := []byte("Command: /synthetic/rookery\nArgs: " + strings.Join(want, " ") + "\n")
	if got := installMatches("claude", text, "/synthetic/rookery", want); got != nil {
		t.Fatal("lossy listing falsely proved exact match", got)
	}
	if got := installMatches("claude", text, "/synthetic/other", want); got != false {
		t.Fatal(got)
	}
}
func TestInstallerInboundPreservesOtherKeysAndExactBackup(t *testing.T) {
	h, root, exec, env := installFixture(t)
	original := []byte("{\n  \"theme\": \"dark\",\n  \"permissions\": {\"allow\":[\"synthetic-tool\"]}\n}\n")
	if err := h.Files.AtomicWrite(context.Background(), root, "settings.json", original); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, h, root, installerArgs("claude", "accept-inbound"), env)
	if len(plan["actions"].([]any)) != 2 {
		t.Fatal(plan)
	}
	now, _ := os.ReadFile(filepath.Join(root, "settings.json"))
	if !bytes.Equal(now, original) {
		t.Fatal("plan altered settings")
	}
	plan = installPlan(t, h, root, installerArgs("claude", "accept-inbound", "apply"), env)
	var settings map[string]any
	now, _ = os.ReadFile(filepath.Join(root, "settings.json"))
	if json.Unmarshal(now, &settings) != nil || settings["crossSessionInbound"] != "accept" || settings["theme"] != "dark" || settings["permissions"] == nil {
		t.Fatal(settings)
	}
	var backup string
	for _, d := range plan["done"].([]any) {
		if name, ok := d.(map[string]any)["backup"].(string); ok {
			backup = name
		}
	}
	if backup == "" {
		t.Fatal("missing backup")
	}
	file, err := h.Files.OpenNoFollow(context.Background(), root, backup, false)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := io.ReadAll(file)
	file.Close()
	if !bytes.Equal(saved, original) {
		t.Fatal("backup altered original bytes")
	}
	info, _ := os.Stat(filepath.Join(root, "settings.json"))
	if info.Mode().Perm() != 0600 && runtime.GOOS == "linux" {
		t.Fatal("settings not private")
	}
	exec.calls = nil
	plan = installPlan(t, h, root, installerArgs("claude", "accept-inbound", "apply"), env)
	if len(plan["actions"].([]any)) != 0 || len(plan["done"].([]any)) != 0 || len(exec.calls) != 1 {
		t.Fatal("install is not idempotent", plan)
	}
}
func TestInstallerExplicitInboundChoiceAndConcurrentChangesPreserved(t *testing.T) {
	h, root, exec, env := installFixture(t)
	exec.configured = true
	original := []byte(`{"crossSessionInbound":"refuse","theme":"dark"}`)
	if err := h.Files.AtomicWrite(context.Background(), root, "settings.json", original); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, h, root, installerArgs("claude", "accept-inbound", "apply"), env)
	now, _ := os.ReadFile(filepath.Join(root, "settings.json"))
	if len(plan["actions"].([]any)) != 0 || !bytes.Equal(now, original) {
		t.Fatal("explicit setting overwritten")
	}
	if err := h.Files.Remove(context.Background(), root, "settings.json"); err != nil {
		t.Fatal(err)
	}
	exec.configured = false
	exec.duringAdd = func() {
		if err := h.Files.AtomicWrite(context.Background(), root, "settings.json", original); err != nil {
			t.Error(err)
		}
	}
	installPlan(t, h, root, installerArgs("claude", "accept-inbound", "apply"), env)
	now, _ = os.ReadFile(filepath.Join(root, "settings.json"))
	if !bytes.Equal(now, original) {
		t.Fatal("setting appeared during registration and was overwritten")
	}
}
func TestInstallerRejectsInvalidSettingsAndRedactsUnknownOutcome(t *testing.T) {
	for _, b := range []string{"[1]", "null", "invalid"} {
		t.Run(b, func(t *testing.T) {
			h, root, _, env := installFixture(t)
			h.Files.AtomicWrite(context.Background(), root, "settings.json", []byte(b))
			if _, err := installWith(context.Background(), h, root, installerArgs("claude", "apply", "accept-inbound"), env); err == nil {
				t.Fatal("invalid settings accepted")
			}
			now, _ := os.ReadFile(filepath.Join(root, "settings.json"))
			if string(now) != b {
				t.Fatal("invalid settings overwritten")
			}
		})
	}
	h, root, exec, env := installFixture(t)
	exec.unknown = true
	if _, err := installWith(context.Background(), h, root, installerArgs("codex", "apply"), env); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("unknown outcome exposed stderr", err)
	}
	if _, err := installWith(context.Background(), h, root, installerArgs("codex", "accept-inbound"), env); err == nil {
		t.Fatal("cross-provider inbound flag accepted")
	}
}

func TestInstallerCreatesAbsentPrivateSettingsAndPreservesLargeNumbers(t *testing.T) {
	h, root, _, env := installFixture(t)
	missing := filepath.Join(root, "new-config")
	t.Setenv("CLAUDE_CONFIG_DIR", missing)
	installPlan(t, h, root, installerArgs("claude", "accept-inbound"), env)
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("plan created config root")
	}
	installPlan(t, h, root, installerArgs("claude", "accept-inbound", "apply"), env)
	err := h.Files.EnsurePrivate(context.Background(), filepath.Join(missing, "settings.json"), false)
	if err != nil {
		t.Fatal("new settings not private", err)
	}
	if err := h.Files.AtomicWrite(context.Background(), missing, "settings.json", []byte(`{"integer":9007199254740993}`)); err != nil {
		t.Fatal(err)
	}
	installPlan(t, h, root, installerArgs("claude", "accept-inbound", "apply"), env)
	body, _ := os.ReadFile(filepath.Join(missing, "settings.json"))
	if !bytes.Contains(body, []byte(`9007199254740993`)) {
		t.Fatal("settings number lost precision")
	}
}
