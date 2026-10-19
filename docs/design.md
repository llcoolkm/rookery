# Design notes

## Parts

Rookery has three parts: provider adapters, a per-account helper with its saved
mailbox, and one hub.

**Providers** (`rookery.providers`) wrap each harness. A provider implements
discovery (live sessions with provider, exact ID, name and a normalized state:
idle, busy, waiting or unknown) and submission of text into one session. Each
declares its capabilities instead of pretending all harnesses are equal: where it
runs (on the host, or as an endpoint attached from inside the conversation),
whether it accepts input during a turn, and whether busy delivery is an option.
Third-party providers register through the `rookery.providers` entry point and
cannot replace a built-in one.

**The helper** (`rookery daemon`) owns all native socket access for one Unix
account. It runs discovery, publishes the eligible sessions to the hub, long-polls
the hub for work, and submits claimed messages through the provider. CLI and MCP
clients never touch provider sockets: they write requests atomically into the
state directory's spool and read a private SQLite store that the helper owns.
This keeps sandboxed agents working without network or socket access. A
process-lifetime lock allows one helper per state directory.

**The hub** (`rookery hub serve`) is the single authority for messages,
enrollments and aliases. Helpers authenticate with per-node bearer tokens, whose
SHA-256 hashes the hub keeps in a file. It stores everything in one SQLite
database in WAL mode with a size budget, serves the API and a separate private
observability listener, and takes periodic consistent snapshots.

## Messages

An envelope carries message ID, sender, recipient, conversation ID, parent ID,
kind (ask, inform, reply), hop count, body, timestamps and delivery state. The
UTF-8 body limit is 16 KiB and a conversation holds at most six messages.
Message IDs are idempotency keys, so a retried send cannot create a second copy.
Sender identity is a same-account assertion, not cryptographic proof.

## Delivery rules

- Before submission the helper rechecks the exact live session and its process
  identity. State observations still race with user actions, so the provider's
  own permission handling remains essential.
- A dispatch claim is recorded at the hub before any provider I/O. Only one
  claim per message can exist.
- A submission whose outcome is not known, including a claim that times out,
  becomes `uncertain` and is never retried automatically. Restarts retry only
  pending mail.
- A later submission result cannot overwrite a receipt or reply. Submitted
  input is not treated as an acknowledgment by the model.
- Discovery fails closed: when a provider cannot be scanned or a recipient's
  state is not eligible, nothing is submitted. There is no terminal-keystroke or
  tmux fallback.

Native interfaces are version-sensitive. Compatibility is shown by adapter
contract tests and live tests against disposable sessions, not inferred from a
zero exit code.

## Codex runtime discovery and transport

Codex's managed app-server control socket speaks WebSocket over a Unix socket,
not JSONL. Rookery's small local transport validates the HTTP upgrade hash and
headers, masks outgoing frames, handles fragmentation and ping/pong, and
enforces deadlines and 16 MiB limits. It supports Unix sockets only, with no TCP,
TLS, compression or extensions.

`thread/loaded/list` followed by `thread/read` supplies live identity and state.
`thread/list` is persisted history and can omit a newly created running session,
so it is not used for delivery discovery. A recipient must be idle and report
`canAcceptDirectInput=true` before `codex queue` is invoked. These are
experimental app-server methods and may change between Codex releases.
