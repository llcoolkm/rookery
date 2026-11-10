package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/llcoolkm/rookery/go/platform"
)

var nodePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var providerPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func CanonicalID(id string) error {
	id = strings.ToLower(id)
	if uuidPattern.MatchString(id) {
		return nil
	}
	raw := strings.ReplaceAll(strings.ReplaceAll(id, "urn:", ""), "uuid:", "")
	raw = strings.ReplaceAll(strings.Trim(raw, "{}"), "-", "")
	if len(raw) == 32 {
		if _, err := hex.DecodeString(raw); err == nil {
			return errors.New("ID must be a canonical UUID")
		}
	}
	return errors.New("ID must be a UUID")
}

func Qualify(actor, node string) (string, error) {
	if actor == "" {
		return "", errors.New("session binding required: --as or ROOKERY_SESSION")
	}
	if actor == "operator" {
		return "", errors.New("network messaging requires --as provider:UUID of an enrolled local session")
	}
	if !strings.Contains(actor, "/") {
		actor = node + "/" + actor
	}
	if !strings.HasPrefix(actor, node+"/") {
		return "", errors.New("actor must belong to this connector node")
	}
	n, local, ok := strings.Cut(actor, "/")
	p, id, ok2 := strings.Cut(local, ":")
	id = strings.ToLower(id)
	if !ok || !ok2 {
		return "", errors.New("address must be node/provider:UUID")
	}
	if !nodePattern.MatchString(n) || !providerPattern.MatchString(p) {
		return "", errors.New("invalid node or provider")
	}
	if e := CanonicalID(id); e != nil {
		return "", e
	}
	return n + "/" + p + ":" + id, nil
}

type Config struct {
	Node             string   `json:"node"`
	URL              string   `json:"url"`
	Token            string   `json:"token"`
	AllowHTTP        bool     `json:"allow_http_loopback"`
	BusyDelivery     []string `json:"busy_delivery"`
	SessionAllowlist []string `json:"session_allowlist"`
}

func (c Config) Validate() error {
	if !nodePattern.MatchString(c.Node) {
		return errors.New("invalid node or token (minimum 32 non-whitespace characters)")
	}
	for _, entry := range c.SessionAllowlist {
		p, id, ok := strings.Cut(entry, ":")
		if !ok || (p != "codex" && p != "claude" && p != "agy") || CanonicalID(id) != nil {
			return errors.New("session allowlist must contain provider:UUID entries")
		}
	}
	_, e := NewHTTPHub(c.URL, c.Token, c.AllowHTTP)
	return e
}

type Client struct {
	Store   *LocalStore
	Config  Config
	Actor   string
	Timeout time.Duration
}

