"""Attached Antigravity (AGY) endpoint: the helper's delivery route into one conversation.

Antigravity grants `agentapi` credentials (the language-server address and a CSRF
token) only to processes started from a conversation's own shell; MCP servers and
hooks never receive them. `rookery attach agy` is therefore run from that shell.
It pins the owning `agy` process, detaches into its own session, and serves a
private Unix socket. For each helper request it executes exactly one
`agentapi send-message`. It never contacts the hub, never claims, never retries,
and never stores, logs or returns the credential values.

The helper keeps the claim, single attempt, expiry and result semantics shared
with every provider. `submitted` means agentapi accepted the input for the exact
conversation; it is not model receipt.
"""
from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import select
import shutil
import signal
import socket
import stat
import struct
import subprocess
import sys
import time
import uuid

from .core import Error
from . import common, polling

PROVIDER = "agy"
CONVERSATION = "ANTIGRAVITY_CONVERSATION_ID"
CREDENTIALS = ("ANTIGRAVITY_LS_ADDRESS", "ANTIGRAVITY_CSRF_TOKEN")
AGENTAPI = "ANTIGRAVITY_AGENTAPI_EXE"
HEARTBEAT = 5          # seconds between liveness checks and record refreshes
STALE = 20             # a record older than this is not deliverable
SUBMIT_TIMEOUT = 20    # agentapi send-message deadline
CLIENT_TIMEOUT = SUBMIT_TIMEOUT + 10
MAX_TEXT = 60_000      # characters, matching the Claude courier cap
MAX_TEXT_BYTES = 120_000  # Linux MAX_ARG_STRLEN is 131072 per argument
MAX_FRAME = 256 * 1024
MAX_RECORD = 4096
MAX_LOG = 1024 * 1024  # bytes before the event log rotates once
MAX_ANCESTRY = 32
READY_TIMEOUT = 10
LOCK_WAIT = 2          # seconds attach waits out a transient cleanup lock
METADATA_INTERVAL = 30  # seconds between conversation-existence checks
BRAIN = Path(".gemini") / "antigravity-cli" / "brain"
RECHECK = 0.5           # seconds before re-confirming a top-level conversation


# --------------------------------------------------------------------------- paths


def _uuid(value):
    try:
        canonical = str(uuid.UUID(value))
    except (ValueError, TypeError, AttributeError):
        raise Error("AGY conversation ID must be a canonical UUID") from None
    if canonical != value:
        raise Error("AGY conversation ID must be a canonical UUID")
    return canonical


