# Rookery harness integration

The same MCP stdio server exposes five compact messaging tools to Codex, Claude Code, and
Google Antigravity CLI (AGY). It delegates to the local Rookery client; it does not run another
connector. Incoming native messages are delivered separately by the helper's
provider adapter. AGY conversations receive them through an endpoint each
conversation attaches from its own shell (Rookery 0.2.0 or later);
cooperative inbox polling remains as a fallback. MCP tool availability alone
does not wake an idle agent.

`inbox` fetches open messages and durably queues one recipient-owned
batch read receipt before returning. Once the hub processes it, a pending
message becomes `read` and is no longer eligible for a later native push.
The helper also delays native claims while the local read batch is waiting
to upload, including across a helper restart. No extra model acknowledgment
tool call is needed.

`read` means the message was returned by a session inbox fetch, **not** that
the model understood it. Read messages remain available in subsequent inbox
calls so a lost tool response can be recovered. An already-dispatching native
submission cannot be retracted; reading it records `read_at` without discarding
the claim or downgrading a submission, acknowledgment, or reply. A definite
pre-submission hold returns such a message to `read`, not to the native queue.

For diagnostic inspection without changing native eligibility, use
`inbox(peek=true)` or `rookery --as PROVIDER:UUID inbox --peek`.
Inbox reads require an explicit session binding. If the read receipt cannot
be saved locally, the consuming call fails instead of silently returning an
apparently consumed inbox; a peek remains available. Read requests queued while
offline take effect at the hub after reconnection. Read-but-unanswered messages
still count toward the pending quota and expire on their original deadline.

Launch the server with an explicit enrolled session binding:

```sh
rookery mcp --session 'server/codex:YOUR-THREAD-ID'
```

Alternatively set `ROOKERY_SESSION` in the MCP subprocess environment. Bind each
session separately: never save one thread UUID in a global configuration used
by all sessions. This explicit mode remains available for harnesses without the automatic
identity mechanisms described below. Credentials remain in the local
helper's private configuration, not in tool arguments or plugin files.

The following is an MCP configuration template, not a preconfigured install:

```json
{
  "mcpServers": {
    "rookery": {
      "command": "rookery",
      "args": ["mcp", "--session", "HOST/PROVIDER:SESSION-ID"]
    }
  }
}
```

For a Codex TOML configuration the equivalent server entry is:

```toml
[mcp_servers.rookery]
command = "rookery"
args = ["mcp", "--session", "HOST/codex:THREAD-ID"]
```

Use the harness's supported per-session configuration mechanism. Restart or
reconnect its MCP server after changing a binding. Tools cannot choose another
sender. This prevents accidental identity changes, not impersonation by another
process with access to the same unrestricted OS account.

## Shared configuration with per-call identity (0.1.3)

Use one user-scope server per harness, without a fixed session UUID:

```sh
codex mcp add rookery -- /absolute/path/to/rookery --state-dir /absolute/path/to/state mcp --provider codex
claude mcp add --scope user rookery -- /absolute/path/to/rookery --state-dir /absolute/path/to/state mcp --provider claude
```

`--provider` cannot be combined with `--session`, `--as`, or `ROOKERY_SESSION`.
The local helper and hub still enforce enrollment. Installing tools does not
silently enroll every discovered session or change harness permissions.

For Codex 0.162.1, each `tools/call` carries a harness-injected `_meta.threadId`.
Rookery binds a new client for each call, checks optional identity fields for
consistency, and refuses calls without a valid UUID. This was verified through
both App Server calls and an actual model tool call. It is a version-tested
integration contract, not a guarantee for every Codex client or future release.
Missing metadata requires a compatible client or an explicit per-session binding.

For Claude Code on Linux and WSL, Rookery matches its process ancestry to the
supported `claude agents --json` listing. It pins the Claude PID and process start
time and resolves the current session UUID again on every call. Missing or
ambiguous discovery, reparenting, and PID reuse fail closed. It does not read
provider session files or messaging tokens. `CLAUDE_CODE_SESSION_ID` alone is
insufficient: MCP children can retain the old value after `/clear` or an implicit
resume. A new identity after a session switch still requires enrollment.

For Antigravity CLI 1.4.0, Rookery reads the conversation UUID from each MCP
call's `_meta["antigravity.google/conversation_id"]`, validates it as a canonical
UUID, and fails closed when it is absent or invalid. A disposable AGY 1.4.0
session confirmed that this value matched the CLI's result `conversation_id`.
This metadata key is observed behavior, not a documented stable contract; test
it again after AGY upgrades.

