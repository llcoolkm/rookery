package core

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

type NativeHooks struct {
	Discover func(context.Context) ([]Session, map[string]string)
	Ready    func(Session) bool
	Submit   func(context.Context, Session, Message) (DeliveryResult, error)
}
type Helper struct {
	Process          platform.Process
	Store            *LocalStore
	Hub              HubClient
	Node             string
	Native           NativeHooks
	Sessions         []Session
	ProviderErrors   map[string]string
	syncMu           sync.Mutex
	lastSync         float64
	syncErr          error
	syncRunning      bool
	directoryVersion uint64
	directoryLoaded  uint64
	directoryReady   bool
	nextDiscovery    time.Time
	BusyDelivery     []string
}

func (h *Helper) Sync(ctx context.Context, wait float64) error {
	cursor, e := h.Store.Cursor(ctx)
	if e != nil {
		return e
	}
	var r struct {
		Events []Event
		Cursor int64
		Reset  bool `json:"reset_required"`
	}
	if e = RPC(ctx, h.Hub, "sync", map[string]any{"cursor": cursor, "limit": 128, "wait": wait}, &r); e != nil {
		return e
	}
	if r.Reset {
		if e = h.Bootstrap(ctx); e != nil {
			return e
		}
	} else if e = h.Store.Apply(ctx, r.Events, r.Cursor); e != nil {
		return e
	}
	h.syncMu.Lock()
	h.lastSync = seconds()
	h.syncErr = nil
	for _, event := range r.Events {
		if event.Type == "directory" {
			h.directoryVersion++
		}
	}
	if r.Reset {
		h.directoryVersion++
	}
	h.syncMu.Unlock()
	return nil
}
func (h *Helper) Bootstrap(ctx context.Context) error {
	data := map[string]any{"limit": 128}
	old, e := h.Store.Cursor(ctx)
	if e != nil {
		return e
	}
	var highWater *int64
	var previousAfter int64
	for {
		var r struct {
			Messages []Message
			Cursor   int64
			Next     *int64 `json:"next_after"`
		}
		if e = RPC(ctx, h.Hub, "bootstrap", data, &r); e != nil {
			return e
		}
		if highWater == nil {
			value := r.Cursor
			highWater = &value
		} else if r.Cursor != *highWater {
			return errors.New("invalid hub bootstrap cursor")
		}
		if r.Next != nil && *r.Next <= previousAfter {
			return errors.New("invalid hub bootstrap pagination")
		}
		events := []Event{}
		for _, m := range r.Messages {
			b, _ := json.Marshal(m)
			events = append(events, Event{Type: "message", Data: b})
		}
		cursor := old
		if r.Next == nil {
			cursor = *highWater
		}
		if e = h.Store.Apply(ctx, events, cursor); e != nil {
			return e
		}
		if r.Next == nil {
			return nil
		}
		data["after"] = *r.Next
		data["cursor"] = *highWater
		previousAfter = *r.Next
	}
}
func (h *Helper) dirtyDirectory() {
	h.syncMu.Lock()
	h.directoryVersion++
	h.syncMu.Unlock()
}

