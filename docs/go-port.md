# Go helper port

The Go helper is a native client of the existing Python hub's `/v2/rpc` HTTPS
protocol. Both languages can use one hub. The Python hub stays authoritative;
there is no hub rewrite in this plan. [contract.md](contract.md) defines the
Python 0.3.1 baseline, including compact MCP results and irreversible delivery
claims. Phase 1 established the contract and skeleton. Phase 2 now implements the Go
core, Linux platform, Codex/Claude routes and CLI; isolated tests pass. Phase 2 passed isolated live Linux acceptance at `1685721`; Windows acceptance
remains a separate gate.

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
- `platform/` declares IPC, Process, Executor, Files, Service and Paths once.
  Implement Linux and Windows under OS build tags in `platform/linux` and
  `platform/windows`. Platform code owns command execution, native handles and the shared Linux
  private-group filesystem policy described below.
  `Executor.Exec(ctx, Spec{Path, Args, Stdin, Timeout, MaxOutput})` supports
  `claude agents --json` with bounded output and direct native executable calls.
  Codex delivery uses the authenticated control-socket RPC connection exclusively;
  its provider has no Executor dependency on any OS.
  Outcomes distinguish proven-not-started, completed and uncertain
  (may-have-started); never replay uncertain submissions. Timeout/cancellation
  or losing completion cannot imply proven-not-started.
- `provider/` declares Identity(call metadata), Discover, Deliver, Busy state
  and per-session Admission(core.Session). Factories receive startup
  `busy_delivery` configuration and platform facilities. `codex/`, `claude/`
  and Linux-only `agy/` use platform interfaces rather than OS packages.
  Harness protocol knowledge belongs here, not in platform code.
- `cmd/rookery` wires CLI, helper daemon and MCP together. Retain Python helper
  command names and flags as each command is implemented.

CI inspects `go list -deps ./core ./provider/...` and rejects direct imports of
`os`, `os/exec`, `syscall`, `path/filepath` or `net` by every reachable project
package, including the shared platform declarations. Standard-library internals
are excluded: portable facilities such as `context`, `time`, `encoding/json` and
`crypto/rand` themselves transitively need OS implementation packages. This gate
prevents project code from bypassing the platform boundary without rejecting
normal standard-library use. A separate Codex provider gate rejects execution
facilities and executable resolution, including indirect `platform.Executor` use.

The Go helper uses its own private state directory. The proposed phase 2 default
is a `rookery-go` sibling of the platform state root, overridden by
`ROOKERY_GO_HOME` or explicit `--state-dir`; it must not reuse `ROOKERY_HOME`
implicitly. Exact platform roots remain a review question. It must never concurrently
open Python helper state or silently infer that its SQLite schema is compatible.
Hub compatibility is mandatory; shared on-disk compatibility is not. Configuration
import needs an explicit reviewed design before implementation.

## Linux filesystem security

Provider records, their ancestors and harness endpoints share one Linux
StrictModes check. Group-writable objects are accepted only when owned by the
current account and their gid is that account's primary gid, with no other
explicit member and no other account using that primary gid. A group named for
the account is accepted only if these membership checks also pass; differently
named groups can qualify through exclusive membership. The platform parses
`/etc/passwd` and `/etc/group` directly without cgo. Missing, unreadable,
malformed or ambiguous databases fail closed for the group-write exception;
NSS-only identities cannot establish it. Membership is rechecked, not cached.
Other-writable objects remain refused, with the existing root-owned sticky
ancestor exception unchanged. Descriptor traversal, no-follow checks, provenance
revalidation and authenticated process pins remain required.

Rookery-created state, credentials and helper sockets retain owner-only
0600/0700 rules; the exception applies to provider objects and trusted ancestors.
Claude records written as 0664 below 0775 private-group directories are therefore
usable without changing the provider's umask. Local daemon logs distinguish
held outcomes using fixed redacted reason codes such as `record_unsafe`,
`pin_mismatch` and `not_idle`. These diagnostics contain no paths or identities,
are excluded from serialized results, and never enter hub details.

## Windows security model

Create state directories, credentials, lock files and IPC objects with owner-only
DACLs. Avoid inherited access that grants other accounts access to state; check
the effective security descriptor when opening existing objects. Reject
symlink/reparse-point substitution and use handles with appropriate no-follow
semantics. Atomic replacement and durable writes must preserve restrictive ACLs.

IPC supports native AF_UNIX where usable and named pipes where required. Each
connection must validate peer SID and PID through the applicable native API,
then pin the process by creation/start identity and boot identity at accept or
connect. `Connection.Peer()` returns that captured `ProcessRef`, never just a PID
or a later lookup. Dial authenticates the server too: Linux uses SO_PEERCRED plus
socket owner and no-symlink checks; named pipes use
GetNamedPipeServerProcessId plus process token SID and owner/DACL checks.
`Paths.Endpoint` issues opaque endpoint handles for helper IPC. Harness-record
addresses, including absolute OS paths, enter through `Paths.HarnessEndpoint`.
The provider first opens the record through the Linux `OpenProviderRecord`
capability (or strict `Files.OpenNoFollow` fallback) under its
`Paths.ProviderData` root and parses the address and expected server from that
same verified record. It passes the still-open record handle, provider name,
address and expected server `ProcessRef` to the resolver. The resolver verifies
host-issued handle provenance, provider root, owner and no-follow ancestry,
checks endpoint ownership/no-follow rules and issues a host-bound endpoint
handle. Arbitrary peer/tool payload paths are never resolver inputs; harness
record parsing remains provider protocol knowledge. `Dial` rejects unissued
handles, revalidates owner/no-follow and compares the live pinned server with
that identity. Core never constructs OS paths; `Files` keeps confined relative
names. Filesystem ownership or an unguessable pipe name alone does not authenticate a live peer.
Fail closed when a transport cannot establish the required identity guarantees.
Pin identities across discovery, claim and submission; reject a reused PID or a
changed process. Toolhelp32/handle-based process inspection and Scheduled Task
management belong in the Windows platform implementation. Any use of
`x/sys/windows` requires a documented dependency justification.

