//go:build linux

package core_test

import (
	"github.com/llcoolkm/rookery/go/platform"
	host "github.com/llcoolkm/rookery/go/platform/linux"
)

func newTestHost(root string) (platform.Host, error) { return host.New(root) }
