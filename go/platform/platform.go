// Package platform defines the only host facilities available to core and providers.
// Implementations must fail closed when ownership or process identity cannot be
// established. Paths and endpoint handles are opaque: callers never construct
// OS-specific paths.
package platform

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrControlEndpointUnsafe reports a refused control endpoint without exposing
// its address, ownership, or process details to portable provider code.
var ErrControlEndpointUnsafe = errors.New("unsafe control endpoint")

// Connection carries OS credentials captured and pinned before Accept/Dial return.
// Peer returns that original ProcessRef, never a fresh lookup of a reused PID.
// Process exit/identity change invalidates use; inability to pin fails closed.
type Connection interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Peer() (ProcessRef, error)
}

// Listener authenticates clients and rejects foreign principals.
type Listener interface {
	Accept(context.Context) (Connection, error)
	Close() error
}

// Endpoint is an opaque host-issued handle, not a path. Implementations reject
// unissued handles. Harness addresses enter only through Paths.HarnessEndpoint.
type Endpoint string

// IPC supports AF_UNIX or secured Windows pipes. Listen protects the endpoint
// before accepting. Dial authenticates the server: Linux uses SO_PEERCRED plus
// socket owner/no-symlink checks (with a verified single-link exception for the
// fixed Codex control rendezvous); Windows pipes use GetNamedPipeServerProcessId
// plus token SID and endpoint owner/DACL. Capture a pinned ProcessRef in both
// directions and compare the expected principal and any expected harness server.
// Revalidate owner/no-follow and identity at connect to prevent substitution.
// Unsupported authentication fails closed; a path or pipe name is not identity.
type IPC interface {
	Listen(context.Context, Endpoint) (Listener, error)
	Dial(context.Context, Endpoint) (Connection, error)
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

// Spec describes direct native execution. Path is an absolute platform-validated
// executable path; no shell or fallback. Windows refuses .cmd/.bat regardless of
// case, shell interpreters and shell/PATHEXT resolution. Windows args contain
// public options only: peer text never enters Windows argv (also limited to 32767
// characters); use Stdin or a verified private file. Timeout/MaxOutput are positive.
type Spec struct {
	Path      string
	Args      []string
	Stdin     []byte
	Timeout   time.Duration
	MaxOutput int64
}

// ExecState distinguishes proven pre-start failure from known completion and a
// command that may have started but whose completion cannot be established.
type ExecState uint8

const (
	// Uncertain is the safe zero value: never automatically retry this outcome.
	Uncertain ExecState = iota
	NotStarted
	Completed
)

// ExecResult accompanies errors too. Timeout, cancellation, output limit, lost
// handle or wait failure after start is Uncertain unless completion is positively
// established. A known nonzero exit is Completed. Stdout/Stderr together are
// bounded by Spec.MaxOutput; output is untrusted.
type ExecResult struct {
	State          ExecState
	ExitCode       int // meaningful only when State == Completed
	Stdout, Stderr []byte
	Truncated      bool
}

// Executor owns process creation, bounded I/O, timeout and cleanup. NotStarted
// requires proof that no command could have run. Never replay Uncertain delivery
// or send raw errors to the hub: redact host paths, process/session data, secrets.
type Executor interface {
	Exec(context.Context, Spec) (ExecResult, error)
}

// ErrCommittedNotDurable means replacement is visible, but its directory fsync
// failed. Admission may report queued_local; delivery claims must fail closed.
var ErrCommittedNotDurable = errors.New("local record committed but durability unconfirmed")

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
	// Optional target account and provider execution environment. Platform code
	// resolves omitted values; credentials never belong in these fields.
	Account    string
	Home       string
	SearchPath string
	Executable string
	Arguments  []string
	StateDir   string
}
type ServiceStatus struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Detail    string `json:"detail"`
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
	Endpoint(string) (Endpoint, error)
	// HarnessEndpoint imports an address parsed by trusted provider code from
	// record, a still-open Files.OpenNoFollow handle under ProviderData(provider).
	// Verify that this host issued the record handle, its provider-root provenance,
	// owner and no-follow ancestry; reject arbitrary/unverified handles. Provider
	// code must derive address and expected server from that same verified record,
	// never tool/peer payloads. Harness-specific record parsing stays in provider.
	// Verify endpoint/ancestry ownership and no-follow rules, bind server, and
	// issue an opaque handle. Absolute addresses are allowed only as input here;
	// Files.OpenNoFollow continues to use a confined relative record name.
	HarnessEndpoint(ctx context.Context, provider string, record File, address string, server ProcessRef) (Endpoint, error)
	ProviderData(string) (string, error)
	IsWSL() bool
}

// Host groups one consistent implementation of the platform boundary.
type Host struct {
	IPC     IPC
	Exec    Executor
	Process Process
	Files   Files
	Service Service
	Paths   Paths
}

// ClaudeRecordAuthenticator is an optional native-Windows capability. The
// provider invokes it on the same open, verified session record used for endpoint
// discovery. It must prove a documented token source belongs to that record and
// exact session; unsupported or unverified formats return an error. The provider
// sends {"type":"auth","token":TOKEN} as the first line, never in argv or logs.
// No fallback to a helper environment token or another session's token is allowed.
type ClaudeRecordAuthenticator interface {
	ClaudeAuthToken(context.Context, File) (string, error)
}
