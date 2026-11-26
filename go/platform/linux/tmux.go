//go:build linux

package linux

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

func (*processes) TmuxPanes(ctx context.Context) map[int]string {
	path, err := ResolveExecutable("tmux")
	if err != nil {
		return nil
	}
	result, err := (executor{}).Exec(ctx, platform.Spec{
		Path: path, Args: []string{"list-panes", "-a", "-F", "#{pane_pid} #{session_name}:#{window_index}"},
		Timeout: time.Second, MaxOutput: 128 << 10,
	})
	if err != nil || result.State != platform.Completed || result.ExitCode != 0 {
		return nil
	}
	panes := map[int]string{}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		rawPID, coordinate, ok := strings.Cut(line, " ")
		pid, err := strconv.Atoi(rawPID)
		if ok && err == nil && pid > 0 && coordinate != "" && panes[pid] == "" {
			panes[pid] = coordinate
		}
	}
	return panes
}
