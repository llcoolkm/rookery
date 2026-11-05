package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
	"github.com/llcoolkm/rookery/go/provider/internal/ws"
)

const IdentityError = "Codex caller identity unavailable or inconsistent. Reconnect MCP with a compatible Codex version, or use an explicit per-session binding."

var errDiscovery = errors.New("Codex session discovery unavailable")

// Options contains installation-selected facilities, never peer tool arguments.
// Endpoint must be issued by the platform. MessageStdinFlag is accepted only
// after queue --help explicitly advertises that exact flag.
type Options struct {
	Executable       string
	Endpoint         platform.Endpoint
	MessageStdinFlag string
}
type Adapter struct {
	host    platform.Host
	options Options
	mu      sync.Mutex
	pins    map[string]platform.ProcessRef
}
type Factory struct{}

func (Factory) New(c provider.Config, h platform.Host) (provider.Provider, error) { return New(c, h) }
func New(c provider.Config, h platform.Host) (*Adapter, error) {
	return NewWithOptions(c, h, Options{})
}
func NewWithOptions(_ provider.Config, h platform.Host, o Options) (*Adapter, error) {
	return &Adapter{host: h, options: o, pins: map[string]platform.ProcessRef{}}, nil
}
func (*Adapter) Name() string                        { return "codex" }
func (*Adapter) Capabilities() provider.Capabilities { return provider.Capabilities{Topology: "host"} }
func (*Adapter) Admission(s core.Session) provider.Admission {
	if s.Delivery == "polling" {
		return provider.AdmitNone
	}
	return provider.AdmitIdle
}
func (*Adapter) Identity(_ context.Context, m core.CallMetadata) (string, error) {
	var id string
	if json.Unmarshal(m["threadId"], &id) != nil || !canonical(id) {
		return "", errors.New(IdentityError)
	}
	var other string
	if raw, ok := m["sessionId"]; ok {
		if json.Unmarshal(raw, &other) != nil || other != id {
			return "", errors.New(IdentityError)
		}
	}
	var turn map[string]json.RawMessage
	if json.Unmarshal(m["x-codex-turn-metadata"], &turn) == nil {
		for _, key := range []string{"thread_id", "session_id"} {
			if raw, ok := turn[key]; ok {
				if json.Unmarshal(raw, &other) != nil || other != id {
					return "", errors.New(IdentityError)
				}
			}
		}
	}
	return "codex:" + id, nil
}
func canonical(id string) bool { _, err := core.RequestCreated(id); return err == nil }

type proxy struct {
	conn    platform.Connection
	ws      *ws.Conn
	counter int
}