func (c *Client) Identity(ctx context.Context) (string, error) {
	return Qualify(c.Actor, c.Config.Node)
}
func (c *Client) Request(ctx context.Context, op string, input map[string]any, timeout time.Duration) (map[string]any, error) {
	data := map[string]any{}
	for k, v := range input {
		data[k] = v
	}
	if op == "send" || op == "reply" {
		body, ok := data["body"].(string)
		if !ok || !utf8.ValidString(body) || strings.TrimSpace(body) == "" || len(body) > MaxBodyBytes {
			return nil, errors.New("body must contain text and be at most 16384 UTF-8 bytes")
		}
		ttl, ok := data["ttl"].(int)
		if !ok || ttl < 1 || ttl > 86400 {
			return nil, errors.New("ttl must be an integer between 1 and 86400 seconds")
		}
		data["id"] = strings.ToLower(stringArg(data, "id", ""))
		if op == "reply" {
			data["message_id"] = strings.ToLower(stringArg(data, "message_id", ""))
		}
		if e := CanonicalID(stringArg(data, "id", "")); e != nil {
			return nil, e
		}
		if op == "reply" {
			if e := CanonicalID(stringArg(data, "message_id", "")); e != nil {
				return nil, e
			}
		} else {
			kind := stringArg(data, "kind", "ask")
			if kind != "ask" && kind != "inform" {
				return nil, errors.New("send kind must be ask or inform")
			}
		}
		if _, ok := data["expires"]; !ok {
			data["expires"] = seconds() + float64(ttl)
		}
	}
	id, e := NewRequestID(time.Now())
	if e != nil {
		return nil, errors.New("request identity unavailable")
	}
	data["_request_id"] = id
	b, e := json.Marshal(data)
	if e != nil {
		return nil, errors.New("invalid request fields")
	}
	queued := func() (map[string]any, error) {
		return map[string]any{"status": "queued_local", "request_id": id, "id": data["id"], "detail": "saved locally; helper will upload when connected"}, nil
	}
	if e = c.Store.Admit(ctx, Command{ID: id, Op: op, Data: b, Created: seconds()}); e != nil {
		if errors.Is(e, platform.ErrCommittedNotDurable) {
			return queued()
		}
		return nil, e
	}
	deadline := time.Now().Add(timeout)
	for timeout > 0 && time.Now().Before(deadline) {
		r, ok, e := c.Store.Result(ctx, id)
		if e != nil {
			if ctx.Err() != nil {
				return queued()
			}
			return nil, e
		}
		if ok {
			if !r.OK {
				return nil, errors.New(r.Error)
			}
			var result map[string]any
			if json.Unmarshal(r.Result, &result) != nil {
				return nil, errors.New("invalid local result")
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
			return queued()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return queued()
}
func Compact(value map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "request_id", "sender", "recipient", "kind", "body", "status", "received_by_host", "read_at", "detail", "reply_id", "in_reply_to", "followup_id", "expires"} {
		if v, ok := value[k]; ok {
			out[k] = v
		}
	}
	return out
}
func messageMap(m Message) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func (c *Client) Invoke(ctx context.Context, op string, args map[string]any) (any, error) {
	actor := ""
	var e error
	if op == "send" || op == "reply" || op == "ack" || op == "inbox" {
		actor, e = c.Identity(ctx)
		if e != nil {
			return nil, e
		}
	}
	switch op {
	case "send", "reply":
		if raw, ok := args["ttl"]; ok {
			valid := false
			switch v := raw.(type) {
			case int:
				valid = true
			case json.Number:
				_, e := v.Int64()
				valid = e == nil
			case float64:
				valid = v == float64(int(v))
			}
			if !valid {
				return nil, errors.New("ttl must be an integer between 1 and 86400 seconds")
			}
		}
		ttl := intArg(args, "ttl", SendTTL)
		if op == "reply" {
			ttl = intArg(args, "ttl", ReplyTTL)
		}
		id := stringArg(args, "id", "")
		if id == "" {
			id, e = NewRequestID(time.Now())
			if e != nil {
				return nil, e
			}
		}
		d := map[string]any{"actor": actor, "id": id, "body": args["body"], "ttl": ttl}
		if op == "send" {
			d["to"] = args["to"]
			d["kind"] = stringArg(args, "kind", "ask")
		} else {
			d["message_id"] = args["message_id"]
		}
		r, e := c.Request(ctx, op, d, c.Timeout)
		return Compact(r), e
	case "ack":
		return c.Request(ctx, "ack", map[string]any{"actor": actor, "message_id": args["message_id"]}, c.Timeout)
	case "enroll", "disable", "clear-alias":
		target := stringArg(args, "target", "")
		if !strings.Contains(target, "/") {
			target = c.Config.Node + "/" + target
		}
		d := map[string]any{"target": target}
		if alias := stringArg(args, "alias", ""); alias != "" {
			d["alias"] = alias
		}
		return c.Request(ctx, op, d, c.Timeout)
	case "inbox":
		limit := intArg(args, "limit", 20)
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
		if raw, ok := args["peek"]; ok {
			if _, valid := raw.(bool); !valid {
				return nil, errors.New("peek must be a boolean")
			}
		}
		peek, _ := args["peek"].(bool)
		msgs, e := c.Store.Messages(ctx, actor, 0)
		if e != nil {
			return nil, e
		}
		filtered := []Message{}
		for _, m := range msgs {
			if m.Status == Acknowledged || m.Status == Replied || m.Status == Cancelled || (m.Status == Expired && m.ReadAt != nil) {
				continue
			}
			filtered = append(filtered, m)
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			if (filtered[i].ReadAt == nil) != (filtered[j].ReadAt == nil) {
				return filtered[i].ReadAt == nil
			}
			return filtered[i].Updated > filtered[j].Updated
		})
		if len(filtered) > limit {
			filtered = filtered[:limit]
		}
		out := []map[string]any{}
		ids := []string{}
		for _, m := range filtered {
			out = append(out, Compact(messageMap(m)))
			if m.ReadAt == nil {
				ids = append(ids, m.ID)
			}
		}
		if !peek && len(ids) > 0 {
			if _, e = c.Request(ctx, "read", map[string]any{"actor": actor, "message_ids": ids}, 0); e != nil {
				return nil, e
			}
		}
		return out, nil
	case "status", "health", "doctor":
		id := stringArg(args, "message_id", "")
		if id != "" {
			m, found, e := c.Store.Message(ctx, id)
			if e != nil {
				return nil, e
			}
			if !found {
				return map[string]any{"id": id, "status": "unknown_local"}, nil
			}
			if c.Actor != "" {
				actor, e := c.Identity(ctx)
				if e != nil {
					return nil, e
				}
				if m.Sender != actor && m.Recipient != actor {
					return nil, errors.New("message belongs to another session")
				}
			}
			return Compact(messageMap(m)), nil
		}
		out := map[string]any{}
		found, e := c.Store.Get(ctx, "health", &out)
		if e != nil {
			return nil, e
		}
		if !found {
			return map[string]any{"running": false, "connected": false, "detail": "helper has not started"}, nil
		}
		updated, _ := out["updated"].(float64)
		out["stale"] = seconds()-updated > 45
		if out["stale"] == true {
			out["connected"] = false
		}
		return out, nil
	case "peers", "list":
		out := map[string]any{}
		found, e := c.Store.Get(ctx, "directory", &out)
		if e != nil {
			return nil, e
		}
		if !found {
			return map[string]any{"sessions": []any{}, "detail": "helper has not connected yet"}, nil
		}
		updated, _ := out["updated"].(float64)
		stale := seconds()-updated > 45
		out["stale"] = stale
		if stale {
			rows, _ := out["sessions"].([]any)
			for _, row := range rows {
				if r, ok := row.(map[string]any); ok {
					r["online"] = false
					r["status"] = "unknown"
				}
			}
		}
		return out, nil
	}
	return nil, errors.New("unknown operation")
}
func stringArg(m map[string]any, key, def string) string {
	v, ok := m[key].(string)
	if !ok {
		return def
	}
	return v
}
func intArg(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		if v == float64(int(v)) {
			return int(v)
		}
	case json.Number:
		i, e := v.Int64()
		if e == nil {
			return int(i)
		}
	}
	return def
}
func ValidName(s string) bool { return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= 256 }
