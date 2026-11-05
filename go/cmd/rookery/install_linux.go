//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	linux "github.com/llcoolkm/rookery/go/platform/linux"
)

type installEnvironment struct {
	executable func(string) (string, error)
	self       func() (string, error)
	managed    func() bool
}

func installCommand(ctx context.Context, h platform.Host, root string, a arguments) (any, error) {
	return installWith(ctx, h, root, a, installEnvironment{executable: linux.ResolveExecutable, self: os.Executable, managed: func() bool {
		info, err := os.Stat("/etc/claude-code/managed-mcp.json")
		if err != nil {
			return !errors.Is(err, fs.ErrNotExist)
		}
		return info.Mode().IsRegular()
	}})
}
func installWith(ctx context.Context, h platform.Host, root string, a arguments, env installEnvironment) (any, error) {
	name := a.pos(0)
	if name != "codex" && name != "claude" {
		return nil, errors.New("install supports codex and claude; AGY is deferred to phase 4")
	}
	if name != "claude" && a.options["accept-inbound"] == "true" {
		return nil, errors.New("--accept-inbound applies only to claude")
	}
	executable, err := env.executable(name)
	if err != nil {
		return nil, errors.New("provider CLI unavailable")
	}
	self, err := env.self()
	if err != nil {
		return nil, errors.New("helper executable unavailable")
	}
	want := []string{"--state-dir", root, "mcp", "--provider", name}
	argv := []string{"mcp", "add"}
	if name == "claude" {
		argv = append(argv, "-s", "user")
	}
	argv = append(argv, "rookery", "--", self)
	argv = append(argv, want...)
	get := []string{"mcp", "get", "rookery"}
	if name == "codex" {
		get = append(get, "--json")
	}
	current, err := h.Exec.Exec(ctx, platform.Spec{Path: executable, Args: get, Timeout: 30 * time.Second, MaxOutput: 1 << 20})
	if err != nil || current.State != platform.Completed || current.Truncated {
		return nil, errors.New("provider configuration unavailable")
	}
	actions := []any{}
	notes := []string{"Tool permission grants are unchanged; allow the rookery tools in the provider's own permission flow if needed."}
	configured := current.ExitCode == 0
	var matches any
	managed := false
	if configured {
		matches = installMatches(name, current.Stdout, self, want)
		if matches == nil {
			notes = append(notes, "Existing rookery MCP entry is preserved; the provider listing cannot prove exact arguments. Inspect it before replacement.")
		} else if matches == false {
			notes = append(notes, "Existing rookery MCP entry differs from this install and is preserved. Replace it explicitly through the provider CLI if intended.")
		}
	} else if name == "claude" && env.managed() {
		managed = true
		notes = append(notes, "Claude MCP is managed; registration is refused. Ask the administrator to add this server to managed configuration.")
	} else {
		actions = append(actions, map[string]any{"run": append([]string{executable}, argv...)})
	}
	providerRoot := ""
	writeInbound := false
	if name == "claude" {
		providerRoot, err = h.Paths.ProviderData(name)
		if err != nil {
			return nil, errors.New("provider configuration unavailable")
		}
		settings, _, err := installSettings(ctx, h, providerRoot)
		if err != nil {
			return nil, err
		}
		if value, exists := settings["crossSessionInbound"]; exists {
			if value != "accept" {
				notes = append(notes, "crossSessionInbound is set explicitly and is preserved; Claude may ask for approval on or refuse incoming messages.")
			}
		} else if a.options["accept-inbound"] == "true" {
			writeInbound = true
			actions = append(actions, map[string]any{"setting": "crossSessionInbound=accept", "replaces": settings != nil})
			notes = append(notes, "crossSessionInbound is not set; --apply sets it to accept for the whole account, keeping other keys. Private settings are required.")
		} else {
			notes = append(notes, "crossSessionInbound is not set, so Claude will ask for approval on each incoming Rookery message. --accept-inbound opts the whole account into accepting these messages.")
		}
	}
	notes = append(notes, "Old-name tool grants must be updated by the operator; the installer never edits permission grants.")
	plan := map[string]any{"provider": name, "configured": configured, "actions": actions, "notes": notes, "applied": false}
	if configured {
		plan["matches"] = matches
	}
	if managed {
		plan["managed"] = true
	}
	if a.options["apply"] != "true" {
		plan["hint"] = "re-run with --apply to make these changes"
		return plan, nil
	}
	done := []any{}
	for _, action := range actions {
		if _, ok := action.(map[string]any)["run"]; !ok {
			continue
		}
		if name == "claude" && env.managed() {
			return nil, errors.New("Claude MCP is managed; registration is refused")
		}
		result, err := h.Exec.Exec(ctx, platform.Spec{Path: executable, Args: argv, Timeout: 30 * time.Second, MaxOutput: 1 << 20})
		if err != nil || result.State != platform.Completed || result.ExitCode != 0 || result.Truncated {
			return nil, errors.New("provider MCP registration outcome unknown; inspect configuration before retry")
		}
		done = append(done, map[string]any{"ran": append([]string{executable}, argv...)})
	}
	if writeInbound {
		// Registration may have changed settings; re-read before merging and never
		// replace an explicit choice which appeared after planning.
		if _, err = h.Files.PrivateState(ctx, providerRoot); err != nil {
			return nil, errors.New("provider configuration unavailable")
		}
		lock, err := h.Files.AdvisoryLock(ctx, providerRoot, "rookery-go-install.lock", true)
		if err != nil {
			return nil, errors.New("provider settings unavailable")
		}
		defer lock.Close()
		settings, original, err := installSettings(ctx, h, providerRoot)
		if err != nil {
			return nil, err
		}
		if _, exists := settings["crossSessionInbound"]; !exists {
			if settings == nil {
				settings = map[string]any{}
			}
			settings["crossSessionInbound"] = "accept"
			if original != nil {
				backup, err := installBackup(ctx, h, providerRoot, original)
				if err != nil {
					return nil, err
				}
				done = append(done, map[string]any{"backup": backup})
			}
			if err = writeJSON(ctx, h, providerRoot, "settings.json", settings); err != nil {
				return nil, errors.New("provider settings write failed")
			}
			done = append(done, map[string]any{"wrote": "settings.json"})
		}
	}
	plan["applied"] = true
	plan["done"] = done
	return plan, nil
}
func installMatches(provider string, b []byte, self string, want []string) any {
	if provider == "codex" {
		var entry struct {
			Transport struct {
				Command *string   `json:"command"`
				Args    *[]string `json:"args"`
			} `json:"transport"`
		}
		if json.Unmarshal(b, &entry) != nil || entry.Transport.Command == nil || entry.Transport.Args == nil {
			return nil
		}
		return *entry.Transport.Command == self && equalStrings(*entry.Transport.Args, want)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	command, ok := fields["Command"]
	if !ok {
		return nil
	}
	if strings.Join(append([]string{command}, strings.Fields(fields["Args"])...), " ") == strings.Join(append([]string{self}, want...), " ") {
		return nil
	}
	return false
}
func installSettings(ctx context.Context, h platform.Host, root string) (map[string]any, []byte, error) {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	file, err := h.Files.OpenNoFollow(ctx, root, "settings.json", false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, errors.New("invalid provider settings")
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, nil, errors.New("invalid provider settings")
	}
	var settings map[string]any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	var extra any
	if decoder.Decode(&settings) != nil || settings == nil || decoder.Decode(&extra) != io.EOF {
		return nil, nil, errors.New("invalid provider settings")
	}
	return settings, b, nil
}
func installBackup(ctx context.Context, h platform.Host, root string, b []byte) (string, error) {
	exclusive, ok := h.Files.(interface {
		AtomicWriteExclusive(context.Context, string, string, []byte) error
	})
	if !ok {
		return "", errors.New("exclusive provider settings backup unavailable")
	}
	for range 10 {
		id, err := core.NewRequestID(time.Now())
		if err != nil {
			return "", errors.New("provider settings backup failed")
		}
		name := "settings.json.bak." + id
		if err = exclusive.AtomicWriteExclusive(ctx, root, name, b); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return "", errors.New("provider settings backup failed")
		}
		return name, nil
	}
	return "", errors.New("provider settings backup failed")
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
