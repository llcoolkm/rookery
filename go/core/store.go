package core

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/llcoolkm/rookery/go/platform"
	"io"
	"io/fs"
	"sort"
	"time"
)

// LocalStore uses one fsynced atomic snapshot, serialized by a cross-process
// admission lock. Commands are their own spool: no unsafe spool/import gap.
// Each transaction reloads under flock, so CLI and daemon never lose updates.
type LocalStore struct {
	Files platform.Files
	Root  string
}
type storedAttempt struct {
	Attempt
	Finished, Reported bool
	Created            float64
	ReportError        string
}
type storedResult struct {
	Value   RPCResponse
	Created float64
}
type localState struct {
	Commands map[string]Command         `json:"commands"`
	Results  map[string]storedResult    `json:"results"`
	Messages map[string]Message         `json:"messages"`
	Attempts map[string]storedAttempt   `json:"attempts"`
	Metadata map[string]json.RawMessage `json:"metadata"`
	Cursor   int64                      `json:"cursor"`
}

func emptyState() *localState {
	return &localState{Commands: map[string]Command{}, Results: map[string]storedResult{}, Messages: map[string]Message{}, Attempts: map[string]storedAttempt{}, Metadata: map[string]json.RawMessage{}}
}
func NewLocalStore(ctx context.Context, files platform.Files, root string) (*LocalStore, error) {
	root, e := files.PrivateState(ctx, root)
	if e != nil {
		return nil, e
	}
	s := &LocalStore{files, root}
	e = s.transaction(ctx, false, func(*localState) error { return nil })
	return s, e
}
func (s *LocalStore) transaction(ctx context.Context, write bool, fn func(*localState) error) error {
	lock, e := s.Files.AdvisoryLock(ctx, s.Root, "admission.lock", true)
	if e != nil {
		return errors.New("local store unavailable")
	}
	defer lock.Close()
	state := emptyState()
	file, e := s.Files.OpenNoFollow(ctx, s.Root, "store.json", false)
	if e == nil {
		raw, re := io.ReadAll(io.LimitReader(file, 256<<20+1))
		file.Close()
		if re != nil || len(raw) > 256<<20 || json.Unmarshal(raw, state) != nil || state.Commands == nil || state.Results == nil || state.Messages == nil || state.Attempts == nil || state.Metadata == nil {
			return errors.New("invalid local store")
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return errors.New("local store unavailable")
	}
	if e = fn(state); e != nil {
		return e
	}
	if !write {
		return nil
	}
	raw, e := json.Marshal(state)
	if e != nil {
		return errors.New("invalid local store")
	}
	if len(raw) > 256<<20 {
		return errors.New("local store capacity reached")
	}
	if s.Files.AtomicWrite(ctx, s.Root, "store.json", raw) != nil {
		return errors.New("local store unavailable")
	}
	return nil
}
func (s *LocalStore) Admit(ctx context.Context, c Command) error {
	return s.transaction(ctx, true, func(st *localState) error {
		if e := CanonicalID(c.ID); e != nil {
			return e
		}
		var data map[string]any
		if json.Unmarshal(c.Data, &data) != nil || data == nil || data["_request_id"] != c.ID {
			return errors.New("invalid request identity")
		}
		raw, _ := json.Marshal(c)
		if len(raw) > 1<<20 {
			return errors.New("request exceeds local size limit")
		}
		if _, ok := st.Results[c.ID]; ok {
			return nil
		}
		if old, ok := st.Commands[c.ID]; ok {
			a, _ := json.Marshal(old)
			if string(a) != string(raw) {
				return errors.New("request ID already used for different content")
			}
			return nil
		}
		size := len(raw)
		for _, v := range st.Commands {
			b, _ := json.Marshal(v)
			size += len(b)
		}
		if len(st.Commands) >= 1000 || size > 32<<20 {
			return errors.New("local outgoing queue is full; wait for the helper to reconnect")
		}
		st.Commands[c.ID] = c
		return nil
	})
}
func (s *LocalStore) Commands(ctx context.Context, limit int) (out []Command, err error) {
	err = s.transaction(ctx, false, func(st *localState) error {
		for _, v := range st.Commands {
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Created == out[j].Created {
				return out[i].ID < out[j].ID
			}
			return out[i].Created < out[j].Created
		})
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return
}
func (s *LocalStore) Complete(ctx context.Context, id string, r RPCResponse) error {
	return s.transaction(ctx, true, func(st *localState) error {
		st.Results[id] = storedResult{r, seconds()}
		delete(st.Commands, id)
		return nil
	})
}
func (s *LocalStore) Result(ctx context.Context, id string) (out RPCResponse, found bool, err error) {
	err = s.transaction(ctx, false, func(st *localState) error { v, ok := st.Results[id]; out = v.Value; found = ok; return nil })
	return
}
func (s *LocalStore) Apply(ctx context.Context, events []Event, cursor int64) error {
	return s.transaction(ctx, true, func(st *localState) error {
		for _, e := range events {
			if e.Type == "message" {
				var m Message
				if json.Unmarshal(e.Data, &m) != nil || m.ID == "" {
					return errors.New("invalid message event")
				}
				st.Messages[m.ID] = m
			}
		}
		st.Cursor = cursor
		return nil
	})
}
func (s *LocalStore) Cursor(ctx context.Context) (out int64, err error) {
	err = s.transaction(ctx, false, func(st *localState) error { out = st.Cursor; return nil })
	return
}
func (s *LocalStore) Messages(ctx context.Context, actor string, limit int) (out []Message, err error) {
	err = s.transaction(ctx, false, func(st *localState) error {
		for _, m := range st.Messages {
			if actor == "" || m.Recipient == actor {
				out = append(out, m)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return
}
func (s *LocalStore) Message(ctx context.Context, id string) (out Message, found bool, err error) {
	err = s.transaction(ctx, false, func(st *localState) error { out, found = st.Messages[id]; return nil })
	return
}
func (s *LocalStore) PendingReads(ctx context.Context) (out map[string][]string, err error) {
	out = map[string][]string{}
	err = s.transaction(ctx, false, func(st *localState) error {
		for _, c := range st.Commands {
			if c.Op == "read" {
				var d struct {
					Actor string
					IDs   []string `json:"message_ids"`
				}
				if json.Unmarshal(c.Data, &d) == nil && d.Actor != "" && len(d.IDs) > 0 && len(d.IDs) <= 100 {
					out[d.Actor] = append(out[d.Actor], d.IDs...)
				}
			}
		}
		return nil
	})
	return
}
func (s *LocalStore) StartAttempt(ctx context.Context, mid, claim string) error {
	return s.transaction(ctx, true, func(st *localState) error {
		if _, ok := st.Attempts[claim]; ok {
			return errors.New("native attempt already exists")
		}
		for _, a := range st.Attempts {
			if a.MessageID == mid && (!a.Finished || a.Result.Status != "held") {
				return errors.New("native attempt cannot be replayed")
			}
		}
		st.Attempts[claim] = storedAttempt{Attempt: Attempt{ClaimID: claim, MessageID: mid}, Created: seconds()}
		return nil
	})
}

// SafeResult discards all provider diagnostic input before persistence or RPC.
func SafeResult(r DeliveryResult) DeliveryResult {
	switch r.Status {
	case "held":
		return DeliveryResult{Status: "held", Detail: "session no longer ready; no input submitted"}
	case "submitted":
		return DeliveryResult{Status: "submitted", Detail: "native input submitted; model receipt unconfirmed"}
	case "failed":
		return DeliveryResult{Status: "failed", Detail: "native submission failed"}
	default:
		return DeliveryResult{Status: "uncertain", Detail: "native submission outcome unknown; no automatic retry"}
	}
}
func (s *LocalStore) FinishAttempt(ctx context.Context, id string, r DeliveryResult) error {
	return s.transaction(ctx, true, func(st *localState) error {
		a, ok := st.Attempts[id]
		if !ok || a.Finished {
			return errors.New("native attempt unavailable")
		}
		a.Result = SafeResult(r)
		a.Finished = true
		st.Attempts[id] = a
		return nil
	})
}
func (s *LocalStore) RecoverAttempts(ctx context.Context) error {
	return s.transaction(ctx, true, func(st *localState) error {
		for id, a := range st.Attempts {
			if !a.Finished {
				a.Finished = true
				a.Result = SafeResult(DeliveryResult{})
				st.Attempts[id] = a
			}
		}
		return nil
	})
}
func (s *LocalStore) Reports(ctx context.Context) (out []Attempt, err error) {
	err = s.transaction(ctx, false, func(st *localState) error {
		for _, a := range st.Attempts {
			if a.Finished && !a.Reported {
				out = append(out, a.Attempt)
			}
		}
		return nil
	})
	return
}
func (s *LocalStore) MarkReported(ctx context.Context, id string) error {
	return s.transaction(ctx, true, func(st *localState) error { a := st.Attempts[id]; a.Reported = true; st.Attempts[id] = a; return nil })
}
func (s *LocalStore) ReportRejected(ctx context.Context, id string) error {
	return s.transaction(ctx, true, func(st *localState) error {
		a := st.Attempts[id]
		a.ReportError = "native result rejected"
		a.Reported = true
		st.Attempts[id] = a
		return nil
	})
}
func (s *LocalStore) Get(ctx context.Context, key string, out any) (found bool, err error) {
	err = s.transaction(ctx, false, func(st *localState) error {
		b, ok := st.Metadata[key]
		found = ok
		if ok {
			return json.Unmarshal(b, out)
		}
		return nil
	})
	return
}
func (s *LocalStore) Put(ctx context.Context, key string, value any) error {
	return s.transaction(ctx, true, func(st *localState) error {
		b, e := json.Marshal(value)
		if e == nil {
			st.Metadata[key] = b
		}
		return e
	})
}
func (s *LocalStore) Prune(ctx context.Context, now time.Time) error {
	return s.transaction(ctx, true, func(st *localState) error {
		cut := float64(now.Add(-RequestWindow).Unix())
		for id, m := range st.Messages {
			if m.Updated < cut {
				switch m.Status {
				case Submitted, Acknowledged, Replied, Expired, Cancelled, Failed:
					delete(st.Messages, id)
				}
			}
		}
		for id, r := range st.Results {
			if r.Created < cut {
				delete(st.Results, id)
			}
		}
		for id, a := range st.Attempts {
			if a.Reported && a.Created < float64(now.Add(-30*24*time.Hour).Unix()) {
				delete(st.Attempts, id)
			}
		}
		return nil
	})
}
func seconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var _ Store = (*LocalStore)(nil)
