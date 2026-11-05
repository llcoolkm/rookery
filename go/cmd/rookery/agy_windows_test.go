//go:build windows

package main

import (
	"context"
	"github.com/llcoolkm/rookery/go/platform"
	"strings"
	"testing"
)

func TestWindowsAGYCommandsReportUnsupportedBeforeHostAccess(t *testing.T) {
	for _, command := range []string{"attach", "detach", "agy-hook"} {
		_, err := agyCommand(context.Background(), platform.Host{}, "", arguments{command: command, options: map[string]string{}})
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatal(command, err)
		}
	}
	status := agyPlatformStatus(context.Background(), platform.Host{}, "").(map[string]any)
	if status["supported"] != false {
		t.Fatal(status)
	}
}
