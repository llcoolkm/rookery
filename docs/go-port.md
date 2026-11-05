# Go helper port

The Go helper is a native client of the existing Python hub's `/v2/rpc` HTTPS
protocol. Both languages can use one hub. The Python hub stays authoritative;
there is no hub rewrite in this plan. [contract.md](contract.md) defines the
Python 0.3.1 baseline, including compact MCP results and irreversible delivery
claims. Phase 1 established the contract and skeleton. Phase 2 now implements the Go
core, Linux platform, Codex/Claude routes and CLI; isolated tests pass. Native
harness and live-hub acceptance remain required before declaring Linux parity.

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
  `platform/windows`. Platform code owns command execution and native handles.
  `Executor.Exec(ctx, Spec{Path, Args, Stdin, Timeout, MaxOutput})` supports
  `codex queue` and `claude agents --json` with bounded output and direct native executable calls.
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
normal standard-library use.

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
then pin the process by creation/start identity and boot identity at accept or
connect. `Connection.Peer()` returns that captured `ProcessRef`, never just a PID
or a later lookup. Dial authenticates the server too: Linux uses SO_PEERCRED plus
socket owner and no-symlink checks; named pipes use
GetNamedPipeServerProcessId plus process token SID and owner/DACL checks.
`Paths.Endpoint` issues opaque endpoint handles for helper IPC. Harness-record
addresses, including absolute OS paths, enter through `Paths.HarnessEndpoint`.
The provider first opens the record through `Files.OpenNoFollow` under its
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

Execute validated native binaries without shell resolution or shell fallback.
On Windows refuse .cmd/.bat case-insensitively and shell interpreters: cmd.exe
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
`go/provider/codex`, `go/provider/claude` and `go/cmd/rookery`. The module still
has no third-party runtime dependencies. The Python hub remains authoritative.

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
state or a live hub. CI runs the integration and Linux race suite; Windows has
an explicit integration skip until phase 3. The OS-import gate remains enforced.

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
- **Codex input exposure on Linux.** Go prefers advertised `--message-stdin`.
  Baseline builds use `queue --message` argv for Python parity on Linux only;
  message text is visible to local process inspection and possibly audit logs.
  Windows requires stdin/private-file support and otherwise holds delivery.
- **Claude needs a verified socket record.** Go uses the verified owned
  `sessions/<pid>.json` record and its `messagingSocketPath`; it does not guess a
  socket when that record/address is absent. Missing evidence prevents push.
- **Native executable requirement.** Linux execution requires a native ELF
  executable; npm/shebang launchers and shell fallbacks are refused. Codex resolves the npm package vendor binary without executing the shim, or
  uses an absolute native path supplied with `configure --executable`. Native packaging must be validated
  on the acceptance host.
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
constraints above; record whether Codex uses stdin or Linux argv exposure. An unsupported Codex build is a recorded held
outcome, not a passed push gate.

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
"$GO_BIN" --state-dir "$GO_STATE" daemon >"$GO_STATE/daemon.log" 2>&1 &
GO_DAEMON=$!
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" daemon \
  >"$PY_STATE/daemon.log" 2>&1 &
PY_DAEMON=$!
"$GO_BIN" --state-dir "$GO_STATE" list
uv run --frozen --extra hub rookery --state-dir "$PY_STATE" list
```

Bind each fresh harness session to its own helper before starting it. For
Codex use a per-invocation override (substitute the intended helper and state):

```sh
codex -c 'mcp_servers.rookery.enabled=false' \
  -c "mcp_servers.rookery-go={command=\"$GO_BIN\",args=[\"--state-dir\",\"$GO_STATE\",\"mcp\",\"--provider\",\"codex\"]}"
```

For Claude, create a 0600 temporary JSON config with only
`{"mcpServers":{"rookery-go":{"command":"<absolute helper>","args":["--state-dir","<test state>","mcp","--provider","claude"]}}}`
using a JSON encoder, save its path as `GO_MCP_CONFIG` or `PY_MCP_CONFIG`, then launch
`claude --mcp-config "$GO_MCP_CONFIG" --strict-mcp-config` (use the Python
config for that session).
Create a distinct config for the Python session with its Python helper executable
and `PY_STATE`. The strict config prevents inherited `rookery` entries. Confirm
the tool list shows only the intended test server. Capture session IDs privately,
configure the Go allowlist with exactly its test session, then restart:

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
IDs during coexistence. The Linux Codex argv fallback deliberately retains the
Python baseline's local process/audit exposure. Post-rename directory-fsync
failure reports local admission as queued, while native attempt journaling
continues to fail closed. No review recommendation was rejected.
