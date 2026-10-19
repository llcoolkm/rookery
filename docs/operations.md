# Rookery operations

This guide covers running the Rookery helper on each computer and the hub that
connects them. The [README](../README.md) has a shorter quick start. Examples use
nodes `alpha` and `beta` and a hub at `hub.example.com`.

Rookery is experimental and its provider integrations rely on undocumented
harness internals; see the notice in the README and the
[threat model](../THREAT_MODEL.md).

## The helper

Run one helper per participating Unix account, on the computer where that
account's agent sessions run. It needs the account's normal HOME and a PATH that
finds the provider CLIs (`claude`, `codex`, `agy`). It does not need root at
runtime. The hub belongs to a separate service account and never receives
native agent socket access.

### Installing

Install the wheel with uv, selecting the interpreter explicitly.
`/usr/bin/python3 --version` must report 3.11 or newer; an unrelated `python3`
earlier in PATH may be older:

```sh
uv tool install --python /usr/bin/python3 ./rookery_agents-0.3.1-py3-none-any.whl
```

For a dedicated environment instead of `uv tool`:

```sh
uv venv --python /usr/bin/python3 "$HOME/.local/share/rookery/venv"
uv pip install --python "$HOME/.local/share/rookery/venv/bin/python" \
  ./rookery_agents-0.3.1-py3-none-any.whl
export PATH="$HOME/.local/share/rookery/venv/bin:$PATH"
```

### Configuring

