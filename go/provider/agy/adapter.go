package agy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

const (
	MaxRecord         = 4096
	Stale             = 20 * time.Second
	HeldDetail        = "AGY delivery held: no verified stdin or file route for agentapi send-message; no input submitted"
	UnsupportedDetail = "AGY is unsupported on this platform; Windows AGY remains deferred"
)

// Record belongs exclusively to the Go state root. It carries process pins and
// public conversation metadata; it never contains credentials or an address.
type Record struct {
	V                 int     `json:"v"`
	Provider          string  `json:"provider"`
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	PID               int     `json:"pid"`
	Starttime         string  `json:"starttime"`
	BootID            string  `json:"boot_id"`
	EndpointPID       int     `json:"endpoint_pid"`
	EndpointStarttime string  `json:"endpoint_starttime"`
	Heartbeat         float64 `json:"heartbeat"`
	DeliveryHeld      bool    `json:"delivery_held"`
}

func RecordName(id string) string         { return "agy-" + id + ".json" }
func SocketName(id string) string         { return "agy-" + id + ".sock" }
func MarkerName(id, suffix string) string { return "agy-" + id + "." + suffix }

type attached struct {
	record        Record
	owner, server platform.ProcessRef
	root          string
}

type Adapter struct {
	config  provider.Config
	paths   platform.Paths
	files   platform.Files
	process platform.Process
	ipc     platform.IPC
	mu      sync.Mutex
	live    map[string]attached
}

type Factory struct{}

func (Factory) New(c provider.Config, h platform.Host) (provider.Provider, error) { return New(c, h) }
func New(c provider.Config, h platform.Host) (*Adapter, error) {
	if c.SessionAllowlist != nil {
		c.SessionAllowlist = append([]string{}, c.SessionAllowlist...)
	}
	return &Adapter{config: c, paths: h.Paths, files: h.Files, process: h.Process, ipc: h.IPC, live: map[string]attached{}}, nil
}
func (*Adapter) Name() string { return "agy" }
func (*Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Topology: "attached", AcceptsBusy: runtime.GOOS == "linux"}
}
func (*Adapter) Identity(_ context.Context, meta core.CallMetadata) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New(UnsupportedDetail)
	}
	return core.AGYMetadataIdentity(meta)
}
func canonical(id string) bool {
	_, err := core.RequestCreated(id)
	return err == nil && id == strings.ToLower(id)
}