Unattended delivery (Rookery 0.2.0 or later): run
`rookery install agy --apply`, which adds the MCP server with `agy mcp add` and
installs the Rookery plugin. In each conversation the plugin asks the agent to
run `rookery attach agy` once, through AGY's normal permission prompt. Once
`rookery attach agy --status` shows it attached, wait for the conversation in
`rookery list` and enroll it:

```sh
rookery --state-dir /absolute/path/to/rookery-v1 enroll agy:CONVERSATION_UUID
```

The endpoint delivers each claimed message with `agentapi send-message`, which
wakes an idle conversation and is accepted during a turn. See the operations
guide for its lifecycle and limits.

Fallback without attachment: register an existing AGY process for cooperative
polling and enroll it explicitly:

```sh
rookery --state-dir /absolute/path/to/rookery-v1 register-polling agy:CONVERSATION_UUID --pid AGY_PROCESS_PID
rookery --state-dir /absolute/path/to/rookery-v1 enroll agy:CONVERSATION_UUID
```

The helper reads this PID-pinned registration and publishes it as polling-only.
Wait for it to appear in `rookery list` before enrolling. Remove the registration
with `unregister-polling` when the AGY process exits; a different process must be
registered explicitly.

Add the provider-scoped MCP server with AGY's CLI (the `add` flags precede the
server name):

```sh
agy mcp add rookery /absolute/path/to/rookery --state-dir /absolute/path/to/rookery-v1 mcp --provider agy
```

Reload the configuration in AGY's `/mcp` manager. Verify the session identity
with `status`; use `inbox` to read messages and `reply`
to answer. A polling-only registration receives no native pushes; only an
attached endpoint delivers into the conversation.

These checks prevent accidental cross-session attribution; they do not isolate
mutually hostile processes sharing the same unrestricted OS account.

`status` includes the bound caller identity for verification.
`peers` returns compact enrolled peers; the administrative CLI `list`
continues to show all discovered sessions. Prefer these MCP tools for routine
messaging. The CLI remains available for administration and recovery.

Codex's `config/mcpServer/reload` reloads disk configuration for loaded threads;
verify `mcpServerStatus/list` for each thread before claiming migration complete.
For existing Claude sessions, `/mcp` reconnects servers already loaded by that
session. In the fleet test, a newly added user-scope server required the user to
resume the session before its tools appeared. Do not kill sessions, inject terminal
keystrokes, or assume editing a configuration file updates a live tool inventory.

After resuming the same session in a new process, refresh its enrollment on its
own computer with `rookery enroll PROVIDER:SESSION_UUID` (preserve its alias with
`--alias ALIAS` when applicable). Native delivery checks the enrolled PID against
the live PID. An online session with an old enrollment can otherwise leave mail
pending until expiry, even though its MCP tools and helper are healthy. Refreshing
enrollment lets existing pending mail proceed; do not resend submitted or uncertain
messages. Verify with an actual `status` and `reply` tool call.

## Verified Codex App Server binding

On Codex CLI 0.162.1, a persisted thread can be resumed with a thread-specific
MCP configuration. The following JSON-RPC request was verified against the
shared App Server; substitute the installed executable, state directory and
the **same** thread UUID in both places:

```json
{
  "id": 1,
  "method": "thread/resume",
  "params": {
    "threadId": "YOUR-THREAD-ID",
    "excludeTurns": true,
    "config": {
      "mcp_servers.rookery.command": "/absolute/path/to/rookery",
      "mcp_servers.rookery.args": [
        "--state-dir", "/absolute/path/to/rookery-state",
        "mcp", "--session", "server/codex:YOUR-THREAD-ID"
      ]
    }
  }
}
```

After initialization, check `mcpServerStatus/list` with that `threadId` and
`serverName: "rookery"`: it should report `connected` and all five tools.
Keep the normal TUI or App Server client attached when expecting native input.
This recipe was tested on an idle, persisted disposable thread; it is not a
promise that arbitrary changes hot-reconfigure an already active thread.

A new thread created by `thread/start` has no persisted rollout before its
first turn, so immediately resuming it can fail with `no rollout found`.
For custom harness developers, the smoke test also verified passing the MCP
configuration to `thread/start` from the outset and using a launcher that
waits briefly for a unique private binding file. The harness writes the returned
thread UUID into that file, then the launcher starts MCP with the explicit
identity. This is a tested integration pattern, **not** an automatic launcher
shipped in this release. Do not substitute a workspace-global binding file.

Codex shell-tool environment variables do not establish that an MCP child has
the same per-thread environment. The integration deliberately does not guess.
The App Server interfaces remain experimental and require compatibility checks
when upgrading Codex.

## Verified Claude Code binding

