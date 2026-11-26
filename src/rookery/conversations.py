"""Read-only operator view of the hub's retained message history."""
import asyncio
import base64
import binascii
import re
import time
from pathlib import Path
from urllib.parse import parse_qs

from .common import canonical_id
from .core import Error
from .hub import MESSAGE_BODY_RETENTION, TERMINAL, Unavailable, authenticate_hash_file, encoded

PAGE_SIZE = 40
STATES = TERMINAL | {"pending", "read", "dispatching"}
LOCAL_HOST = re.compile(rb"(?:localhost|127\.0\.0\.1|\[::1\])(?::[0-9]{1,5})?", re.I)
ASSETS = {
    "/": ("index.html", b"text/html; charset=utf-8"),
    "/app.js": ("app.js", b"text/javascript; charset=utf-8"),
    "/style.css": ("style.css", b"text/css; charset=utf-8"),
}


def _participant(key, name):
    host, identity = key.split("/", 1)
    return {"id": key, "name": name or identity, "host": host}


def _thread_page(hub, query):
    try:
        if len(query) > 2048:
            raise ValueError()
        params = parse_qs(query.decode("ascii"), keep_blank_values=True, strict_parsing=True, errors="strict")
        if params.keys() - {"host", "agent", "status", "before"} or any(
            len(values) != 1 or len(values[0]) > 256 for values in params.values()
        ):
            raise ValueError()
        host, agent, status, cursor = (params.get(key, [""])[0] for key in ("host", "agent", "status", "before"))
        before = int(cursor) if cursor else 2**63 - 1
        if (status and status not in STATES) or not 1 <= before <= 2**63 - 1:
            raise ValueError()
    except (UnicodeError, ValueError):
        raise Error("invalid conversation filters") from None

    with hub._tx(write=False) as db:
        rows = db.execute("""
            WITH latest AS (
                SELECT MAX(seq) AS seq, COUNT(*) AS count FROM messages
                GROUP BY json_extract(data,'$.conversation') HAVING MAX(seq) < ?
            ), threads AS (
                SELECT json_extract(m.data,'$.conversation') AS id,
                    json_extract(m.data,'$.sender') AS sender,
                    json_extract(m.data,'$.recipient') AS recipient,
                    json_extract(m.data,'$.created') AS created,
                    m.sender_node, m.recipient_node, m.status, latest.count, m.seq
                FROM latest JOIN messages m USING (seq)
            )
            SELECT t.*, COALESCE(s.alias,json_extract(s.data,'$.name')) AS sender_name,
                COALESCE(r.alias,json_extract(r.data,'$.name')) AS recipient_name
            FROM threads t
            LEFT JOIN sessions s ON s.key=t.sender
            LEFT JOIN sessions r ON r.key=t.recipient
            WHERE (?='' OR t.status=?)
              AND (?='' OR instr(t.sender_node,?) OR instr(t.recipient_node,?))
              AND (?='' OR instr(lower(t.sender),?) OR instr(lower(t.recipient),?)
                   OR instr(lower(s.alias),?) OR instr(lower(r.alias),?)
                   OR instr(lower(json_extract(s.data,'$.name')),?)
                   OR instr(lower(json_extract(r.data,'$.name')),?))
            ORDER BY t.seq DESC LIMIT ?
            """, (before, status, status, *([host.lower()] * 3), *([agent.lower()] * 7), PAGE_SIZE + 1)).fetchall()
        threads = [{
            "id": row["id"],
            "sender": _participant(row["sender"], row["sender_name"]),
            "recipient": _participant(row["recipient"], row["recipient_name"]),
            "created": row["created"],
            "status": row["status"],
            "count": row["count"],
        } for row in rows[:PAGE_SIZE]]
    return {"threads": threads, "next_before": rows[PAGE_SIZE - 1]["seq"] if len(rows) > PAGE_SIZE else None}