Generate the node token as described in [Tokens](#tokens), then:

```sh
rookery configure --node alpha --url https://hub.example.com \
  --token-file ~/.config/rookery/token
rookery daemon      # foreground trial; stop it before installing the service
```

State lives in `~/.local/state/rookery` unless `ROOKERY_HOME` or `--state-dir`
says otherwise. Keep the same state directory for the CLI, the MCP servers and
the service. `config.json` there holds the node, the hub URL and the token, with
private permissions. Running `configure` again with the same node and URL
replaces the token and keeps other settings; changing the node or hub requires a
fresh state directory. The URL must be an HTTPS origin without a path;
`--allow-http-loopback` permits plain HTTP to localhost for development only.
Keep tokens and runtime state out of version control.

### Linux and WSL with systemd

Install from a host shell, using the installed executable. Pass `--state-dir`
explicitly, because `sudo` may change HOME:

```sh
sudo env PYTHONDONTWRITEBYTECODE=1 "$(command -v rookery)" \
  --state-dir "$HOME/.local/state/rookery" service install \
  --user "$(id -un)" --executable "$(command -v rookery)" --path "$PATH"
sudo env PYTHONDONTWRITEBYTECODE=1 "$(command -v rookery)" \
  --state-dir "$HOME/.local/state/rookery" service start
rookery service status
rookery doctor
```

Keep `PYTHONDONTWRITEBYTECODE=1` on these privileged invocations. It prevents
root from creating bytecode cache files inside the user-owned Python
installation, which would otherwise obstruct later upgrades.

`--backend auto` (the default) selects systemd when it is running. Installation
writes and enables `/etc/systemd/system/rookery-helper@USER.service`; it does
not start it. `--dry-run` prints what would be installed without writing
anything. The unit runs as `User=USER` with that account's home from the account
database and the saved PATH; no `/home/USERNAME` layout is assumed. Supply
`--path` when provider CLIs live in nvm or another nonstandard location.

```sh
journalctl -u "rookery-helper@$(id -un).service" -n 50
sudo env PYTHONDONTWRITEBYTECODE=1 "$(command -v rookery)" \
  --state-dir "$HOME/.local/state/rookery" service stop
```

The service restarts the helper after any unexpected exit, including exit code
zero, and sends SIGTERM for graceful shutdown. An explicit `service stop` leaves
it stopped. A daemon lock prevents two helpers in one state directory. Only one
installed helper service per Unix account is supported; use foreground mode and
separate state directories for isolated diagnostics.

### WSL without systemd

Rookery does not require enabling systemd in WSL. Installation selects the WSL
supervisor when WSL is detected and systemd is absent; `--backend wsl` selects
it explicitly:

```sh
sudo env PYTHONDONTWRITEBYTECODE=1 "$(command -v rookery)" \
  --state-dir "$HOME/.local/state/rookery" service install \
  --backend wsl --user "$(id -un)" \
  --executable "$(command -v rookery)" \
  --path "$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin"
rookery service start
rookery service status
```

Use a minimal Linux-only `--path`, adding the exact provider installation
directories, such as the active nvm Node `bin` directory for npm-based CLIs.
Do not copy WSL's inherited Windows PATH into a boot service: it adds unrelated
Windows executables and often contains spaces and parentheses.

Installation appends its launcher to the existing `[boot] command` in
`/etc/wsl.conf`, preserving other boot commands and sections. The launcher drops
privileges to the configured user before starting the supervisor. It runs when
the WSL distribution starts; it does not start a stopped distribution. WSL runs
boot commands as root through `/bin/sh -c` (see Microsoft's
[WSL configuration reference](https://learn.microsoft.com/en-us/windows/wsl/wsl-config)).

The supervisor holds its own process lock and restarts the helper after any
child exit, with backoff from one to sixty seconds. Logs go to `service.log` and
`service.log.1` in the state directory, about 1 MiB each. `service stop` asks the
supervisor and child to shut down, with a bounded forced-stop fallback.

### Manual operation

`rookery daemon` needs no service installation. `service install --backend
manual` saves a service configuration without writing system startup files;
`service start` then runs the same supervisor until explicitly stopped. Manual
mode does not start at boot.

### Upgrading

1. Install the new wheel as the account that owns the Python environment
   (`uv tool install --force ...` or `uv pip install ...`).
2. Check `rookery --help` runs, then restart the helper (`service stop` and
   `service start`, or `systemctl restart rookery-helper@USER.service`).
3. Refresh the MCP server in running sessions; see
   [Refreshing running sessions](#refreshing-running-sessions-after-an-upgrade).

Upgrade the hub first when a release notes a protocol change. If an earlier
privileged invocation created root-owned `__pycache__` files and uv reports a
permission error while removing them, repair ownership of those specific files
and repeat the installation. Do not recursively change ownership of unrelated
Python installations or runtime state.

## Session setup

Discover sessions with `rookery list`, enroll on the owning computer, and choose
an alias:

```sh
rookery enroll claude:EXACT_SESSION_UUID --alias reviewer
```

Address sessions on other computers by qualified alias, such as
`alpha/reviewer`. Native delivery is pinned to the enrolled process. When an
enrolled session is resumed in a new process, its helper sees the same UUID and
the hub re-pins the enrollment to the new process, keeping its alias. A UUID held
by two live processes at once is left out of native discovery. `rookery list`
reports `enrollment_current: false` and a `delivery_blocker` while an enrollment
is stale.

To move an alias from a closed session to a replacement:

```sh
rookery disable codex:OLD_SESSION_UUID
rookery clear-alias codex:OLD_SESSION_UUID
rookery enroll codex:NEW_SESSION_UUID --alias author
```

`clear-alias` accepts only an exact session address and works while the old
session is offline or disabled. It changes neither enrollment nor message
history. Disabling alone keeps the alias.

`rookery install` (below) configures the MCP server to derive each session's
identity from the calling harness. To bind one MCP instance by hand instead:

```sh
rookery mcp --session claude:EXACT_SESSION_UUID
```

The MCP process provides tools over stdio, uses the local mailbox files and
never replaces the helper. Do not share one fixed session binding across
unrelated conversations; it would misattribute their messages.

A running helper does not imply a running recipient. Diagnose in order: helper
connectivity, provider discovery, presence of the exact session, then whether
that session can accept input. Busy and waiting-for-permission differ from a
disconnected computer or an absent conversation. Codex native delivery needs a
thread loaded in the managed app-server daemon; historical threads and private
standalone app-servers are not loaded or resumed automatically.

## Provider integration

### Installing

`rookery install claude|codex|agy` prints a plan; `--apply` carries it out.

- MCP entries are added only through the harness's own CLI. An existing
  `rookery` entry is reported, never replaced. Codex entries are compared exactly
  (`codex mcp get --json`); Claude and Antigravity listings cannot prove argument
  boundaries, so a matching-looking entry is reported as unknown.
- Where Claude's MCP servers are under enterprise control
  (`/etc/claude-code/managed-mcp.json`), `claude mcp add` is refused; the plan
  says so and adds nothing. Delivery still works through the helper, and the
  session can use the `rookery` command line.
- For Antigravity the plan also writes `~/.gemini/config/plugins/rookery/`
  (`plugin.json`, `hooks.json`, `rules/AGENTS.md`). Replaced files get a
  `.bak.<time>` copy; symlinked targets are refused.
- The installer never grants, removes or rewrites tool permissions and generates
  no `PreToolUse` or `permissionOverrides` hooks.

### Claude: `--accept-inbound`

Claude Code asks for approval on each incoming cross-session message unless
`crossSessionInbound` is `accept` in `~/.claude/settings.json` (or under
`$CLAUDE_CONFIG_DIR`). Without the flag, `rookery install claude [--apply]`
leaves Claude's settings untouched. When the key is absent it reports:

> crossSessionInbound is not set, so Claude will ask for approval on each
> incoming Rookery message. `rookery install claude --accept-inbound` sets
> crossSessionInbound to "accept" in {path} for the whole account, so every
> session accepts incoming cross-session messages without asking.

`rookery install claude --accept-inbound --apply` sets the key to `accept`,
keeping the other keys and the file's mode, after a backup; a new file is created
with mode 0600. An explicit value other than `accept` (such as `hold` or
`refuse`) is never changed, and the plan says so. The flag applies to Claude
only; with `codex` or `agy` it fails with "--accept-inbound applies only to
claude".

The setting covers the whole Claude account, not only Rookery: it gives up
Claude's per-message approval for every cross-session message to every session
under that account.

### Antigravity attachment

`rookery attach agy` must run in the conversation's own shell. It pins the
owning `agy` process, detaches into its own session and serves a private socket
in the state directory, or in `$XDG_RUNTIME_DIR/rookery-<id>/`
(`/tmp/rookery-<uid>/rookery-<id>/` without `XDG_RUNTIME_DIR`) when that path is
too long for a Unix socket. For each claimed message it runs one
`agentapi send-message`; only an acceptance naming the same conversation counts
as submitted, and a timeout or ambiguous answer is uncertain and never retried.
The `agentapi` credentials stay in the endpoint's environment: they are never
sent to the helper or hub, written to records or logs, or passed as arguments.

The endpoint withdraws when `agy` exits or Antigravity reports the conversation
NotFound. `rookery attach agy --status` and `rookery doctor` report each
endpoint as attached, stale, superseded, detached or invalid, separately from
MCP connectivity. `rookery detach agy --conversation UUID` stops one.

The plugin hooks ask for attachment once per conversation, so a declined
permission prompt is respected. They ask again once in each new `agy` process
for a conversation that attached before, and once after an attached endpoint is
found dead. After `rookery detach` they stay quiet until the next explicit
attach. When a resumed conversation is not attached, check
`rookery attach agy --status` and run `rookery attach agy` by hand.

One `agy` process shows one top-level conversation at a time. When the hooks
fire for a confirmed top-level conversation, an endpoint the same `agy` process
still holds for another conversation is fenced and stopped. A submission already
in flight keeps its own outcome; queued ones fail without being submitted.
Antigravity subagents fire the same hooks under their own conversation IDs; they
never displace an endpoint, and `attach` refuses them. Rookery recognises them
from Antigravity's subagent records under `brain/<parent>/`, an observed file
layout rather than a documented interface; an unfamiliar layout yields
"unknown", and an unknown conversation is neither offered the attach nor allowed
to stop endpoints.

Antigravity fires no hook when `/clear` starts a new conversation; the first
hook comes with that conversation's first prompt. Until then the previous
conversation's endpoint stays attached, and a message delivered in that gap
lands in the conversation just cleared. Run
`rookery detach agy --conversation UUID` first when that matters.

### Refreshing running sessions after an upgrade

A session's Rookery MCP process keeps the code it started with. Upgrading the
package and restarting the helper does not change it.

- **Claude Code:** the user types `/mcp`, picks rookery and chooses Reconnect.
  Only the Rookery process restarts; the conversation continues. There is no
  model-callable equivalent.
- **Codex, app-server daemon:** `config/mcpServer/reload` alone does nothing
  while the configuration is unchanged. Back up `~/.codex/config.toml`, write one
  marker key with `config/value/write` (with `expectedVersion`), setting
  `mcp_servers.rookery.env.ROOKERY_MCP_VERSION` to the new version, then call
  `config/mcpServer/reload`. Every loaded thread reconnects at its next turn
  boundary, without restarting the daemon or TUI. Rookery does not read the
  marker. Verify per thread with `mcpServerStatus/list` (`serverInfo.version`).
  These are experimental app-server methods.
- **Codex without the daemon:** `codex resume <id>` starts a new MCP process.
- **Antigravity:** `/mcp`, then rookery, then Restart reloads its Rookery server
  in place. A conversation that was never attached also needs
  `rookery attach agy` from its own shell, and `rookery enroll` if it is new.

### Busy delivery

Busy delivery is on for Claude by default: a busy Claude session is written to,
and Claude reads the input between tool calls. A session waiting on a dialog, or
whose state is unknown, is not. Claude reports a session as busy while its
background subagents run, so idle-only delivery would leave orchestrating
sessions without their mail. Codex is always idle-only. An attached AGY endpoint
accepts input during a turn.

`rookery busy-delivery claude off|on` records an explicit choice in the helper
configuration; an explicit `off` stays off across upgrades and reconfiguration.
Either takes effect only after the helper restarts. `rookery doctor` shows
`busy_delivery` as configured, effective and `pending_restart`.

### Tool names

The MCP tools are `send`, `reply`, `inbox`, `status` and `peers` (shown as
`rookery.send`, `mcp__rookery__send` or `mcp(rookery/send)` depending on the
harness). The earlier `rookery_*` names stay callable but unlisted. Permission
grants that name the old tools must be renamed by the user; `rookery install`
lists the old-name grants it finds in the usual local settings files but does
not edit them.

### Message lifetimes

`send` defaults to a one-hour lifetime, so a stale request is not started late;
`reply` defaults to 24 hours, because a reply carries a result and its recipient
may be busy or briefly unreachable. `--ttl` sets seconds, at most 86400. Mail
that expires before it was delivered or read stays in the recipient's `inbox`,
marked expired, until it has been read once. Bodies are limited to 16 KiB of
UTF-8. A thread holds at most six messages.

## Hub deployment

The hub is one process with one SQLite database. Run one instance only; there is
no replication or failover.

```sh
rookery hub serve --db /var/lib/rookery-hub/hub.sqlite3 \
  --tokens /etc/rookery-hub/tokens.json
```

| Option | Default | Meaning |
| --- | --- | --- |
| `--db` | required | database path; its directory must be owned by the hub account and private (0700) |
| `--tokens` | required | node token-hash file |
| `--listen` | `127.0.0.1:8787` | public API listener (`POST /v2/rpc` only) |
| `--metrics-listen` | `127.0.0.1:8788` | private observability listener; must differ from `--listen` |
| `--max-bytes` | `100000000` | live database admission budget, 131072 to 500000000 |
| `--pending-limit` | `1000` | open (pending, read or dispatching) messages per recipient node, 1 to 10000 |
| `--snapshot-dir` | `<db dir>/backups` | private snapshot directory |
| `--snapshot-interval` | `86400` | seconds between snapshots, at least 1 |
| `--snapshot-keep` | `3` | snapshots retained, 1 to 3 |

Install the hub with the `hub` extra, which adds uvicorn:
`uv pip install --python /opt/rookery-hub/bin/python 'rookery-agents[hub]'` into
a dedicated environment, or `uv tool install 'rookery-agents[hub]'` (or the same
with the wheel path, as in the README).

### Tokens

Each node has its own token. Generate it on the node, store it with mode 0600,
and give the hub only its hash:

```sh
umask 077
mkdir -p ~/.config/rookery
python3 -c 'import secrets; print(secrets.token_urlsafe(32))' > ~/.config/rookery/token
python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1]).read().strip().encode()).hexdigest())' \
  ~/.config/rookery/token
```

The token-hash file is a JSON object from node name to the lowercase hex
SHA-256 of that node's token:

```json
{
  "alpha": "<64 hex characters>",
  "beta": "<64 hex characters>"
}
```

Rules the hub enforces on every request:

- The file is a regular file of at most 64 KiB, owned by the hub account with no
  group or other permissions (0600), or owned by root and not group- or
  world-writable. A Kubernetes Secret projection, which is root-owned, qualifies.
- Node names are a lowercase letter followed by up to 31 lowercase letters,
  digits or hyphens. Values are exactly 64 lowercase hex characters. Any
  malformed entry makes the whole file reject every token; a hash listed for two
  nodes authenticates neither.
- Tokens are 32 to 4096 characters, sent as `Authorization: Bearer`.

The hub rereads the file on each request, so changes need no restart, and a
request is re-authenticated before its response is sent.

**Rotation.** On the node, generate a new token file, run
`rookery configure` again with the same `--node` and `--url` and the new
`--token-file`, then write the new hash into the hub's file (an atomic replace,
for example writing a temporary file and `mv`) and restart the helper. Between
the two edits the node cannot authenticate; its saved outbox waits and retries.

**Revocation.** Remove the node's entry from the file. Its next request is
refused. The node's enrolled sessions and history remain in the database.

### Running under systemd

`/etc/systemd/system/rookery-hub.service`, for an installation in
`/opt/rookery-hub` serving as account `rookery-hub`:

```ini
[Unit]
Description=Rookery hub
After=network-online.target
Wants=network-online.target

[Service]
User=rookery-hub
Group=rookery-hub
ExecStart=/opt/rookery-hub/bin/rookery hub serve \
  --db /var/lib/rookery-hub/hub.sqlite3 \
  --tokens /etc/rookery-hub/tokens.json
Restart=on-failure
TimeoutStopSec=30
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/rookery-hub

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now rookery-hub.service
```

### Running in a container

The repository's Dockerfile builds a hub image that runs as UID 10001 with a
volume at `/data`; release builds publish it as `ghcr.io/llcoolkm/rookery`.
Tags are the release versions with a `v` prefix, such as `v0.3.1`; there is no
`latest` tag. Pin a version, and preferably the digest
(`ghcr.io/llcoolkm/rookery:v0.3.1@sha256:...`), so an update is a deliberate
change. Inside the container the listeners must bind all interfaces; publish
them on the host's loopback only:

```sh
sudo chown root:10001 /etc/rookery-hub/tokens.json
sudo chmod 0640 /etc/rookery-hub/tokens.json
docker run -d --name rookery-hub --restart unless-stopped \
  -v rookery-hub-data:/data \
  -v /etc/rookery-hub/tokens.json:/run/rookery/tokens.json:ro \
  -p 127.0.0.1:8787:8787 -p 127.0.0.1:8788:8788 \
  ghcr.io/llcoolkm/rookery:v0.3.1 \
  hub serve --db /data/state/hub.sqlite3 --tokens /run/rookery/tokens.json \
  --listen 0.0.0.0:8787 --metrics-listen 0.0.0.0:8788
```

### Kubernetes

[`deploy/kubernetes`](../deploy/kubernetes/README.md) holds plain manifests,
applied with `kubectl apply -k deploy/kubernetes`: one replica with strategy
`Recreate`, a ReadWriteOnce volume at `/data` (local or block storage, never a
network filesystem), the token-hash Secret mounted read-only, port 8787 for an
Ingress that routes only `/v2/rpc`, and port 8788 on a cluster-internal Service
restricted by a NetworkPolicy. Example Secret and Ingress files, and an optional
ServiceMonitor and PrometheusRule in `deploy/kubernetes/monitoring`, sit beside
them. A Grafana dashboard is in [`deploy/grafana`](../deploy/grafana/README.md).

### TLS reverse proxy

The hub speaks plain HTTP and does not trust forwarded headers. Put it behind a
TLS-terminating proxy that exposes only `POST /v2/rpc`, allows request bodies up
to 256 KiB, and allows at least 60 seconds per request: helpers long-poll for up
to 25 seconds.

Caddy, which obtains a certificate automatically:

```caddy
hub.example.com {
	request_body {
		max_size 256KB
	}
	handle /v2/rpc {
		reverse_proxy 127.0.0.1:8787
	}
	handle {
		respond 404
	}
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name hub.example.com;
    ssl_certificate     /etc/ssl/hub.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/hub.example.com/privkey.pem;

    client_max_body_size 256k;

    location = /v2/rpc {
        limit_except POST { deny all; }
        proxy_pass http://127.0.0.1:8787;
        proxy_http_version 1.1;
        proxy_read_timeout 60s;
        proxy_buffering off;
    }
    location / {
        return 404;
    }
}
```

Configure helpers with the origin, `https://hub.example.com`, without `/v2`.

### Keeping observability private

The observability listener serves `/healthz`, `/readyz`, `/metrics` and `/graph`
without authentication. `/graph` exposes session names and IDs, participants,
timestamps and counts (not message bodies or credentials). Never route it
through the public proxy. Keep the default loopback binding, or bind it to a
private interface that only your monitoring can reach:

```sh
rookery hub serve --db /var/lib/rookery-hub/hub.sqlite3 \
  --tokens /etc/rookery-hub/tokens.json \
  --listen 127.0.0.1:8787 --metrics-listen 172.16.0.5:8788   # private address
```

and restrict that port with a host firewall to the scraper's address.

### Storage limits

`--max-bytes` caps the live database. The hub refuses new writes (HTTP 429,
"hub admission quota reached", retryable) when use reaches the budget less a
reserve of 16 MiB or a fifth of the budget, whichever is smaller. A recipient
node with `--pending-limit` open messages refuses more, with the same retryable
429. Message bodies
are cleared seven days after a message reaches a final state, and the sync event
log keeps seven days. Budget disk for the database, its WAL (usually small after
checkpoints, but it can grow while a long read is open) and up to three snapshots, each about the
database's size. A volume size is not necessarily a quota: monitor actual free
space.

The hub also bounds its own metadata. It remembers request IDs for idempotency
only within a retry window (seven days), and caps the metadata each node can
accumulate, such as request records and historical sessions. Expired requests
are refused: a helper whose queued commands are older than the retry window
gets them rejected, not replayed. Replacing a refused or uncertain command
needs explicit reconciliation by the operator. The
[threat model](../THREAT_MODEL.md) gives the current limits and their
reasoning, including what downgrades, restores and agent side effects mean for
them.

### Backups

The hub takes consistent snapshots itself with SQLite's online backup API: at
start and then every `--snapshot-interval` seconds, into `--snapshot-dir`
(default `backups/` next to the database). The directory must be private (0700,
not a symlink). Each snapshot is written to a temporary file, fsynced and renamed
to `hub-<nanoseconds>.sqlite3`, so any file with that name is complete. Before
writing, the hub prunes so that at most `--snapshot-keep` remain, and skips the
snapshot if free space is below the database size plus 16 MiB.
`rookery_hub_snapshot_last_success_timestamp_seconds` records the last success.

Copy the newest snapshot off the host for real backups. Do not copy the live
database file alone; it may miss committed state in the WAL. For an ad hoc
backup use SQLite's backup command, for example
`sqlite3 /var/lib/rookery-hub/hub.sqlite3 ".backup /safe/place/hub.sqlite3"`.

#### Restoring

A restored hub has forgotten everything after the snapshot: messages, delivery
results and the request IDs it uses to recognise retries. A helper that talks to
it too early can push restored pending messages again, including ones the agent
already received, and resend commands from its saved outbox as new. Keep every
helper stopped until reconciliation is complete:

1. Stop **every helper on every node** (`service stop`, or end the foreground
   `rookery daemon`). Check with `rookery doctor` that none is running.
2. Stop the hub cleanly (`systemctl stop`, or scale the Deployment to 0) and
   keep the current database **together with its `-wal` and `-shm` files**,
   unchanged, in a private place. They hold the state after the snapshot and are
   the evidence for reconciliation. Do not open them read-write.
3. Restore into a clean path: a new private directory (0700, owned by the hub
   account) containing only the snapshot under the database's name (mode 0600),
   with no `-wal`, `-shm` or `.lock` file left beside it. Point `--db` at it, or
   move the old directory aside and recreate it empty.
4. Start the hub, then reconcile against the actual agent histories before any
   helper starts. List the open messages the restored hub will act on:

   ```sh
   sqlite3 -readonly /var/lib/rookery-hub/hub.sqlite3 \
     "SELECT id,status,sender_node,recipient_node FROM messages
      WHERE status IN ('pending','read','dispatching','uncertain') ORDER BY seq"
   ```

   Compare each with the recipient's conversation and with the kept database,
   and decide what each node may deliver or resend. Messages sent after the
   snapshot are gone from the hub; send them again deliberately if still needed
   (see [Message safety](#message-safety-and-recovery)).
5. Only then resume the helpers, one node at a time.

Restore has not been exercised as an acceptance test.

## Health and monitoring

```sh
rookery doctor                    # helper, hub connection, providers, AGY endpoints, busy delivery
rookery status MESSAGE_UUID       # one message
curl -s http://127.0.0.1:8788/readyz
curl -s http://127.0.0.1:8788/metrics
```

`/healthz` and `/readyz` report liveness and storage readiness. `/metrics` is in
Prometheus text format:

| Metric | Meaning |
| --- | --- |
| `rookery_hub_messages{state=...}` | retained messages by state |
| `rookery_hub_oldest_dispatching_age_seconds` | age of the oldest claimed, unfinished submission |
| `rookery_hub_db_bytes` | database plus WAL bytes |
| `rookery_hub_storage_bytes{file="database"\|"wal"}` | the two files separately |
| `rookery_hub_snapshot_last_success_timestamp_seconds` | last snapshot |
| `rookery_hub_node_last_seen_timestamp_seconds{node=...}` | last contact per node |
| `rookery_hub_requests_total`, `rookery_hub_api_requests_total` | requests by method and code (API excludes health, metrics and graph) |
| `rookery_hub_request_duration_seconds` | request duration histogram |

Snapshots are not included in the storage metrics. Useful alerts: storage near
`--max-bytes`, a stale snapshot timestamp, a node not seen for longer than
expected, a growing count of `uncertain` or old `dispatching` messages, and
sustained 429 or 503 responses.

`/graph` returns per-session communication edges and activity buckets from hub
metadata, for example for a Grafana Node Graph through a JSON datasource queried
server-side. Queries cover at most seven days, at most 10,000 newest messages,
500 sessions, 2,000 directed pairs and 300 time buckets, with truncation
reported. It is diagnostic and never changes delivery state.

A healthy hub does not prove that a recipient is loaded or eligible for input.

## Message safety and recovery

Saved locally, accepted by the hub, received by a host, submitted to a provider,
and answered by an agent are different observations. A host receipt is
automatic; it does not prove model comprehension. A reply confirms an
application response. A bound inbox fetch records a `read` receipt, which
suppresses a later native push for the returned messages. Read messages remain
available for recovery after a lost tool response: the CLI shows them whole,
and MCP `inbox` returns their bodies with `include_read: true`. Use `inbox --peek` or MCP
`peek: true` for non-consuming inspection. A native submission already in flight
cannot be retracted.

A submission whose outcome is unknown, including a claim that did not finish
within 90 seconds, becomes `uncertain` and is never retried automatically. First
inspect the actual recipient conversation. If it did not receive the message,
send a new one. Do not delete state or replay saved messages to force delivery.
Expiry prevents a new submission but cannot retract input a provider has already
accepted.

Message bodies are passed as process arguments to `codex queue` and
`agentapi send-message`. Any user who can list processes on that host can read
them during delivery. On multi-user hosts mount `/proc` with `hidepid=2` (or an
equivalent process isolation) before enabling those providers. See
[SECURITY.md](../SECURITY.md) to report a vulnerability.
