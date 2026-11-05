package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"github.com/llcoolkm/rookery/go/provider"
)

const testID = "00000000-0000-4000-8000-000000000001"

var ownerPin = platform.ProcessRef{PID: 42, Principal: "synthetic", Start: "100", BootID: "synthetic-boot"}
var serverPin = platform.ProcessRef{PID: 43, Principal: "synthetic", Start: "101", BootID: "synthetic-boot"}

type fakeFile struct{ *bytes.Reader }

func (*fakeFile) Write([]byte) (int, error) { panic("provider wrote disk") }
func (*fakeFile) Sync() error               { return nil }
func (*fakeFile) Close() error              { return nil }

type fakeFiles struct {
	data    map[string][]byte
	openErr map[string]error
	opened  []string
}

func (*fakeFiles) PrivateState(context.Context, string) (string, error) {
	panic("provider created state")
}
func (*fakeFiles) EnsurePrivate(context.Context, string, bool) error { return nil }
func (f *fakeFiles) OpenNoFollow(_ context.Context, _ string, n string, _ bool) (platform.File, error) {
	f.opened = append(f.opened, n)
	if err := f.openErr[n]; err != nil {
		return nil, err
	}
	b, ok := f.data[n]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &fakeFile{bytes.NewReader(b)}, nil
}
func (*fakeFiles) AtomicWrite(context.Context, string, string, []byte) error {
	panic("provider wrote disk")
}
func (f *fakeFiles) List(context.Context, string) ([]platform.Entry, error) {
	r := []platform.Entry{}
	for n, b := range f.data {
		r = append(r, platform.Entry{Name: n, Size: int64(len(b))})
	}
	return r, nil
}
func (*fakeFiles) Remove(context.Context, string, string) error { panic("provider removed file") }
func (*fakeFiles) AdvisoryLock(context.Context, string, string, bool) (platform.Lock, error) {
	panic("provider locked")
}

type fakeProcess struct{ pins map[int]platform.ProcessRef }

func (*fakeProcess) Current(context.Context) (platform.ProcessRef, error) {
	panic("identity used process")
}
func (p *fakeProcess) Lookup(_ context.Context, pid int) (platform.ProcessRef, error) {
	r, ok := p.pins[pid]
	if !ok {
		return r, errors.New("dead")
	}
	return r, nil
}
func (p *fakeProcess) Pin(_ context.Context, r platform.ProcessRef) error {
	if p.pins[r.PID] != r {
		return errors.New("changed")
	}
	return nil
}
func (*fakeProcess) Ancestors(context.Context, platform.ProcessRef) ([]platform.ProcessRef, error) {
	panic("provider inspected ancestors")
}
func (*fakeProcess) CommandLine(context.Context, platform.ProcessRef) ([]string, error) {
	panic("provider scraped command line")
}

type fakePaths struct{}

func (fakePaths) StateDir() (string, error)                    { return "synthetic state", nil }
func (fakePaths) Endpoint(n string) (platform.Endpoint, error) { return platform.Endpoint(n), nil }
func (fakePaths) HarnessEndpoint(context.Context, string, platform.File, string, platform.ProcessRef) (platform.Endpoint, error) {
	panic("provider opened harness address")
}
func (fakePaths) ProviderData(string) (string, error) { panic("provider read harness data") }
func (fakePaths) IsWSL() bool                         { return false }

type fakeConn struct {
	*bytes.Reader
	writes *bytes.Buffer
	peer   platform.ProcessRef
}

func (c *fakeConn) Write(b []byte) (int, error)        { return c.writes.Write(b) }
func (*fakeConn) Close() error                         { return nil }
func (*fakeConn) SetDeadline(time.Time) error          { return nil }
func (*fakeConn) SetReadDeadline(time.Time) error      { return nil }
func (*fakeConn) SetWriteDeadline(time.Time) error     { return nil }
func (c *fakeConn) Peer() (platform.ProcessRef, error) { return c.peer, nil }

