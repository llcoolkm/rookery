//go:build linux || windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
	"github.com/llcoolkm/rookery/go/provider/agy"
)

// AGY credentials are intentionally absent from this boundary. Until a safe
// documented native input route exists, even attached endpoints cannot submit.
type agyEnvironment struct {
	getenv   func(string) string
	self     func() (string, error)
	ancestor func(context.Context) (platform.ProcessRef, error)
	classify func(context.Context, string, string) (string, error)
	spawn    func(context.Context, string, string, string) (any, error)
	ready    func(any) error
}

func agyID(id string) bool {
	return id == strings.ToLower(id) && core.CanonicalID(id) == nil
}
func agyMarker(ctx context.Context, h platform.Host, root, id, suffix string) bool {
	f, err := h.Files.OpenNoFollow(ctx, root, agy.MarkerName(id, suffix), false)
	if err == nil {
		f.Close()
		return true
	}
	return !errors.Is(err, fs.ErrNotExist) // unreadable marker fails closed
}
func agyProcessTag(ref platform.ProcessRef) string {
	b, _ := json.Marshal(ref)
	return fmt.Sprintf("%x", sha256.Sum256(b))[:24]
}
func agyMarkOnce(ctx context.Context, h platform.Host, root, name string) bool {
	exclusive, ok := h.Files.(interface {
		AtomicWriteExclusive(context.Context, string, string, []byte) error
	})
	return ok && exclusive.AtomicWriteExclusive(ctx, root, name, []byte("{}")) == nil
}
func agyConfirm(ctx context.Context, env agyEnvironment, id, artifact string) bool {
	for i := 0; i < 2; i++ {
		kind, err := env.classify(ctx, id, artifact)
		if err != nil || kind != "top-level" {
			return false
		}
		if i == 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return true
}
func agyRecords(ctx context.Context, h platform.Host, root string) []agy.Record {
	entries, err := h.Files.List(ctx, root)
	if err != nil {
		return nil
	}
	var out []agy.Record
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name, "agy-") || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		var r agy.Record
		if readJSON(ctx, h, root, entry.Name, &r) == nil && r.V == 1 && r.Provider == "agy" && agyID(r.ID) && agy.RecordName(r.ID) == entry.Name && r.DeliveryHeld {
			out = append(out, r)
		}
	}
	return out
}
func agySupersede(ctx context.Context, h platform.Host, root, id string, owner platform.ProcessRef) error {
	for _, r := range agyRecords(ctx, h, root) {
		if r.ID == id || r.PID != owner.PID || r.Starttime != owner.Start || r.BootID != owner.BootID {
			continue
		}
		if err := h.Files.AtomicWrite(ctx, root, agy.MarkerName(r.ID, "superseded"), []byte("{}")); err != nil {
			return errors.New("AGY supersession failed")
		}
		// A return to this conversation may offer attachment again in this process.
		_ = h.Files.Remove(ctx, root, agy.MarkerName(r.ID, "asked-"+agyProcessTag(owner)))
	}
	return nil
}
func agyStatus(ctx context.Context, h platform.Host, root string) any {
	p, _ := agy.New(provider.Config{}, h)
	live, _ := p.Discover(ctx)
	found := map[string]bool{}
	for _, s := range live {
		found[s.ID] = true
	}
	out := []any{}
	for _, r := range agyRecords(ctx, h, root) {
		state := "stale"
		if agyMarker(ctx, h, root, r.ID, "detached") {
			state = "detached"
		} else if agyMarker(ctx, h, root, r.ID, "superseded") {
			state = "superseded"
		} else if found[r.ID] {
			state = "attached"
		}
		out = append(out, map[string]any{"id": r.ID, "state": state, "delivery": "held", "detail": agy.HeldDetail})
	}
	return out
}
func agyCommandWith(ctx context.Context, h platform.Host, root string, a arguments, env agyEnvironment) (any, error) {
	if a.pos(0) != "agy" {
		return nil, errors.New("attach/detach supports agy")
	}
	if a.command == "attach" && a.options["status"] == "true" {
		return agyStatus(ctx, h, root), nil
	}
	id := a.options["conversation"]
	if a.command == "detach" {
		if !agyID(id) {
			return nil, errors.New("detach requires a canonical --conversation UUID")
		}
		if err := h.Files.AtomicWrite(ctx, root, agy.MarkerName(id, "detached"), []byte("{}")); err != nil {
			return nil, errors.New("AGY detach failed")
		}
		return map[string]any{"detached": "agy:" + id, "delivery": "held"}, nil
	}
	ownID := env.getenv("ANTIGRAVITY_CONVERSATION_ID")
	if id == "" {
		id = ownID
	}
	if !agyID(id) || ownID == "" || ownID != id {
		return nil, errors.New("attach requires this AGY shell's canonical conversation UUID")
	}
	name := a.options["name"]
	if name == "" {
		name = "AGY " + id[:8]
	}
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 256 {
		return nil, errors.New("endpoint name must contain 1 to 256 characters")
	}
	owner, err := env.ancestor(ctx)
	if err != nil {
		return nil, errors.New("run attach agy from the AGY conversation's own shell")
	}
	if !agyConfirm(ctx, env, id, "") {
		return nil, errors.New("cannot confirm a top-level AGY conversation; nothing attached or superseded")
	}
	p, _ := agy.New(provider.Config{}, h)
	live, _ := p.Discover(ctx)
	for _, s := range live {
		if s.ID == id && s.ProcessRef != nil && *s.ProcessRef == owner {
			return map[string]any{"attached": "agy:" + id, "already": true, "delivery": "held", "detail": agy.HeldDetail}, nil
		}
	}
	if a.options["foreground"] != "true" {
		return env.spawn(ctx, id, name, root)
	}
	return serveAGY(ctx, h, root, id, name, owner, env.ready)
}
func serveAGY(ctx context.Context, h platform.Host, root, id, name string, owner platform.ProcessRef, ready func(any) error) (any, error) {
	lock, err := h.Files.AdvisoryLock(ctx, root, agy.MarkerName(id, "lock"), false)
	if err != nil {
		return nil, errors.New("AGY endpoint lock busy; inspect attach agy --status")
	}
	defer lock.Close()
	if err = h.Process.Pin(ctx, owner); err != nil {
		return nil, errors.New("AGY owner changed")
	}
	mine, err := h.Process.Current(ctx)
	if err != nil {
		return nil, errors.New("AGY endpoint identity unavailable")
	}
	// No prior endpoint can own this socket while its lifetime lock is held.
	ep, err := h.Paths.Endpoint(agy.SocketName(id))
	if err != nil {
		return nil, errors.New("AGY endpoint path unsupported")
	}
	cleaner, ok := h.Paths.(interface {
		RemoveEndpoint(context.Context, platform.Endpoint) error
	})
	if !ok || cleaner.RemoveEndpoint(ctx, ep) != nil {
		return nil, errors.New("AGY stale socket cleanup failed")
	}
	listener, err := h.IPC.Listen(ctx, ep)
	if err != nil {
		return nil, errors.New("AGY endpoint unavailable")
	}
	defer listener.Close()
	// Serialise lifecycle transitions across conversations of the same AGY process.
	transition, err := h.Files.AdvisoryLock(ctx, root, "agy-lifecycle.lock", true)
	if err != nil {
		return nil, errors.New("AGY lifecycle unavailable")
	}
	err = agySupersede(ctx, h, root, id, owner)
	if err == nil {
		for _, suffix := range []string{"detached", "superseded"} {
			e := h.Files.Remove(ctx, root, agy.MarkerName(id, suffix))
			if e != nil && !errors.Is(e, fs.ErrNotExist) {
				err = e
				break
			}
		}
	}
	if err == nil {
		err = h.Files.AtomicWrite(ctx, root, agy.MarkerName(id, "attached"), []byte("{}"))
	}
	transition.Close()
	if err != nil {
		return nil, errors.New("AGY lifecycle update failed")
	}
	defer h.Files.Remove(context.Background(), root, agy.RecordName(id))
	result := map[string]any{"attached": "agy:" + id, "delivery": "held", "detail": agy.HeldDetail}
	record := agy.Record{V: 1, Provider: "agy", ID: id, Name: name, PID: owner.PID, Starttime: owner.Start, BootID: owner.BootID, EndpointPID: mine.PID, EndpointStarttime: mine.Start, DeliveryHeld: true}
	nextBeat := time.Time{}
	announced := false
	for ctx.Err() == nil {
		if h.Process.Pin(ctx, owner) != nil || agyMarker(ctx, h, root, id, "detached") || agyMarker(ctx, h, root, id, "superseded") {
			break
		}
		if time.Now().After(nextBeat) {
			record.Heartbeat = float64(time.Now().UnixNano()) / 1e9
			if writeJSON(ctx, h, root, agy.RecordName(id), record) != nil {
				return nil, errors.New("AGY heartbeat failed")
			}
			nextBeat = time.Now().Add(5 * time.Second)
			if !announced {
				if ready != nil {
					if err = ready(result); err != nil {
						return nil, errors.New("AGY readiness failed")
					}
				}
				announced = true
			}
		}
		acceptCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		c, e := listener.Accept(acceptCtx)
		cancel()
		if e != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		handleAGY(ctx, c, id)
	}
	return result, nil
}
func handleAGY(ctx context.Context, c platform.Connection, id string) {
	defer c.Close()
	if _, err := c.Peer(); err != nil {
		return
	}
	if c.SetDeadline(time.Now().Add(time.Second)) != nil {
		return
	}
	var request struct {
		Op           string `json:"op"`
		Conversation string `json:"conversation"`
		RequestID    string `json:"request_id"`
	}
	line, err := bufio.NewReader(io.LimitReader(c, 256*1024+1)).ReadBytes('\n')
	if err != nil || len(line) > 256*1024 || json.Unmarshal(line, &request) != nil {
		return
	}
	response := map[string]any{"status": "held", "detail": agy.HeldDetail, "request_id": request.RequestID}
	if request.Op == "ping" {
		response = map[string]any{"conversation": id, "status": "attached", "delivery": "held"}
	}
	// submit is deliberately never passed to any sender, file or subprocess.
	_ = json.NewEncoder(c).Encode(response)
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func agyHookWith(ctx context.Context, h platform.Host, root, event string, in io.Reader, env agyEnvironment) (any, error) {
	empty := map[string]any{}
	if event != "session-start" && event != "pre-invocation" {
		return nil, errors.New("unknown AGY hook event")
	}
	b, err := io.ReadAll(io.LimitReader(in, 65537))
	if err != nil || len(b) > 65536 {
		return empty, nil
	}
	var payload struct {
		Conversation string `json:"conversationId"`
		Artifact     string `json:"artifactDirectoryPath"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return empty, nil
	}
	id := payload.Conversation
	if id == "" {
		id = env.getenv("ANTIGRAVITY_CONVERSATION_ID")
	}
	if !agyID(id) {
		return empty, nil
	}
	if !agyConfirm(ctx, env, id, payload.Artifact) {
		return empty, nil
	}
	owner, err := env.ancestor(ctx)
	if err != nil {
		return empty, nil
	}
	lock, err := h.Files.AdvisoryLock(ctx, root, "agy-lifecycle.lock", true)
	if err != nil {
		return empty, nil
	}
	defer lock.Close()
	if agySupersede(ctx, h, root, id, owner) != nil || agyMarker(ctx, h, root, id, "detached") {
		return empty, nil
	}
	p, _ := agy.New(provider.Config{}, h)
	live, _ := p.Discover(ctx)
	for _, s := range live {
		if s.ID == id {
			return empty, nil
		}
	}
	once := "asked"
	if agyMarker(ctx, h, root, id, "attached") {
		once = "asked-" + agyProcessTag(owner)
	}
	for _, r := range agyRecords(ctx, h, root) {
		if r.ID == id {
			once = "lost-" + agyProcessTag(platform.ProcessRef{PID: r.EndpointPID, Start: r.EndpointStarttime, BootID: r.BootID})
			break
		}
	}
	self, err := env.self()
	if err != nil {
		return empty, nil
	}
	if !agyMarkOnce(ctx, h, root, agy.MarkerName(id, once)) {
		return empty, nil
	}
	command := shellQuote(self) + " --state-dir " + shellQuote(root) + " attach agy"
	text := "Rookery Go native AGY delivery is held pending a documented stdin or file input route. To attach this conversation's local endpoint, run this exact shell command once: " + command + ". Attaching does not enable native delivery; use the rookery-go inbox tool to read peer messages."
	if event == "session-start" {
		return map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]any{"systemMessage": text}}}}, nil
	}
	return map[string]any{"injectSteps": []any{map[string]any{"ephemeralMessage": text}}}, nil
}