func (h *Helper) Requests(ctx context.Context) error {
	commands, e := h.Store.Commands(ctx, 64)
	if e != nil {
		return e
	}
	for _, c := range commands {
		r, e := h.Hub.Call(ctx, RPCRequest{Op: c.Op, Data: c.Data})
		if e != nil {
			if !Definite(e) {
				return e
			}
			if e = h.Store.Complete(ctx, c.ID, RPCResponse{OK: false, Error: e.Error()}); e != nil {
				return e
			}
		} else if e = h.Store.Complete(ctx, c.ID, RPCResponse{OK: true, Result: r}); e != nil {
			return e
		} else if c.Op == "enroll" || c.Op == "disable" || c.Op == "clear-alias" {
			h.dirtyDirectory()
		}
	}
	return nil
}
func (h *Helper) Report(ctx context.Context) error {
	attempts, e := h.Store.Reports(ctx)
	if e != nil {
		return e
	}
	for _, a := range attempts {
		r := SafeResult(a.Result)
		if e = RPC(ctx, h.Hub, "result", map[string]any{"message_id": a.MessageID, "claim_id": a.ClaimID, "status": r.Status, "detail": r.Detail, "_request_id": a.RequestID}, nil); e != nil {
			if !Definite(e) {
				return e
			}
			if e = h.Store.ReportRejected(ctx, a.ClaimID); e != nil {
				return e
			}
		} else if e = h.Store.MarkReported(ctx, a.ClaimID); e != nil {
			return e
		}
	}
	return nil
}
func (h *Helper) Publish(ctx context.Context) error {
	sessions := []Session{}
	errs := map[string]string{}
	if h.Native.Discover != nil {
		sessions, errs = h.Native.Discover(ctx)
	}
	h.Sessions = sessions
	h.ProviderErrors = errs
	if len(sessions) > 128 {
		return errors.New("provider discovery exceeds publish limit")
	}
	rows := []map[string]any{}
	for _, s := range sessions {
		row := map[string]any{"provider": s.Provider, "id": s.ID, "name": s.Name, "status": s.Status, "admission": s.Admission}
		if s.PID > 0 {
			row["pid"] = s.PID
		}
		if s.Delivery != "" {
			row["delivery"] = s.Delivery
		}
		rows = append(rows, row)
	}
	if e := RPC(ctx, h.Hub, "publish", map[string]any{"sessions": rows, "errors": errs}, nil); e != nil {
		return e
	}
	h.nextDiscovery = time.Now().Add(15 * time.Second)
	h.dirtyDirectory()
	return nil
}
func (h *Helper) Deliver(ctx context.Context) error {
	messages, e := h.Store.PendingMessages(ctx, h.Node, 100)
	if e != nil {
		return e
	}
	for _, m := range messages {
		if m.Status != Pending || m.Expires <= seconds() || !strings.HasPrefix(m.Recipient, h.Node+"/") {
			continue
		}
		eligible, e := h.Store.DeliveryEligible(ctx, m.ID)
		if e != nil {
			return e
		}
		if !eligible {
			continue
		}
		reads, e := h.Store.PendingReads(ctx)
		if e != nil {
			return e
		}
		skip := false
		for _, id := range reads[m.Recipient] {
			if id == m.ID {
				skip = true
			}
		}
		if skip {
			continue
		}
		if m.ReceivedByHost == nil || !*m.ReceivedByHost {
			if e = RPC(ctx, h.Hub, "received", map[string]any{"message_id": m.ID}, nil); e != nil {
				return e
			}
		}
		var target *Session
		for i := range h.Sessions {
			s := &h.Sessions[i]
			if h.Node+"/"+s.Provider+":"+s.ID == m.Recipient {
				target = s
				break
			}
		}
		if target == nil || h.Native.Ready == nil || !h.Native.Ready(*target) {
			continue
		}
		var claim struct {
			Delivery *struct {
				Message Message
				Session Session
				ClaimID string `json:"claim_id"`
			}
		}
		if e = RPC(ctx, h.Hub, "claim", map[string]any{"message_id": m.ID}, &claim); e != nil {
			return e
		}
		if claim.Delivery == nil {
			continue
		}
		d := claim.Delivery
		if e = h.Store.StartAttempt(ctx, m.ID, d.ClaimID); e != nil {
			return e
		}
		result := DeliveryResult{Status: "held", LocalReason: "not_ready"}
		fresh := []Session{}
		if h.Native.Discover != nil {
			fresh, _ = h.Native.Discover(ctx)
		}
		var live *Session
		for i := range fresh {
			s := &fresh[i]
			if s.ID == target.ID && s.Provider == target.Provider {
				live = s
				break
			}
		}
		latestReads, e := h.Store.PendingReads(ctx)
		if e != nil {
			return e
		}
		pendingRead := false
		for _, id := range latestReads[m.Recipient] {
			if id == m.ID {
				pendingRead = true
			}
		}
		exactClaim := d.Message.ID == m.ID && d.Message.Recipient == m.Recipient && d.Session.ID == target.ID && d.Session.Provider == target.Provider
		pinnedSame := live != nil && target.ProcessRef != nil && live.ProcessRef != nil && *target.ProcessRef == *live.ProcessRef
		switch {
		case pendingRead:
			result.LocalReason = "pending_read"
		case !exactClaim:
			result.LocalReason = "claim_mismatch"
		case !pinnedSame || live.PID != target.PID || live.PID != d.Session.PID:
			result.LocalReason = "pin_mismatch"
		case !h.Native.Ready(*live):
			result.LocalReason = "not_idle"
		case d.Message.Expires <= seconds():
			result.LocalReason = "expired"
		}
		if !pendingRead && exactClaim && pinnedSame && live.PID == target.PID && live.PID == d.Session.PID && h.Native.Ready(*live) && d.Message.Expires > seconds() && h.Native.Submit != nil {
			result, e = h.Native.Submit(ctx, *live, d.Message)
			if e != nil {
				result = DeliveryResult{Status: "uncertain"}
			}
		}
		if result.Status == "held" {
			logHeld(result.LocalReason)
		}
		if e = h.Store.FinishAttempt(ctx, d.ClaimID, result); e != nil {
			return e
		}
		if live == nil {
			for i := range h.Sessions {
				if &h.Sessions[i] == target {
					h.Sessions = append(h.Sessions[:i], h.Sessions[i+1:]...)
					break
				}
			}
		} else {
			*target = *live
			if result.Status != "held" {
				target.Status = "busy"
			}
		}
		if result.Status == "held" {
			h.nextDiscovery = time.Time{}
		}
		if e = h.Report(ctx); e != nil {
			return e
		}
	}
	return nil
}
func (h *Helper) Cycle(ctx context.Context) error {
	if e := h.Report(ctx); e != nil {
		return e
	}
	if time.Now().After(h.nextDiscovery) {
		if e := h.Publish(ctx); e != nil {
			return e
		}
	}
	if e := h.Requests(ctx); e != nil {
		return e
	}
	h.syncMu.Lock()
	background := h.syncRunning
	h.syncMu.Unlock()
	if !background {
		if e := h.Sync(ctx, 0); e != nil {
			return e
		}
	}
	h.syncMu.Lock()
	version := h.directoryVersion
	dirty := !h.directoryReady || version != h.directoryLoaded
	h.syncMu.Unlock()
	if dirty {
		var directory map[string]any
		if e := RPC(ctx, h.Hub, "list", map[string]any{}, &directory); e != nil {
			return e
		}
		directory["updated"] = seconds()
		if e := h.Store.Put(ctx, "directory", directory); e != nil {
			return e
		}
		h.syncMu.Lock()
		h.directoryLoaded, h.directoryReady = version, true
		h.syncMu.Unlock()
	}
	return h.Deliver(ctx)
}
func (h *Helper) Health(ctx context.Context, running bool, cycleErr error) error {
	pid := 0
	if h.Process != nil {
		if ref, e := h.Process.Current(ctx); e == nil {
			pid = ref.PID
		}
	}
	var detail any
	if cycleErr != nil {
		detail = "helper cycle unavailable; requests remain queued"
	}
	h.syncMu.Lock()
	lastSync, syncErr := h.lastSync, h.syncErr
	h.syncMu.Unlock()
	if syncErr != nil {
		detail = "helper sync unavailable; requests remain queued"
	}
	return h.Store.Put(ctx, "health", map[string]any{"pid": pid, "running": running, "connected": running && cycleErr == nil && syncErr == nil && seconds()-lastSync < 40, "updated": seconds(), "node": h.Node, "error": detail, "provider_errors": h.ProviderErrors, "busy_delivery": h.BusyDelivery})
}
func (h *Helper) Run(ctx context.Context, once bool) error {
	lock, e := h.Store.Files.AdvisoryLock(ctx, h.Store.Root, "daemon.lock", false)
	if e != nil {
		return errors.New("a Rookery helper already owns this state directory")
	}
	defer lock.Close()
	if e = h.Store.RecoverAttempts(ctx); e != nil {
		return e
	}
	defer h.Health(context.Background(), false, nil)
	if once {
		e = h.Cycle(ctx)
		_ = h.Health(ctx, false, e)
		return e
	}
	syncCtx, stopSync := context.WithCancel(ctx)
	h.syncMu.Lock()
	h.syncRunning = true
	h.syncMu.Unlock()
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		delay := time.Second
		for syncCtx.Err() == nil {
			if err := h.Sync(syncCtx, 10); err != nil {
				h.syncMu.Lock()
				h.syncErr = err
				h.syncMu.Unlock()
				select {
				case <-syncCtx.Done():
					return
				case <-time.After(delay):
				}
				delay = min(30*time.Second, delay*2)
			} else {
				delay = time.Second
			}
		}
	}()
	defer func() {
		stopSync()
		<-syncDone
		h.syncMu.Lock()
		h.syncRunning = false
		h.syncMu.Unlock()
	}()
	pruneAt := time.Time{}
	healthAt := time.Time{}
	delay := 250 * time.Millisecond
	for {
		e = h.Cycle(ctx)
		if time.Now().After(healthAt) || e != nil {
			_ = h.Health(ctx, true, e)
			healthAt = time.Now().Add(10 * time.Second)
		}
		if time.Now().After(pruneAt) {
			_ = h.Store.Prune(ctx, time.Now())
			pruneAt = time.Now().Add(time.Hour)
		}
		if e != nil {
			delay = min(30*time.Second, max(time.Second, delay*2))
		} else {
			delay = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// logHeld accepts only fixed codes; provider errors and session metadata never
// enter local logs. The wire result is independently sanitized by SafeResult.
func logHeld(reason string) {
	switch reason {
	case "record_unsafe", "record_invalid", "endpoint_unsafe", "pin_mismatch", "not_idle", "polling", "expired", "pending_read", "claim_mismatch", "not_ready":
	default:
		reason = "not_ready"
	}
	log.Printf("native delivery held reason=%s", reason)
}
