//go:build windows

package contract_test

import (
	"github.com/llcoolkm/rookery/go/platform"
	host "github.com/llcoolkm/rookery/go/platform/windows"
)

func newTestHost(root string) (platform.Host, error) { return host.New(root) }
