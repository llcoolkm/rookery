//go:build windows

package windows

import (
	"context"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
)

// New opens isolated Go state and wires only native Windows facilities.
func New(state string) (platform.Host, error) {
	files, err := newFiles()
	if err != nil {
		return platform.Host{}, err
	}
	process, err := NewProcess()
	if err != nil {
		return platform.Host{}, err
	}
	paths, err := newPaths(state, files, process)
	if err != nil {
		return platform.Host{}, err
	}
	return platform.Host{Paths: paths, Files: files, Process: process, IPC: &ipc{paths: paths}, Exec: Executor{}, Service: &service{paths: paths}}, nil
}

// ClaudeAuthToken deliberately fails closed. The official Windows messaging docs
// define the first-line token handshake but not a session-record token field.
// Never guess a field, borrow an environment token, or use another session's key.
func (*paths) ClaudeAuthToken(_ context.Context, _ platform.File) (string, error) {
	return "", errors.New("UNVERIFIED Claude session-record token source")
}