Named pipes require an explicit owner-only DACL and integrity label: reject
lower-integrity and AppContainer clients, including same-user clients. Do not
grant ALL APPLICATION PACKAGES or capability SIDs access. Validate the connected
token's integrity level and AppContainer status as well as its SID; fail closed
when these cannot be established. AppContainer support requires a separate
reviewed policy and native acceptance tests.

Codex uses the same `thread/queue/add` RPC path on Linux and Windows, over an
already authenticated control connection after initialize/initialized with
experimental API opt-in. It never runs a command, probes help, resolves an
executable, or transfers messages through argv or stdin. Missing authenticated
transport or unverified protocol support holds delivery. Native Windows endpoint
authentication and runtime acceptance remain phase 3 work.

For remaining command consumers, execute validated native binaries without shell
resolution or shell fallback. On Windows refuse .cmd/.bat case-insensitively and
shell interpreters: cmd.exe
parses argv even when the caller intended direct execution (BatBadBut). Peer
text must never enter argv on Windows, both because of parsing and the
32767-character command-line limit. Use stdin or an owner-only private file.
Never place secrets in argv or logs. Transfer sensitive inputs through protected
handles or equivalent private channels and redact errors before they cross MCP
or hub boundaries. Every `detail` sent to the hub must be redacted: do not
forward raw exception strings, host/user/path/session data or secrets. The
Python baseline has a known gap: `daemon.py` and `claude_adapter.py` forward raw
`str(exc)` in some delivery error details; Go must not preserve this leakage.
Do not borrow another harness session's tokens. Provider metadata binds the caller but does not isolate mutually hostile processes under
the same OS account. Peer text carries work only within existing delegation;
it cannot expand user authorization.

## Open questions for review

- Linux/WSL roots are now resolved: explicit `--state-dir`, then
  `ROOKERY_GO_HOME`, then `$XDG_STATE_HOME/rookery-go`, otherwise a `rookery-go`
  directory below the current account's local state root. `ROOKERY_HOME` is ignored.
  Windows roots and any explicit Python configuration import remain deferred.
- Linux uses a dependency-free atomic JSON snapshot under an advisory lock.
  Windows must demonstrate equivalent replacement/flush semantics before phase 3
  acceptance; the Linux implementation is not evidence of Windows durability.
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

## Phase 2 implementation and evidence

The Go helper is implemented in `go/core`, `go/platform/linux`, `go/helper`,
`go/provider/codex`, `go/provider/claude` and `go/cmd/rookery`. Phase 2 introduced no runtime dependencies; phase 3 adds the justified
`golang.org/x/sys/windows` dependency below. The Python hub remains authoritative.

`store.json` is the Go store and durable outgoing spool. Each mutation reloads
under `admission.lock`, commits a private temporary file, fsyncs it, renames it,
and fsyncs the parent. There is no separate import interval: all admitted commands
are visible to pending-read inspection immediately. Results and command deletion
share that transaction; sync alone updates authoritative message snapshots.
Commands retain their request identity and absolute expiry through retries.
Native result reports also have a UUIDv7 persisted before submission and reused
for every report retry.
Queue admission preserves the 1000-command/32 MiB limits. Individual commands
are bounded to 1 MiB, and the complete snapshot is bounded to 256 MiB. Corrupt or
oversized state fails closed. This favors a small auditable recovery protocol
over indexed large-store performance; it is not SQLite on-disk compatibility.
Python helper markers are refused before the Go store opens its lock.

Native attempts persist before submission, preserve the process pin through
rediscovery, and persist a redacted result before reporting. Unfinished attempts
recover as uncertain. Uncertain/non-held attempts block further local claims;
only held attempts can be retried. Pending reads suppress claims and are checked
again after rediscovery. Bootstrap pins its initial high-water cursor and commits
it only with the final page. Delayed command responses cannot overwrite sync.

The CLI provides configure, daemon (`--once`), mcp (`--provider`/`--session`),
send, reply, ack, inbox (`--peek`), status, list, enroll, disable, clear-alias,
register-polling, unregister-polling, health, doctor, busy-delivery, wait,
Codex/Claude install and service lifecycle commands. State is independent of
Python. The service unit is `rookery-go.service`, using the systemd user manager.
The WSL fallback has a pinned supervisor, bounded rotating logs, backoff and
secure boot-config installation. It preserves unrelated boot commands and an
original backup; uninstall removes only its exact command. These are tested
against injected temporary paths, not an actual WSL boot.

