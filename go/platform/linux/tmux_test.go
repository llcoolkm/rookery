//go:build linux

package linux

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

func TestTmuxSnapshotSkipsInvalidLinesIndividually(t *testing.T) {
	p := &processes{}
	got := p.snapshotTmuxPanes(t.Context(), time.Now, func(context.Context) (platform.ExecResult, error) {
		return platform.ExecResult{State: platform.Completed, Stdout: []byte("bad\n2 no-index\n1 main:2\n2 workspace:4\n3 junk\x00:1\n4 bad\xffname:3\n5 bad\xffname:2\n5 after:0\n")}, nil
	})
	want := map[int]string{1: "main:2", 2: "workspace:4", 5: "after:0"}
	if !maps.Equal(got, want) {
		t.Fatalf("tmux panes = %v, want %v", got, want)
	}
}

func TestTmuxSnapshotFailureAndTimeoutCooldown(t *testing.T) {
	p := &processes{}
	now := time.Now()
	clock := func() time.Time { return now }
	for _, tc := range []struct {
		result platform.ExecResult
		err    error
	}{
		{platform.ExecResult{State: platform.NotStarted}, errors.New("unavailable")},
		{platform.ExecResult{State: platform.Completed, ExitCode: 1, Stdout: []byte("1 ignored:1\n")}, nil},
		{platform.ExecResult{State: platform.Uncertain, Stdout: []byte("1 partial:2\n")}, errors.New("output limit")},
	} {
		if panes := p.snapshotTmuxPanes(t.Context(), clock, func(context.Context) (platform.ExecResult, error) {
			return tc.result, tc.err
		}); panes != nil || !p.tmuxRetryAfter.IsZero() {
			t.Fatalf("ordinary tmux failure returned panes or started cooldown: %v", panes)
		}
	}
	if panes := p.snapshotTmuxPanes(t.Context(), clock, func(ctx context.Context) (platform.ExecResult, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("tmux command missing deadline")
		}
		return platform.ExecResult{}, context.DeadlineExceeded
	}); panes != nil {
		t.Fatalf("timeout returned panes: %v", panes)
	}
	now = now.Add(time.Minute - time.Nanosecond)
	p.snapshotTmuxPanes(t.Context(), clock, func(context.Context) (platform.ExecResult, error) {
		t.Fatal("retried tmux before cooldown ended")
		return platform.ExecResult{}, nil
	})
	now = now.Add(time.Nanosecond)
	got := p.snapshotTmuxPanes(t.Context(), clock, func(context.Context) (platform.ExecResult, error) {
		return platform.ExecResult{State: platform.Completed, Stdout: []byte("1 recovered:1\n")}, nil
	})
	if got[1] != "recovered:1" {
		t.Fatalf("did not recover tmux after cooldown: %v", got)
	}
}