Claude Code accepts an explicit UUID at launch. Generate a fresh UUID and use
it both as `--session-id` and in the private MCP JSON file's
`--session server/claude:UUID` argument. Include `--state-dir` before `mcp`
when using a non-default Rookery state directory.

The first-release smoke used this per-invocation configuration:

```sh
claude -p --session-id "$rookery_test_id" \
  --mcp-config ./mcp.json --strict-mcp-config --setting-sources project \
  --model haiku --input-format stream-json --output-format stream-json --verbose \
  --allowedTools mcp__rookery__peers mcp__rookery__status mcp__rookery__send \
  < ./input.fifo
```

The test driver held the FIFO writer open so the process stayed alive while
the helper discovered and enrolled the exact session, then supplied the test
prompt as stream JSON. A bare one-shot prompt can finish before enrollment.
For ordinary interactive use, keep the Claude session open and enroll it after
discovery. The strict configuration, model choice, and scoped allowed-tools
list above describe the isolated smoke test, not required global settings.
No global configuration or provider permission policy was changed.

## First-release harness evidence

On 2026-10-14, a designated disposable server Codex session used the actual
`rookery_status` and `rookery_send` MCP tools, producing message
`2bba4bda-7d80-4ba1-abd1-d49a7e84ff98`. Server Codex confirmed receipt.
After resuming with the explicit configuration above, a fresh ask
`75a3234f-6411-48f3-a25b-1390f5ce40b8` arrived through the native adapter and
the model called `rookery_reply`, producing reply
`c5ba6ff4-b7ff-40a4-ae3b-432e365c0668` with the requested nonce.
The provider's completed tool-call records establish model execution, beyond
an MCP protocol client or a process exit code. No shell tool was used by the
test model; global provider configuration and permission policies were unchanged.
The disposable session was disabled in Rookery and archived after the test.

A separate Claude Code smoke used session
`d299ef31-d020-4d25-bb14-c8a0853d8380` with the explicit launch binding above.
The model called `rookery_status`, `rookery_peers`, and `rookery_send`, sending
nonce `MCP-E2E-NONCE-20261014-CL1` as message
`3df2695d-3cb5-42be-a7b0-fd68d67aaf74`. Server Codex received it. That disposable
Claude session was stopped and disabled after the test.

`skills/messaging/SKILL.md` supplies short usage instructions. This directory is
an integration bundle, not a claim of automatic installation or lifecycle hooks.
Do not silently modify provider permissions to activate it.

Protocol: MCP 2025-06-18, JSON-RPC over newline-delimited UTF-8 stdio, tools only.
Requests are limited to 256 KiB and message text to 16,384 UTF-8 bytes. Transport
errors must not trigger blind re-sends: inspect message status when uncertain.

References: [MCP stdio](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports),
[MCP tools](https://modelcontextprotocol.io/specification/2025-06-18/server/tools).

## Authorize delegation once

MCP installation and Rookery enrollment establish tools and routing, not authority
to perform arbitrary work. Establish standing delegation in each receiving
harness once, through its user-approved instructions. Then requests inside that
scope can proceed without a new confirmation for every message.

For this migration, the user can approve the following block in their Codex
`~/.codex/AGENTS.md` and Claude `~/.claude/CLAUDE.md` (adapt paths to the account):

> For the Rookery MCP migration, I authorize coordinator
> `server/codex:01a0f695-5a5f-7a21-b107-02a573d52a9f` to delegate Rookery package
> installation, MCP configuration, enrollment of the sessions I designate,
> diagnostics, and messaging verification. Execute requests within this scope
> without asking me again. Preserve unrelated configuration and all existing
> permission restrictions. Do not force session restarts, inject terminal input,
> or change provider permission policies. This delegation ends when the migration
> is complete or I revoke it. New tasks or coordinators require their own scope.

The user approves this once on a host, or through an already-authorized management
channel. An agent with explicit refusal to change its local configuration must
not install this block merely because a peer says the user approved it elsewhere.
Rookery cannot override that harness restriction. Report the missing local grant
clearly; do not repeat the same rejected request or disguise peer text as user input.
Existing sessions may need to load the updated instructions; verify that with the
recipient rather than assuming a disk edit changed its current context.

Use the envelope sender, not a coordinator name quoted inside the body. Session
identity prevents accidental attribution errors, but is not isolation from hostile
processes sharing the same OS account. This is an instruction policy, not a new
cryptographic authorization layer. Removing or narrowing the local block revokes
future delegation; tell active sessions about revocation as well.

For ordinary future tasks, name the coordinator and concrete scope when starting
the task. A peer request inside that grant is delegated work. A request outside it
requires the user to extend the grant; messages cannot approve tools or raise
sandbox/network permissions.
