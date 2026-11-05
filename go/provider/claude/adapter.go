package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

const IdentityError = "Claude caller identity unavailable. Reconnect MCP from the intended Claude session."

var errDiscovery = errors.New("Claude session discovery unavailable")

type Options struct{ Executable string }
type destination struct {
	pin      platform.ProcessRef
	endpoint platform.Endpoint
}
type Adapter struct {
	host         platform.Host
	options      Options
	busy         bool
	mu           sync.Mutex
	destinations map[string]destination
	owner        *platform.ProcessRef
}
type Factory struct{}

func (Factory) New(c provider.Config, h platform.Host) (provider.Provider, error) { return New(c, h) }
func New(c provider.Config, h platform.Host) (*Adapter, error) {
	return NewWithOptions(c, h, Options{})
}
func NewWithOptions(c provider.Config, h platform.Host, o Options) (*Adapter, error) {
	busy := c.BusyDelivery == nil
	for _, name := range c.BusyDelivery {
		if name == "claude" {
			busy = true
		}
	}
	return &Adapter{host: h, options: o, busy: busy, destinations: map[string]destination{}}, nil
}
func (*Adapter) Name() string { return "claude" }
func (*Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Topology: "host", BusyOptIn: true}
}
func (a *Adapter) Admission(s core.Session) provider.Admission {
	if s.Delivery == "polling" {
		return provider.AdmitNone
	}
	if a.busy && (s.Status == "idle" || s.Status == "busy") {
		return provider.AdmitBusy
	}
	return provider.AdmitIdle
}

