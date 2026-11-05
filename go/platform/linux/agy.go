//go:build linux

package linux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/llcoolkm/rookery/go/platform"
)

// AGYAncestor identifies the AGY shell owner from process stat metadata alone.
// It deliberately does not call CommandLine: AGY may carry credentials there.
func (p *processes) AGYAncestor(ctx context.Context) (platform.ProcessRef, error) {
	return agyAncestor(ctx, os.Getppid(), p.Lookup, p.Pin, p.agyStat)
}

func (p *processes) agyStat(ctx context.Context, ref platform.ProcessRef) (string, int, error) {
	if err := p.Pin(ctx, ref); err != nil {
		return "", 0, err
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(ref.PID) + "/stat")
	if err != nil {
		return "", 0, errors.New("AGY process unavailable")
	}
	start, end := strings.IndexByte(string(data), '('), strings.LastIndexByte(string(data), ')')
	if start < 0 || end < start {
		return "", 0, errUnsafe
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || fields[19] != ref.Start {
		return "", 0, errUnsafe
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return "", 0, errUnsafe
	}
	if err := p.Pin(ctx, ref); err != nil {
		return "", 0, err
	}
	return string(data[start+1 : end]), parent, nil
}

func agyAncestor(ctx context.Context, start int,
	lookup func(context.Context, int) (platform.ProcessRef, error),
	pin func(context.Context, platform.ProcessRef) error,
	stat func(context.Context, platform.ProcessRef) (string, int, error),
) (platform.ProcessRef, error) {
	seen := map[int]bool{}
	principal := ""
	var child *platform.ProcessRef
	for depth, pid := 0, start; depth < 32 && pid > 1; depth++ {
		if ctx.Err() != nil {
			return platform.ProcessRef{}, ctx.Err()
		}
		if seen[pid] {
			break
		}
		seen[pid] = true
		ref, err := lookup(ctx, pid)
		if err != nil || ref.PID != pid || ref.Start == "" || ref.BootID == "" || ref.Principal == "" {
			break
		}
		if principal == "" {
			principal = ref.Principal
		} else if ref.Principal != principal {
			break
		}
		if child != nil {
			// A parent PID observed in stat is only a candidate. Re-read the
			// child's relationship after looking it up, so reparenting or a
			// reused parent cannot silently change which AGY owner we select.
			childStart, childErr := strconv.ParseUint(child.Start, 10, 64)
			parentStart, parentErr := strconv.ParseUint(ref.Start, 10, 64)
			if childErr != nil || parentErr != nil || child.BootID != ref.BootID || parentStart > childStart || pin(ctx, *child) != nil {
				break
			}
			_, parentAgain, relationErr := stat(ctx, *child)
			if relationErr != nil || parentAgain != pid || pin(ctx, *child) != nil {
				break
			}
		}
		name, parent, err := stat(ctx, ref)
		if err != nil || pin(ctx, ref) != nil {
			break
		}
		if name == "agy" {
			return ref, nil
		}
		captured := ref
		child = &captured
		pid = parent
	}
	return platform.ProcessRef{}, errors.New("no owning AGY process; run attach from the AGY conversation shell")
}

// AGYConversation classifies only the observed AGY conversation-store layout.
// Unknown and child conversations must not attach or supersede another endpoint.
// This reads no credentials and does not execute agentapi.
func (p *paths) AGYConversation(ctx context.Context, id, artifact string) (string, error) {
	if !agyConversationID(id) {
		return "unknown", nil
	}
	var brain string
	if filepath.IsAbs(artifact) && filepath.Base(artifact) == id {
		brain = filepath.Dir(artifact)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "unknown", nil
		}
		brain = filepath.Join(home, ".gemini", "antigravity-cli", "brain")
	}
	return classifyAGYConversation(ctx, brain, id)
}

func agyConversationID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func agySubdirectory(parent int, name string) (int, error) {
	fd, err := syscall.Openat(parent, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || !safeStat(&st, false, true) {
		syscall.Close(fd)
		return -1, errUnsafe
	}
	return fd, nil
}

func classifyAGYConversation(ctx context.Context, brain, id string) (string, error) {
	if ctx.Err() != nil {
		return "unknown", ctx.Err()
	}
	if !agyConversationID(id) {
		return "unknown", nil
	}
	fd, err := dirOpen(filepath.Clean(brain), false)
	if err != nil {
		return "unknown", nil
	}
	dir := os.NewFile(uintptr(fd), "AGY conversation store")
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return "unknown", nil
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return "unknown", ctx.Err()
		}
		if !entry.IsDir() {
			continue
		}
		parent, err := agySubdirectory(fd, entry.Name())
		if err != nil {
			return "unknown", nil
		}
		generated, err := agySubdirectory(parent, ".system_generated")
		syscall.Close(parent)
		if err == syscall.ENOENT {
			continue
		}
		if err != nil {
			return "unknown", nil
		}
		children, err := agySubdirectory(generated, "subagents")
		syscall.Close(generated)
		if err == syscall.ENOENT {
			continue
		}
		if err != nil {
			return "unknown", nil
		}
		child, err := syscall.Openat(children, id+".json", 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(children)
		if err == syscall.ENOENT {
			continue
		}
		if err != nil {
			return "unknown", nil
		}
		var st syscall.Stat_t
		err = syscall.Fstat(child, &st)
		syscall.Close(child)
		if err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || !safeStat(&st, false, false) {
			return "unknown", nil
		}
		return "subagent", nil
	}
	own, err := agySubdirectory(fd, id)
	if err != nil {
		return "unknown", nil
	}
	syscall.Close(own)
	return "top-level", nil
}

// RemoveEndpoint removes a state-local private socket. The caller must hold the
// endpoint's lifetime lock, so no live generation can own or replace the path.
// Records and provider-owned sockets cannot be removed through this capability.
func (p *paths) RemoveEndpoint(ctx context.Context, handle platform.Endpoint) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ep, err := p.get(handle)
	if err != nil || !ep.helper || filepath.Dir(ep.path) != p.state {
		return errUnsafe
	}
	fd, base, err := parentOpen(p.state, filepath.Base(ep.path))
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var directory syscall.Stat_t
	if syscall.Fstat(fd, &directory) != nil || !safeStat(&directory, true, true) {
		return errUnsafe
	}
	child, err := syscall.Openat(fd, base, 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT {
		return nil
	}
	if err != nil {
		return errUnsafe
	}
	var st syscall.Stat_t
	err = syscall.Fstat(child, &st)
	syscall.Close(child)
	if err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || !safeStat(&st, true, false) || st.Nlink != 1 {
		return errUnsafe
	}
	if err = syscall.Unlinkat(fd, base); err != nil {
		return errUnsafe
	}
	return syscall.Fsync(fd)
}
