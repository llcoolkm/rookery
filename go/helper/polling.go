package helper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

type pollingRecord struct {
	Provider string              `json:"provider"`
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Pin      platform.ProcessRef `json:"process"`
}

func pollingIdentity(provider, id string) error {
	if provider != "codex" && provider != "claude" && provider != "agy" {
		return errors.New("polling provider must be a registered provider: agy, claude, codex")
	}
	if _, err := core.RequestCreated(id); err != nil {
		return errors.New("polling session ID must be a canonical UUID")
	}
	return nil
}
func pollingName(provider, id string) string { return "polling-" + provider + "-" + id + ".json" }
func validName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 256
}
func RegisterPolling(ctx context.Context, h platform.Host, root, provider, id string, pid int, name string) (any, error) {
	if err := pollingIdentity(provider, id); err != nil {
		return nil, err
	}
	if pid <= 0 {
		return nil, errors.New("polling PID must be a positive integer")
	}
	if name == "" {
		name = provider + " polling " + id
	}
	if !validName(name) {
		return nil, errors.New("polling name must be nonempty text of at most 256 characters")
	}
	pin, err := h.Process.Lookup(ctx, pid)
	if err != nil {
		return nil, errors.New("polling process is unavailable")
	}
	if root, err = h.Files.PrivateState(ctx, root); err != nil {
		return nil, errors.New("polling directory must be private and owned by this user")
	}
	record := pollingRecord{Provider: provider, ID: id, Name: name, Pin: pin}
	b, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New("polling registration failed")
	}
	if err = h.Files.AtomicWrite(ctx, root, pollingName(provider, id), b); err != nil {
		return nil, errors.New("polling registration failed")
	}
	return map[string]any{"registered": provider + ":" + id, "pid": pid, "delivery": "polling"}, nil
}
func UnregisterPolling(ctx context.Context, h platform.Host, root, provider, id string) (any, error) {
	if err := pollingIdentity(provider, id); err != nil {
		return nil, err
	}
	if err := h.Files.Remove(ctx, root, pollingName(provider, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("polling removal failed")
	}
	return map[string]any{"unregistered": provider + ":" + id}, nil
}
func DiscoverPolling(ctx context.Context, h platform.Host, root string) ([]core.Session, error) {
	return discoverPolling(ctx, h, root, provider.Config{})
}
func discoverPolling(ctx context.Context, h platform.Host, root string, config provider.Config) ([]core.Session, error) {
	entries, err := h.Files.List(ctx, root)
	if err != nil {
		return nil, err
	}
	result := []core.Session{}
	for _, entry := range entries {
		if entry.Directory || !strings.HasPrefix(entry.Name, "polling-") || !strings.HasSuffix(entry.Name, ".json") || entry.Size > 4096 {
			continue
		}
		file, err := h.Files.OpenNoFollow(ctx, root, entry.Name, false)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(file, 4097))
		file.Close()
		if err != nil || len(b) > 4096 {
			continue
		}
		var record pollingRecord
		if json.Unmarshal(b, &record) != nil || pollingIdentity(record.Provider, record.ID) != nil || entry.Name != pollingName(record.Provider, record.ID) || !validName(record.Name) || !config.Allows(record.Provider, record.ID) || h.Process.Pin(ctx, record.Pin) != nil {
			continue
		}
		result = append(result, core.Session{ProcessRef: &record.Pin, Provider: record.Provider, ID: record.ID, PID: record.Pin.PID, Name: record.Name, Status: "waiting", Delivery: "polling", Admission: "none"})
	}
	return result, nil
}
