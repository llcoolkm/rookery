"""Regenerate public cross-language fixtures: uv run --frozen --extra hub python scripts/generate_contract_vectors.py.

All identities and credentials here are synthetic. No network or user state is read.
Fixtures are derived from Hub/create_app, Client.compact, MCP Server and render,
not a second implementation of their response formats.
"""
from __future__ import annotations

import asyncio
import copy
import hashlib
import json
import io
from pathlib import Path
import tempfile
from unittest.mock import Mock, patch
import urllib.error
import uuid

from rookery.client import Client, configure
from rookery.daemon import HubClient, render
from rookery.core import Error
from rookery.common import RemoteError
from rookery.hub import Hub, Unavailable, create_app
from rookery.mcp import Server, serve, MAX_LINE, codex_client_resolver
from rookery.local_store import Store
from rookery.common import atomic
from rookery.request_ids import new_request_id, request_created, REQUEST_WINDOW

ROOT = Path(__file__).resolve().parents[1]
DESTINATION = ROOT / "go" / "contract" / "testdata" / "contract.json"
NOW = 1800000000.0


def identifier(number):
    """Clearly synthetic canonical UUIDs; never discovered session identities."""
    return f"00000000-0000-4000-8000-{number:012d}"


def request_id(when=NOW):
    with patch("rookery.request_ids.time.time", return_value=when), patch("rookery.request_ids.secrets.randbits", return_value=0):
        return new_request_id()


async def http_call(app, node, request, *, authenticated=True, path="/v2/rpc"):
    # Ephemeral synthetic credentials never enter the generated JSON.
    credential = ("fixture-only-" + node + "-") * 4
    events = []
    async def receive():
        return {"type": "http.request", "body": json.dumps(request).encode()}
    async def send(event):
        events.append(event)
    await app({"type": "http", "method": "POST", "path": path, "headers":
               [(b"authorization", ("Bearer " + credential).encode())] if authenticated else []}, receive, send)
    return events[0]["status"], json.loads(events[1]["body"])