def _transcript(hub, thread_id):
    canonical_id(thread_id)
    cutoff = time.time() - MESSAGE_BODY_RETENTION
    with hub._tx(write=False) as db:
        rows = db.execute("""
            SELECT m.id, m.status,
                json_extract(m.data,'$.sender') AS sender,
                json_extract(m.data,'$.recipient') AS recipient,
                COALESCE(s.alias,json_extract(s.data,'$.name')) AS sender_name,
                COALESCE(r.alias,json_extract(r.data,'$.name')) AS recipient_name,
                json_extract(m.data,'$.created') AS created,
                json_extract(m.data,'$.kind') AS kind,
                json_extract(m.data,'$.in_reply_to') AS in_reply_to,
                CASE WHEN retained=1 AND (terminal_at IS NULL OR terminal_at>=?)
                     THEN json_extract(m.data,'$.body') END AS body
            FROM messages m
            LEFT JOIN sessions s ON s.key=json_extract(m.data,'$.sender')
            LEFT JOIN sessions r ON r.key=json_extract(m.data,'$.recipient')
            WHERE json_extract(m.data,'$.conversation')=?
            ORDER BY created,seq
            """, (cutoff, thread_id)).fetchall()
        return {"messages": [{
            "id": row["id"],
            "sender": _participant(row["sender"], row["sender_name"]),
            "recipient": _participant(row["recipient"], row["recipient_name"]),
            "created": row["created"],
            "status": row["status"],
            "kind": row["kind"],
            "in_reply_to": row["in_reply_to"],
            "body": row["body"],
        } for row in rows]}


def _authorized(headers, password_hashes):
    values = [value for key, value in headers if key == b"authorization"]
    if len(values) != 1 or not values[0].startswith(b"Basic ") or len(values[0]) > 5500:
        return False
    try:
        username, _, password = base64.b64decode(values[0][6:], validate=True).decode("ascii").partition(":")
    except (binascii.Error, UnicodeError):
        return False
    return username == "viewer" and authenticate_hash_file(password_hashes, "Bearer " + password) == "viewer"


def create_conversations_app(hub, password_hashes):
    assets_dir = Path(__file__).with_name("conversations_ui")
    assets = {path: ((assets_dir / name).read_bytes(), mime) for path, (name, mime) in ASSETS.items()}
    password_hashes = Path(password_hashes)

    async def app(scope, receive, send):
        if scope["type"] != "http":
            return

        async def respond(code, payload, mime=b"application/json", extra=()):
            headers = [
                (b"content-type", mime),
                (b"cache-control", b"no-store"),
                (b"x-content-type-options", b"nosniff"),
                (b"referrer-policy", b"no-referrer"),
                (b"cross-origin-resource-policy", b"same-origin"),
                (b"cross-origin-opener-policy", b"same-origin"),
                (b"content-security-policy",
                 b"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "
                 b"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"),
                *extra,
            ]
            await send({"type": "http.response.start", "status": code, "headers": headers})
            body = payload if isinstance(payload, bytes) else encoded(payload).encode()
            await send({"type": "http.response.body", "body": body})

        headers = scope.get("headers", [])
        hosts = [value for key, value in headers if key == b"host"]
        if len(hosts) != 1 or not LOCAL_HOST.fullmatch(hosts[0]):
            await respond(400, {"error": "invalid local host"})
            return
        sites = [value for key, value in headers if key == b"sec-fetch-site"]
        if sites and sites not in ([b"same-origin"], [b"none"]):
            await respond(403, {"error": "cross-origin request refused"})
            return
        if not _authorized(headers, password_hashes):
            await respond(401, {"error": "authentication required"},
                          extra=[(b"www-authenticate", b'Basic realm="Rookery Conversations"')])
            return
        if scope.get("method") != "GET":
            await respond(405, {"error": "method not allowed"}, extra=[(b"allow", b"GET")])
            return
        path = scope["path"]
        try:
            if path in assets:
                await respond(200, *assets[path])
            elif path == "/api/conversations":
                await respond(200, await asyncio.to_thread(_thread_page, hub, scope.get("query_string", b"")))
            elif path.startswith("/api/conversations/"):
                result = await asyncio.to_thread(_transcript, hub, path.removeprefix("/api/conversations/"))
                if result["messages"]:
                    await respond(200, result)
                else:
                    await respond(404, {"error": "conversation not found"})
            else:
                await respond(404, {"error": "not found"})
        except Unavailable:
            await respond(503, {"error": "hub storage unavailable"})
        except Error:
            await respond(400, {"error": "invalid conversation request"})

    return app
