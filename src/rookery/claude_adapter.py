"""Native Claude inbox adapter, verified against 2.1.296 on Linux.

Frames are pinned to the destination session UUID. Peer attribution uses the
native cross-session-message envelope without claiming any permission class.
Native accepted submissions provide no positive receipt; only Rookery ack/reply
establishes receipt. No foreign tokens or reply sockets are used.
"""
from __future__ import annotations

import json
import os
import re
import socket
import stat
import subprocess
import time
import uuid
from pathlib import Path

PROVIDER = "claude"
TAG = "cross-session-message"
MAX_BODY = 60_000  # chars; native cap is ~1M serialized, keep courier mail small
_NAME_STRIP = re.compile(r'["<>\r\n]')
_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", re.I)
_SOCK_DIRS = [
    Path(f"/run/user/{os.getuid()}/cc-socks"),
    Path(f"/tmp/cc-socks-{os.getuid()}"),
    Path("/tmp/cc-socks"),
]

# --------------------------------------------------------------------------- discovery


def _sessions_dir() -> Path:
    return Path(os.environ.get("CLAUDE_CONFIG_DIR", Path.home() / ".claude")) / "sessions"


def _read_record(pid: int) -> dict:
    """Live-session record Claude Code itself uses for peer discovery
    (~/.claude/sessions/<pid>.json). Undocumented; treated as enrichment only."""
    try:
        return json.loads((_sessions_dir() / f"{pid}.json").read_text())
    except (OSError, ValueError):
        return {}


def _guess_socket(pid: int) -> str | None:
    for d in _SOCK_DIRS:
        p = d / f"{pid}.sock"
        if p.is_socket():
            return str(p)
    return None


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def discover(timeout: float = 15.0) -> list[dict]:
    """List live Claude Code sessions on this host.

    Primary source: `claude agents --json` (documented, stable). Fields used:
    pid, sessionId, name, cwd, kind, status (busy|waiting|idle), waitingFor.
    Enrichment: ~/.claude/sessions/<pid>.json for messagingSocketPath, tmux,
    version. Sessions without a reachable socket are reported with
    socket=None and are not deliverable.
    """
    out = subprocess.run(
        ["claude", "agents", "--json"], capture_output=True, text=True, timeout=timeout, check=False
    )
    if out.returncode != 0:
        raise RuntimeError(f"claude agents --json failed: {out.stderr.strip()[:200]}")
    rows = json.loads(out.stdout or "[]")
    result = []
    for r in rows:
        pid = r.get("pid")
        if pid is None or not _pid_alive(int(pid)):
            continue  # exited background sessions cannot receive
        if not r.get("sessionId") or not _UUID.match(r["sessionId"]):
            continue
        rec = _read_record(int(pid))
        sock = rec.get("messagingSocketPath") or _guess_socket(int(pid))
        if sock and not Path(sock).is_socket():
            sock = None
        result.append(
            {
                "provider": PROVIDER,
                "id": r.get("sessionId") or rec.get("sessionId"),
                "name": r.get("name") or rec.get("name"),
                "status": r.get("status") if r.get("status") in {"idle", "busy", "waiting"} else "unknown",
                "waiting_for": r.get("waitingFor"),
                "pid": int(pid),
                "cwd": r.get("cwd"),
                "kind": r.get("kind"),
                "socket": sock,
                "tmux": rec.get("tmux"),
                "version": rec.get("version"),
                "started_at": r.get("startedAt"),
                "deliverable": bool(sock),
            }
        )
    return result


# --------------------------------------------------------------------------- framing


def wrap(body: str, sender_name: str, from_addr: str | None = None) -> str:
    """Build Claude Code's peer envelope with ONLY from / from-name attributes.
    Attribute order (from, from-name) matches the native serializer; the
    receiver round-trips the header and ignores it when non-canonical."""
    attrs = []
    if from_addr:
        attrs.append(f'from="{from_addr}"')
    name = _NAME_STRIP.sub("", sender_name).strip()
    if name:
        attrs.append(f'from-name="{name}"')
    head = (" " + " ".join(attrs)) if attrs else ""
    body = re.sub(r"</(?=" + TAG + r"\s*>)", "<\\\\/", body)  # neutralise closing-tag lookalikes
    return f"<{TAG}{head}>\n{body}\n</{TAG}>"


def build_frame(body: str, sender_name: str, *, session_id: str | None, from_addr: str | None,
                priority: str = "next", msg_id: str | None = None) -> dict:
    if priority not in ("now", "next", "later"):
        raise ValueError("priority must be now|next|later")
    if not body or len(body) > MAX_BODY:
        raise ValueError(f"body must be 1..{MAX_BODY} chars")
    frame = {
        "msgV": 1,
        "msg_id": msg_id or str(uuid.uuid4()),
        "type": "user",
        "message": {"role": "user", "content": wrap(body, sender_name, from_addr)},
        "priority": priority,
    }
    if from_addr:
        frame["from"] = from_addr
    if session_id:
        if not _UUID.match(session_id):
            raise ValueError("session_id must be a UUID")
        frame["session_id"] = session_id  # receiver drops the frame on mismatch (verified)
    return frame



def submit(session: dict, text: str, *, sender_name: str = "rookery-local", allow_busy: bool = False) -> dict:
    """Write only to a pinned idle session, or a busy one when the operator
    opted in (Claude reads it between tool calls). A session waiting on a
    dialog is never written to. Sending is not proof of receipt."""
    allowed = {"idle", "busy"} if allow_busy else {"idle"}
    if session.get("status") not in allowed or not session.get("socket"):
        return {"status": "held", "detail": "no confirmed " + ("idle or busy" if allow_busy else "idle") + " native inbox"}
    if not _UUID.match(str(session.get("id", ""))):
        return {"status": "failed", "detail": "Claude destination must be an exact UUID"}
    sock_path = session["socket"]
    try:
        info = os.lstat(sock_path)
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
            return {"status": "failed", "detail": "inbox must be an owned socket, not a symlink"}
        # Display provenance only. All processes under this uid share trust.
        match = re.search(r"^From: ([^\r\n]+)$", text, re.MULTILINE)
        if match:
            sender_name = "rookery/" + match.group(1)
        message_id = re.search(r"^ID: ([0-9a-f-]{36})$", text, re.MULTILINE)
        frame = build_frame(text, sender_name, session_id=session["id"], from_addr=None,
                            msg_id=message_id.group(1) if message_id else None)
        data = (json.dumps(frame) + "\n").encode()
    except (OSError, ValueError) as exc:
        return {"status": "failed", "detail": str(exc)}
    sent_may_have_started = False
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(5)
            connection.connect(sock_path)
            sent_may_have_started = True
            connection.sendall(data)
        return {"status": "submitted", "detail": "frame written; native hold/loss and model receipt unconfirmed"}
    except OSError as exc:
        return {"status": "uncertain" if sent_may_have_started else "failed", "detail": str(exc)}
