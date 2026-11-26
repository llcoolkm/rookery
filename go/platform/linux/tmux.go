//go:build linux

package linux

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/platform"
)

func (p *processes) TmuxPanes(ctx context.Context) map[int]string {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return nil
	}
	return p.snapshotTmuxPanes(ctx, time.Now, listTmuxPanes)
}

func listTmuxPanes(ctx context.Context) (platform.ExecResult, error) {
	path, err := ResolveExecutable("tmux")
	if err != nil {
		return platform.ExecResult{}, err
	}
	return (executor{}).Exec(ctx, platform.Spec{
		Path: path, Args: []string{"list-panes", "-a", "-F", "#{pane_pid} #{session_name}:#{window_index}"},
		Timeout: time.Second, MaxOutput: 128 << 10,
	})
}

func (p *processes) snapshotTmuxPanes(ctx context.Context, now func() time.Time, list func(context.Context) (platform.ExecResult, error)) map[int]string {
	p.tmuxMu.Lock()
	defer p.tmuxMu.Unlock()
	if ctx.Err() != nil || now().Before(p.tmuxRetryAfter) {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	result, err := list(probeCtx)
	if ctx.Err() == nil && (errors.Is(probeCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)) {
		p.tmuxRetryAfter = now().Add(time.Minute)
		return nil
	}
	if err != nil || result.State != platform.Completed || result.ExitCode != 0 {
		return nil
	}
	panes := map[int]string{}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		if !utf8.ValidString(line) {
			continue
		}
		rawPID, coordinate, ok := strings.Cut(line, " ")
		pid, err := strconv.Atoi(rawPID)
		if ok && err == nil && pid > 0 && platform.ValidTmuxCoordinate(coordinate) && panes[pid] == "" {
			panes[pid] = coordinate
		}
	}
	return panes
}
