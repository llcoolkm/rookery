//go:build linux

package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
)

const messageID = "00000000-0000-4000-8000-000000000010"
const claimID = "00000000-0000-4000-8000-000000000011"
const sessionID = "00000000-0000-4000-8000-000000000012"

type helperHub struct {
	call func(context.Context, core.RPCRequest) (json.RawMessage, error)
}

func (h helperHub) Call(ctx context.Context, r core.RPCRequest) (json.RawMessage, error) {
	return h.call(ctx, r)
}
func helperMessage() core.Message {
	return core.Message{ID: messageID, Recipient: "alpha/claude:" + sessionID, Status: core.Pending, Expires: float64(time.Now().Add(time.Hour).Unix()), Updated: 1}
}
func helperSession() core.Session {
	return core.Session{ID: sessionID, Provider: "claude", PID: 42, Status: "idle", ProcessRef: &platform.ProcessRef{PID: 42, Principal: "example", Start: "1", BootID: "example"}}
}
func applyMessage(t *testing.T, s *core.LocalStore, m core.Message) {
	t.Helper()
	b, _ := json.Marshal(m)
	if e := s.Apply(context.Background(), []core.Event{{Type: "message", Data: b}}, 1); e != nil {
		t.Fatal(e)
	}
}
func deliveryFixture(t *testing.T) (*core.Helper, *int) {
	t.Helper()
	s := newStore(t)
	m := helperMessage()
	session := helperSession()
	applyMessage(t, s, m)
	claims := new(int)
	h := &core.Helper{Store: s, Node: "alpha", Sessions: []core.Session{session}}
	h.Native = core.NativeHooks{Discover: func(context.Context) ([]core.Session, map[string]string) { return []core.Session{session}, nil }, Ready: func(core.Session) bool { return true }, Submit: func(ctx context.Context, _ core.Session, _ core.Message) (core.DeliveryResult, error) {
		if e := s.StartAttempt(ctx, messageID, claimID); e == nil {
			t.Error("native I/O began without durable journal")
		}
		return core.DeliveryResult{Status: "submitted", Detail: "untrusted diagnostic"}, nil
	}}
	h.Hub = helperHub{call: func(_ context.Context, r core.RPCRequest) (json.RawMessage, error) {
		switch r.Op {
		case "claim":
			*claims++
			return json.Marshal(map[string]any{"delivery": map[string]any{"message": m, "session": session, "claim_id": claimID}})
		case "received", "result":
			return json.RawMessage(`{}`), nil
		default:
			t.Fatalf("unexpected op %s", r.Op)
			return nil, nil
		}
	}}
	return h, claims
}
func TestHelperJournalBeforeSubmissionAndNoReplay(t *testing.T) {
	h, claims := deliveryFixture(t)
	ctx := context.Background()
	if e := h.Deliver(ctx); e != nil {
		t.Fatal(e)
	}
	if *claims != 1 {
		t.Fatal("claim not performed")
	}
	if e := h.Deliver(ctx); e != nil {
		t.Fatal(e)
	}
	if *claims != 1 {
		t.Fatal("claimed an already submitted attempt")
	}
	reports, e := h.Store.Reports(ctx)
	if e != nil || len(reports) != 0 {
		t.Fatal("result was not durably reported")
	}
}
func TestHelperPendingReadSuppressesClaimAndLateReadHolds(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_claim", true: "during_rediscovery"}[late], func(t *testing.T) {
			h, claims := deliveryFixture(t)
			ctx := context.Background()
			admit := func() {
				c := core.Client{Store: h.Store}
				if _, e := c.Request(ctx, "read", map[string]any{"actor": helperMessage().Recipient, "message_ids": []string{messageID}}, 0); e != nil {
					t.Fatal(e)
				}
			}
			submitted := false
			h.Native.Submit = func(context.Context, core.Session, core.Message) (core.DeliveryResult, error) {
				submitted = true
				return core.DeliveryResult{Status: "submitted"}, nil
			}
			if late {
				h.Native.Discover = func(context.Context) ([]core.Session, map[string]string) {
					admit()
					return []core.Session{helperSession()}, nil
				}
			} else {
				admit()
			}
			if e := h.Deliver(ctx); e != nil {
				t.Fatal(e)
			}
			if submitted {
				t.Fatal("delivered while durable read pending")
			}
			if !late && *claims != 0 {
				t.Fatal("claimed read mail")
			}
			if late {
				eligible, e := h.Store.DeliveryEligible(ctx, messageID)
				if e != nil || !eligible {
					t.Fatal("pre-I/O hold did not release local attempt")
				}
			}
		})
	}
}
func TestHelperCrashRecoveryDoesNotReplay(t *testing.T) {
	h, claims := deliveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	submissions := 0
	h.Native.Submit = func(context.Context, core.Session, core.Message) (core.DeliveryResult, error) {
		submissions++
		cancel()
		return core.DeliveryResult{}, errors.New("possible write")
	}
	if h.Deliver(ctx) == nil {
		t.Fatal("cancelled journal completion unexpectedly succeeded")
	}
	fresh := context.Background()
	if e := h.Store.RecoverAttempts(fresh); e != nil {
		t.Fatal(e)
	}
	reports, e := h.Store.Reports(fresh)
	if e != nil || len(reports) != 1 || reports[0].Result.Status != "uncertain" {
		t.Fatal("unfinished attempt was not recovered uncertain")
	}
	if e = h.Report(fresh); e != nil {
		t.Fatal(e)
	}
	if e = h.Deliver(fresh); e != nil {
		t.Fatal(e)
	}
	if *claims != 1 || submissions != 1 {
		t.Fatal("replayed crashed native attempt")
	}
}
func TestHelperRefusesChangedProcessOrClaimIdentity(t *testing.T) {
	for _, change := range []string{"process", "claim"} {
		t.Run(change, func(t *testing.T) {
			h, _ := deliveryFixture(t)
			h.Native.Submit = func(context.Context, core.Session, core.Message) (core.DeliveryResult, error) {
				t.Fatal("submitted to changed process or mismatched claim")
				return core.DeliveryResult{}, nil
			}
			if change == "process" {
				fresh := helperSession()
				fresh.ProcessRef = &platform.ProcessRef{PID: 42, Principal: "example", Start: "2", BootID: "example"}
				h.Native.Discover = func(context.Context) ([]core.Session, map[string]string) { return []core.Session{fresh}, nil }
			} else {
				m := helperMessage()
				m.Recipient = "alpha/claude:00000000-0000-4000-8000-000000000099"
				h.Hub = helperHub{call: func(_ context.Context, r core.RPCRequest) (json.RawMessage, error) {
					if r.Op == "claim" {
						return json.Marshal(map[string]any{"delivery": map[string]any{"message": m, "session": helperSession(), "claim_id": claimID}})
					}
					return json.RawMessage(`{}`), nil
				}}
			}
			if e := h.Deliver(context.Background()); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestHelperResultRejectionKeepsEvidence(t *testing.T) {
	h, _ := deliveryFixture(t)
	ctx := context.Background()
	if e := h.Store.StartAttempt(ctx, messageID, claimID); e != nil {
		t.Fatal(e)
	}
	if e := h.Store.FinishAttempt(ctx, claimID, core.DeliveryResult{Status: "uncertain", Detail: "sensitive diagnostic"}); e != nil {
		t.Fatal(e)
	}
	h.Hub = helperHub{call: func(context.Context, core.RPCRequest) (json.RawMessage, error) {
		return nil, &core.RemoteError{Text: "native claim does not match"}
	}}
	if e := h.Report(ctx); e != nil {
		t.Fatal(e)
	}
	eligible, e := h.Store.DeliveryEligible(ctx, messageID)
	if e != nil || eligible {
		t.Fatal("rejected report enabled replay")
	}
	file, e := h.Store.Files.OpenNoFollow(ctx, h.Store.Root, "store.json", false)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	data, e := io.ReadAll(file)
	if e != nil {
		t.Fatal(e)
	}
	var state struct {
		Attempts map[string]struct {
			ReportError string
			Result      core.DeliveryResult
		}
	}
	if json.Unmarshal(data, &state) != nil || state.Attempts[claimID].ReportError != "native result rejected" || state.Attempts[claimID].Result.Detail == "sensitive diagnostic" {
		t.Fatal("rejection evidence absent or raw diagnostic persisted")
	}
}
func TestBootstrapPinsCursorAndCommitsOnlyAfterFinalPage(t *testing.T) {
	for _, failure := range []string{"interrupt", "changed_cursor", "repeated_page", "none"} {
		t.Run(failure, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			if e := s.Apply(ctx, nil, 3); e != nil {
				t.Fatal(e)
			}
			calls := 0
			h := core.Helper{Store: s}
			h.Hub = helperHub{call: func(_ context.Context, r core.RPCRequest) (json.RawMessage, error) {
				calls++
				if calls == 1 {
					return json.Marshal(map[string]any{"messages": []core.Message{helperMessage()}, "cursor": 10, "next_after": 1})
				}
				var data map[string]any
				json.Unmarshal(r.Data, &data)
				if data["cursor"] != float64(10) || data["after"] != float64(1) {
					t.Fatal("bootstrap high water was not pinned")
				}
				if cursor, e := s.Cursor(ctx); e != nil || cursor != 3 {
					t.Fatal("partial bootstrap advanced durable cursor")
				}
				switch failure {
				case "interrupt":
					return nil, errors.New("interrupted")
				case "changed_cursor":
					return json.RawMessage(`{"messages":[],"cursor":11,"next_after":null}`), nil
				case "repeated_page":
					return json.RawMessage(`{"messages":[],"cursor":10,"next_after":1}`), nil
				}
				return json.RawMessage(`{"messages":[],"cursor":10,"next_after":null}`), nil
			}}
			e := h.Bootstrap(ctx)
			if (e == nil) != (failure == "none") {
				t.Fatal("wrong bootstrap outcome", e)
			}
			want := int64(3)
			if failure == "none" {
				want = 10
			}
			if cursor, e := s.Cursor(ctx); e != nil || cursor != want {
				t.Fatal("incorrect durable bootstrap cursor")
			}
			if _, found, e := s.Message(ctx, messageID); e != nil || !found {
				t.Fatal("first page was not persisted")
			}
		})
	}
}
