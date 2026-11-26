package core

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/platform"
)

type sessionObservation struct {
	status string
	since  float64
	pid    int
	ref    platform.ProcessRef
}

func (h *Helper) observeSessions(ctx context.Context, discovered []Session, now float64) []Session {
	var panes map[int]string
	if tmux, ok := h.Process.(platform.TmuxPanes); ok {
		for _, session := range discovered {
			if sessionPaneProcess(session) != nil {
				panes = tmux.TmuxPanes(ctx)
				break
			}
		}
	}
	sessions := make([]Session, 0, len(discovered))
	observations := make(map[string]sessionObservation, len(discovered))
	for _, session := range discovered {
		key := session.Provider + ":" + session.ID
		current := sessionObservation{status: session.Status, since: now, pid: session.PID}
		if session.ProcessRef != nil {
			current.ref = *session.ProcessRef
		}
		if old, ok := h.observations[key]; ok && old.since <= now && old.status == current.status && old.pid == current.pid && old.ref == current.ref {
			current.since = old.since
		}
		observations[key] = current
		session.StatusSince = current.since
		if session.Status == "waiting" {
			session.WaitingFor = sessionText(session.WaitingFor, 128)
		} else {
			session.WaitingFor = ""
		}
		session.Cwd = sessionText(session.Cwd, 512)
		session.Tmux = h.sessionTmux(ctx, sessionPaneProcess(session), panes)
		sessions = append(sessions, session)
	}
	h.observations = observations
	return sessions
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
		coordinate := sessionText(panes[process.PID], 128)
		name, window, ok := strings.Cut(coordinate, ":")
		if ok && name != "" && window != "" && strings.Trim(window, "0123456789") == "" {
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
	optional := [...]string{"cwd", "tmux", "waiting_for", "status_since"}
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