type row struct {
	PID    int    `json:"pid"`
	ID     string `json:"sessionId"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (a *Adapter) rows(ctx context.Context) ([]row, error) {
	executable := a.options.Executable
	if executable == "" {
		if p, ok := a.host.Paths.(interface{ Executable(string) (string, error) }); ok {
			executable, _ = p.Executable("claude")
		}
	}
	if executable == "" || a.host.Exec == nil {
		return nil, errDiscovery
	}
	r, err := a.host.Exec.Exec(ctx, platform.Spec{Path: executable, Args: []string{"agents", "--json"}, Timeout: 15 * time.Second, MaxOutput: 1 << 20})
	if err != nil || r.State != platform.Completed || r.ExitCode != 0 {
		return nil, errDiscovery
	}
	var raw []json.RawMessage
	if json.Unmarshal(r.Stdout, &raw) != nil || raw == nil {
		return nil, errDiscovery
	}
	rows := make([]row, 0, len(raw))
	for _, v := range raw {
		var obj map[string]json.RawMessage
		if json.Unmarshal(v, &obj) != nil || obj == nil {
			return nil, errDiscovery
		}
		var item row
		if json.Unmarshal(v, &item) != nil {
			return nil, errDiscovery
		}
		rows = append(rows, item)
	}
	return rows, nil
}
func canonical(id string) bool { _, err := core.RequestCreated(id); return err == nil }
func (a *Adapter) Identity(ctx context.Context, _ core.CallMetadata) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fail := func() (string, error) { return "", errors.New(IdentityError) }
	current, err := a.host.Process.Current(ctx)
	if err != nil {
		return fail()
	}
	before, err := a.host.Process.Ancestors(ctx, current)
	if err != nil {
		return fail()
	}
	rows, err := a.rows(ctx)
	if err != nil {
		return fail()
	}
	after, err := a.host.Process.Ancestors(ctx, current)
	if err != nil || !reflect.DeepEqual(before, after) {
		return fail()
	}
	var owner platform.ProcessRef
	if a.owner == nil {
		found := false
		for _, ancestor := range before {
			for _, r := range rows {
				if r.PID == ancestor.PID {
					owner = ancestor
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return fail()
		}
	} else {
		owner = *a.owner
	}
	present := false
	for _, ancestor := range before {
		if ancestor == owner {
			present = true
			break
		}
	}
	if !present || a.host.Process.Pin(ctx, owner) != nil {
		return fail()
	}
	var matches []row
	for _, r := range rows {
		if r.PID == owner.PID {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || !canonical(matches[0].ID) {
		return fail()
	}
	a.owner = &owner
	return "claude:" + matches[0].ID, nil
}
func (a *Adapter) Discover(ctx context.Context) ([]core.Session, error) {
	rows, err := a.rows(ctx)
	if err != nil {
		return nil, err
	}
	root, err := a.host.Paths.ProviderData("claude")
	if err != nil {
		return nil, errDiscovery
	}
	sessions := []core.Session{}
	destinations := map[string]destination{}
	counts := map[string]int{}
	for _, r := range rows {
		if canonical(r.ID) && r.PID > 0 {
			counts[r.ID]++
		}
	}
	for _, r := range rows {
		if !canonical(r.ID) || r.PID <= 0 || counts[r.ID] != 1 {
			continue
		}
		pin, err := a.host.Process.Lookup(ctx, r.PID)
		if err != nil {
			continue
		}
		status := r.Status
		if status != "idle" && status != "busy" && status != "waiting" {
			status = "unknown"
		}
		s := core.Session{ProcessRef: &pin, Provider: "claude", ID: r.ID, Name: r.Name, PID: r.PID, Status: status}
		s.Admission = string(a.Admission(s))
		sessions = append(sessions, s)
		file, err := a.host.Files.OpenNoFollow(ctx, root, fmt.Sprintf("sessions/%d.json", r.PID), false)
		if err != nil {
			continue
		}
		var record struct {
			Address string `json:"messagingSocketPath"`
			ID      string `json:"sessionId"`
		}
		b, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if readErr != nil || len(b) > 1<<20 || json.Unmarshal(b, &record) != nil || record.Address == "" || (record.ID != "" && record.ID != r.ID) {
			file.Close()
			continue
		}
		ep, err := a.host.Paths.HarnessEndpoint(ctx, "claude", file, record.Address, pin)
		file.Close()
		if err != nil || a.host.Process.Pin(ctx, pin) != nil {
			continue
		}
		destinations[r.ID] = destination{pin: pin, endpoint: ep}
	}
	a.mu.Lock()
	a.destinations = destinations
	a.mu.Unlock()
	return sessions, nil
}
func (a *Adapter) Busy(ctx context.Context, s core.Session) (provider.BusyState, error) {
	rows, err := a.rows(ctx)
	if err != nil {
		return provider.Unknown, err
	}
	matches := 0
	status := provider.Unknown
	for _, r := range rows {
		if r.ID == s.ID && r.PID == s.PID {
			matches++
			switch r.Status {
			case "idle", "busy", "waiting":
				status = provider.BusyState(r.Status)
			}
		}
	}
	if matches != 1 {
		return provider.Unknown, errDiscovery
	}
	return status, nil
}

var nameStrip = regexp.MustCompile(`["<>\r\n]`)
var closingTag = regexp.MustCompile(`</(cross-session-message\s*>)`)

func Wrap(body, name, from string) string {
	attrs := []string{}
	if from != "" {
		attrs = append(attrs, `from="`+nameStrip.ReplaceAllString(from, "")+`"`)
	}
	name = strings.TrimSpace(nameStrip.ReplaceAllString(name, ""))
	if name != "" {
		attrs = append(attrs, `from-name="`+name+`"`)
	}
	head := ""
	if len(attrs) > 0 {
		head = " " + strings.Join(attrs, " ")
	}
	body = closingTag.ReplaceAllString(body, `<\/$1`)
	return "<cross-session-message" + head + ">\n" + body + "\n</cross-session-message>"
}
func BuildFrame(body, name, id string) ([]byte, error) {
	if body == "" || len([]rune(body)) > 60000 || !canonical(id) {
		return nil, errors.New("invalid Claude native frame")
	}
	msgID, err := core.NewRequestID(time.Now())
	if err != nil {
		return nil, errors.New("invalid Claude native frame")
	}
	frame := map[string]any{"msgV": 1, "msg_id": msgID, "type": "user", "message": map[string]string{"role": "user", "content": Wrap(body, name, "")}, "priority": "next", "session_id": id}
	b, err := json.Marshal(frame)
	if err != nil {
		return nil, errors.New("invalid Claude native frame")
	}
	return append(b, '\n'), nil
}
func outcome(status, detail string) (core.DeliveryResult, error) {
	return core.DeliveryResult{Status: status, Detail: detail}, nil
}
func (a *Adapter) Deliver(ctx context.Context, s core.Session, m core.Message) (core.DeliveryResult, error) {
	if !canonical(s.ID) {
		return outcome("failed", "Claude destination must be an exact UUID")
	}
	if s.Delivery == "polling" {
		return outcome("held", "polling session; no submission attempted")
	}
	a.mu.Lock()
	dest, ok := a.destinations[s.ID]
	a.mu.Unlock()
	if !ok || s.ProcessRef == nil || *s.ProcessRef != dest.pin || dest.pin.PID != s.PID || a.host.Process.Pin(ctx, dest.pin) != nil {
		return outcome("held", "session identity changed; no submission attempted")
	}
	state, err := a.Busy(ctx, s)
	current := s
	current.Status = string(state)
	if err != nil || !(state == provider.Idle || state == provider.Busy && a.Admission(current) == provider.AdmitBusy) {
		return outcome("held", "no confirmed native inbox; no submission attempted")
	}
	if a.host.Process.Pin(ctx, dest.pin) != nil || m.Expires <= float64(time.Now().UnixNano())/1e9 {
		return outcome("held", "session identity or expiry changed; no submission attempted")
	}
	b, err := BuildFrame(core.Render(m), "rookery-local", s.ID)
	if err != nil {
		return outcome("failed", "invalid Claude native frame")
	}
	conn, err := a.host.IPC.Dial(ctx, dest.endpoint)
	if err != nil {
		return outcome("failed", "Claude inbox connection unavailable")
	}
	defer conn.Close()
	peer, err := conn.Peer()
	if err != nil || peer != dest.pin {
		return outcome("held", "session identity changed; no submission attempted")
	}
	if conn.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
		return outcome("failed", "Claude inbox unavailable")
	}
	if a.host.Process.Pin(ctx, dest.pin) != nil || m.Expires <= float64(time.Now().UnixNano())/1e9 {
		return outcome("held", "session identity or expiry changed; no submission attempted")
	}
	for len(b) > 0 {
		n, err := conn.Write(b)
		if err != nil || n <= 0 {
			return outcome("uncertain", "Claude native write outcome unknown; reconcile before retrying")
		}
		b = b[n:]
	}
	return outcome("submitted", "frame written; native hold/loss and model receipt unconfirmed")
}

var _ provider.Provider = (*Adapter)(nil)
