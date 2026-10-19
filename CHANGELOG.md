# Changelog

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
