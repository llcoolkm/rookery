# Go helper port

The Go helper is a native client of the existing Python hub's `/v2/rpc` HTTPS
protocol. Both languages can use one hub. The Python hub stays authoritative;
there is no hub rewrite in this plan. [contract.md](contract.md) defines the
Python 0.3.1 baseline, including compact MCP results and irreversible delivery
claims. Phase 1 is a contract and compilable skeleton, not a working helper.

## Phases and acceptance gates

1. **Contract and skeleton.** Extract source-cited normative rules, generate
   deterministic shared JSON vectors directly from Python, check them with Python
   unittest and load them from Go contract tests. Declare platform/provider/core
   boundaries and core wire types, build a version-printing command, add Linux
   and Windows CI. Preserve existing Python tests. Later behavioral parity tests
   may be skipped only with a phase-specific TODO.
2. **Go core and Linux parity.** Implement durable local state, request IDs,
   HTTPS transport, enrollment, delivery/read/expiry state machine and MCP using
   the shared vectors. Implement the Linux platform, including WSL detection,
   process identity, private files, locks and local peer checks. Port the Linux
   Codex/Claude routes needed for parity; prove against the live Python hub with
   isolated synthetic sessions. Verify reconnects, lost responses, crashes during
   dispatch, stale cursors, clock movement, inbox read suppression, resumed
   sessions and no automatic replay of uncertain attempts. Live-hub proof is a
   required gate, not implied by passing unit tests.
3. **Windows platform and Codex/Claude adapters.** Implement native IPC, process
   inspection, private state and Scheduled Task lifecycle. Reuse the same core
   and provider contracts; add platform-specific endpoint discovery through the
   platform interfaces. Verify on native Windows with actual harnesses, not just
   cross-compilation or WSL. Demonstrate session identity, busy admission, owner
   checks, process reuse refusal and unknown-outcome handling.
4. **AGY on Linux.** Port attached endpoints, credential confinement, process
   pinning, endpoint lifecycle/fencing and parsed agentapi acceptance. Verify
   parity for idle and busy conversations without credential borrowing or blind
   retries. Windows AGY remains deferred until its own native acceptance gate.
5. **Release 0.4.0.** Package native Linux and Windows binaries and document
   installation, migration and rollback. Keep the Python helper as fallback for
   one release. Release only after contract, native-platform, security and
   live-hub gates pass and review approves the artifacts. Python fallback does
   not mean concurrent claims by both helpers for the same sessions.

## Boundaries

`go/` is module `github.com/llcoolkm/rookery/go`, targeting Go 1.26. Use the
standard library first; every future dependency needs a concrete justification.
Phase 1 requires no third-party runtime dependencies.

- `core/` owns messages, envelopes, UUIDv7 request lifetime, hub protocol, local
  store abstraction, enrollment, delivery state and MCP. It uses platform
  interfaces, no direct OS commands, syscalls or hard-coded OS paths.
- `platform/` declares IPC, Process, Files, Service and Paths once. Implement
  Linux and Windows under OS build tags in `platform/linux` and
  `platform/windows`. Platform code owns command execution and native handles.
- `provider/` declares Identity(call metadata), Discover, Deliver and Busy state.
  `codex/`, `claude/` and Linux-only `agy/` use platform interfaces rather than
  OS packages. Harness protocol knowledge belongs here, not in platform code.
- `cmd/rookery` wires CLI, helper daemon and MCP together. Retain Python helper
  command names and flags as each command is implemented.

The Go helper uses its own private state directory. The proposed phase 2 default
is a `rookery-go` sibling of the platform state root, overridden by
`ROOKERY_GO_HOME` or explicit `--state-dir`; it must not reuse `ROOKERY_HOME`
implicitly. Exact platform roots remain a review question. It must never concurrently
open Python helper state or silently infer that its SQLite schema is compatible.
Hub compatibility is mandatory; shared on-disk compatibility is not. Configuration
import needs an explicit reviewed design before implementation.

## Windows security model

Create state directories, credentials, lock files and IPC objects with owner-only
DACLs. Avoid inherited access that grants other accounts access to state; check
the effective security descriptor when opening existing objects. Reject
symlink/reparse-point substitution and use handles with appropriate no-follow
semantics. Atomic replacement and durable writes must preserve restrictive ACLs.

IPC supports native AF_UNIX where usable and named pipes where required. Each
connection must validate peer SID and PID through the applicable native API,
then pin the process by creation/start identity and boot identity. Filesystem
ownership or an unguessable pipe name alone does not authenticate a live peer.
Fail closed when a transport cannot establish the required identity guarantees.
Pin identities across discovery, claim and submission; reject a reused PID or a
changed process. Toolhelp32/handle-based process inspection and Scheduled Task
management belong in the Windows platform implementation. Any use of
`x/sys/windows` requires a documented dependency justification.

Never place secrets in argv or logs. Transfer sensitive inputs through protected
handles or equivalent private channels and redact errors before they cross MCP
or hub boundaries. Do not borrow another harness session's tokens. Provider
metadata binds the caller but does not isolate mutually hostile processes under
the same OS account. Peer text carries work only within existing delegation;
it cannot expand user authorization.

## Open questions for review

- Choose exact independent Go state roots and environment-variable precedence on
  Linux, WSL and Windows; decide how explicit Python configuration import works
  without copying in-flight commands or enabling duplicate claims.
- Choose the Go durable store backend and transaction/flush semantics. The
  standard library has no SQLite driver; adding one must be justified against a
  bounded journal design, recovery complexity and Windows durability requirements.
- Verify actual Windows Codex/Claude endpoint transports and discoverable
  per-session process identity. Do not assume Linux socket paths or undocumented
  provider records apply on Windows.
- Establish whether Windows AF_UNIX can prove both peer SID and PID on every
  supported target; define named-pipe fallback and minimum supported Windows
  version from evidence before claiming parity.
- Define a stable boot identity on Windows and WSL, native no-follow/reparse
  behavior, and secure per-user Scheduled Task installation/update/removal.
- Decide whether Python provider entry-point plugins need a Go extension
  counterpart; built-in parity does not imply loading Python plugins into Go.
- Define exact version output, binary/artifact naming, release signing and the
  one-release Python fallback selection. Release automation and publication
  remain separate from phase 1.
- Agree an isolated live-hub acceptance procedure for phase 2 and native Windows
  provider test coverage for phase 3. Cross-build success is not runtime proof.
