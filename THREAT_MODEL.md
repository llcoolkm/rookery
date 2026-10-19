# Threat model

Rookery is experimental messaging infrastructure for agents operated by mutually
trusted participants. It is not a sandbox for hostile agents or a multi-tenant
messaging service. Enrollment makes an agent reachable; it does not authorize
the agent to carry out arbitrary work.

## Assets and trust boundaries

Protect node bearer tokens, provider credentials, conversation identities,
message contents, delivery history, and the tools and data accessible to each
participating agent. A malicious message can affect an agent that has shell,
network or repository access; the consequences extend beyond the mailbox.

- A node token authenticates a **host account**, not an individual model. Its
  holder can assert any session identity on that node, publish session state,
  enroll sessions, read that node's mail and send to other enrolled peers. There
  is no per-recipient allowlist or cryptographic model identity. Give tokens only
  to mutually trusted participants; do not distribute a shared public token.
- Processes running under the same Unix account share access to Rookery state,
  tokens and provider endpoints. Session bindings and MCP caller metadata are
  routing mechanisms, not isolation from another process under that account.
- The hub operator, its storage administrator and a TLS-terminating proxy can
  read message contents. HTTPS protects traffic in transit; there is no
  end-to-end encryption or signed message provenance independent of the hub.
- Provider plugins are Python code loaded into the helper process. Installing a
  plugin grants it the helper account's privileges and access to its secrets.
  They are not isolated by the provider registry.

## Peer text and agent authority

Any authorized sender can place arbitrary text into an enrolled agent's turn.
Treat message bodies, peer names and tool results as untrusted input. Envelope
quoting limits delimiter forgery; it does not prevent semantic prompt injection,
false claims of user authorization, secret exfiltration requests or unsafe tool
use. The receiving agent must follow its existing user delegation and provider
permissions. Peer text cannot grant new authority, expand scope or waive approval.
Rookery does not mechanically enforce those instruction boundaries inside a model.

MCP exposes `peers`, `send`, `reply`, `inbox` and `status` over stdio. It does not
expose token provisioning or hub administration. Harness metadata and explicit
session bindings identify the caller for routing; neither proves which model or
person caused a call. Inbox reads change delivery state unless requested as a
peek. Tool annotations are hints, not an authorization system.

## Native input and approvals

Claude delivery uses an internal cross-session socket. Since 0.2.7 Claude busy
delivery is enabled by default and input may be read between tool calls. Set
`rookery busy-delivery claude off` and restart the helper for idle-only delivery.
Waiting/unknown sessions are held. Claude's `crossSessionInbound: accept` setting
removes its native per-message approval prompt **for the account**, not only for
Rookery. Review that setting and the installer's plan before applying changes;
ordinary tool permission grants remain a separate control.
`rookery install claude --apply` leaves inbound acceptance unchanged. Adding
`--accept-inbound` explicitly opts into setting it when it is absent; an
existing explicit value is preserved.

Codex receives input through `codex queue` after eligibility checks against its
managed app-server. Attached Antigravity conversations accept input while busy;
their endpoint does not provide the same approval-dialog admission signal as
Claude or Codex. Attachment inherits the conversation's provider credentials;
those credentials stay with the endpoint rather than being sent to the hub.

These routes rely on provider-specific interfaces and observed behavior,
including Claude session records/socket framing, Codex managed control sockets
and experimental app-server APIs, and AGY caller metadata. They are not promised
stable upstream APIs. An upgrade can break discovery, identity or admission.
Check the compatibility information in the README and validate changes against
designated disposable sessions before using them for valuable work. Never fall
back to terminal keystroke injection or weakening provider permissions.

## Local confidentiality limitation: process arguments

The current Codex and AGY delivery commands put the complete message text in
process arguments. Other local users may see it through process inspection on
systems with a normally mounted `/proc`, even though Rookery files are private.
The supported command help checked for Codex 0.163.0 and AGY 1.4.2 offers no
stdin/file input option for these delivery commands. Rookery does not interpret
`-` as a secret input channel or invent an unsupported delivery route.

Use a dedicated machine/VM with trusted local users, or have an administrator
configure and verify process-visibility isolation appropriate to the host.
Private state directories alone do not solve this exposure. Do not send secrets
over these routes when command-line visibility cannot be controlled. This limit
also applies when the originating send call arrived through MCP.
For CLI-originated mail, use `--body -` to read text from stdin rather than
putting it in Rookery's own arguments. This protects that invocation only;
Codex and AGY native delivery still have the limitation above.

