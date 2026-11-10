# Changelog

## Unreleased

- `send` defaults to a 12-hour lifetime (was one hour) in the Python and Go
  clients, the CLI, the MCP `send` tool and the hub. `reply` stays at 24 hours.
  A recipient that is busy for a long turn or held by an open dialog no longer
  loses mail after an hour.
- Expired messages say why: `detail` names the recipient's state at expiry
  (`waiting on a dialog`, `busy`, `offline`, ...) and the last hold reason;
  `expired_while` gives the state alone. The Python helper puts one notice
  into the sending session when its message expired undelivered.
- `rookery auto-enroll on|off` (Python helper) enrolls every discovered
  session. The hub records `disable`, lists it as `disabled`, and such a
  session is never auto-enrolled.
- The hub forgets sessions unseen for seven days, enrolled or not; aliased and
  disabled sessions are kept.
- Enrollment errors name the session, say whether sender or recipient, and give
  the next step.
- Go helper: `rookery auto-enroll on|off` enrolls every session the helper
  discovers (off by default, read at each discovery, kept by `configure`,
  shown by `doctor`). A session the hub reports as disabled is never
  auto-enrolled. Needs a hub that reports `disabled`.

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