type fakeIPC struct {
	calls   int
	peer    platform.ProcessRef
	reply   string
	replies map[platform.Endpoint]string
	err     error
	writes  bytes.Buffer
}

func (*fakeIPC) Listen(context.Context, platform.Endpoint) (platform.Listener, error) {
	panic("provider listened")
}
func (i *fakeIPC) Dial(_ context.Context, ep platform.Endpoint) (platform.Connection, error) {
	i.calls++
	if i.err != nil {
		return nil, i.err
	}
	reply := i.reply
	if i.replies != nil {
		reply = i.replies[ep]
	}
	return &fakeConn{bytes.NewReader([]byte(reply)), &i.writes, i.peer}, nil
}

type fakeExec struct{}

func (fakeExec) Exec(context.Context, platform.Spec) (platform.ExecResult, error) {
	panic("AGY executed a process")
}
func fixture(t *testing.T) (*Adapter, *fakeFiles, *fakeProcess, *fakeIPC, Record) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux AGY only")
	}
	r := Record{V: 1, Provider: "agy", ID: testID, Name: "Synthetic AGY", PID: 42, Starttime: "100", BootID: "synthetic-boot", EndpointPID: 43, EndpointStarttime: "101", Heartbeat: float64(time.Now().UnixNano())/1e9 - 1, DeliveryHeld: true}
	b, _ := json.Marshal(r)
	f := &fakeFiles{data: map[string][]byte{RecordName(testID): b}, openErr: map[string]error{}}
	p := &fakeProcess{map[int]platform.ProcessRef{42: ownerPin, 43: serverPin}}
	i := &fakeIPC{peer: serverPin, reply: `{"conversation":"` + testID + `","status":"attached","delivery":"held"}` + "\n"}
	a, _ := New(provider.Config{}, platform.Host{Paths: fakePaths{}, Files: f, Process: p, IPC: i, Exec: fakeExec{}})
	return a, f, p, i, r
}
func TestPerCallIdentity(t *testing.T) {
	a, _ := New(provider.Config{}, platform.Host{Exec: fakeExec{}})
	if runtime.GOOS != "linux" {
		if _, err := a.Identity(context.Background(), nil); err == nil {
			t.Fatal("Windows supported")
		}
		return
	}
	for _, id := range []string{testID, "00000000-0000-4000-8000-000000000002"} {
		b, _ := json.Marshal(id)
		got, err := a.Identity(context.Background(), core.CallMetadata{"antigravity.google/conversation_id": b})
		if err != nil || got != "agy:"+id {
			t.Fatal(got, err)
		}
	}
	for _, raw := range []string{"", `null`, `12`, `{}`, `"bad"`, `"ABCDEF00-0000-4000-8000-000000000001"`} {
		_, err := a.Identity(context.Background(), core.CallMetadata{"antigravity.google/conversation_id": json.RawMessage(raw)})
		if err == nil || err.Error() != core.AGYIdentityError {
			t.Fatal(raw, err)
		}
	}
}
func TestAttachedAdmissionAndHeldDelivery(t *testing.T) {
	a, _, _, ipc, _ := fixture(t)
	rows, err := a.Discover(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	s := rows[0]
	if s.Status != "unknown" || s.Delivery != "attached" || a.Admission(s) != provider.AdmitBusy {
		t.Fatal(s)
	}
	calls := ipc.calls
	result, err := a.Deliver(context.Background(), s, core.Message{Body: "CONFIDENTIAL BODY"})
	if err != nil || result.Status != "held" || result.Detail != HeldDetail || ipc.calls != calls || strings.Contains(ipc.writes.String(), "CONFIDENTIAL BODY") {
		t.Fatal(result, err, ipc.calls)
	}
	s.Delivery = "polling"
	if a.Admission(s) != provider.AdmitNone {
		t.Fatal("polling admitted")
	}
}
func TestDiscoveryRejectsStaleUnsafeAndChangedEndpoints(t *testing.T) {
	cases := map[string]func(*fakeFiles, *fakeProcess, *fakeIPC, *Record){
		"stale":            func(_ *fakeFiles, _ *fakeProcess, _ *fakeIPC, r *Record) { r.Heartbeat -= 30 },
		"future":           func(_ *fakeFiles, _ *fakeProcess, _ *fakeIPC, r *Record) { r.Heartbeat += 30 },
		"wrong generation": func(_ *fakeFiles, _ *fakeProcess, _ *fakeIPC, r *Record) { r.Starttime = "other" },
		"endpoint reused": func(_ *fakeFiles, p *fakeProcess, _ *fakeIPC, _ *Record) {
			v := p.pins[43]
			v.Start = "reused"
			p.pins[43] = v
		},
		"foreign peer": func(_ *fakeFiles, _ *fakeProcess, i *fakeIPC, _ *Record) { i.peer.Principal = "foreign" },
		"wrong conversation": func(_ *fakeFiles, _ *fakeProcess, i *fakeIPC, _ *Record) {
			i.reply = `{"conversation":"other","status":"attached","delivery":"held"}` + "\n"
		},
		"not held endpoint": func(_ *fakeFiles, _ *fakeProcess, _ *fakeIPC, r *Record) { r.DeliveryHeld = false },
		"unsafe record": func(f *fakeFiles, _ *fakeProcess, _ *fakeIPC, _ *Record) {
			f.openErr[RecordName(testID)] = errors.New("unsafe")
		},
		"linked fence": func(f *fakeFiles, _ *fakeProcess, _ *fakeIPC, _ *Record) {
			f.openErr[MarkerName(testID, "superseded")] = errors.New("linked")
		},
		"detached": func(f *fakeFiles, _ *fakeProcess, _ *fakeIPC, _ *Record) {
			f.data[MarkerName(testID, "detached")] = []byte("{}")
		},
		"superseded": func(f *fakeFiles, _ *fakeProcess, _ *fakeIPC, _ *Record) {
			f.data[MarkerName(testID, "superseded")] = []byte("{}")
		},
		"no listener": func(_ *fakeFiles, _ *fakeProcess, i *fakeIPC, _ *Record) { i.err = errors.New("not listening") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a, f, p, i, r := fixture(t)
			change(f, p, i, &r)
			f.data[RecordName(testID)], _ = json.Marshal(r)
			rows, err := a.Discover(context.Background())
			if err != nil || len(rows) != 0 {
				t.Fatal(rows, err)
			}
		})
	}
}
func TestAdmissionRechecksAfterDiscovery(t *testing.T) {
	a, f, p, _, _ := fixture(t)
	rows, _ := a.Discover(context.Background())
	s := rows[0]
	f.data[MarkerName(testID, "detached")] = []byte("{}")
	if a.Admission(s) != provider.AdmitNone {
		t.Fatal("detached endpoint admitted")
	}
	delete(f.data, MarkerName(testID, "detached"))
	delete(p.pins, 43)
	if a.Admission(s) != provider.AdmitNone {
		t.Fatal("dead endpoint admitted")
	}
}
func TestAllowlistBeforeOpeningRecords(t *testing.T) {
	a, f, _, i, _ := fixture(t)
	a.config.SessionAllowlist = []string{}
	rows, err := a.Discover(context.Background())
	if err != nil || len(rows) != 0 || len(f.opened) != 0 || i.calls != 0 {
		t.Fatal(rows, err, f.opened, i.calls)
	}
}
func TestAmbiguousSameProcessConversationsAreWithheld(t *testing.T) {
	a, f, _, ipc, r := fixture(t)
	r.ID = "00000000-0000-4000-8000-000000000002"
	f.data[RecordName(r.ID)], _ = json.Marshal(r)
	ipc.replies = map[platform.Endpoint]string{platform.Endpoint(SocketName(testID)): ipc.reply,
		platform.Endpoint(SocketName(r.ID)): `{"conversation":"` + r.ID + `","status":"attached","delivery":"held"}` + "\n"}
	rows, err := a.Discover(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
