// Package provider defines harness integration independent of the host OS.
// Implementations depend only on core models and platform facilities; they
// never call operating-system APIs or spawn processes directly.
package provider

import (
	"context"
	"github.com/llcoolkm/rookery/go/core"
)

// Capabilities express real delivery guarantees; transport acceptance is not
// model receipt. BusyOptIn is applied once when the helper starts.
type Capabilities struct {
	Topology               string
	AcceptsBusy, BusyOptIn bool
}

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
	Identity(context.Context, core.CallMetadata) (string, error)
	Discover(context.Context) ([]core.Session, error)
	Deliver(context.Context, core.Session, core.Message) (core.DeliveryResult, error)
	Busy(context.Context, core.Session) (BusyState, error)
}
