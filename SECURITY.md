# Security policy

Rookery is experimental. Read [the threat model](THREAT_MODEL.md) before enrolling
agents or deploying a hub. Use mutually trusted participants, private credentials
and a protected HTTPS deployment. A node token permits sending arbitrary text
into enrolled peers' agent turns; agent permissions and user delegation remain
essential controls. Current Codex and AGY native delivery also exposes message
text in process arguments; private state files alone do not provide isolation
from other local users.

## Supported versions

Security fixes target the latest published release. Older releases have no
promised backports. Provider compatibility is version-dependent; an upstream
upgrade can change behavior even without a Rookery version change. Review the
README's compatibility information before upgrading production participants.

## Reporting a vulnerability

Do not put tokens, private transcripts, personal infrastructure details or
unredacted exploit payloads in a public issue or pull request.

Report vulnerabilities through the repository's
[private vulnerability reporting form](https://github.com/llcoolkm/rookery/security/advisories/new).
Do not assume an ordinary issue or discussion is private.

Include the affected Rookery and provider versions, relevant deployment/trust
boundaries, a redacted reproduction using disposable sessions, expected versus
observed behavior, and the impact. Never test against someone else's agents or
replay uncertain submissions without the operator's reconciliation.

If a token was exposed, revoke it and investigate accepted messages immediately;
waiting for a software fix does not contain a stolen credential. No guaranteed
response time, disclosure deadline or independent security certification is
promised.