## Deployment and credential handling

Use HTTPS with certificate verification for remote clients. The hub's HTTP
listener belongs behind a TLS proxy or another protected transport; it does not
terminate TLS itself. Explicit HTTP development mode is restricted to loopback.
Keep the internal metrics/health/graph listener private: it is unauthenticated
and reveals routing/activity metadata. Do not expose it through the public proxy.

Generate separate high-entropy random tokens for each node; a minimum length is
not proof of entropy. Keep raw tokens in private files, not shell arguments,
source control, logs or screenshots. The hub stores SHA-256 token hashes; clients
retain bearer credentials. Protect both storage locations and their backups.
Revoke a compromised node's hash, provision a fresh token, and investigate its
messages and local/provider access. Revocation does not undo already accepted
work or remove copies of stolen data.

State roots must be private and owned by the runtime account. Protect ancestor
directories, provider sockets and service configuration as well. Run helpers as
their agent account and the hub as a dedicated unprivileged service account.
Rookery's permission checks are not protection against root or a compromised
account. Limit access to logs, crash dumps and deployment secret stores.

## Availability, retention and recovery

Request sizes and message admission are bounded, but an authenticated hostile
participant can still consume resources, flood peers or cause expensive model
turns. Use proxy connection/rate limits, filesystem quotas, monitoring and token
revocation; admission settings are not filesystem quotas or protection against
all denial of service. Keep unrelated services away from the hub's disk budget.

The hub caps cached request identities at 10,000 per node and 50,000 overall.
New admission operations stop at 8,000 per node or 40,000 overall, leaving the
remaining 20% for completion operations such as reads and acknowledgements.
Byte admission also reserves the smaller of 16 MiB and 20% of the configured
budget for completion. Capacity checks include metadata changes and WAL size;
these reserves do not guarantee unlimited completion or availability under load.
Session metadata is capped at 512 identities per node and 4,096 overall, with at
most 128 nodes. Unenrolled sessions without aliases become eligible for pruning
after seven days without an update; enrolled or aliased sessions are preserved.
At the session cap, the hub continues to refresh existing advertised sessions
and the node heartbeat, but refuses new session identities. The helper reports
those refusals through its health diagnostics. Disable and clear aliases from
obsolete sessions to make them eligible for retention cleanup.

New clients use UUIDv7 request identities with a seven-day validity window from
their embedded creation time. IDs more than five minutes ahead of the hub clock
are rejected. The hub rejects expired identities before executing an operation,
so deleting their expired cache entries does not authorize a replay. A persisted
pruning boundary also rejects forgotten identities after a backward clock step.
Keep clocks
synchronized. A helper offline for seven days may return with expired queued
operations: the hub rejects those operations rather than issuing new identities
or silently replaying them. Reconcile their outcomes before creating a new
request, even if the original response never reached the helper.

Legacy UUID4 request identities have no creation timestamp. The upgraded hub
establishes a bounded seven-day migration grace period for them; their cache
entries count against the same caps and expire at that migration deadline.
After the grace period, uncached UUID4 requests are rejected rather than treated
as new work. Upgrade participating helpers during the grace period and reconcile
any remaining legacy queue. Do not delete deduplication records or merely change
request identities to clear a quota or bypass expiry. Message ID bindings remain
durable independently of the request-cache window.

The database remains structurally compatible with 0.2.7, but **downgrading to that
hub is unsafe**: old code ignores expiry and can execute a request whose cache
entry was already pruned. A subsequent upgrade adds expiry metadata to entries
created while running the older hub; it cannot undo work already replayed during
the downgrade. Do not use an older executable as a recovery shortcut. Preserve
history and reconcile agent side effects before any rollback or migration.

Mail and routing metadata are stored in plaintext. Terminal message bodies are
pruned after the retention interval; this is not secure erasure. SQLite free
pages, WAL files, snapshots, provider transcripts and other backups may retain
copies. Establish backup access and deletion policies independently.

Provider acceptance means input was submitted, not that a model read it or
completed a task. Only explicit acknowledgement/reply supplies corresponding
application evidence. Never replay an uncertain submission automatically. A
rolled-back database can forget an earlier provider submission; reconcile actual
session history before restoring delivery from backups. There is no automatic
database failover or exactly-once guarantee for arbitrary agent side effects.

## Review status

Automated tests and operational acceptance checks do not establish resistance to
hostile participants. This repository does not claim an independent external
security audit. Security-sensitive deployments require their own review of the
agents' privileges, local users, provider versions and network/storage boundaries.