func (a *Adapter) open(ctx context.Context) (*proxy, error) {
	ep := a.options.Endpoint
	if ep == "" {
		if p, ok := a.host.Paths.(interface {
			ControlEndpoint(context.Context, string) (platform.Endpoint, error)
		}); ok {
			var err error
			ep, err = p.ControlEndpoint(ctx, "codex")
			if err != nil {
				return nil, errDiscovery
			}
		}
	}
	if ep == "" || a.host.IPC == nil {
		return nil, errDiscovery
	}
	conn, err := a.host.IPC.Dial(ctx, ep)
	if err != nil {
		return nil, errDiscovery
	}
	if err = conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		conn.Close()
		return nil, errDiscovery
	}
	transport, err := ws.Open(conn)
	if err != nil {
		conn.Close()
		return nil, errDiscovery
	}
	p := &proxy{conn: conn, ws: transport}
	if err = p.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "rookery", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		conn.Close()
		return nil, errDiscovery
	}
	if err = p.notify("initialized", map[string]any{}); err != nil {
		conn.Close()
		return nil, errDiscovery
	}
	return p, nil
}
func (p *proxy) notify(method string, params any) error {
	b, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return errDiscovery
	}
	return p.ws.Send(b)
}
func (p *proxy) call(method string, params, out any) error {
	p.counter++
	b, err := json.Marshal(map[string]any{"id": p.counter, "method": method, "params": params})
	if err != nil {
		return errDiscovery
	}
	if err = p.ws.Send(b); err != nil {
		return errDiscovery
	}
	for {
		b, err = p.ws.Receive()
		if err != nil {
			return errDiscovery
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(b, &row) != nil || row == nil {
			return errDiscovery
		}
		var id int
		if json.Unmarshal(row["id"], &id) != nil || id != p.counter {
			continue
		}
		if _, ok := row["method"]; ok {
			continue
		}
		if _, ok := row["error"]; ok {
			return errDiscovery
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(row["result"], &obj) != nil || obj == nil {
			return errDiscovery
		}
		if out != nil && json.Unmarshal(row["result"], out) != nil {
			return errDiscovery
		}
		return nil
	}
}

type Thread struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status struct {
		Type  string            `json:"type"`
		Flags []json.RawMessage `json:"activeFlags"`
	} `json:"status"`
	Accept bool `json:"canAcceptDirectInput"`
}

func Normalize(t Thread) core.Session {
	status := "unknown"
	switch t.Status.Type {
	case "idle", "systemError":
		if t.Accept {
			status = "idle"
		}
	case "active":
		status = "busy"
		if len(t.Status.Flags) > 0 {
			status = "waiting"
		}
	}
	name := t.Name
	if name == "" {
		name = t.ID
	}
	return core.Session{Provider: "codex", ID: t.ID, Name: name, Status: status}
}
func (a *Adapter) Discover(ctx context.Context) ([]core.Session, error) {
	p, err := a.open(ctx)
	if err != nil {
		return nil, err
	}
	defer p.conn.Close()
	peer, err := p.conn.Peer()
	if err != nil {
		return nil, errDiscovery
	}
	cursor := ""
	cursors := map[string]bool{}
	seen := map[string]bool{}
	rows := []core.Session{}
	pins := map[string]platform.ProcessRef{}
	for page := 0; page < 100; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var listing struct {
			Data   []string `json:"data"`
			Cursor string   `json:"nextCursor"`
		}
		if err = p.call("thread/loaded/list", params, &listing); err != nil {
			return nil, err
		}
		for _, id := range listing.Data {
			if seen[id] {
				continue
			}
			seen[id] = true
			if !canonical(id) {
				return nil, errDiscovery
			}
			var reply struct {
				Thread Thread `json:"thread"`
			}
			if err = p.call("thread/read", map[string]any{"threadId": id, "includeTurns": false}, &reply); err != nil {
				return nil, err
			}
			if reply.Thread.ID != id {
				return nil, errDiscovery
			}
			if reply.Thread.Status.Type == "notLoaded" {
				continue
			}
			row := Normalize(reply.Thread)
			row.PID = peer.PID
			row.Admission = string(a.Admission(row))
			rows = append(rows, row)
			pins[id] = peer
		}
		cursor = listing.Cursor
		if cursor == "" {
			a.mu.Lock()
			a.pins = pins
			a.mu.Unlock()
			return rows, nil
		}
		if cursors[cursor] {
			return nil, errDiscovery
		}
		cursors[cursor] = true
	}
	return nil, errDiscovery
}
func (a *Adapter) Busy(ctx context.Context, s core.Session) (provider.BusyState, error) {
	p, err := a.open(ctx)
	if err != nil {
		return provider.Unknown, err
	}
	defer p.conn.Close()
	var reply struct {
		Thread Thread `json:"thread"`
	}
	if err = p.call("thread/read", map[string]any{"threadId": s.ID, "includeTurns": false}, &reply); err != nil || reply.Thread.ID != s.ID {
		return provider.Unknown, errDiscovery
	}
	return provider.BusyState(Normalize(reply.Thread).Status), nil
}
func outcome(status, detail string) (core.DeliveryResult, error) {
	return core.DeliveryResult{Status: status, Detail: detail}, nil
}
func (a *Adapter) Deliver(ctx context.Context, s core.Session, m core.Message) (core.DeliveryResult, error) {
	if !canonical(s.ID) {
		return outcome("failed", "Codex destination must be an exact UUID")
	}
	if s.Delivery == "polling" {
		return outcome("held", "polling session; no submission attempted")
	}
	if m.Expires <= float64(time.Now().UnixNano())/1e9 {
		return outcome("held", "message expired; no submission attempted")
	}
	a.mu.Lock()
	pin, ok := a.pins[s.ID]
	a.mu.Unlock()
	if !ok || pin.PID != s.PID || a.host.Process.Pin(ctx, pin) != nil {
		return outcome("held", "session identity changed; no submission attempted")
	}
	p, err := a.open(ctx)
	if err != nil {
		return outcome("held", "Codex session discovery unavailable; no submission attempted")
	}
	peer, err := p.conn.Peer()
	if err != nil || peer != pin {
		p.conn.Close()
		return outcome("held", "session identity changed; no submission attempted")
	}
	var reply struct {
		Thread Thread `json:"thread"`
	}
	err = p.call("thread/read", map[string]any{"threadId": s.ID, "includeTurns": false}, &reply)
	p.conn.Close()
	if err != nil || reply.Thread.ID != s.ID || Normalize(reply.Thread).Status != "idle" {
		return outcome("held", "Codex recipient is not confirmed idle; no submission attempted")
	}
	executable := a.options.Executable
	if executable == "" {
		if p, ok := a.host.Paths.(interface{ Executable(string) (string, error) }); ok {
			executable, _ = p.Executable("codex")
		}
	}
	if executable == "" || a.host.Exec == nil {
		return outcome("held", "Codex executable unavailable; no submission attempted")
	}
	// The baseline only documents --message argv. Never infer that '-' means stdin.
	flag := a.options.MessageStdinFlag
	if flag == "" {
		flag = "--message-stdin"
	}
	if flag != "--message-stdin" {
		return outcome("held", "Codex queue has no verified private input route; no submission attempted")
	}
	help, err := a.host.Exec.Exec(ctx, platform.Spec{Path: executable, Args: []string{"queue", "--help"}, Timeout: 5 * time.Second, MaxOutput: 64 << 10})
	if err != nil || help.State != platform.Completed || help.ExitCode != 0 || !hasFlag(string(help.Stdout), flag) {
		return outcome("held", "Codex queue has no verified private input route; no submission attempted")
	}
	if a.host.Process.Pin(ctx, pin) != nil || m.Expires <= float64(time.Now().UnixNano())/1e9 {
		return outcome("held", "session identity or expiry changed; no submission attempted")
	}
	result, err := a.host.Exec.Exec(ctx, platform.Spec{Path: executable, Args: []string{"queue", "--thread", s.ID, flag}, Stdin: []byte(core.Render(m)), Timeout: 20 * time.Second, MaxOutput: 64 << 10})
	if result.State == platform.NotStarted {
		return outcome("failed", "Codex queue did not start")
	}
	if err != nil || result.State != platform.Completed || result.ExitCode != 0 {
		return outcome("uncertain", "Codex queue outcome unknown; reconcile before retrying")
	}
	return outcome("submitted", "Codex queue accepted input; recipient receipt unconfirmed")
}
func hasFlag(help, flag string) bool {
	for _, v := range strings.Fields(help) {
		if strings.Trim(v, ",[]") == flag {
			return true
		}
	}
	return false
}

var _ provider.Provider = (*Adapter)(nil)