def generate_vectors():
    vectors = {"format_version": 1, "hub": [], "delivery": [], "mcp": [], "local": [], "request_ids": [], "mcp_frames": []}
    # Pin helper diagnostics independently of the hub's unchanged wire error.
    transport = HubClient({'node': 'alpha', 'url': 'https://hub.example', 'token': 'x' * 32})
    transport.opener = Mock()
    transport.opener.open.side_effect = urllib.error.HTTPError('https://hub.example', 401, '', {}, None)
    try:
        transport.call('list')
    except Error as exc:
        vectors['helper_http'] = [{'node': 'alpha', 'status': 401, 'error': str(exc),
                                   'retryable': not isinstance(exc, RemoteError)}]
    alpha = "alpha/claude:" + identifier(1)
    beta = "beta/codex:" + identifier(2)
    with tempfile.TemporaryDirectory() as temporary, patch("rookery.hub.time.time", return_value=NOW):
        root = Path(temporary)
        tokens = root / "credentials.json"
        tokens.write_text(json.dumps({node: hashlib.sha256((("fixture-only-" + node + "-") * 4).encode()).hexdigest() for node in ("alpha", "beta")}))
        tokens.chmod(0o600)
        hub = Hub(root / "hub.sqlite3")
        try:
            app = create_app(hub, tokens)
            def call(name, node, op, data=None, *, authenticated=True, path="/v2/rpc"):
                request = {"op": op, "data": data or {}}
                status, response = asyncio.run(http_call(app, node, request, authenticated=authenticated, path=path))
                limits = {"request_too_large": {"max_request": 1}, "quota_overloaded": {"max_requests": 0}}
                vectors["hub"].append(copy.deepcopy({"name": name, "node": node, "path": path, "request": request, "status": status, "response": response,
                    **({"limits": limits[name]} if name in limits else {}),
                    **({"fault": "storage_unavailable"} if name == "storage_unavailable" else {})}))
                return copy.deepcopy(response.get("result"))
            call("unauthorized", "alpha", "list", authenticated=False)
            for node, provider, number, pid in (("alpha", "claude", 1, 101), ("beta", "codex", 2, 202)):
                call("publish_" + node, node, "publish", {"sessions": [{"provider": provider, "id": identifier(number), "name": node, "status": "idle", "pid": pid}]})
                call("enroll_" + node, node, "enroll", {"target": f"{node}/{provider}:" + identifier(number), "alias": node})
            call("directory", "alpha", "list")
            send_data = {"actor": alpha, "to": "beta", "body": 'Review “alpha”.\nSecond line\u0085\u2028\u2029', "id": identifier(10), "ttl": 3600, "expires": NOW + 3600, "_request_id": request_id()}
            ask = call("send_ask", "alpha", "send", send_data)
            call("retry_same_request", "alpha", "send", send_data)
            call("request_content_conflict", "alpha", "send", {**send_data, "body": "Changed"})
            call("received", "beta", "received", {"message_id": ask["id"]})
            with patch("rookery.hub.uuid.uuid4", return_value=uuid.UUID(identifier(90))):
                delivery = call("claim", "beta", "claim", {"message_id": ask["id"]})["delivery"]
            call("submitted", "beta", "result", {"message_id": ask["id"], "claim_id": delivery["claim_id"], "status": "submitted", "detail": "accepted by provider"})
            call("read", "beta", "read", {"actor": beta, "message_ids": [ask["id"]]})
            reply = call("reply", "beta", "reply", {"actor": beta, "message_id": ask["id"], "id": identifier(11), "body": "Reviewed.", "ttl": 86400})
            followup = call("followup", "alpha", "reply", {"actor": alpha, "message_id": reply["id"], "id": identifier(12), "body": "Check once more."})
            call("duplicate_followup", "alpha", "reply", {"actor": alpha, "message_id": reply["id"], "id": identifier(13), "body": "Again."})
            call("ack", "beta", "ack", {"actor": beta, "message_id": followup["id"]})
            inform = call("send_inform", "alpha", "send", {"actor": alpha, "to": beta, "id": identifier(14), "kind": "inform", "body": "For information."})
            call("inform_reply_refused", "beta", "reply", {"actor": beta, "message_id": inform["id"], "id": identifier(15), "body": "Answer."})
            call("cancel", "beta", "reconcile", {"message_id": inform["id"], "action": "cancel"})
            call("bootstrap", "alpha", "bootstrap", {"limit": 2})
            call("sync", "beta", "sync", {"cursor": 0, "limit": 2, "wait": 0})
            call("inbox", "beta", "inbox", {"actor": beta})
            call("status", "alpha", "status")
            call("no_pending_claim", "beta", "claim")
            call("disable", "beta", "disable", {"target": beta})
            call("clear_alias", "beta", "clear-alias", {"target": beta})
            for name, when in (("expired_request", NOW - REQUEST_WINDOW), ("future_request", NOW + 301)):
                call(name, "alpha", "enroll", {"target": alpha, "_request_id": request_id(when)})
            call("invalid_body", "alpha", "send", {"actor": alpha, "to": beta, "id": identifier(16), "body": " "})
            call("unknown_operation", "alpha", "unknown")
            call("not_found", "alpha", "list", path="/missing")
            with patch("rookery.hub.MAX_REQUEST", 1):
                call("request_too_large", "alpha", "list")
            with patch("rookery.hub.MAX_REQUESTS", 0):
                call("quota_overloaded", "alpha", "enroll", {"target": alpha, "_request_id": request_id(NOW + 1)})
            with patch.object(hub, "call", side_effect=Unavailable()):
                call("storage_unavailable", "alpha", "list")
            # A held result releases only the definite pre-I/O claim. Re-enroll
            # the recipient after the earlier disable/clear-alias cases.
            call("reenroll_beta", "beta", "enroll", {"target": beta})
            held = call("send_held", "alpha", "send", {"actor": alpha, "to": beta, "id": identifier(19), "body": "Hold before I/O."})
            with patch("rookery.hub.uuid.uuid4", return_value=uuid.UUID(identifier(91))):
                held_claim = call("claim_held", "beta", "claim", {"message_id": held["id"]})["delivery"]
            call("result_held", "beta", "result", {"message_id": held["id"], "claim_id": held_claim["claim_id"], "status": "held", "detail": ""})
            snapshots = call("empty_detail_status", "alpha", "status")["messages"]
            empty_detail = next(row for row in snapshots if row["id"] == held["id"])
            thread = call("hop_root", "alpha", "send", {"actor": alpha, "to": beta, "id": identifier(20), "body": "Bounded thread."})
            for hop in range(1, 7):
                node, actor = ("beta", beta) if hop % 2 else ("alpha", alpha)
                result = call("hop_limit" if hop == 6 else "hop_" + str(hop), node, "reply",
                    {"actor": actor, "message_id": thread["id"], "id": identifier(20 + hop), "body": "Thread continuation."})
                if result is not None:
                    thread = result
            with hub._tx() as db:
                db.execute("UPDATE metadata SET value='1' WHERE key='event_floor'")
            call("sync_reset_required", "beta", "sync", {"cursor": 0, "wait": 0})
            for name, message in (("ask", ask), ("reply", reply), ("followup", followup), ("inform", inform), ("empty_detail", empty_detail)):
                text = render(message)
                vectors["delivery"].append({"name": name, "message": message, "envelope": json.loads(text.splitlines()[1]), "text": text})
            messages = {row["id"]: row for row in hub.call("alpha", "status")["messages"]}
            local_root = root / "local"
            configure(local_root, "beta", "https://hub.invalid", "fixture-only-token-" * 4)
            local_client = Client(local_root, actor=beta)
            store = Store(local_root)
            with patch("rookery.client.new_request_id", return_value=request_id()):
                queued = local_client.request("send", send_data, timeout=0)
            spool_path = local_root / "spool" / (queued["request_id"] + ".json")
            admitted = json.loads(spool_path.read_text())
            store.import_spool()
            vectors["local"].append({"name": "queued_send", "request": admitted, "response": queued,
                                     "imported": store.commands()[0], "spool_removed": not spool_path.exists()})
            _mcp_vectors(vectors, local_client, store, alpha, beta, ask, reply, followup, messages)

        finally:
            hub.close()
    for when in (NOW, NOW - REQUEST_WINDOW, NOW + 301):
        rid = request_id(when)
        vectors["request_ids"].append({"id": rid, "created": request_created(rid)})
    vectors["request_ids"].append({"id": identifier(1), "created": request_created(identifier(1))})
    return vectors