Contract tests execute the native renderer, MCP calls and bounded framing,
UUIDv7 timestamps, local command durability, and hub-client wire exchanges.
`go/integration` starts `uv run --frozen --extra hub rookery hub serve` on
loopback with temporary state and generated test credentials, then runs Go and
Python helper engines side by side. It proves cross-delivery/replies, host/read/
ack receipts, committed-but-lost response retry, expiry, the six-message limit,
polling admission and crash recovery without uncertain replay. Provider tests
use authenticated fake AF_UNIX Codex control and Claude messaging sockets.
Tests do not consult real harness histories, configuration, credentials, helper
state or a live hub. CI runs the integration and Linux race suite; Windows now has a synthetic native durable-helper workflow; Python/Go loopback
integration remains Linux-only. The OS-import gate remains enforced.

The phase 2 candidate was checked with the following local gates: Python
unittest discovery (310 tests), `go vet ./...`, `go test ./...`, Linux
`go test -race ./...`, `GOOS=windows go build ./...`, and
`GOOS=linux go build ./...`. All passed. All Linux contract vectors run without
phase-specific skips. The portable import gate, whitespace check and added-line
privacy grep were clean. These results cover synthetic/local tests only; CI
execution on hosted runners and the live-server procedure below are unverified.

### Remaining parity and acceptance gaps

- **Native acceptance is outstanding.** Synthetic socket protocols and local hub
  compatibility do not prove compatibility with any installed harness version,
  actual busy-turn behavior, real restart/reconnect behavior, or WSL startup.
