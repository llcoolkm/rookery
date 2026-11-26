# Changelog

## 0.3.3

A read-only dashboard on the hub: conversations and the state of every session
in the fleet. The hub protocol gains optional fields only; 0.3.2 helpers keep
working against a 0.3.3 hub.

- Conversations dashboard. `hub serve --conversations-listen HOST:PORT
  --conversations-password-hash-file FILE` starts a separate read-only listener
  with its own `viewer` credential: recent threads, host, agent and status
  filters, and a chronological transcript with reply links and delivery states.
  Both flags must be given together and the hash file is validated at startup.
  Keep the listener on loopback and off any ingress; see `docs/operations.md`.
- Sessions view on the same listener, and `rookery sessions [--watch N]` in the
  terminal: every session with its node, provider, name, status, what it is
  waiting for, time in that status, tmux `session:window` and working
  directory. Waiting sessions sort first. `GET /sessions` serves the same data
  as JSON behind the viewer credential.
- `publish` accepts optional `waiting_for`, `tmux` and `cwd` per session. The
  hub records `status_since` itself. `cwd` is shown only to the session's own
  node and to the viewer.
- An invalid optional value is dropped and counted instead of failing the
  publish: the reply carries `invalid_metadata`, helpers log it, and the hub
  exports `rookery_hub_invalid_session_metadata_total`.
- The hub keeps terminal message bodies for 30 days (was seven), so the
  dashboard can show recent transcripts. Anything that can read the hub
  database or sign in as `viewer` can read them for that long.
- The Go helper publishes the same session details on Linux and has the
  `sessions` command. Releases still ship no Go binaries.

## 0.3.2

Mail no longer expires silently, and new sessions no longer start unreachable.
The hub protocol is unchanged apart from the added fields below; 0.3.1 helpers
keep working against a 0.3.2 hub.

- `send` defaults to a 12-hour lifetime (was one hour) in the client, the CLI,
  the MCP `send` tool and the hub. `reply` stays at 24 hours.
- Expired messages say why: `detail` names the recipient's state at expiry
  (`waiting on a dialog`, `busy`, `offline`, ...) and the last hold reason;
  `expired_while` gives the state alone. The sender's helper puts one notice
  into the sending session when its message expired undelivered.
- `rookery auto-enroll on|off` enrolls every session the helper discovers (off
  by default, applied at the next discovery, kept by `configure`, shown by
  `doctor`). The hub records `disable` and lists it as `disabled`; a disabled
  session is never auto-enrolled.
- The hub forgets sessions unseen for seven days, enrolled or not; aliased and
  disabled sessions are kept.
- Enrollment errors name the session, say whether sender or recipient, and give
  the next step.
- The repository now includes the experimental native Go helper (`go/`, see
  `docs/go-port.md`). It supports `send` defaults, `auto-enroll` and the
  `disabled` marker, but does not yet post expiry notices. Releases ship no Go
  binaries.

## 0.3.1

Reduce the tokens Rookery puts in front of a model. Client methods, the CLI, the
hub protocol, read durability, delivery and idempotency are unchanged.

- Native delivery text is shorter: one boundary line, a compact one-line JSON
  envelope, and a reply hint for asks only. `conversation` is omitted when it
  equals `in_reply_to`.
- MCP `send` and `reply` return `id`, the resolved recipient as `to`, the stored
  `kind` and `status`, plus every reconciliation field the result carries,
  without the body echo. `status(message_id)` omits the body. `verbose: true`
  returns the full record.
- MCP `inbox` summarises messages already fetched (`body_omitted: true`);
  unread mail and mail that expired unread stay whole. `include_read: true`
  returns every body.
- MCP `peers` rows are `key` and `status`, with `name` and `alias` only when
  they add information; offline peers read `unknown`.
- Shorter MCP instructions and tool descriptions; updated agent rules preserve
  delegation and recovery safeguards. String limits moved out of the public
  tool schema; validation is unchanged.

## 0.3.0

First public release.