def _mcp_vectors(vectors, client, store, alpha, beta, ask, reply, followup, messages):
    # The real Client reads synthetic helper snapshots and persists every
    # mutation through its actual atomic spool admission and import path.
    directory = {"sessions": [{"key": alpha, "status": "idle", "enrolled": True, "alias": "alpha", "name": "Alpha"},
                               {"key": beta, "status": "busy", "enrolled": True, "online": False},
                               {"key": "unlisted", "enrolled": False}], "updated": NOW}
    atomic(client.path / "directory.json", directory)
    first = {**messages[ask["id"]], "status": "read", "read_at": NOW}
    second = {**followup, "status": "pending", "read_at": None}
    store.apply([{"type": "message", "data": first}, {"type": "message", "data": second}], 0)
    server = Server(client)
    def exchange(name, method, params=None):
        request = {"jsonrpc": "2.0", "id": len(vectors["mcp"]) + 1, "method": method, "params": params or {}}
        # Pre-existing helper result files model the other local process. The
        # Client still writes the command spool and reads the result itself.
        rid = request_id(NOW + len(vectors["mcp"]) + 1)
        result_path = client.path / "results" / (rid + ".json")
        operation = (params or {}).get("name", "")
        arguments = (params or {}).get("arguments", {})
        if name == "operation_error":
            atomic(result_path, {"ok": False, "error": "session is not enrolled"})
        elif operation in ("send", "reply") and name != "send_queued":
            value = ask if operation == "send" else followup if arguments.get("message_id") == reply["id"] else reply
            atomic(result_path, {"ok": True, "result": value})
        with patch("rookery.client.new_request_id", return_value=rid), patch("rookery.client.uuid.uuid4", return_value=uuid.UUID(identifier(80))), patch("rookery.client.time.monotonic", side_effect=[0, 0, 6] * 10):
            response = server.handle(request)
        record = {"name": name, "request": request, "response": response}
        spool_path = client.path / "spool" / (rid + ".json")
        if spool_path.exists():
            record["spooled"] = json.loads(spool_path.read_text())
        if operation == "inbox":
            record["pending_reads"] = [list(pair) for pair in sorted(store.pending_reads())]
        vectors["mcp"].append(record)
    exchange("initialize_required", "tools/list")
    # Freeze distribution metadata independently of the installation environment.
    with patch("rookery.mcp.PACKAGE_VERSION", "0.3.1"):
        exchange("initialize", "initialize", {"protocolVersion": "2025-06-18"})
    exchange("tools", "tools/list")
    def tool(name, operation, arguments=None):
        exchange(name, "tools/call", {"name": operation, "arguments": arguments or {}})
    tool("peers", "peers")
    atomic(client.path / "directory.json", {**directory, "updated": NOW - 46})
    tool("peers_stale", "peers")
    (client.path / "directory.json").unlink()
    tool("peers_missing", "peers")
    tool("send_compact", "send", {"to": "beta", "body": "Review."})
    tool("send_queued", "send", {"to": "beta", "body": "queued"})
    tool("send_verbose", "send", {"to": "beta", "body": "Review.", "verbose": True})
    tool("reply_compact", "reply", {"message_id": ask["id"], "body": "Reviewed."})
    tool("followup_compact", "reply", {"message_id": reply["id"], "body": "Again."})
    tool("inbox_compact", "inbox")
    tool("inbox_include_read", "inbox", {"include_read": True})
    tool("inbox_peek", "inbox", {"peek": True})
    tool("health_missing", "status")
    atomic(client.path / "health.json", {"running": True, "connected": True, "updated": NOW})
    tool("health", "status")
    tool("unknown_local", "status", {"message_id": identifier(99)})
    tool("message_status", "status", {"message_id": ask["id"]})
    tool("message_status_verbose", "status", {"message_id": ask["id"], "verbose": True})
    tool("legacy_tool", "rookery_status", {"message_id": ask["id"]})
    tool("operation_error", "send", {"to": "beta", "body": "reject"})
    tool("unknown_argument", "send", {"to": "beta", "body": "Review.", "actor": alpha})
    tool("invalid_boolean", "inbox", {"peek": 1})
    tool("invalid_integer", "send", {"to": "beta", "body": "Review.", "ttl": True})
    tool("missing_body", "send", {"to": "beta"})
    exchange("unknown_method", "unknown")
    identity_server = Server(client_resolver=codex_client_resolver(client.path))
    identity_server.initialized = True
    identity_request = {"jsonrpc": "2.0", "id": 100, "method": "tools/call", "params": {"name": "status", "arguments": {}}}
    vectors["mcp"].append({"name": "missing_caller_identity", "request": identity_request, "response": identity_server.handle(identity_request)})
    for name, frame in (("parse_error", b"{\n"), ("invalid_utf8", b"\xff\n"),
                        ("invalid_request", b"[]\n"),
                        ("oversize_then_ping", b"x" * (MAX_LINE + 1) + b"\n" + b'{"jsonrpc":"2.0","id":1,"method":"ping"}\n'),
                        ("notification_then_ping", b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n' + b'{"jsonrpc":"2.0","id":1,"method":"ping"}\n')):
        output = io.BytesIO()
        serve(client, io.BytesIO(frame), output)
        # Oversize input is described by a repeat count to keep public vectors
        # small; replay reconstructs precisely the same bytes.
        vectors["mcp_frames"].append({"name": name,
            "input_hex": frame.hex() if len(frame) <= MAX_LINE else None,
            **({"repeat": "x", "count": MAX_LINE + 1, "suffix": '\n{"jsonrpc":"2.0","id":1,"method":"ping"}\n'} if len(frame) > MAX_LINE else {}),
            "responses": [json.loads(line) for line in output.getvalue().splitlines()]})


def main():
    value = generate_vectors()
    DESTINATION.parent.mkdir(parents=True, exist_ok=True)
    DESTINATION.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Generated {DESTINATION.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
