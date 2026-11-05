// Package platform defines the only host facilities available to core and providers.
// Implementations must fail closed when ownership or process identity cannot be
// established. Paths are opaque: callers never construct OS-specific paths.
package platform

import (
	"context"
	"io"
	"net"
)

// Identity is an authenticated local peer. Principal is a decimal UID on Linux
// or a SID on Windows. PID alone must never establish identity.
type Identity struct {
	Principal string
	PID       int
}

// Connection carries credentials obtained from the OS, never from payload data.
type Connection interface {
	net.Conn
	Peer() (Identity, error)
}

// Listener returns authenticated connections, rejecting foreign principals.
type Listener interface {
	Accept(context.Context) (Connection, error)
	Close() error
}

// IPC supports AF_UNIX on Linux and Windows, or secured named pipes on Windows.
// Listen must protect the endpoint before accepting any connection.
type IPC interface {
	Listen(context.Context, string) (Listener, error)
	Dial(context.Context, string) (Connection, error)
}

// ProcessRef pins a PID to its start time and machine boot identity. Start is an
// opaque platform value, not wall time; lookup and every use must revalidate it.
type ProcessRef struct {
	PID                      int
	Principal, Start, BootID string
}

// Process supplies process inspection without exposing commands or syscalls.
// Ancestors must detect cycles, PID reuse, exits and foreign owners.
type Process interface {
	Current(context.Context) (ProcessRef, error)
	Lookup(context.Context, int) (ProcessRef, error)
	Pin(context.Context, ProcessRef) error
	Ancestors(context.Context, ProcessRef) ([]ProcessRef, error)
	CommandLine(context.Context, ProcessRef) ([]string, error)
}

// File is an already verified regular file; Sync persists its contents.
type File interface {
	io.ReadWriteSeeker
	io.Closer
	Sync() error
}

// Lock remains held until Close. Implementations reject linked lock files.
type Lock interface{ io.Closer }

// Entry describes a direct child; names are relative, never arbitrary paths.
type Entry struct {
	Name      string
	Size      int64
	Directory bool
}

// Files confines relative names to a verified owner-only state directory.
// Every operation rejects symlinks/reparse points and unsafe ancestry. Atomic
// writes persist contents and directory metadata before reporting success.
// Private means mode 0700/0600 on Linux and an owner-only DACL on Windows.
type Files interface {
	PrivateState(context.Context, string) (string, error)
	EnsurePrivate(context.Context, string, bool) error
	OpenNoFollow(context.Context, string, string, bool) (File, error)
	AtomicWrite(context.Context, string, string, []byte) error
	List(context.Context, string) ([]Entry, error)
	Remove(context.Context, string, string) error
	AdvisoryLock(context.Context, string, string, bool) (Lock, error)
}

// ServiceSpec contains no bearer credentials. Arguments may contain only public
// options; configuration and credentials are read from protected files.
type ServiceSpec struct {
	Executable string
	Arguments  []string
	StateDir   string
}
type ServiceStatus struct {
	Installed, Running bool
	Detail             string
}

// Service manages a per-user systemd unit or Windows Scheduled Task.
type Service interface {
	Install(context.Context, ServiceSpec) error
	Uninstall(context.Context) error
	Status(context.Context) (ServiceStatus, error)
}

// Paths resolves platform defaults. Go state is separate from Python state.
// ProviderData locates harness-owned data without exposing OS paths in core.
type Paths interface {
	StateDir() (string, error)
	Endpoint(string) (string, error)
	ProviderData(string) (string, error)
	IsWSL() bool
}

// Host groups one consistent implementation of the platform boundary.
type Host struct {
	IPC     IPC
	Process Process
	Files   Files
	Service Service
	Paths   Paths
}
