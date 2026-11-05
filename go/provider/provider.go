// Package provider defines harness integration independent of the host OS.
// Implementations depend only on core models and platform facilities; they
// never call operating-system APIs or spawn processes directly.
package provider

import (
	"context"
	"github.com/llcoolkm/rookery/go/core"
	"github.com/llcoolkm/rookery/go/platform"
	"strings"
)

// Capabilities express real delivery guarantees; transport acceptance is not
// model receipt. BusyOptIn is applied once when the helper starts.
type Capabilities struct {
	Topology               string
	AcceptsBusy, BusyOptIn bool
}

// Config is supplied once at construction, never read from mutable globals.
// A nil BusyDelivery selects the Python default ["claude"]; an explicit empty
// slice disables opt-in busy delivery. Unknown providers cannot gain capability.
// Factories must snapshot the slice so later caller mutations cannot change policy.
type Config struct {
	CodexExecutable  string
	BusyDelivery     []string
	SessionAllowlist []string
}

// Factory constructs a provider with its startup busy_delivery policy and host.
// Built-in factories are implemented in their respective later port phases.
type Factory interface {
	New(Config, platform.Host) (Provider, error)
}

// Admission is per session: polling is None; Claude waiting/unknown is Idle
// even when busy delivery is enabled; AGY requires a live attached endpoint.
// Unknown/invalid admission must fail closed to None before publication or claim.
type Admission string

const (
	AdmitNone Admission = "none"
	AdmitIdle Admission = "idle"
	AdmitBusy Admission = "busy"
)

// BusyState includes idle, busy, waiting and unknown; unknown fails closed.
type BusyState string

const (
	Idle    BusyState = "idle"
	Busy    BusyState = "busy"
	Waiting BusyState = "waiting"
	Unknown BusyState = "unknown"
)

// Provider is the single harness boundary. Identity uses trusted call metadata
// or a pinned local ancestor, never caller-controlled tool arguments. Deliver
// returns held only if no native I/O has occurred. Discover never enrolls.
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Admission(core.Session) Admission
	Identity(context.Context, core.CallMetadata) (string, error)
	Discover(context.Context) ([]core.Session, error)
	Deliver(context.Context, core.Session, core.Message) (core.DeliveryResult, error)
	Busy(context.Context, core.Session) (BusyState, error)
}

// Allows limits daemon discovery before provider records or sockets are opened.
// Nil is reserved for explicitly unrestricted library callers; daemons pass []
// when no coexistence allowlist has been configured.
func (c Config) Allows(name, id string) bool {
	if c.SessionAllowlist == nil {
		return true
	}
	for _, target := range c.SessionAllowlist {
		if target == name+":"+strings.ToLower(id) {
			return true
		}
	}
	return false
}
func (c Config) AllowsProvider(name string) bool {
	if c.SessionAllowlist == nil {
		return true
	}
	for _, target := range c.SessionAllowlist {
		if strings.HasPrefix(target, name+":") {
			return true
		}
	}
	return false
}
