package core

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/platform"
)

func (h *Helper) tmuxPanes(ctx context.Context, discovered []Session) map[int]string {
	if tmux, ok := h.Process.(platform.TmuxPanes); ok {
		for _, session := range discovered {
			if sessionPaneProcess(session) != nil {
				return tmux.TmuxPanes(ctx)
			}
		}
	}
	return nil
}

func sessionPaneProcess(session Session) *platform.ProcessRef {
	// Codex's discovered PID pins the shared app server. It does not identify
	// the thread's UI process, so its ancestor chain cannot locate the thread.
	if session.Provider == "codex" {
		return nil
	}
	return session.ProcessRef
}

func (h *Helper) sessionTmux(ctx context.Context, ref *platform.ProcessRef, panes map[int]string) string {
	if ref == nil || len(panes) == 0 {
		return ""
	}
	ancestors, err := h.Process.Ancestors(ctx, *ref)
	if err != nil {
		return ""
	}
	for _, process := range ancestors {
		if coordinate := panes[process.PID]; platform.ValidTmuxCoordinate(coordinate) {
			return coordinate
		}
	}
	return ""
}

func sessionText(value string, maxBytes int) string {
	if len(value) > maxBytes || strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool {
		return r < 32 || r >= 127 && r <= 159
	}) {
		return ""
	}
	return value
}

// Drop optional fields only when their exact HTTP encoding would push an
// otherwise working publication over the request size limit.
func sessionPublishRequest(rows []map[string]any, errs map[string]string) (RPCRequest, error) {
	payload := map[string]any{"sessions": rows, "errors": errs}
	optional := [...]string{"cwd", "tmux", "waiting_for"}
	for i := 0; ; i++ {
		data, err := json.Marshal(payload)
		if err != nil {
			return RPCRequest{}, err
		}
		request := RPCRequest{Op: "publish", Data: data}
		body, err := json.Marshal(request)
		if err != nil {
			return RPCRequest{}, err
		}
		if len(body) <= maxRequestBytes || i == len(optional) {
			return request, nil
		}
		for _, row := range rows {
			delete(row, optional[i])
		}
	}
}
