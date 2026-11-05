//go:build windows

package main

import (
	"context"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider/agy"
)

func agyCommand(context.Context, platform.Host, string, arguments) (any, error) {
	return nil, errors.New(agy.UnsupportedDetail)
}
func agyPlatformStatus(context.Context, platform.Host, string) any {
	return map[string]any{"supported": false, "detail": agy.UnsupportedDetail}
}
