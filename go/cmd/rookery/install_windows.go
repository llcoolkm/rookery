//go:build windows

package main

import (
	"context"
	"github.com/llcoolkm/rookery/go/platform"
	host "github.com/llcoolkm/rookery/go/platform/windows"
	"os"
	"path/filepath"
)

func installCommand(ctx context.Context, h platform.Host, root string, a arguments) (any, error) {
	return installWith(ctx, h, root, a, installEnvironment{executable: host.ResolveExecutable, self: os.Executable, managed: func() bool {
		// Policy in the machine installation directory takes precedence. An unreadable
		// policy is managed too; it must never be bypassed as an absent policy.
		base := os.Getenv("ProgramFiles")
		if base == "" {
			return true
		}
		_, err := os.Lstat(filepath.Join(base, "ClaudeCode", "managed-mcp.json"))
		return !os.IsNotExist(err)
	}})
}
