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
from pathlib import Path
import tempfile
from unittest.mock import patch
import uuid

from rookery.client import Client
from rookery.core import Error
from rookery.daemon import render
from rookery.hub import Hub, create_app
from rookery.mcp import Server
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


async def http_call(app, node, request, *, authenticated=True):
    # Ephemeral synthetic credentials never enter the generated JSON.
    credential = ("fixture-only-" + node + "-") * 4
    events = []
    async def receive():
        return {"type": "http.request", "body": json.dumps(request).encode()}
    async def send(event):
        events.append(event)
    await app({"type": "http", "method": "POST", "path": "/v2/rpc", "headers":
               [(b"authorization", ("Bearer " + credential).encode())] if authenticated else []}, receive, send)
    return events[0]["status"], json.loads(events[1]["body"])


def generate_vectors():
    vectors = {"format_version": 1, "hub": [], "delivery": [], "mcp": [], "local": [], "request_ids": []}
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
            def call(name, node, op, data=None, *, authenticated=True):
                request = {"op": op, "data": data or {}}
                status, response = asyncio.run(http_call(app, node, request, authenticated=authenticated))
                vectors["hub"].append(copy.deepcopy({"name": name, "node": node, "request": request, "status": status, "response": response}))
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
            for name, message in (("ask", ask), ("reply", reply), ("followup", followup), ("inform", inform)):
                text = render(message)
                vectors["delivery"].append({"name": name, "message": message, "envelope": json.loads(text.splitlines()[1]), "text": text})
            messages = {row["id"]: row for row in hub.call("alpha", "status")["messages"]}
            local_client = Client.__new__(Client)
            local_client.path = root
            admitted = []
            local_client._admit = admitted.append
            with patch("rookery.client.new_request_id", return_value=request_id()):
                queued = local_client.request("send", send_data, timeout=0)
            vectors["local"].append({"name": "queued_send", "request": admitted[0], "response": queued})
            _mcp_vectors(vectors, alpha, beta, ask, reply, followup, messages, queued)
        finally:
            hub.close()
    for when in (NOW, NOW - REQUEST_WINDOW, NOW + 301):
        rid = request_id(when)
        vectors["request_ids"].append({"id": rid, "created": request_created(rid)})
    vectors["request_ids"].append({"id": identifier(1), "created": request_created(identifier(1))})
    return vectors


def _mcp_vectors(vectors, alpha, beta, ask, reply, followup, messages, queued):
    class FixtureClient:
        def identity(self):
            return beta
        def peers(self):
            return {"sessions": [{"key": alpha, "status": "idle", "enrolled": True, "alias": "alpha", "name": "Alpha"}, {"key": beta, "status": "busy", "enrolled": True, "online": False}, {"key": "unlisted", "enrolled": False}], "stale": False}
        def send(self, **arguments):
            if arguments["body"] == "reject":
                raise Error("session is not enrolled")
            return Client.compact(queued if arguments["body"] == "queued" else ask)
        def reply(self, **arguments):
            return Client.compact(followup if arguments["message_id"] == reply["id"] else reply)
        def inbox(self, **arguments):
            return [Client.compact(messages[ask["id"]]), Client.compact(reply)]
        def status(self, message_id=None):
            return Client.compact(messages[ask["id"]]) if message_id else {"running": True, "connected": True}
    server = Server(FixtureClient())
    def exchange(name, method, params=None):
        request = {"jsonrpc": "2.0", "id": len(vectors["mcp"]) + 1, "method": method, "params": params or {}}
        vectors["mcp"].append({"name": name, "request": request, "response": server.handle(request)})
    exchange("initialize_required", "tools/list")
    # Freeze distribution metadata independently of the installation environment.
    with patch("rookery.mcp.PACKAGE_VERSION", "0.3.1"):
        exchange("initialize", "initialize", {"protocolVersion": "2025-06-18"})
    exchange("tools", "tools/list")
    def tool(name, operation, arguments=None):
        exchange(name, "tools/call", {"name": operation, "arguments": arguments or {}})
    tool("peers", "peers")
    tool("send_compact", "send", {"to": "beta", "body": "Review."})
    tool("send_queued", "send", {"to": "beta", "body": "queued"})
    tool("send_verbose", "send", {"to": "beta", "body": "Review.", "verbose": True})
    tool("reply_compact", "reply", {"message_id": ask["id"], "body": "Reviewed."})
    tool("followup_compact", "reply", {"message_id": reply["id"], "body": "Again."})
    tool("inbox_compact", "inbox")
    tool("inbox_include_read", "inbox", {"include_read": True})
    tool("inbox_peek", "inbox", {"peek": True})
    tool("health", "status")
    tool("message_status", "status", {"message_id": ask["id"]})
    tool("message_status_verbose", "status", {"message_id": ask["id"], "verbose": True})
    tool("legacy_tool", "rookery_status", {"message_id": ask["id"]})
    tool("operation_error", "send", {"to": "beta", "body": "reject"})
    tool("unknown_argument", "send", {"to": "beta", "body": "Review.", "actor": alpha})
    tool("invalid_boolean", "inbox", {"peek": 1})
    tool("invalid_integer", "send", {"to": "beta", "body": "Review.", "ttl": True})
    tool("missing_body", "send", {"to": "beta"})
    exchange("unknown_method", "unknown")


def main():
    value = generate_vectors()
    DESTINATION.parent.mkdir(parents=True, exist_ok=True)
    DESTINATION.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Generated {DESTINATION.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