func (a *Adapter) marker(ctx context.Context, root, id string) bool {
	// Any sticky detach or supersession marker withdraws this generation before
	// the endpoint exits. An unreadable state directory fails closed.
	for _, suffix := range []string{"detached", "superseded"} {
		f, err := a.files.OpenNoFollow(ctx, root, MarkerName(id, suffix), false)
		if err == nil {
			f.Close()
			return true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}
func (a *Adapter) read(ctx context.Context, root, id string) (Record, error) {
	var r Record
	f, err := a.files.OpenNoFollow(ctx, root, RecordName(id), false)
	if err != nil {
		return r, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxRecord+1))
	if err != nil || len(b) > MaxRecord || json.Unmarshal(b, &r) != nil || r.V != 1 || r.Provider != "agy" || r.ID != id || !canonical(id) || !r.DeliveryHeld || r.PID <= 0 || r.EndpointPID <= 0 || r.Starttime == "" || r.EndpointStarttime == "" || r.BootID == "" {
		return Record{}, errors.New("AGY endpoint record invalid")
	}
	return r, nil
}
func fresh(r Record) bool {
	age := float64(time.Now().UnixNano())/1e9 - r.Heartbeat
	return age >= 0 && age <= Stale.Seconds()
}
func (a *Adapter) ping(ctx context.Context, dest attached) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ep, err := a.paths.Endpoint(SocketName(dest.record.ID))
	if err != nil {
		return false
	}
	if revoke, ok := a.paths.(interface{ Revoke(platform.Endpoint) }); ok {
		defer revoke.Revoke(ep)
	}
	c, err := a.ipc.Dial(ctx, ep)
	if err != nil {
		return false
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return false
	}
	peer, err := c.Peer()
	if err != nil || peer != dest.server {
		return false
	}
	if _, err = io.WriteString(c, "{\"op\":\"ping\"}\n"); err != nil {
		return false
	}
	line, err := bufio.NewReader(io.LimitReader(c, MaxRecord+1)).ReadString('\n')
	if err != nil || len(line) > MaxRecord {
		return false
	}
	var result struct{ Conversation, Status, Delivery string }
	if json.Unmarshal([]byte(line), &result) != nil || result.Conversation != dest.record.ID || result.Status != "attached" || result.Delivery != "held" {
		return false
	}
	peer, err = c.Peer()
	return err == nil && peer == dest.server && a.process.Pin(ctx, dest.owner) == nil && a.process.Pin(ctx, dest.server) == nil
}
func (a *Adapter) valid(ctx context.Context, dest attached) bool {
	r, err := a.read(ctx, dest.root, dest.record.ID)
	return err == nil && r.PID == dest.record.PID && r.Starttime == dest.record.Starttime && r.BootID == dest.record.BootID && r.EndpointPID == dest.record.EndpointPID && r.EndpointStarttime == dest.record.EndpointStarttime && fresh(r) && !a.marker(ctx, dest.root, r.ID) && a.process.Pin(ctx, dest.owner) == nil && a.process.Pin(ctx, dest.server) == nil && a.ping(ctx, dest)
}
func (a *Adapter) Admission(s core.Session) provider.Admission {
	if runtime.GOOS != "linux" || s.Provider != "agy" || s.Delivery != "attached" {
		return provider.AdmitNone
	}
	a.mu.Lock()
	dest, ok := a.live[s.ID]
	a.mu.Unlock()
	if !ok || s.PID != dest.owner.PID || s.ProcessRef == nil || *s.ProcessRef != dest.owner || !a.valid(context.Background(), dest) {
		return provider.AdmitNone
	}
	return provider.AdmitBusy
}
func (a *Adapter) Discover(ctx context.Context) ([]core.Session, error) {
	rows := []core.Session{}
	if runtime.GOOS != "linux" || !a.config.AllowsProvider("agy") {
		return rows, nil
	}
	root, err := a.paths.StateDir()
	if err != nil {
		return nil, errors.New("AGY endpoint discovery unavailable")
	}
	entries, err := a.files.List(ctx, root)
	if err != nil {
		return nil, errors.New("AGY endpoint discovery unavailable")
	}
	candidates := map[string]attached{}
	owners := map[platform.ProcessRef]int{}
	for _, entry := range entries {
		name := entry.Name
		if entry.Directory || !strings.HasPrefix(name, "agy-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agy-"), ".json")
		if !canonical(id) || !a.config.Allows("agy", id) {
			continue
		}
		r, err := a.read(ctx, root, id)
		if err != nil || !fresh(r) || a.marker(ctx, root, id) {
			continue
		}
		owner, err := a.process.Lookup(ctx, r.PID)
		if err != nil || owner.Start != r.Starttime || owner.BootID != r.BootID {
			continue
		}
		server, err := a.process.Lookup(ctx, r.EndpointPID)
		if err != nil || server.Start != r.EndpointStarttime || server.BootID != r.BootID || owner.Principal != server.Principal {
			continue
		}
		dest := attached{record: r, owner: owner, server: server, root: root}
		if !a.valid(ctx, dest) {
			continue
		}
		candidates[id] = dest
		owners[owner]++
	}
	live := map[string]attached{}
	for id, dest := range candidates {
		if owners[dest.owner] != 1 {
			continue
		}
		live[id] = dest
		pin := dest.owner
		rows = append(rows, core.Session{Provider: "agy", ID: id, Name: dest.record.Name, PID: pin.PID, ProcessRef: &pin, Status: "unknown", Delivery: "attached", Admission: "busy"})
	}
	a.mu.Lock()
	a.live = live
	a.mu.Unlock()
	return rows, nil
}
func (*Adapter) Deliver(context.Context, core.Session, core.Message) (core.DeliveryResult, error) {
	detail := HeldDetail
	if runtime.GOOS != "linux" {
		detail = UnsupportedDetail
	}
	return core.DeliveryResult{Status: "held", Detail: detail}, nil
}
func (*Adapter) Busy(context.Context, core.Session) (provider.BusyState, error) {
	return provider.Unknown, nil
}