def directory(ledger):
    path = common.directory(ledger) / "endpoints"
    path.mkdir(mode=0o700, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise Error("endpoint directory must be private and owned by this user")
    return path


SOCKET_PATH_MAX = 100  # sockaddr_un.sun_path is 108 bytes on Linux, including NUL


def _runtime_tag(ledger):
    return "rookery-" + hashlib.sha256(str(Path(ledger.path).resolve()).encode()).hexdigest()[:12]


def _runtime_directory(ledger):
    """Short private socket directory for state paths too long for AF_UNIX.

    Keyed by the state directory so separate helpers never share sockets. Its
    base comes from the creating shell's environment, so readers validate the
    recorded path (see _valid_socket) instead of recomputing it.
    """
    base = Path(os.environ.get("XDG_RUNTIME_DIR") or f"/tmp/rookery-{os.getuid()}")
    path = base / _runtime_tag(ledger)
    base.mkdir(mode=0o700, exist_ok=True)
    path.mkdir(mode=0o700, exist_ok=True)
    for item in (base, path):
        info = item.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or (item == path and info.st_mode & 0o077):
            raise Error("endpoint runtime directory must be private and owned by this user")
    return path


def _paths(ledger, conversation):
    base = directory(ledger) / f"{PROVIDER}-{conversation}"
    sock = base.with_suffix(".sock")
    if len(os.fsencode(str(sock))) > SOCKET_PATH_MAX:
        sock = _runtime_directory(ledger) / f"{PROVIDER}-{conversation}.sock"
    return {"record": base.with_suffix(".json"), "socket": sock,
            "lock": base.with_suffix(".lock"), "log": base.with_suffix(".log")}


# --------------------------------------------------------------------------- process identity


def _stat_fields(pid):
    """Return (comm, state, ppid, starttime) from /proc/<pid>/stat."""
    raw = (polling.PROC / str(pid) / "stat").read_text()
    end = raw.rfind(")")
    start = raw.find("(")
    if start < 0 or end < start:
        raise Error("invalid process stat")
    fields = raw[end + 1:].split()
    if len(fields) < 20 or not fields[1].isdigit() or not fields[19].isdigit():
        raise Error("invalid process stat")
    return raw[start + 1:end], fields[0], int(fields[1]), fields[19]


def find_agy(start=None):
    """Walk the parent chain from this process to the owning `agy` CLI process."""
    pid = os.getppid() if start is None else start
    for _ in range(MAX_ANCESTRY):
        if pid <= 1:
            break
        try:
            comm, _, ppid, _ = _stat_fields(pid)
            owner = (polling.PROC / str(pid)).stat().st_uid
        except (OSError, Error):
            break
        if owner != os.getuid():
            break
        if comm == "agy":
            return pid
        pid = ppid
    raise Error("no owning agy process found; run `rookery attach agy` from the AGY conversation's own shell")


def _pin(pid):
    return polling._pin(pid)


def _pinned(pid, pin):
    try:
        return _pin(pid) == pin
    except Error:
        return False


# --------------------------------------------------------------------------- conversations


def brain_directory(artifact=None, conversation=None):
    """AGY's per-conversation store.

    A hook payload names this conversation's own directory inside it; otherwise
    the store under the agy process's HOME, which hooks and shells inherit.
    """
    if isinstance(artifact, str) and os.path.isabs(artifact) and conversation and Path(artifact).name == conversation:
        return Path(artifact).parent
    return Path.home() / BRAIN


def classify(conversation, brain=None):
    """"subagent", "top-level" or "unknown" for this AGY conversation.

    Subagents fire the same hooks under their own IDs; they must neither attach
    nor displace the top-level conversation's endpoint. AGY 1.4 records a child
    at brain/<parent>/.system_generated/subagents/<child>.json before it creates
    brain/<child>. That ordering is an observed compatibility assumption, not a
    documented guarantee, so "top-level" needs positive evidence: the
    conversation's own store exists and a complete scan finds no parent record.
    A missing or unreadable store, or any scan error, is "unknown".
    """
    conversation = _uuid(conversation)
    brain = brain_directory() if brain is None else Path(brain)
    try:
        with os.scandir(brain) as entries:
            for entry in entries:
                if not entry.is_dir(follow_symlinks=False):
                    continue
                try:
                    os.stat(Path(entry.path) / ".system_generated" / "subagents" / f"{conversation}.json")
                    return "subagent"
                except (FileNotFoundError, NotADirectoryError):
                    continue
        if not stat.S_ISDIR(os.stat(brain / conversation).st_mode):
            return "unknown"
    except OSError:
        return "unknown"
    return "top-level"


class Unconfirmed(Error):
    """A conversation first classified top-level could not be re-confirmed."""


def confirmed_top_level(conversation, brain=None):
    """Top-level on two scans RECHECK apart, required before a destructive step.

    The recheck bounds, but cannot rule out, a child record written late.
    """
    if classify(conversation, brain) != "top-level":
        return False
    time.sleep(RECHECK)
    return classify(conversation, brain) == "top-level"


# --------------------------------------------------------------------------- records


def _read_private(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise Error("endpoint record must be a private regular file")
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise Error("endpoint record is too large")
    return json.loads(data)


def _owned_socket(path):
    info = os.lstat(path)
    return stat.S_ISSOCK(info.st_mode) and info.st_uid == os.getuid()


def _valid_socket(ledger, conversation, recorded):
    """Accept the recorded socket only at a location this design can create.

    Either beside the record, or in a private runtime directory named for this
    state directory, independent of the reader's own environment.
    """
    if not isinstance(recorded, str) or not os.path.isabs(recorded):
        return False
    path = Path(recorded)
    if path.name != f"{PROVIDER}-{conversation}.sock":
        return False
    try:
        if path.parent == directory(ledger):
            return _owned_socket(path)
        if path.parent.name != _runtime_tag(ledger):
            return False
        info = path.parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            return False
        return _owned_socket(path)
    except OSError:
        return False


def inspect(ledger):
    """Every endpoint record with an explicit state: attached, stale, superseded or detached."""
    result = []
    for path in sorted(directory(ledger).glob(f"{PROVIDER}-*.json")):
        entry = {"record": path.name}
        try:
            record = _read_private(path, MAX_RECORD)
            conversation = _uuid(record["id"])
            if path.name != f"{PROVIDER}-{conversation}.json" or record.get("provider") != PROVIDER:
                raise Error("endpoint record does not match its file name")
            entry.update(id=conversation, name=record.get("name"), agy_pid=record.get("pid"),
                         endpoint_pid=record.get("endpoint_pid"))
            age = time.time() - float(record["heartbeat"])
            agy_alive = _pinned(record["pid"], {"starttime": record["starttime"], "boot_id": record["boot_id"]})
            endpoint_alive = _pinned(record["endpoint_pid"], {"starttime": record["endpoint_starttime"],
                                                              "boot_id": record["boot_id"]})
            socket_ok = _valid_socket(ledger, conversation, record["socket"])
            if _fenced(ledger, conversation, record["endpoint_pid"], record["endpoint_starttime"]):
                entry["state"] = "superseded"
            elif agy_alive and endpoint_alive and socket_ok and 0 <= age <= STALE:
                entry["state"] = "attached"
            elif agy_alive and endpoint_alive:
                entry["state"] = "stale"
            else:
                entry["state"] = "detached"
            entry.update(heartbeat_age=round(age, 1), agy_alive=agy_alive, endpoint_alive=endpoint_alive,
                         socket=socket_ok)
            entry["_session"] = {"provider": PROVIDER, "id": conversation, "pid": record["pid"],
                                 "name": record.get("name") or f"AGY {conversation}",
                                 "status": "unknown", "delivery": "attached", "socket": record["socket"]}
        except (Error, OSError, ValueError, TypeError, KeyError, AttributeError) as exc:
            entry.update(state="invalid", error=type(exc).__name__)
        result.append(entry)
    return result


def discover(ledger):
    """Deliverable sessions only: a live pinned agy, a live endpoint, a fresh heartbeat."""
    return [entry["_session"] for entry in inspect(ledger) if entry.get("state") == "attached"]


def status(ledger):
    return [{k: v for k, v in entry.items() if not k.startswith("_")} for entry in inspect(ledger)]


# --------------------------------------------------------------------------- helper side


def submit(session, text):
    """Hand one claimed input to the attached endpoint. Never retried here."""
    try:
        conversation = _uuid(session.get("id"))
        path = session.get("socket")
        if not path or not _owned_socket(path):
            return {"status": "failed", "detail": "AGY endpoint socket missing or not owned; no input submitted"}
        request = {"op": "submit", "request_id": str(uuid.uuid4()), "conversation": conversation, "text": text}
        data = (json.dumps(request, ensure_ascii=False) + "\n").encode()
        if len(data) > MAX_FRAME:
            return {"status": "failed", "detail": "message exceeds the AGY endpoint frame limit; no input submitted"}
    except (Error, OSError, TypeError, ValueError) as exc:
        return {"status": "failed", "detail": f"invalid AGY delivery: {exc}"}
    sent = False
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(3)
            connection.connect(path)
            _check_peer(connection)
            connection.settimeout(CLIENT_TIMEOUT)
            sent = True
            connection.sendall(data)
            connection.shutdown(socket.SHUT_WR)
            reply = _read_line(connection)
    except (OSError, Error) as exc:
        if not sent:
            return {"status": "failed", "detail": f"AGY endpoint unreachable; no input submitted ({type(exc).__name__})"}
        return {"status": "uncertain", "detail": "AGY endpoint stopped responding after the request was sent; do not blindly retry"}
    try:
        response = json.loads(reply)
        if response.get("request_id") != request["request_id"] or response.get("status") not in {"submitted", "failed", "uncertain", "held"}:
            raise ValueError()
        detail = response.get("detail", "")
        if not isinstance(detail, str):
            raise ValueError()
        return {"status": response["status"], "detail": detail[:500]}
    except (ValueError, AttributeError, TypeError):
        return {"status": "uncertain", "detail": "AGY endpoint returned an invalid response; do not blindly retry"}


def ping(session, timeout=3):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(timeout)
        connection.connect(session["socket"])
        _check_peer(connection)
        connection.sendall(b'{"op":"ping"}\n')
        connection.shutdown(socket.SHUT_WR)
        return json.loads(_read_line(connection))


def _check_peer(connection):
    creds = connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i"))
    _, uid, _ = struct.unpack("3i", creds)
    if uid != os.getuid():
        raise Error("peer belongs to another user")


def _read_line(connection):
    chunks, size = [], 0
    while True:
        chunk = connection.recv(65536)
        if not chunk:
            break
        chunks.append(chunk)
        size += len(chunk)
        if size > MAX_FRAME:
            raise Error("frame too large")
        if b"\n" in chunk:
            break
    data = b"".join(chunks)
    if b"\n" not in data:
        raise Error("incomplete frame")
    return data.split(b"\n", 1)[0].decode()


# --------------------------------------------------------------------------- endpoint side


def _redact(text, secrets):
    for value in secrets:
        if value:
            text = text.replace(value, "[redacted]")
    return text


def _agentapi():
    """Command prefix for agentapi.

    The documented `agentapi` wrapper on the AGY shell's PATH is preferred.
    ANTIGRAVITY_AGENTAPI_EXE names the multi-call `agy` binary, which needs the
    `agentapi` subcommand before its arguments.
    """
    wrapper = shutil.which("agentapi")
    if wrapper and os.path.isabs(wrapper):
        return [wrapper]
    exe = os.environ.get(AGENTAPI)
    if exe and os.path.isabs(exe) and os.access(exe, os.X_OK):
        return [exe, "agentapi"]
    raise Error("agentapi executable not found in this AGY shell")


def send_message(conversation, text, *, timeout=SUBMIT_TIMEOUT):
    """Run one `agentapi send-message` with this process's inherited credentials."""
    secrets = [os.environ.get(name, "") for name in CREDENTIALS]
    if not isinstance(text, str) or not text or len(text) > MAX_TEXT or len(text.encode()) > MAX_TEXT_BYTES:
        return {"status": "failed", "detail": "message text is empty or exceeds the AGY limit; no input submitted"}
    try:
        command = _agentapi()
    except Error as exc:
        return {"status": "failed", "detail": str(exc)}
    try:
        result = subprocess.run([*command, "send-message", conversation, text], capture_output=True, text=True,
                                timeout=timeout, stdin=subprocess.DEVNULL, start_new_session=True)
    except subprocess.TimeoutExpired:
        return {"status": "uncertain", "detail": "agentapi send-message timed out; input may have been accepted; do not blindly retry"}
    except OSError as exc:
        return {"status": "failed", "detail": f"agentapi could not start: {type(exc).__name__}"}
    try:
        value = json.loads(result.stdout[:MAX_FRAME])
    except ValueError:
        value = None
    accepted = (isinstance(value, dict) and isinstance(value.get("response"), dict)
                and isinstance(value["response"].get("sendMessage"), dict)
                and value["response"]["sendMessage"].get("recipientId") == conversation)
    if result.returncode == 0 and accepted:
        return {"status": "submitted", "detail": "agentapi accepted input for this conversation; model receipt unconfirmed"}
    if isinstance(value, dict) and isinstance(value.get("error"), str) and "response" not in value:
        error = _redact(value["error"], secrets)[:300]
        # Configuration errors are raised before any request leaves the process.
        if error.endswith("is not set"):
            return {"status": "failed", "detail": f"agentapi refused before sending: {error}"}
        return {"status": "uncertain", "detail": f"agentapi reported an error; acceptance unknown: {error}"}
    return {"status": "uncertain", "detail": f"agentapi exit {result.returncode} without a matching acceptance; do not blindly retry"}


def conversation_exists(conversation, *, timeout=10):
    """True if AGY knows the conversation, False on an explicit NotFound, None if unknown.

    agentapi exits 0 even for errors, so only the parsed body decides. Existence
    does not prove the conversation is the one shown in the TUI.
    """
    try:
        result = subprocess.run([*_agentapi(), "get-conversation-metadata", conversation], capture_output=True,
                                text=True, timeout=timeout, stdin=subprocess.DEVNULL, start_new_session=True)
        value = json.loads(result.stdout[:MAX_FRAME])
    except (Error, OSError, subprocess.TimeoutExpired, ValueError):
        return None
    if not isinstance(value, dict):
        return None
    response = value.get("response")
    if isinstance(response, dict) and isinstance(response.get("conversationMetadata"), dict) \
            and response["conversationMetadata"].get("metadata"):
        return True
    error = value.get("error")
    if isinstance(error, str) and "code = NotFound" in error:
        return False
    return None


def _append_log(path, event, **fields):
    # Events carry IDs and outcomes only: never message text or credentials.
    line = json.dumps({"t": round(time.time(), 3), "event": event, **fields}, ensure_ascii=False)
    with contextlib.suppress(FileNotFoundError):
        if os.lstat(path).st_size > MAX_LOG:
            os.replace(path, str(path) + ".1")
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "a") as stream:
        stream.write(line + "\n")


def _terminate(pid, pin):
    """SIGTERM the pinned process; True if it was signalled.

    A pidfd is opened before the pin is checked, so the signal cannot reach a
    process that reused the pid after the check.
    """
    try:
        fd = os.pidfd_open(pid)
    except (AttributeError, OSError):
        if not _pinned(pid, pin):
            return False
        os.kill(pid, signal.SIGTERM)  # no pidfd support: check-then-kill window remains
        return True
    try:
        if not _pinned(pid, pin):
            return False
        signal.pidfd_send_signal(fd, signal.SIGTERM)
        return True
    except ProcessLookupError:
        return False
    finally:
        os.close(fd)


def _fence(ledger, conversation):
    return directory(ledger) / f"{PROVIDER}-{conversation}.superseded"


def _fenced(ledger, conversation, endpoint_pid, endpoint_starttime):
    """True if this endpoint generation was superseded; it may not take input."""
    try:
        fence = _read_private(_fence(ledger, conversation), MAX_RECORD)
    except FileNotFoundError:
        return False
    except (OSError, ValueError, Error):
        return True  # an unreadable fence fails closed
    return fence.get("endpoint_pid") == endpoint_pid and fence.get("endpoint_starttime") == endpoint_starttime


class Endpoint:
    def __init__(self, ledger, conversation, agy_pid, name, *, sender=send_message, checker=conversation_exists, log=None):
        self.ledger = ledger
        self.conversation = _uuid(conversation)
        self.paths = _paths(ledger, self.conversation)
        self.agy_pid = agy_pid
        self.agy_pin = _pin(agy_pid)
        self.name = name
        self.sender = sender
        self.checker = checker
        self.stopping = False
        self.log_path = log or self.paths["log"]
        self.started = time.time()
        self.lock_fd = None
        self.server = None

    def log(self, event, **fields):
        _append_log(self.log_path, event, **fields)

    def acquire(self):
        fd = os.open(self.paths["lock"], os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            return False
        self.lock_fd = fd
        return True

    def bind(self):
        path = self.paths["socket"]
        # Holding the lock proves no other endpoint serves this conversation.
        with contextlib.suppress(FileNotFoundError):
            info = os.lstat(path)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                raise Error("refusing to replace a non-socket endpoint path")
            os.unlink(path)
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        old = os.umask(0o177)
        try:
            server.bind(str(path))
        finally:
            os.umask(old)
        os.chmod(path, 0o600)
        server.listen(8)
        server.settimeout(1)
        self.server = server

    def write_record(self):
        mine = _pin(os.getpid())
        record = {"v": 1, "provider": PROVIDER, "id": self.conversation, "name": self.name,
                  "pid": self.agy_pid, "starttime": self.agy_pin["starttime"], "boot_id": self.agy_pin["boot_id"],
                  "endpoint_pid": os.getpid(), "endpoint_starttime": mine["starttime"],
                  "socket": str(self.paths["socket"]), "started": self.started, "heartbeat": time.time()}
        common.atomic(self.paths["record"], record)

    def cleanup(self):
        with contextlib.suppress(OSError, ValueError, Error):
            record = _read_private(self.paths["record"], MAX_RECORD)
            if record.get("endpoint_pid") == os.getpid():
                self.paths["record"].unlink()
        if self.server is not None:
            with contextlib.suppress(OSError):
                self.server.close()
            with contextlib.suppress(OSError):
                if _owned_socket(self.paths["socket"]):
                    self.paths["socket"].unlink()

    def handle(self, connection):
        try:
            connection.settimeout(5)
            _check_peer(connection)
            request = json.loads(_read_line(connection))
            if not isinstance(request, dict):
                raise ValueError()
        except (Error, OSError, ValueError):
            self.log("rejected_request")
            return
        if request.get("op") == "ping":
            response = {"ok": True, "provider": PROVIDER, "conversation": self.conversation,
                        "agy_pid": self.agy_pid, "uptime": round(time.time() - self.started, 1)}
        elif request.get("op") == "submit":
            rid = request.get("request_id")
            try:
                _uuid(rid)
                if request.get("conversation") != self.conversation:
                    raise Error("request is for another conversation")
                if self.superseded():
                    raise Error("endpoint was superseded by another conversation")
                result = self.sender(self.conversation, request.get("text"))
            except Error as exc:
                result = {"status": "failed", "detail": f"{exc}; no input submitted"}
            except Exception as exc:
                result = {"status": "uncertain", "detail": f"endpoint error during submission: {type(exc).__name__}"}
            response = {"request_id": rid, **result}
            self.log("submit", request_id=rid if isinstance(rid, str) else None, status=result["status"])
        else:
            response = {"error": "unknown operation"}
        with contextlib.suppress(OSError):
            connection.settimeout(5)
            connection.sendall((json.dumps(response, ensure_ascii=False) + "\n").encode())

    def superseded(self):
        return _fenced(self.ledger, self.conversation, os.getpid(), _pin(os.getpid())["starttime"])

    def refuse_pending(self):
        """Answer requests still queued at shutdown: failed, nothing was submitted."""
        with contextlib.suppress(OSError):
            self.server.setblocking(False)
            while True:
                try:
                    connection, _ = self.server.accept()
                except (BlockingIOError, InterruptedError):
                    return
                with connection:
                    try:
                        connection.setblocking(True)
                        connection.settimeout(2)
                        _check_peer(connection)
                        request = json.loads(_read_line(connection))
                        rid = request.get("request_id") if isinstance(request, dict) else None
                        response = {"request_id": rid, "status": "failed",
                                    "detail": "endpoint stopped; no input submitted"}
                        connection.sendall((json.dumps(response) + "\n").encode())
                        self.log("refused_at_stop", request_id=rid if isinstance(rid, str) else None)
                    except (Error, OSError, ValueError):
                        continue

    def serve(self):
        next_beat = next_check = 0
        try:
            while not self.stopping:
                now = time.monotonic()
                if now >= next_beat:
                    if not _pinned(self.agy_pid, self.agy_pin):
                        self.log("agy_exited", agy_pid=self.agy_pid)
                        break
                    if self.superseded():
                        break
                    if now >= next_check:
                        # Withdraw only on an explicit NotFound; an unknown answer
                        # leaves the pinned endpoint in place.
                        if self.checker(self.conversation) is False:
                            self.log("conversation_not_found")
                            break
                        next_check = now + METADATA_INTERVAL
                    self.write_record()
                    next_beat = now + HEARTBEAT
                try:
                    connection, _ = self.server.accept()
                except (socket.timeout, InterruptedError):
                    continue
                with connection:
                    self.handle(connection)
        finally:
            if self.server is not None:
                self.refuse_pending()
            self.cleanup()
            self.log("stopped")


def _validate_shell(conversation):
    missing = [name for name in CREDENTIALS if not os.environ.get(name)]
    if missing:
        raise Error("AGY agentapi credentials are absent; run `rookery attach agy` from the AGY conversation's own shell")
    env_conversation = os.environ.get(CONVERSATION)
    conversation = conversation or env_conversation
    if not conversation:
        raise Error(f"{CONVERSATION} is not set; pass --conversation from the AGY conversation")
    conversation = _uuid(conversation)
    if env_conversation and env_conversation != conversation:
        raise Error("--conversation does not match this AGY shell's conversation")
    return conversation


def attach(ledger, conversation=None, name=None, *, foreground=False):
    """Start (or confirm) the endpoint for this AGY conversation. Idempotent."""
    conversation = _validate_shell(conversation)
    name = name or f"AGY {conversation[:8]}"
    if not isinstance(name, str) or not name.strip() or len(name) > 256:
        raise Error("endpoint name must be nonempty text of at most 256 characters")
    kind = classify(conversation)
    if kind == "subagent":
        raise Error("this AGY conversation is a subagent; only top-level conversations attach")
    if kind != "top-level":
        raise Error("cannot confirm this is a top-level AGY conversation (unsupported or unreadable "
                    "AGY conversation store); not attaching")
    agy_pid = find_agy()
    supersede(ledger, conversation, agy_pid, confirm=lambda: confirmed_top_level(conversation))
    for entry in inspect(ledger):
        if entry.get("id") == conversation and entry.get("state") == "attached":
            return {"attached": f"{PROVIDER}:{conversation}", "agy_pid": entry["agy_pid"],
                    "endpoint_pid": entry["endpoint_pid"], "already": True}
    if foreground:
        return _run(ledger, conversation, agy_pid, name, None)
    read_fd, write_fd = os.pipe()
    pid = os.fork()
    if pid == 0:  # pragma: no cover - exercised by the live acceptance test
        os.close(read_fd)
        try:
            os.setsid()
            if os.fork():
                os._exit(0)
            os.chdir("/")
            null = os.open(os.devnull, os.O_RDWR)
            for fd in (0, 1, 2):
                os.dup2(null, fd)
            # Drop every descriptor inherited from the AGY shell (its sockets,
            # /proc handles and pipes) except the readiness pipe.
            os.closerange(3, write_fd)
            os.closerange(write_fd + 1, os.sysconf("SC_OPEN_MAX"))
            _run(ledger, conversation, agy_pid, name, write_fd)
        except BaseException:
            with contextlib.suppress(OSError):
                os.write(write_fd, b'{"error":"endpoint failed to start"}\n')
        finally:
            os._exit(0)
    os.close(write_fd)
    os.waitpid(pid, 0)
    deadline = time.monotonic() + READY_TIMEOUT
    data = b""
    with os.fdopen(read_fd, "rb") as stream:
        while time.monotonic() < deadline and b"\n" not in data:
            ready, _, _ = select.select([stream], [], [], max(0, deadline - time.monotonic()))
            if not ready:
                break
            chunk = os.read(stream.fileno(), 4096)
            if not chunk:
                break
            data += chunk
    try:
        result = json.loads(data.split(b"\n", 1)[0])
    except ValueError:
        raise Error("AGY endpoint did not report readiness; check `rookery attach agy --status`") from None
    if "error" in result:
        raise Error(result["error"])
    return result


def _run(ledger, conversation, agy_pid, name, ready_fd):
    endpoint = Endpoint(ledger, conversation, agy_pid, name)

    def report(value):
        if ready_fd is not None:
            with contextlib.suppress(OSError):
                os.write(ready_fd, (json.dumps(value) + "\n").encode())
                os.close(ready_fd)

    # The lock is also taken briefly by record cleanup, so a held lock alone does
    # not prove a live endpoint: retry briefly, and claim "already attached"
    # only when discovery shows one.
    deadline = time.monotonic() + LOCK_WAIT
    while not endpoint.acquire():
        if any(e.get("id") == conversation and e.get("state") == "attached" for e in inspect(ledger)):
            report({"attached": f"{PROVIDER}:{conversation}", "already": True})
            return {"attached": f"{PROVIDER}:{conversation}", "already": True}
        if time.monotonic() >= deadline:
            report({"error": "endpoint lock is busy and no attached endpoint was found; retry"})
            raise Error("endpoint lock is busy and no attached endpoint was found; retry")
        time.sleep(0.1)
    signal.signal(signal.SIGTERM, lambda *_: setattr(endpoint, "stopping", True))
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    try:
        endpoint.bind()
        endpoint.write_record()
    except (Error, OSError) as exc:
        endpoint.cleanup()
        report({"error": f"endpoint failed to start: {exc}"})
        raise
    endpoint.log("started", agy_pid=agy_pid, endpoint_pid=os.getpid())
    # Lifecycle markers read by the AGY hooks: this conversation has attached
    # successfully, and an explicit attach supersedes an earlier detach.
    base = directory(ledger) / f"{PROVIDER}-{conversation}"
    with contextlib.suppress(OSError):
        os.close(os.open(base.with_suffix(".attached"), os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600))
    with contextlib.suppress(FileNotFoundError):
        base.with_suffix(".detached").unlink()
    with contextlib.suppress(FileNotFoundError):
        _fence(ledger, conversation).unlink()  # fenced an earlier endpoint generation only
    result = {"attached": f"{PROVIDER}:{conversation}", "agy_pid": agy_pid, "endpoint_pid": os.getpid()}
    report(result)
    endpoint.serve()
    return result


def _unlink_if_unchanged(ledger, conversation, record):
    """Remove a dead endpoint's record unless a newer generation replaced it.

    Every endpoint holds its conversation's lock for its whole life and only
    then writes a record. Holding that lock here therefore excludes any live
    generation, so the compare and the unlink cannot be split by a replacement.
    If the lock is held, a live endpoint owns the record and it is left alone.
    """
    paths = _paths(ledger, conversation)
    try:
        fd = os.open(paths["lock"], os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    except OSError:
        return
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        try:
            current = _read_private(paths["record"], MAX_RECORD)
        except (OSError, ValueError, Error):
            return
        if (current.get("endpoint_pid"), current.get("endpoint_starttime")) == \
                (record.get("endpoint_pid"), record.get("endpoint_starttime")):
            paths["record"].unlink(missing_ok=True)
    finally:
        os.close(fd)


def supersede(ledger, conversation, agy_pid, *, confirm=None):
    """Stop endpoints that this same agy process holds for other conversations.

    One agy process shows one top-level conversation at a time. After a switch,
    an endpoint left for the previous conversation would push peer input into a
    conversation nobody is looking at. Each one is first fenced (a private
    marker naming its endpoint generation), which discovery and the endpoint's
    own pre-submit check honour at once, and then sent SIGTERM. A submission
    already in flight keeps its own outcome; queued ones are refused as failed.
    `confirm` is called once, only if there is something to stop; if it does
    not return True, nothing is stopped and Unconfirmed is raised, which is
    distinct from having nothing to stop. No sticky marker is written:
    returning to one of them offers the attach again. Returns the
    conversations stopped.
    """
    conversation = _uuid(conversation)
    pin = _pin(agy_pid)
    candidates = []
    for entry in inspect(ledger):
        other = entry.get("id")
        if not other or other == conversation or entry.get("agy_pid") != agy_pid:
            continue
        paths = _paths(ledger, other)
        try:
            record = _read_private(paths["record"], MAX_RECORD)
            if {"starttime": record["starttime"], "boot_id": record["boot_id"]} != pin:
                continue  # an earlier agy process that reused this pid
            candidates.append((other, paths, record))
        except (Error, OSError, ValueError, TypeError, KeyError):
            continue
    if not candidates:
        return []
    if confirm is not None and not confirm():
        raise Unconfirmed("cannot re-confirm this is a top-level AGY conversation; nothing was stopped")
    stopped = []
    for other, paths, record in candidates:
        try:
            common.atomic(_fence(ledger, other), {"endpoint_pid": record["endpoint_pid"],
                                                   "endpoint_starttime": record["endpoint_starttime"],
                                                   "by": conversation, "t": time.time()})
            with contextlib.suppress(OSError):
                _append_log(paths["log"], "superseded", by=conversation, agy_pid=agy_pid)
            endpoint_pin = {"starttime": record["endpoint_starttime"], "boot_id": record["boot_id"]}
            if not _terminate(record["endpoint_pid"], endpoint_pin):
                _unlink_if_unchanged(ledger, other, record)
        except (Error, OSError, ValueError, TypeError, KeyError):
            continue
        stopped.append(other)
    return stopped


def detach(ledger, conversation):
    """Stop this conversation's endpoint after verifying the recorded process."""
    conversation = _uuid(conversation)
    paths = _paths(ledger, conversation)
    # Sticky: the hooks stay quiet for this conversation until an explicit attach.
    with contextlib.suppress(OSError):
        os.close(os.open(paths["record"].with_suffix(".detached"), os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600))
    try:
        record = _read_private(paths["record"], MAX_RECORD)
    except FileNotFoundError:
        return {"detached": f"{PROVIDER}:{conversation}", "running": False}
    pin = {"starttime": record["endpoint_starttime"], "boot_id": record["boot_id"]}
    if _terminate(record["endpoint_pid"], pin):
        return {"detached": f"{PROVIDER}:{conversation}", "running": True, "endpoint_pid": record["endpoint_pid"]}
    _unlink_if_unchanged(ledger, conversation, record)
    return {"detached": f"{PROVIDER}:{conversation}", "running": False}
