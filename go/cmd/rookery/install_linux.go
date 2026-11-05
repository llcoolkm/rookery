//go:build linux

package main

import (
	"context"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
	linux "github.com/llcoolkm/rookery/go/platform/linux"
	"io/fs"
	"os"
)

func installCommand(ctx context.Context, h platform.Host, root string, a arguments) (any, error) {
	return installWith(ctx, h, root, a, installEnvironment{executable: linux.ResolveExecutable, self: os.Executable, managed: func() bool {
		info, err := os.Stat("/etc/claude-code/managed-mcp.json")
		if err != nil {
			return !errors.Is(err, fs.ErrNotExist)
		}
		return info.Mode().IsRegular()
	}})
}
