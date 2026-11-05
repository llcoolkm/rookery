//go:build windows

package main

import (
	"context"
	"github.com/llcoolkm/rookery/go/platform"
	host "github.com/llcoolkm/rookery/go/platform/windows"
)

func newHost(state string) (platform.Host, error) { return host.New(state) }
func platformSignals(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