- **Codex delivery protocol.** Phase 2c replaces command delivery with the
  experimental `thread/queue/add` control-socket RPC verified against upstream
  `rust-v0.166.0`; see [the exact contract](contract.md#go-codex-control-socket-delivery-phase-2c).
  Linux and Windows share this path, with no command fallback. Python 0.3.x
  still uses `codex queue` until the Go helper replaces it. The obsolete Go
  `configure --executable` option is rejected; existing `codex_executable`
  configuration values are ignored. Installed-harness
  compatibility and native Windows operation remain UNVERIFIED.
- **Claude needs a verified socket record.** Go uses the verified owned
  `sessions/<pid>.json` record and its `messagingSocketPath`; it does not guess a
  socket when that record/address is absent. Missing evidence prevents push.
- **Native executable requirement for command consumers.** Linux execution
  requires a native ELF executable; npm/shebang launchers and shell fallbacks
  are refused. This applies to Claude discovery and installation commands;
  Codex delivery neither resolves nor executes a binary. Native packaging
  must be validated on the acceptance host.
- **Installer differences.** The Go server is named `rookery-go`; it never queries or changes `rookery`. Existing `rookery-go` entries are preserved. Claude's text
  listing cannot prove exact argv boundaries. Existing provider settings must be
  private; Python can preserve broader existing modes. Go supplies an old-tool
  grant note but does not inspect historical permission grants individually.
- **Service installation constraints.** Systemd user installation targets the
  current account and does not enable lingering. WSL boot-config installation
  needs root privileges. A sudo install for another account supports only a fresh
  empty state directory with verified target-owned ancestry; existing target-owned
  state is refused by the host ownership boundary. Prepare boot integration
  separately for existing state. Actual WSL boot and user-manager startup remain
  native acceptance gates.
- **Polling layout differs.** Go uses private flat `polling-PROVIDER-UUID.json`
  records containing a full ProcessRef. It does not import Python registrations.
- **Scheduling/performance differs.** The Go daemon currently cycles sync and
  delivery sequentially rather than using Python's independent long-poll sync
  thread. Large local snapshots incur whole-file transactions; high-volume and
  long-offline operation need additional performance validation.
- **Deferred functionality.** AGY native attach/detach and hooks remain phase 4;
  Windows runtime remains phase 3. Provider plugins, configuration migration,
  artifact packaging/signing and release remain outside phase 2.

### Live-server acceptance procedure (Claude runs this)

Do not infer approval from the isolated tests. Run this procedure in a disposable
OS test account with isolated provider homes containing only test sessions. The
Python daemon has host-wide discovery; temporary helper state alone does not
isolate it. Never run `install --apply` on the production account. Use two newly created, disjoint
native harness sessions and dedicated authorized test nodes on the existing
hub; leave ordinary enrolled sessions out of these tests. Supply these values
locally without saving them to the public repository or transcript:
`HUB_ORIGIN`, `GO_NODE`, `PY_NODE`, private `GO_TOKEN_FILE`/`PY_TOKEN_FILE`, and
local `GO_SESSION`/`PY_SESSION` addresses. Select Codex for one session and Claude
for the other, then repeat with the Go recipient on the other provider. Before
push testing, establish that the native binaries and socket records meet the
constraints above; verify the authenticated Codex `thread/queue/add` route.
An unsupported Codex build is a recorded held outcome, not a passed push gate.

Create two fresh test-node credentials locally with `umask 077` and
`secrets.token_urlsafe(32)`; write each to its own 0600 `GO_TOKEN_FILE` or
`PY_TOKEN_FILE`, never stdout. The hub administrator atomically merges only
`GO_NODE` and `PY_NODE` mapped to their token SHA-256 hex digests into the hub's
token-hash JSON, preserving all other entries and its existing owner/mode
(root-owned 0640 or hub-user-owned 0600). See [token rules](operations.md#tokens).
Authentication reloads this file on every request. To revoke, atomically remove
exactly those two node keys, preserve all others, and verify requests with both
test credentials receive 401. Do not copy or expose existing credentials.

From the reviewed bundle checkout, with `GO_BIN` an absolute temporary binary:

```sh
(cd go && go build -o "$GO_BIN" ./cmd/rookery)
GO_STATE=$(mktemp -d)
PY_STATE=$(mktemp -d)
chmod 700 "$GO_STATE" "$PY_STATE"
"$GO_BIN" --state-dir "$GO_STATE" configure --node "$GO_NODE" \
  --url "$HUB_ORIGIN" --token-file "$GO_TOKEN_FILE"
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" configure \
  --node "$PY_NODE" --url "$HUB_ORIGIN" --token-file "$PY_TOKEN_FILE"
```

Bind each fresh harness session to its own helper before starting it. Codex
MUST use a separate CODEX_HOME with only the intended test MCP server, and an
empty trusted working directory **outside the user's home**. Trusting the home
can load its `.codex/config.toml` as project configuration, reintroducing
production MCP entries and permissions. Do not use `-c`, `--enable`, `--disable`
or `--search` for MCP isolation: configuration overrides force embedded mode,
which prevents shared-daemon discovery
([upstream `daemon_startup.rs:90-124`](https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/tui/src/daemon_startup.rs#L90-L124)).

Prepare a distinct home/config for each Codex test session and keep those values
private. For the Go recipient, prepare the following before starting the Go daemon;
its exported CODEX_HOME must also be present at every Go daemon restart. Substitute the Python helper and PY_STATE when testing the
Python Codex recipient, keeping each provider home separate:

```sh
umask 077
CODEX_HOME=$(mktemp -d /tmp/rookery-codex-home.XXXXXXXX)
CODEX_WORKDIR=$(mktemp -d /tmp/rookery-codex-work.XXXXXXXX)
export CODEX_HOME CODEX_WORKDIR
uv run python - "$GO_BIN" "$GO_STATE" <<'PYCONFIG'
import json
import os
import pathlib
import sys

quote = lambda value: json.dumps(value, ensure_ascii=False)
config = (
    '[mcp_servers.rookery-go]\n'
    f'command = {quote(sys.argv[1])}\n'
    f'args = {quote(["--state-dir", sys.argv[2], "mcp", "--provider", "codex"])}\n'
    f'\n[projects.{quote(os.environ["CODEX_WORKDIR"])}]\n'
    'trust_level = "trusted"\n'
)
path = pathlib.Path(os.environ["CODEX_HOME"]) / "config.toml"
path.write_text(config, encoding="utf-8")
path.chmod(0o600)
PYCONFIG
```

Start the Codex harness from that empty trusted directory with the same
CODEX_HOME and no command-line configuration overrides:

```sh
(cd "$CODEX_WORKDIR" && codex)
```

Confirm it uses the shared daemon and publishes the owned control rendezvous
symlink described in [the endpoint contract](contract.md#verified-unix-rendezvous-layout-phase-2e).
Do not copy production configuration, authentication files or session history
into the test home; authenticate the disposable account separately if required.

For Claude, create a 0600 temporary JSON config with only
`{"mcpServers":{"rookery-go":{"command":"<absolute helper>","args":["--state-dir","<test state>","mcp","--provider","claude"]}}}`
using a JSON encoder, save its path as `GO_MCP_CONFIG` or `PY_MCP_CONFIG`, then launch
`claude --mcp-config "$GO_MCP_CONFIG" --strict-mcp-config` (use the Python
config for that session).
Create a distinct config for the Python session with its Python helper executable
and `PY_STATE`. The strict config prevents inherited `rookery` entries.
With the Go recipient's CODEX_HOME exported in the daemon shell, start the helpers:

```sh
"$GO_BIN" --state-dir "$GO_STATE" daemon >"$GO_STATE/daemon.log" 2>&1 &
GO_DAEMON=$!
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" daemon \
  >"$PY_STATE/daemon.log" 2>&1 &
PY_DAEMON=$!
"$GO_BIN" --state-dir "$GO_STATE" list
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" list
```

Confirm the tool list shows only the intended test server. Capture session IDs
privately, configure the Go allowlist with exactly its test session, then restart:

```sh
"$GO_BIN" --state-dir "$GO_STATE" configure --node "$GO_NODE" \
  --url "$HUB_ORIGIN" --token-file "$GO_TOKEN_FILE" \
  --session-allowlist "$GO_SESSION"
kill "$GO_DAEMON"
wait "$GO_DAEMON"
"$GO_BIN" --state-dir "$GO_STATE" daemon >>"$GO_STATE/daemon.log" 2>&1 &
GO_DAEMON=$!
```

 Missing/empty `session_allowlist` discovers nothing and logs that fact.
Do not add production IDs. The Python account must contain only disposable tests.

Wait until those two fresh sessions are visible at their intended nodes. Then:

```sh
"$GO_BIN" --state-dir "$GO_STATE" enroll "$GO_SESSION" --alias go-check
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" \
  enroll "$PY_SESSION" --alias python-check
"$GO_BIN" --state-dir "$GO_STATE" --as "$GO_SESSION" send \
  --to "$PY_NODE/python-check" --body 'Reply once with a short acceptance result.'
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" --as "$PY_SESSION" \
  send --to "$GO_NODE/go-check" --body 'Reply once with a short acceptance result.'
```

Capture each returned message ID privately as `GO_MESSAGE_ID` and
`PY_MESSAGE_ID`. Inspect the corresponding sender/recipient views with:

```sh
"$GO_BIN" --state-dir "$GO_STATE" --as "$GO_SESSION" status "$GO_MESSAGE_ID"
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" --as "$PY_SESSION" \
  status "$PY_MESSAGE_ID"
"$GO_BIN" --state-dir "$GO_STATE" --as "$GO_SESSION" inbox --peek
"$GO_BIN" --state-dir "$GO_STATE" --as "$GO_SESSION" inbox
```

In each native session, verify one
arrival with the canonical two/three-line rendered envelope and answer using its
Rookery reply tool. Inspect `status MESSAGE_ID` on the sender; require submitted
without claiming model receipt, then read/ack or actual reply evidence. Test
`inbox --peek` versus non-peek separately. Stop/restart the Go daemon and verify
that completed or uncertain native attempts are not resubmitted. Pause the
recipient beyond a short send TTL and require expiry without late input.
Verify Codex busy is held; verify Claude busy obeys startup busy-delivery policy,
and that changing the setting requires restart. For each provider, repeat MCP
identity checks from the actual harness, including an absent/conflicting identity
that must return JSON-RPC -32602. Register a separate fresh session for polling,
verify inbox receipt/read/ack, and require no native push. Retain sanitized
outcomes only; no message bodies or actual identity/credential values in public
evidence.

For a concrete restart and expiry check (after the two ordinary exchanges):

```sh
kill "$GO_DAEMON"
wait "$GO_DAEMON"
"$GO_BIN" --state-dir "$GO_STATE" daemon >>"$GO_STATE/daemon.log" 2>&1 &
GO_DAEMON=$!
kill "$PY_DAEMON"
wait "$PY_DAEMON"
"$GO_BIN" --state-dir "$GO_STATE" --as "$GO_SESSION" send \
  --to "$PY_NODE/python-check" --ttl 1 --kind inform --body 'Expired acceptance probe.'
sleep 2
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" daemon \
  >>"$PY_STATE/daemon.log" 2>&1 &
PY_DAEMON=$!
```

Require no duplicate native arrival after restart and no native arrival of the
expired probe. Inspect that probe's returned ID via status and its recipient's
peek inbox; expired unread mail should remain visible until fetched once.

Cleanup only these test enrollments and the processes started above:

```sh
"$GO_BIN" --state-dir "$GO_STATE" clear-alias "$GO_SESSION"
"$GO_BIN" --state-dir "$GO_STATE" disable "$GO_SESSION"
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" clear-alias "$PY_SESSION"
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" disable "$PY_SESSION"
kill "$GO_DAEMON" "$PY_DAEMON"
wait "$GO_DAEMON" "$PY_DAEMON"
```

After reviewing uncertain-attempt evidence, have the hub administrator revoke
both test-node tokens as above, confirm 401, then remove temporary MCP configs,
credential files and test roots:

```sh
rm -f -- "${GO_MCP_CONFIG:-}" "${PY_MCP_CONFIG:-}" "$GO_TOKEN_FILE" "$PY_TOKEN_FILE"
rm -rf -- "${GO_STATE:?}" "${PY_STATE:?}"
```

 Publication, replacement of the Python helper and claiming ordinary
live sessions remain separate decisions.

### Phase 2 review-fix verification

The review-fix candidate passed 310 Python tests, `go vet ./...`, `go test ./...`
(including built `daemon`/`mcp` cross-delivery against a temporary Python hub),
`go test -race ./...`, Linux and Windows cross-builds, the portable OS-import
gate, whitespace validation, and an added-line privacy grep. Tests use synthetic
credentials, sessions, records, executables and local sockets. Windows build
success is not native Windows runtime evidence. No live acceptance was run.

The acceptance procedure additionally requires a disposable OS account because
Python discovery is host-wide. Go discovery requires explicit configured session
IDs during coexistence. Phase 2c supersedes the earlier Linux command-delivery
decision: Codex delivery uses RPC on every OS and never invokes a command.
Python 0.3.x retains `codex queue` until replacement by the Go helper.
Post-rename directory-fsync failure reports local admission as queued, while native attempt journaling
continues to fail closed. No review recommendation was rejected.


### Phase 2c verification

The phase 2c candidate passed all 310 Python tests, `go vet ./...`,
`go test ./...` including isolated Python-hub integration, `go test -race ./...`,
Linux and Windows builds, the portable OS-import gate for both targets, the Codex
command-dependency AST test, whitespace validation and an added-line privacy
grep. Fake authenticated control-socket tests cover queue acceptance, error
responses, connection loss after request write (uncertain), and an unloaded
thread (held without queue submission). No live hub, live sessions or real
credentials were used. Windows runtime and installed-harness acceptance remain
UNVERIFIED; cross-build success does not establish them.

### Phase 2d verification

The private-group compatibility fix passed all 310 Python tests, `go vet ./...`,
`go test ./...`, `go test -race ./...`, cgo-disabled Linux and Windows builds,
and the portable OS-import gate for both targets. Synthetic account databases
cover private groups, additional explicit and primary-gid members, malformed
records and read failures. Filesystem tests cover 0664 provider records under
0775 private-group ancestors, authenticated provider endpoints, changed unsafe
provenance and strict helper socket modes. Redacted-log tests verify fixed codes
and exclusion from serialized and hub results. Whitespace and added-line privacy
checks passed. No live hub, live harness sessions or real credentials were used;
installed-harness acceptance remains outstanding.

### Phase 2e verification

The control-rendezvous fix passed all 310 Python tests, `go vet ./...`,
`go test ./...`, `go test -race ./...`, cgo-disabled Linux and Windows builds,
and the portable OS-import gate for both targets. Synthetic filesystem tests
cover authenticated single-link control endpoints, long advertised paths,
group/other-accessible target directories, multiple links, unsafe ancestry,
foreign ownership policy, changed ProcessRef pins, and target/link replacement.
The target replacement test retains the original listener and confirms that
connect reaches its pinned inode before post-connect verification refuses the
replacement. A filesystem foreign-owner test runs when lchown is available;
synthetic ownership checks run without that privilege. Helper and record-based
endpoints still reject links. Discovery-log tests verify fixed redacted reasons.
Whitespace and added-line/new-file privacy checks passed. No live hub, harness
sessions, real state or credentials were used; installed-harness and native
Windows acceptance remain unverified.

## Phase 3 implementation status

Phase 2 passed isolated live Linux acceptance on public `1685721`, as reported
by the operator. Phase 3 adds native Windows code and CI-ready tests; **native
Windows runtime acceptance has not been performed**. This development environment
cannot execute Windows tests. Phase 3 is not accepted for production.

`platform/windows` implements the shared interfaces using pure Go and
`golang.org/x/sys v0.42.0`. This dependency provides maintained typed Win32 security,
process, socket, job, lock and filesystem bindings absent from the standard
library. There are no additional runtime dependencies, cgo or shell commands.
The portable CLI and installer are shared with Linux. Codex retains its sole
control-connection `thread/queue/add` route. AGY is an unsupported stub on Windows.

State precedence is explicit `--state-dir`, `ROOKERY_GO_HOME`, then
`%LOCALAPPDATA%\rookery-go`; `ROOKERY_HOME` is ignored. Provider roots honor
`CODEX_HOME` and `CLAUDE_CONFIG_DIR`, otherwise `%USERPROFILE%\.codex` and
`%USERPROFILE%\.claude`. State objects use protected owner-only DACLs with no
SYSTEM grant. Existing unsafe ACLs are refused, never silently repaired. Native
Scheduled Tasks grant current SID plus SYSTEM. System ancestry trusts SYSTEM,
Administrators and TrustedInstaller; the volume root is a pinned trust anchor.
All other ancestors are checked for unauthorized mutation grants and reparse
points while open without delete sharing during validation. Provider-record
ancestry guards are released after opening; record handles allow delete sharing
and endpoint resolution reopens the path and verifies the volume/file identity.
Files also refuse hardlinks, alternate
data streams, UNC/device paths, DOS device names and trailing-dot/space aliases.
Provider records stay host-issued and open through endpoint resolution.

Writes create a private same-directory temporary file, flush contents, close it,
then use `MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH)`; exclusive backups omit
REPLACE_EXISTING. Locks use `LockFileEx` with cancellation-aware acquisition.
Storage device/power-loss durability remains a native acceptance item; successful
Linux fsync tests are not Windows evidence.

Process pins use token SID, creation FILETIME and a boot identity combining the
boot-environment GUID with `KUSER_SHARED_DATA.BootId`, which Windows increments
for each boot attempt. The environment GUID alone is not a per-boot identity.
See [Microsoft’s KUSER_SHARED_DATA reference](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntddk/ns-ntddk-kuser_shared_data). Toolhelp
supplies ancestry only; child/parent creation order and pins are rechecked.
Image paths come from `QueryFullProcessImageName`. Arbitrary foreign command-line
inspection reports unsupported rather than reading the PEB or querying NtQuery
process information. The boot-only native query fails closed on missing API,
zero GUID or layout mismatch; reading the boot counter also fails closed. There
is no uptime or rounded wall-clock fallback.

Own IPC is a first-instance local-only named pipe with an explicit protected
owner DACL and mandatory integrity label. Accept captures client PID, validates
process and impersonation token SID/integrity/AppContainer, then pins it. It
requires the first incoming byte within five seconds (or the caller deadline) to
establish the connected client's impersonation token, retaining that byte for
the caller. Pending transfers and accepts keep their overlapped structures on
the heap until completion or cancellation is drained; caller buffers are pinned
for the entire transfer. Dial checks the pipe server PID, token and security descriptor.
Codex AF_UNIX uses x/sys sockets and the pinned upstream peer policy cited in
[contract.md](contract.md#go-windows-native-boundary-phase-3); elevated peers are
refused. The terminal AF_UNIX reparse tag is a narrow nonredirecting socket
exception; symlinks, junctions and reparse ancestors remain refused.

Exec validates an explicit absolute native PE executable, refuses scripts and
shell interpreters and alternate data streams, creates suspended with an explicit inherited-handle list,
assigns a kill-on-close job and resumes only after assignment. Output is bounded
across stdout/stderr; timeout/cancellation after resume is uncertain. Peer input
uses stdin or authenticated IPC; no provider places peer text in argv.

Service installation uses Task Scheduler COM directly, on an initialized locked
OS thread, passing XML as BSTR in memory. It does not invoke schtasks or write
XML files. The logon task is named `rookery-go`, uses InteractiveToken and
LeastPrivilege, stores no password and does not replace an existing task.
Start/stop/status/uninstall validate the existing task's owner and protected DACL.
Installation deletes its newly created task if the post-registration ownership
check fails and reports any rollback failure. The requested descriptor grants
only the current account and SYSTEM; the descriptor actually persisted by Task
Scheduler, including any normalization or extra ACEs, remains unverified on
native Windows and must pass the post-check.
Scheduled Tasks use the logon account environment, not temporary environment
overrides from the installing terminal. Use a dedicated test account with
persistently isolated provider roots before testing logon startup.
The current COM aggregate ABI implementation supports Windows amd64 only; other
architectures refuse service operations. Manual daemon operation is available.

### Windows installation

Use an unelevated PowerShell on Windows 11 amd64, native `codex.exe` and
`claude.exe`. Provider CLIs must be native `.exe` binaries; npm `.cmd` shims
and other scripts are refused. Build from the reviewed source:

```powershell
cd go
go build -o rookery.exe ./cmd/rookery
.\rookery.exe --version
.\rookery.exe install codex
.\rookery.exe install claude
```

The last two commands only plan registration. After reviewing their output,
use `install codex --apply` or `install claude --apply`. Only `rookery-go` MCP
registration is added; an existing `rookery` entry and permission grants are
preserved. A differing existing `rookery-go` entry is preserved too. Optional
`install claude --accept-inbound --apply` adds `crossSessionInbound=accept` only
when absent, with an exclusive private backup; an explicit setting wins.
Managed MCP configuration refuses user registration.

Provision a bearer file through an approved private channel, protect its owner
and DACL, then run `configure --node <temporary-node> --url <approved-origin>
--token-file <private-file> --session-allowlist <provider:UUID,...>`. Do not put
the bearer in argv. Use a fresh directory to change node/origin. Run `daemon`
interactively first. `service install --dry-run` renders the task;
`service install`, `service status`, `service start`, `service stop` and
`service uninstall` manage only the validated `rookery-go` task. Restart after
changing `busy-delivery`. Claude native delivery remains held until its token
source is verified; installing MCP does not remove that gate.

### UNVERIFIED and unsupported Windows behavior

- Actual Codex Desktop Windows version/protocol compatibility and runtime peer
  authentication. Source verification is pinned to `rust-v0.166.0`, not a claim
  about every installed Desktop build. Unsupported protocol or peer checks hold
  discovery/delivery; there is no executable fallback.
- Claude `agents --json` Windows record behavior and token publication in a
  session record. The official docs prove named-pipe auth and inbound policy,
  but not that schema. Claude’s real named-pipe DACL is also unverified against
  the required owner-only policy. Production `ClaudeAuthToken` always returns unsupported;
  native delivery is held before any connection. A private local source review
  with a citable, redistributable schema is required before enabling this path.
- Native boot GUID/counter compatibility, reboots/fast startup, ACL and
  reparse behavior, disconnect/timeout cleanup, COM lifecycle and power-loss
  durability. These have test code, not native execution evidence from this task.
  Explicit foreign-account and AppContainer negative-process fixtures remain
  outstanding; low-integrity token refusal has a native CI-ready test.
- AppContainer and lower-integrity peers are intentionally unsupported.
  Elevated Codex peers are intentionally refused to match upstream.
- Arbitrary process argv inspection and non-amd64 Scheduled Task COM are
  unsupported. Windows AGY remains deferred.

### Exact read-only verification checklist (real Windows 11)

Do this before install/configure/daemon. Keep all output private: ACLs, process
IDs, task XML and harness listings contain machine/session data. None belongs
in an issue, commit or acceptance transcript committed to this public repo.
These commands inspect existing state and do not start a helper or harness.

```powershell
[Environment]::OSVersion.Version
[Runtime.InteropServices.RuntimeInformation]::OSArchitecture
whoami /user
whoami /groups
Get-Command codex.exe,claude.exe | Select-Object Name,Source
codex.exe --version
claude.exe --version
Get-Item Env:CODEX_HOME,Env:CLAUDE_CONFIG_DIR,Env:ROOKERY_GO_HOME -ErrorAction SilentlyContinue
$codexRoot = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $env:USERPROFILE '.codex' }
$claudeRoot = if ($env:CLAUDE_CONFIG_DIR) { $env:CLAUDE_CONFIG_DIR } else { Join-Path $env:USERPROFILE '.claude' }
$stateRoot = if ($env:ROOKERY_GO_HOME) { $env:ROOKERY_GO_HOME } else { Join-Path $env:LOCALAPPDATA 'rookery-go' }
foreach ($root in @($codexRoot,$claudeRoot,$stateRoot)) {
  for ($p=$root; $p; $p=Split-Path -Parent $p) {
    if (Test-Path -LiteralPath $p) {
      Get-Item -Force -LiteralPath $p | Select-Object Attributes,LinkType
      Get-Acl -LiteralPath $p | Select-Object Owner,AreAccessRulesProtected,Access
    }
  }
}
$control = Join-Path $codexRoot 'app-server-control\app-server-control.sock'
Get-Item -Force -LiteralPath $control -ErrorAction SilentlyContinue | Select-Object Attributes,LinkType
Get-ScheduledTask -TaskName rookery-go -ErrorAction SilentlyContinue | Select-Object TaskName,State,Principal
```

Check each ancestor's ACL privately; root-volume creation grants are the only
root exception. Refuse any redirecting reparse object, foreign-owned provider
record, or writable foreign ancestor. Do not dump session records or tokens.
Privately inspect installed MCP configuration for `rookery` coexistence and exact
`rookery-go` command/arguments. Verify the Desktop process is unelevated in Task
Manager and both harness versions match reviewed evidence. Do not use `doctor`
on a production root as a substitute for this read-only preflight: opening state
can create state/locks. Tests below create only disposable synthetic fixtures.

### Windows CI and isolated live acceptance procedure

CI's windows-latest job runs `go vet ./...` and **the full `go test ./...`**,
including ACL, reparse, LockFileEx, two-process pipe, process, Exec, store,
contract vectors and Windows durable-helper workflow tests. Contract JSON decoding
preserves escaped LF envelopes on CRLF checkouts. Fake Codex AF_UNIX and fake
Claude pipes exercise transport with synthetic records/keys. Elevated CI asserts
Codex refusal; successful AF_UNIX protocol cases require an unelevated run.
Linux retains Python/Go isolated integration and `-race`. The OS-import and
Codex no-command gates remain in place. Cross-vet and `go test -c` of every
Windows package are separate compile gates, not native execution proof.

On a real Windows 11 amd64 machine, acceptance is an operator procedure, **not
permission for this development task to contact a hub or real sessions**:

1. Complete the read-only preflight above. Record only redacted booleans and
   version numbers in public evidence. Keep the raw evidence private.
2. Use a disposable local test account and a short private working directory.
   Set fresh `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `ROOKERY_GO_HOME` before launching
   either harness; never copy normal state, sessions or tokens. Enroll/login
   through each harness's own UI if required. Confirm Desktop actually honors
   the isolated CODEX_HOME; stop if it uses the ordinary profile. Do not proceed
   with an assumed Desktop launch flag.
3. Run `go test ./...` unelevated in the disposable account. Require successful
   Codex fake protocol cases, pipe tests, native boot identity, private files,
   concurrent independent stores and all contract vectors. Run again after reboot;
   privately compare boot IDs: stable during a boot and changed after reboot.
4. Create a temporary hub node through the existing approved operator procedure,
   with a fresh short-lived bearer delivered only to a protected file. Use a
   fresh helper state and strict nonempty session allowlist containing only the
   exact synthetic test sessions. Never let Python and Go claim the same session.
5. Register only `rookery-go` MCP in isolated provider profiles. Configure strict
   MCP/tool permissions (no broad tool allow grant), exact executable and state
   arguments, and verify existing `rookery` registration is untouched. Start the
   helper interactively, not as a task. Discover/enroll only the allowlisted
   synthetic sessions; unrelated live sessions must never publish or receive.
6. For Codex Desktop: prove loaded-thread listing, exact thread/read, idle-only
   queue, pending while busy, later single submission when idle, and actual
   model receipt by explicit ack/reply. Submitted alone is not receipt. Exercise
   disconnect after queue write, restart, unchanged request IDs, expired offline
   messages, stale/process-reused pins and no replay after uncertainty. Verify
   missing/unsafe rendezvous and elevated peers refuse without command fallback.
7. For Claude: verify agents output and record provenance privately. With this
   revision require `auth_unverified`, zero native connects and no delivery.
   **Successful Claude live acceptance is blocked** until a source/doc-backed
   session-record token schema is implemented and reviewed. Once that gate is
   satisfied, require first-line auth with that session's token, failed auth
   rejection, crossSessionInbound accept/hold/refuse, idle/busy on/off with restart,
   wrapper preservation, exact PID/session pin and actual ack/reply. Never use a
   different session's token to make this test pass.
8. In disposable roots only, test foreign DACL grants, junction/reparse ancestry,
   stale pipe server and PID, lower-integrity/AppContainer rejection, `.cmd/.bat`
   refusal, output limits, timeout child-job cleanup and replacement interrupted
   before/after move. Keep uncertain claims unreplayed. Power-loss durability
   requires its own disposable VM/storage test, not killing the production host.
9. Only after foreground acceptance, inspect `service install --dry-run`, install
   the disposable account's `rookery-go` task, log out/in, verify one helper,
   protected task descriptor, status/start/stop/uninstall and preservation of an
   unrelated `rookery` task. Test installer idempotence and existing-entry refusal.
10. Stop helper/harnesses, disable/remove temporary hub enrollment and node,
    revoke the bearer, uninstall the test task/MCP entries and remove only the
    disposable profile/state. Privately retain evidence until review; publish
    only synthetic results. Phase 3 acceptance requires both harnesses; the Claude
    block must not be counted as a successful end-to-end messaging test.

### Phase 3 local validation evidence

The development run passed 310 Python unit tests, Linux `go vet ./...`,
`go test ./...`, `go test -race ./...`, `GOOS=windows go vet ./...`, and
`GOOS=windows go test -c` for every package listed for that target. The portable
OS-import gate passed for Linux and Windows; the Codex no-command AST gate ran
in the Go suite. Staged whitespace and privacy checks passed. No native Windows
test execution, CI run, real harness session, Scheduled Task installation or
live-hub acceptance occurred during this work. Windows CI execution and the
native acceptance gates above remain outstanding.

### Phase 3 review corrections

Pending Windows reads/writes and pipe accepts now retain heap-owned overlapped
state and drain completion before releasing resources; buffers remain pinned.
Native regression tests force stack growth and garbage collection while reads
and accepts are pending, and check listener-close cancellation. Other corrections
stop ancestry at a reused parent PID, roll back newly created tasks that fail
ownership validation, use the per-boot counter, permit provider record rotation
with identity revalidation, and test elevation policy independently of socket
ownership. Native Windows execution, actual Task Scheduler descriptors, Claude
pipe DACL compatibility and the reboot comparison above remain acceptance gates.
