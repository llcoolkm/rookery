"""Indexed, single-authority messaging hub. Native provider I/O stays on each host.

A committed claim is irreversible unless its owner reports a definite pre-I/O
hold. HTTP receipt, durable host ingestion, and explicit agent ACK are separate.
"""
from __future__ import annotations

import asyncio
import contextlib
import fcntl
import hashlib
import hmac
import json
import math
import os
from pathlib import Path
import re
import sqlite3
import stat
import time
import tempfile
import uuid
from urllib.parse import parse_qs

from .core import Error, MAX_BYTES, MAX_HOPS
from .common import NODE, address, canonical_id, owned
from .request_ids import CLOCK_SKEW, REQUEST_WINDOW, request_created

RETENTION = 7 * 86400
GRAPH_MESSAGES = 10000
GRAPH_NODES = 500
GRAPH_EDGES = 2000
FRESH = 60
DISPATCH_TIMEOUT = 90
MAX_REQUEST = 256 * 1024
TERMINAL = {"submitted", "uncertain", "failed", "acknowledged", "replied", "expired", "cancelled"}
MUTATIONS = {"enroll", "disable", "clear-alias", "send", "reply", "ack", "read", "received", "result", "reconcile"}
ALIAS = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
MAX_REQUESTS_PER_NODE = 10000
MAX_REQUESTS = 50000
MAX_SESSIONS_PER_NODE = 512
MAX_SESSIONS = 4096
MAX_NODES = 128
ADMISSIONS = {"enroll", "disable", "clear-alias", "send", "reply"}


class Overloaded(Error):
    """New admission was definitely refused; existing delivery keeps its reserve."""


class Unavailable(Error):
    """Storage unavailable. A caller must preserve its original request identity."""


def encoded(value):
    try:
        result = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False)
        result.encode("utf-8")
        return result
    except (ValueError, TypeError, UnicodeError):
        raise Error("invalid JSON data") from None


def number(value):
    try:
        return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)
    except OverflowError:
        return False


def integer(value, minimum, maximum):
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise Error("integer outside permitted range")
    return value


def authenticate_hash_file(path, authorization):
    """Accept private user files or integrity-protected root-owned K8s projections.

    These files contain hashes, not bearer credentials. Following a projected
    Secret symlink is intentional; its resolved regular-file ownership matters.
    """
    try:
        info = path.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in {0, os.getuid()} or info.st_mode & 0o022:
            return None
        if info.st_uid != 0 and info.st_mode & 0o077:
            return None
        if info.st_size > 65536 or not authorization.startswith("Bearer "):
            return None
        token = authorization[7:]
        if not 32 <= len(token) <= 4096:
            return None
        records = json.loads(path.read_text())
        if not isinstance(records, dict):
            return None
        digest, matched = hashlib.sha256(token.encode()).hexdigest(), None
        for owner, expected in records.items():
            if (not isinstance(owner, str) or not NODE.fullmatch(owner) or not isinstance(expected, str)
                    or not re.fullmatch(r"[a-f0-9]{64}", expected)):
                return None
            if hmac.compare_digest(digest, expected):
                if matched is not None:
                    return None
                matched = owner
        return matched
    except (OSError, ValueError, UnicodeError):
        return None


class Hub:
    def __init__(self, path, *, max_bytes=100_000_000, pending_limit=1000):
        self.path, self.max_bytes = Path(path), integer(max_bytes, 131072, 500_000_000)
        self.pending_limit = integer(pending_limit, 1, 10000)
        self.snapshot_last_success = 0.0
        self.request_counts, self.durations = {}, []
        self.api_request_counts = {}
        self.duration_sum, self.duration_count = 0.0, 0
        self.buckets = {value: 0 for value in (.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30)}
        self._lock = None
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        parent = self.path.parent.lstat()
        if (not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.getuid()
                or parent.st_mode & 0o077 or self.path.is_symlink()):
            raise Error("hub storage must be private and owned by this user")
        try:
            self._lock = os.open(str(self.path) + ".lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
            info = os.fstat(self._lock)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise Error("hub lock must be private")
            fcntl.flock(self._lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            fd = os.open(self.path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
            try:
                info = os.fstat(fd)
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                    raise Error("hub database must be private")
            finally:
                os.close(fd)
            with contextlib.closing(self._connect()) as db:
                db.execute("PRAGMA journal_mode=WAL")
                db.executescript("""
                    CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
                    CREATE TABLE IF NOT EXISTS nodes(node TEXT PRIMARY KEY,updated REAL NOT NULL);
                    CREATE TABLE IF NOT EXISTS sessions(
                        key TEXT PRIMARY KEY,node TEXT NOT NULL,alias TEXT,data TEXT NOT NULL,
                        enrolled INTEGER NOT NULL DEFAULT 0,enrollment TEXT,live INTEGER NOT NULL DEFAULT 1,updated REAL NOT NULL,
                        UNIQUE(node,alias));
                    CREATE INDEX IF NOT EXISTS sessions_node ON sessions(node,enrolled);
                    CREATE TABLE IF NOT EXISTS messages(
                        seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,
                        sender_node TEXT NOT NULL,recipient_node TEXT NOT NULL,status TEXT NOT NULL,
                        expires REAL NOT NULL,updated REAL NOT NULL,terminal_at REAL,retained INTEGER NOT NULL DEFAULT 1,binding TEXT NOT NULL,data TEXT NOT NULL);
                    CREATE INDEX IF NOT EXISTS messages_sender ON messages(sender_node,seq);
                    CREATE INDEX IF NOT EXISTS messages_recipient ON messages(recipient_node,seq);
                    CREATE INDEX IF NOT EXISTS messages_pending ON messages(recipient_node,status,seq);
                    CREATE INDEX IF NOT EXISTS messages_expiry ON messages(status,expires);
                    CREATE INDEX IF NOT EXISTS messages_status ON messages(status,updated);
                    CREATE INDEX IF NOT EXISTS messages_terminal ON messages(retained,terminal_at);
                    CREATE INDEX IF NOT EXISTS messages_created ON messages(json_extract(data, '$.created'));
                    CREATE TABLE IF NOT EXISTS requests(
                        node TEXT NOT NULL,id TEXT NOT NULL,binding TEXT NOT NULL,result TEXT NOT NULL,
                        PRIMARY KEY(node,id));
                    CREATE TABLE IF NOT EXISTS request_expiry(
                        node TEXT NOT NULL,id TEXT NOT NULL,expires REAL NOT NULL,
                        PRIMARY KEY(node,id));
                    CREATE INDEX IF NOT EXISTS request_expiry_time ON request_expiry(expires);
                    CREATE TABLE IF NOT EXISTS events(
                        seq INTEGER PRIMARY KEY AUTOINCREMENT,node TEXT NOT NULL,type TEXT NOT NULL,data TEXT NOT NULL,created REAL NOT NULL);
                    CREATE INDEX IF NOT EXISTS events_node ON events(node,seq);
                    CREATE INDEX IF NOT EXISTS events_created ON events(created);
                    INSERT OR IGNORE INTO metadata VALUES ('version','2');
                    INSERT OR IGNORE INTO metadata VALUES ('event_floor','0');
                    INSERT OR IGNORE INTO metadata VALUES ('request_floor','0');
                """)
                if db.execute("SELECT value FROM metadata WHERE key='version'").fetchone()[0] != "2":
                    raise Error("unsupported hub database version")
                if db.execute("PRAGMA quick_check").fetchone()[0] != "ok":
                    raise Unavailable("hub database failed integrity check")
                db.execute("BEGIN IMMEDIATE")
                db.execute("INSERT OR IGNORE INTO metadata VALUES ('legacy_request_deadline',?)", (str(time.time() + REQUEST_WINDOW),))
                deadline = float(db.execute("SELECT value FROM metadata WHERE key='legacy_request_deadline'").fetchone()[0])
                # Old executables may have inserted records without an expiry.
                # Add one without renewing the original migration grace period.
                for row in db.execute("SELECT r.node,r.id FROM requests r LEFT JOIN request_expiry e USING(node,id) WHERE e.id IS NULL").fetchall():
                    created = request_created(row['id'])
                    db.execute("INSERT INTO request_expiry VALUES (?,?,?)", (row['node'], row['id'], deadline if created is None else created + REQUEST_WINDOW))
                for row in db.execute("SELECT data FROM messages WHERE status='dispatching'").fetchall():
                    message = json.loads(row[0])
                    message.update(status="uncertain", detail="hub restarted after durable dispatch claim")
                    self._save(db, message)
                db.commit()
            self._fsync_parent()
        except BlockingIOError:
            self.close()
            raise Error("another hub process owns this database") from None
        except Exception:
            self.close()
            raise

    def _connect(self):
        if self._lock is None:
            raise Unavailable("hub is closed")
        db = sqlite3.connect(self.path, timeout=5)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA synchronous=FULL")
        db.execute("PRAGMA busy_timeout=5000")
        db.execute("PRAGMA wal_autocheckpoint=128")
        db.execute("PRAGMA journal_size_limit=16777216")
        db.execute(f"PRAGMA max_page_count={self.max_bytes // 4096}")
        return db

    @contextlib.contextmanager
    def _tx(self, *, write=True):
        db = None
        try:
            db = self._connect()
            db.execute("BEGIN IMMEDIATE" if write else "BEGIN")
            yield db
            db.commit()
        except sqlite3.Error as exc:
            if db is not None:
                db.rollback()
            if getattr(exc, 'sqlite_errorcode', None) == sqlite3.SQLITE_FULL:
                raise Overloaded("hub storage quota reached; operation was not accepted") from None
            raise Unavailable("hub storage operation failed") from None
        except Exception:
            if db is not None:
                db.rollback()
            raise
        finally:
            if db is not None:
                db.close()

    def close(self):
        if self._lock is not None:
            os.close(self._lock)
            self._lock = None

    def __del__(self):
        with contextlib.suppress(Exception):
            self.close()

    def _fsync_parent(self):
        fd = os.open(self.path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    @staticmethod
    def _message(db, mid):
        row = db.execute("SELECT data FROM messages WHERE id=?", (canonical_id(mid),)).fetchone()
        return json.loads(row[0]) if row else None

    @staticmethod
    def _highwater(db):
        row = db.execute("SELECT seq FROM sqlite_sequence WHERE name='events'").fetchone()
        return row[0] if row else 0

    @staticmethod
    def _emit(db, audience, kind, value):
        raw = encoded(value)
        for owner in sorted(set(audience)):
            db.execute("INSERT INTO events(node,type,data,created) VALUES (?,?,?,?)", (owner, kind, raw, time.time()))

    def _save(self, db, message):
        now = time.time()
        message["updated"] = now
        db.execute("UPDATE messages SET status=?,expires=?,updated=?,terminal_at=?,data=? WHERE id=?",
                   (message["status"], message["expires"], now, now if message["status"] in TERMINAL else None,
                    encoded(message), message["id"]))
        self._emit(db, (address(message["sender"])[0], address(message["recipient"])[0]), "message", message)

    def _maintenance(self, db, *, now=None):
        now = time.time() if now is None else now
        for row in db.execute("SELECT data FROM messages WHERE status='dispatching' AND updated<=?", (now - DISPATCH_TIMEOUT,)).fetchall():
            message = json.loads(row[0])
            message.update(status="uncertain", detail="dispatch claim timed out; automatic retry is forbidden")
            self._save(db, message)
        for row in db.execute("SELECT data FROM messages WHERE status IN ('pending','read') AND expires<=?", (now,)).fetchall():
            message = json.loads(row[0])
            message.update(status="expired", detail="deadline passed before native submission")
            self._save(db, message)
        for row in db.execute("SELECT id,data FROM messages WHERE retained=1 AND terminal_at<? LIMIT 128", (now - RETENTION,)).fetchall():
            message = json.loads(row["data"])
            if message.get("body_retained", True):
                message.update(body="", body_retained=False)
                db.execute("UPDATE messages SET data=?,retained=0 WHERE id=?", (encoded(message), row["id"]))
        floor = db.execute("SELECT MAX(seq) FROM events WHERE created<?", (now - RETENTION,)).fetchone()[0]
        if floor is not None:
            db.execute("DELETE FROM events WHERE created<?", (now - RETENTION,))
            db.execute("UPDATE metadata SET value=? WHERE key='event_floor'", (str(floor),))
        # Persist the highest forgotten expiry, not merely the current clock.
        # An NTP/manual backwards step must not make a forgotten ID valid again.
        pruned = db.execute("SELECT MAX(expires) FROM request_expiry WHERE expires<=?", (now,)).fetchone()[0]
        if pruned is not None:
            floor = float(db.execute("SELECT value FROM metadata WHERE key='request_floor'").fetchone()[0])
            db.execute("UPDATE metadata SET value=? WHERE key='request_floor'", (str(max(floor, pruned)),))
        db.execute("DELETE FROM requests WHERE (node,id) IN (SELECT node,id FROM request_expiry WHERE expires<=?)", (now,))
        db.execute("DELETE FROM request_expiry WHERE expires<=?", (now,))
        # Never discard enrollment/alias choices, even for long-offline hosts.
        # Messages retain their own routing identity and permanent ID binding.
        db.execute("DELETE FROM sessions WHERE enrolled=0 AND alias IS NULL AND updated<?", (now - RETENTION,))
        db.execute("DELETE FROM nodes WHERE updated<? AND NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.node=nodes.node)", (now - RETENTION,))

    def _capacity(self, db, required, *, reserve=True):
        pages = db.execute("PRAGMA page_count").fetchone()[0] - db.execute("PRAGMA freelist_count").fetchone()[0]
        used = pages * db.execute("PRAGMA page_size").fetchone()[0]
        # WAL duplicates also consume the host's disk, including when a reader
        # prevents checkpointing. This is admission control, not a disk quota.
        wal = Path(str(self.path) + '-wal')
        used += wal.stat().st_size if wal.exists() else 0
        headroom = min(16 * 1024 * 1024, self.max_bytes // 5) if reserve else 0
        if used + required >= self.max_bytes - headroom:
            raise Overloaded("hub admission quota reached; operation was not accepted")

    @staticmethod
    def _session(db, key):
        row = db.execute("SELECT * FROM sessions WHERE key=?", (key,)).fetchone()
        if row is None or not row["enrolled"]:
            raise Error("session is not enrolled")
        return row

    @staticmethod
    def _resolve(db, selector, *, owner=None):
        if not isinstance(selector, str):
            raise Error("recipient must be an address or alias")
        try:
            address(selector)
            return selector
        except Error:
            pass
        if "/" in selector:
            where, args = "node=? AND alias=?", selector.split("/", 1)
        elif owner:
            where, args = "node=? AND alias=?", (owner, selector)
        else:
            where, args = "alias=?", (selector,)
        rows = db.execute("SELECT key FROM sessions WHERE enrolled=1 AND " + where, args).fetchall()
        if len(rows) != 1:
            raise Error("alias must identify exactly one enrolled session")
        return rows[0][0]

    @staticmethod
    def _directory(db, node):
        rows = db.execute("SELECT * FROM sessions WHERE node=? OR enrolled=1 ORDER BY node,key", (node,)).fetchall()
        now, result = time.time(), []
        for row in rows:
            data = json.loads(row["data"])
            online = bool(row["live"] and 0 <= now - row["updated"] <= FRESH)
            data.update(enrolled=bool(row["enrolled"]), online=online, alias=row["alias"],
                        status=data["status"] if online else "unknown")
            if row["enrolled"]:
                enrollment = json.loads(row["enrollment"])
                data["enrollment_current"] = enrollment.get("pid") is None or enrollment["pid"] == data.get("pid")
                if not data["enrollment_current"]:
                    data["delivery_blocker"] = "session PID changed; re-enroll this exact session"
            result.append(data)
        return result

    def call(self, node, op, data=None):
        if not isinstance(node, str) or not NODE.fullmatch(node) or not isinstance(op, str):
            raise Error("invalid node or operation")
        data = {} if data is None else data
        if not isinstance(data, dict) or len(encoded(data).encode()) > MAX_REQUEST:
            raise Error("invalid or oversized request")
        data = dict(data)
        rid = data.pop("_request_id", None)
        created = None
        if rid is not None:
            canonical_id(rid)
            if op not in MUTATIONS:
                raise Error("this operation does not accept a request ID")
            created = request_created(rid)
        binding = hashlib.sha256(encoded({"op": op, "data": data}).encode()).hexdigest()
        try:
            with self._tx() as db:
                # Take the clock only after BEGIN IMMEDIATE acquires the lock.
                # Maintenance and validation must share that exact instant.
                now = time.time()
                before = db.total_changes
                self._maintenance(db, now=now)
                expires = None
                if rid is not None:
                    floor = float(db.execute("SELECT value FROM metadata WHERE key='request_floor'").fetchone()[0])
                    if created is not None:
                        expires = created + REQUEST_WINDOW
                        if created > now + CLOCK_SKEW or expires <= max(now, floor):
                            raise Error("request ID is expired or from a future clock; reconcile before creating a new request")
                    else:
                        expires = float(db.execute("SELECT value FROM metadata WHERE key='legacy_request_deadline'").fetchone()[0])
                        if expires <= max(now, floor):
                            raise Error("legacy request grace period ended; upgrade the client and reconcile queued requests")
                    old = db.execute("SELECT binding,result FROM requests WHERE node=? AND id=?", (node, rid)).fetchone()
                    if old:
                        if old["binding"] != binding:
                            raise Error("request ID already used for different content")
                        result = json.loads(old["result"])
                        if db.total_changes != before:
                            self._capacity(db, 0, reserve=False)
                        return self._message(db, result["_message"]) if "_message" in result else result
                    per_node = MAX_REQUESTS_PER_NODE - (MAX_REQUESTS_PER_NODE // 5 if op in ADMISSIONS else 0)
                    total = MAX_REQUESTS - (MAX_REQUESTS // 5 if op in ADMISSIONS else 0)
                    if (db.execute("SELECT COUNT(*) FROM requests WHERE node=?", (node,)).fetchone()[0] >= per_node
                            or db.execute("SELECT COUNT(*) FROM requests").fetchone()[0] >= total):
                        raise Overloaded("hub request-identity quota reached; operation was not accepted")
                result = self._call(db, node, op, data)
                if rid is not None:
                    cached = {"_message": result["id"]} if op in {"send", "reply"} else result
                    db.execute("INSERT INTO requests VALUES (?,?,?,?)", (node, rid, binding, encoded(cached)))
                    if expires is not None:
                        db.execute("INSERT INTO request_expiry VALUES (?,?,?)", (node, rid, expires))
                if db.total_changes != before:
                    # Check every write, including events and request caches.
                    # Completion of existing delivery may use the reserve.
                    self._capacity(db, 8192, reserve=op in ADMISSIONS)
                return result
        except (KeyError, TypeError, ValueError, AttributeError, OverflowError):
            raise Error("invalid request fields") from None

    def _call(self, db, node, op, data):
        now = time.time()
        if op == "publish":
            sessions = data["sessions"]
            if not isinstance(sessions, list) or len(sessions) > 128:
                raise Error("publish requires at most 128 sessions")
            clean, seen = [], set()
            for item in sessions:
                key = f"{node}/{item['provider']}:{item['id']}"
                owned(key, node)
                if key in seen or item.get("status") not in {"idle", "busy", "waiting", "unknown"}:
                    raise Error("duplicate or invalid published session")
                seen.add(key)
                name = item.get("name") or item["id"]
                if not isinstance(name, str) or len(name) > 256:
                    raise Error("invalid session name")
                value = {"key": key, "node": node, "provider": item["provider"], "id": item["id"],
                         "name": name, "status": item["status"]}
                if "pid" in item:
                    value["pid"] = integer(item["pid"], 1, 2**31 - 1)
                if item.get("delivery") in {"polling", "attached"}:
                    value["delivery"] = item["delivery"]
                # Absent means the idle-only policy of older helpers; a present
                # but invalid value is rejected rather than read as absent.
                if "admission" in item:
                    if item["admission"] not in {"idle", "busy", "none"}:
                        raise Error("invalid session admission")
                    value["admission"] = item["admission"]
                clean.append(value)
            heartbeat = db.execute("SELECT updated FROM nodes WHERE node=?", (node,)).fetchone()
            if heartbeat is None and db.execute("SELECT COUNT(*) FROM nodes").fetchone()[0] >= MAX_NODES:
                raise Overloaded("hub node quota reached")
            existing = {row[0] for row in db.execute("SELECT key FROM sessions WHERE node=?", (node,))}
            total = db.execute("SELECT COUNT(*) FROM sessions").fetchone()[0]
            accepted, refused, admission_bytes = [], 0, 0
            for value in clean:
                if value['key'] not in existing:
                    if len(existing) >= MAX_SESSIONS_PER_NODE or total >= MAX_SESSIONS:
                        refused += 1
                        continue
                    try:
                        required = len(encoded(value).encode()) * 4 + 8192
                        self._capacity(db, admission_bytes + required)
                    except Overloaded:
                        refused += 1
                        continue
                    admission_bytes += required
                    existing.add(value['key'])
                    total += 1
                accepted.append(value)
            # Existing sessions and the heartbeat survive a new-session quota.
            # Do not evict enrollments or make an established node unreachable.
            clean = accepted
            was_offline = heartbeat is None or not 0 <= now - heartbeat[0] <= FRESH
            db.execute("INSERT INTO nodes VALUES (?,?) ON CONFLICT(node) DO UPDATE SET updated=excluded.updated", (node, now))
            previous = {row["key"]: (row["data"], row["live"]) for row in db.execute("SELECT key,data,live FROM sessions WHERE node=?", (node,))}
            db.execute("UPDATE sessions SET live=0 WHERE node=?", (node,))
            changed = was_offline
            for value in clean:
                raw = encoded(value)
                changed |= previous.get(value["key"]) != (raw, 1)
                db.execute("INSERT INTO sessions(key,node,data,updated) VALUES (?,?,?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data,live=1,updated=excluded.updated",
                           (value["key"], node, raw, now))
                # The owning helper publishes each session UUID from the
                # harness's own registry, at most once (duplicates are
                # rejected above, and helpers drop UUIDs held by two live
                # processes). A new PID for an enrolled UUID is therefore that
                # same conversation resumed: re-pin delivery to it, as an
                # explicit enroll would, instead of silently refusing claims.
                row = db.execute("SELECT enrolled,enrollment FROM sessions WHERE key=?", (value["key"],)).fetchone()
                if row["enrolled"] and "pid" in value:
                    enrollment = json.loads(row["enrollment"])
                    if enrollment.get("pid") is not None and enrollment["pid"] != value["pid"]:
                        db.execute("UPDATE sessions SET enrollment=? WHERE key=?", (raw, value["key"]))
                        changed = True
            changed |= any(previous[key][1] for key in set(previous) - seen)
            if changed:
                self._emit(db, ("*",), "directory", {"changed": True})
            return {"published": len(clean), **({"refused": refused} if refused else {})}
        if op == "clear-alias":
            # Require the exact address, including for disabled/offline rows.
            # Resolving a friendly alias here could clear the wrong session
            # after another operator has reassigned it.
            key = owned(data["target"], node)
            row = db.execute("SELECT alias FROM sessions WHERE key=?", (key,)).fetchone()
            if row is None:
                raise Error("session has not been discovered")
            db.execute("UPDATE sessions SET alias=NULL WHERE key=?", (key,))
            self._emit(db, ("*",), "directory", {"changed": True})
            return {"target": key, "cleared_alias": row["alias"]}
        if op in {"enroll", "disable"}:
            key = owned(self._resolve(db, data["target"], owner=node), node)
            row = db.execute("SELECT * FROM sessions WHERE key=?", (key,)).fetchone()
            if row is None:
                raise Error("session has not been discovered")
            if op == "disable":
                db.execute("UPDATE sessions SET enrolled=0 WHERE key=?", (key,))
            else:
                if not row["live"] or not 0 <= now - row["updated"] <= FRESH:
                    raise Error("enrollment requires fresh host discovery")
                alias = data.get("alias", row["alias"])
                if alias is not None and (not isinstance(alias, str) or not ALIAS.fullmatch(alias)):
                    raise Error("alias must be a lowercase name")
                if alias is not None and db.execute("SELECT 1 FROM sessions WHERE node=? AND alias=? AND key<>?", (node, alias, key)).fetchone():
                    raise Error("alias already belongs to another session")
                db.execute("UPDATE sessions SET enrolled=1,alias=?,enrollment=? WHERE key=?", (alias, row["data"], key))
            self._emit(db, ("*",), "directory", {"changed": True})
            return {"enrolled" if op == "enroll" else "disabled": key, "alias": data.get("alias", row["alias"])}
        if op == "list":
            return {"sessions": self._directory(db, node)}
        if op in {"status", "inbox", "bootstrap"}:
            limit = integer(data.get("limit", 128), 1, 128)
            after = integer(data.get("after", 0), 0, 2**63 - 1)
            where, args = "(sender_node=? OR recipient_node=?) AND seq>?", [node, node, after]
            if op == "inbox":
                actor = owned(data["actor"], node)
                where += " AND json_extract(data,'$.recipient')=?"
                args.append(actor)
            rows = db.execute("SELECT seq,data FROM messages WHERE " + where + " ORDER BY seq LIMIT ?", (*args, limit + 1)).fetchall()
            result = {"messages": [json.loads(row["data"]) for row in rows[:limit]],
                      "next_after": rows[limit - 1]["seq"] if len(rows) > limit else None}
            if op == "bootstrap":
                high = self._highwater(db)
                cursor = integer(data.get("cursor", high), 0, high)
                result.update(sessions=self._directory(db, node), cursor=cursor)
            return result
        if op == "sync":
            cursor = integer(data.get("cursor", 0), 0, 2**63 - 1)
            limit = integer(data.get("limit", 128), 1, 128)
            floor = int(db.execute("SELECT value FROM metadata WHERE key='event_floor'").fetchone()[0])
            high = self._highwater(db)
            if cursor < floor:
                return {"events": [], "cursor": high, "more": False, "reset_required": True}
            if cursor > high:
                raise Error("sync cursor exceeds hub history")
            rows = db.execute("SELECT seq,type,data FROM events WHERE (node=? OR node='*') AND seq>? ORDER BY seq LIMIT ?", (node, cursor, limit + 1)).fetchall()
            more = len(rows) > limit
            events = [{"seq": row["seq"], "type": row["type"], "data": json.loads(row["data"])} for row in rows[:limit]]
            return {"events": events, "cursor": events[-1]["seq"] if more else high, "more": more, "reset_required": False}
        if op in {"send", "reply"}:
            actor = owned(data["actor"], node)
            parent = self._message(db, data["message_id"]) if op == "reply" else None
            if op == "reply" and (parent is None or parent["recipient"] != actor):
                raise Error("only original recipient can reply")
            recipient = parent["sender"] if parent else self._resolve(db, data["to"])
            mid, ttl = canonical_id(data["id"]), integer(data.get("ttl", 3600), 1, 86400)
            # Answering a reply is a follow-up: a new ask in the same thread.
            followup = parent is not None and parent["kind"] == "reply"
            kind = ("ask" if followup else "reply") if parent else data.get("kind", "ask")
            body, deadline = data["body"], data.get("expires")
            if not isinstance(body, str) or not body.strip() or len(body.encode()) > MAX_BYTES or kind not in {"ask", "inform", "reply"}:
                raise Error("invalid message body or kind")
            if not parent and kind == "reply":
                raise Error("reply requires a parent")
            if deadline is not None and not number(deadline):
                raise Error("absolute expiry must be finite")
            immutable = {"sender": actor, "recipient": recipient, "body": body, "kind": kind,
                         "in_reply_to": parent["id"] if parent else None, "ttl": ttl, "requested_expires": deadline}
            digest = hashlib.sha256(encoded(immutable).encode()).hexdigest()
            old = db.execute("SELECT binding,data FROM messages WHERE id=?", (mid,)).fetchone()
            if old:
                if old["binding"] != digest:
                    raise Error("message ID already used for different content or target")
                return json.loads(old["data"])
            self._session(db, actor)
            self._session(db, recipient)
            if actor == recipient or (deadline is not None and deadline <= now):
                raise Error("self messaging or an expired deadline is forbidden")
            hops = parent["hops"] + 1 if parent else 0
            if parent and hops >= MAX_HOPS:
                raise Error(f"thread reached the {MAX_HOPS}-message limit; send a new message instead")
            if followup and (parent["status"] == "cancelled" or parent.get("followup_id")):
                raise Error("this reply already has a follow-up or was cancelled; send a new message instead")
            if parent and not followup and parent["kind"] != "ask":
                raise Error("only asks and replies can be answered; an inform needs a new message")
            if parent and not followup and parent["status"] == "replied":
                raise Error("this ask was already answered; reply to the reply for a follow-up")
            if parent and not followup and (parent["status"] in {"expired", "cancelled"} or parent["expires"] <= now):
                raise Error("this ask expired or was cancelled; send a new message instead")
            message = {"v": 1, "id": mid, **immutable, "created": now,
                       "expires": min(deadline, now + ttl) if deadline is not None else now + ttl,
                       "conversation": parent["conversation"] if parent else mid, "hops": hops,
                       "status": "pending", "received_by_host": False, "updated": now}
            if db.execute("SELECT COUNT(*) FROM messages WHERE recipient_node=? AND status IN ('pending','read','dispatching')", (address(recipient)[0],)).fetchone()[0] >= self.pending_limit:
                raise Overloaded("recipient pending-message quota reached")
            self._capacity(db, len(encoded(message).encode()) * 4 + 8192)
            db.execute("INSERT INTO messages(id,sender_node,recipient_node,status,expires,updated,binding,data) VALUES (?,?,?,?,?,?,?,?)",
                       (mid, node, address(recipient)[0], "pending", message["expires"], now, digest, encoded(message)))
            self._emit(db, (node, address(recipient)[0]), "message", message)
            if followup:
                parent.update(followup_id=mid)
                self._save(db, parent)
            elif parent:
                parent.update(status="replied", reply_id=mid)
                self._save(db, parent)
            return message
        if op == "read":
            actor = owned(data["actor"], node)
            self._session(db, actor)
            mids = data["message_ids"]
            if not isinstance(mids, list) or not 1 <= len(mids) <= 100:
                raise Error("read requires between 1 and 100 message IDs")
            messages = []
            for mid in dict.fromkeys(canonical_id(mid) for mid in mids):
                message = self._message(db, mid)
                if message is None or message["recipient"] != actor:
                    raise Error("only addressed session can mark messages read")
                messages.append(message)
            for message in messages:
                # An expired message is still marked read (its status kept) so
                # the inbox can stop listing missed mail once it has been seen.
                if not message.get("read_at") and message["status"] != "cancelled":
                    message["read_at"] = now
                    if message["status"] == "pending":
                        message.update(status="read", detail="returned by session inbox fetch; model receipt unconfirmed")
                    self._save(db, message)
            return {"message_ids": [message["id"] for message in messages], "read": True}
        if op in {"ack", "received", "result", "reconcile"}:
            message = self._message(db, data["message_id"])
            if message is None or address(message["recipient"])[0] != node:
                raise Error("only recipient node can change delivery")
            changed = False
            if op == "received":
                if not message["received_by_host"]:
                    message.update(received_by_host=True, received_at=now)
                    changed = True
            elif op == "ack":
                if owned(data["actor"], node) != message["recipient"]:
                    raise Error("only addressed session can acknowledge")
                self._session(db, data["actor"])
                if message["status"] in {"expired", "cancelled"}:
                    raise Error("message is no longer active")
                if message["status"] not in {"acknowledged", "replied"}:
                    message["status"], changed = "acknowledged", True
            elif op == "reconcile":
                if data.get("action") != "cancel":
                    raise Error("only explicit cancellation is supported")
                if message["status"] not in {"acknowledged", "replied", "cancelled"}:
                    message.update(status="cancelled", detail="explicit operator cancellation")
                    changed = True
            else:
                if canonical_id(data["claim_id"]) != message.get("claim_id"):
                    raise Error("native claim does not match")
                status, detail = data["status"], data.get("detail", "")
                if status not in {"submitted", "held", "failed", "uncertain"} or not isinstance(detail, str) or len(detail) > 4096:
                    raise Error("invalid native result")
                if message["status"] == "dispatching":
                    message.update(status=("read" if message.get("read_at") else "pending") if status == "held" else status, detail=detail)
                    changed = True
            if changed:
                self._save(db, message)
            return {"message_id": message["id"], "status": message["status"], "received_by_host": message["received_by_host"]}
        if op == "claim":
            mid = data.get("message_id")
            query, args = "recipient_node=? AND status='pending'", [node]
            if mid is not None:
                query += " AND id=?"
                args.append(canonical_id(mid))
            for row in db.execute("SELECT data FROM messages WHERE " + query + " ORDER BY seq LIMIT 128", args).fetchall():
                message = json.loads(row[0])
                target = db.execute("SELECT * FROM sessions WHERE key=? AND enrolled=1", (message["recipient"],)).fetchone()
                if target is None or not target["live"] or not 0 <= now - target["updated"] <= FRESH:
                    continue
                live, enrolled = json.loads(target["data"]), json.loads(target["enrollment"])
                # The publishing node states admission; an absent field is the
                # conservative idle-only policy of older helpers. Polling
                # sessions are never pushed, whatever they claim.
                policy = live.get("admission", "idle")
                admissible = live.get("delivery") != "polling" and (
                    policy == "busy" or (policy == "idle" and live["status"] == "idle"))
                if (not admissible
                        or (enrolled.get("pid") is not None and live.get("pid") != enrolled["pid"])):
                    continue
                message.update(status="dispatching", claim_id=str(uuid.uuid4()))
                self._save(db, message)
                return {"delivery": {"message": message, "session": live, "claim_id": message["claim_id"]}}
            return {"delivery": None}
        raise Error("unknown hub operation")

    def backup(self, destination):
        destination = Path(destination)
        if destination.absolute() == self.path.absolute() or destination.exists() or destination.is_symlink():
            raise Error("backup destination must be a new file distinct from the live database")
        fd, name = tempfile.mkstemp(prefix=".tmp-hub-", suffix=".sqlite3", dir=destination.parent)
        os.close(fd)
        temporary = Path(name)
        try:
            with contextlib.closing(self._connect()) as source, contextlib.closing(sqlite3.connect(temporary)) as target:
                source.backup(target, pages=128, sleep=0.01)
                target.execute("PRAGMA journal_mode=DELETE")
            fd = os.open(temporary, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
            os.replace(temporary, destination)
            fd = os.open(destination.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        finally:
            temporary.unlink(missing_ok=True)
            for suffix in ("-wal", "-shm", "-journal"):
                Path(str(temporary) + suffix).unlink(missing_ok=True)
        self.snapshot_last_success = time.time()
        return {"snapshot": str(destination)}

    def maintain(self):
        with self._tx() as db:
            self._maintenance(db)
            self._capacity(db, 0, reserve=False)

    def checkpoint(self):
        with contextlib.closing(self._connect()) as db:
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")

    def observe(self, method, code, duration, *, api=False):
        key = (method if method in {"GET", "POST"} else "OTHER", code)
        self.request_counts[key] = self.request_counts.get(key, 0) + 1
        if api:
            self.api_request_counts[key] = self.api_request_counts.get(key, 0) + 1
        self.duration_sum += duration
        self.duration_count += 1
        for bound in self.buckets:
            self.buckets[bound] += duration <= bound

    def metrics(self):
        values = self.stats()
        lines = [f'rookery_hub_messages{{state="{state}"}} {values["messages"].get(state, 0)}'
                 for state in sorted(TERMINAL | {"pending", "read", "dispatching"})]
        lines += [f'rookery_hub_oldest_dispatching_age_seconds {values["oldest_dispatching_age_seconds"]}',
                  f'rookery_hub_db_bytes {values["database_bytes"]}',
                  f'rookery_hub_snapshot_last_success_timestamp_seconds {self.snapshot_last_success}']
        lines += [f'rookery_hub_storage_bytes{{file="{name}"}} {size}'
                  for name, size in values['storage_bytes'].items()]
        lines += [f'rookery_hub_node_last_seen_timestamp_seconds{{node="{node}"}} {stamp}' for node, stamp in sorted(values["nodes"].items())]
        lines += [f'rookery_hub_requests_total{{method="{method}",code="{code}"}} {count}' for (method, code), count in sorted(self.request_counts.items())]
        lines += [f'rookery_hub_api_requests_total{{method="{method}",code="{code}"}} {count}' for (method, code), count in sorted(self.api_request_counts.items())]
        lines += [f'rookery_hub_request_duration_seconds_bucket{{le="{bound}"}} {count}' for bound, count in sorted(self.buckets.items())]
        lines += [f'rookery_hub_request_duration_seconds_bucket{{le="+Inf"}} {self.duration_count}',
                  f'rookery_hub_request_duration_seconds_count {self.duration_count}',
                  f'rookery_hub_request_duration_seconds_sum {self.duration_sum}']
        return "\n".join(lines) + "\n"

    def graph(self, query=b""):
        """Read metadata only. Activity counts creations, never state transitions.

        Graph limits select newest messages; activity stays exact for the window.
        UTC-aligned first and last buckets may cover only part of the window.
        """
        try:
            if len(query) > 1024:
                raise ValueError()
            params = parse_qs(query.decode("ascii"), strict_parsing=True, keep_blank_values=True)
            if set(params) - {"from", "to", "interval"} or any(len(v) != 1 for v in params.values()):
                raise ValueError()
            values = {key: int(value[0]) for key, value in params.items()}
            if any(value < 0 or value > 2**53 for value in values.values()):
                raise ValueError()
            now = int(time.time() * 1000)
            start, end = values.get("from", now - 86400000), values.get("to", now)
            if start >= end or values.get("interval", 60) < 1:
                raise ValueError()
            start, end = max(start, now - RETENTION * 1000), min(end, now)
            if start >= end:
                raise ValueError()
            default_interval = next(step for step in (60, 300, 900, 3600, 21600)
                                    if (end - start) / (step * 1000) <= 100)
            seconds = max(values.get("interval", default_interval),
                          math.ceil((end - start) / 299000))
        except (ValueError, UnicodeError, AttributeError):
            raise Error("invalid graph range; use epoch milliseconds within the last seven days") from None
        width = seconds * 1000
        bucket_start = start // width * width
        activity = [{"time": stamp, "ask": 0, "reply": 0, "inform": 0}
                    for stamp in range(bucket_start, end, width)]
        with self._tx(write=False) as db:
            rows = db.execute("""SELECT json_extract(data,'$.sender') AS sender,
                json_extract(data,'$.recipient') AS recipient,status
                FROM messages WHERE json_extract(data,'$.created')>=?
                AND json_extract(data,'$.created')<?
                ORDER BY json_extract(data,'$.created') DESC,seq DESC LIMIT ?""",
                (start / 1000, end / 1000, GRAPH_MESSAGES + 1)).fetchall()
            counts = db.execute("""SELECT CAST((json_extract(data,'$.created') * 1000 - ?) / ? AS INTEGER),
                json_extract(data,'$.kind'),COUNT(*) FROM messages
                WHERE json_extract(data,'$.created')>=? AND json_extract(data,'$.created')<?
                GROUP BY 1,2""", (bucket_start, width, start / 1000, end / 1000)).fetchall()
            for bucket, kind, count in counts:
                if kind in {"ask", "reply", "inform"}:
                    activity[bucket][kind] = count
            nodes, edges, truncated = {}, {}, len(rows) > GRAPH_MESSAGES
            for row in rows[:GRAPH_MESSAGES]:
                sender, recipient, state = row
                pair = (sender, recipient)
                if ((pair not in edges and len(edges) >= GRAPH_EDGES)
                        or len(set(nodes) | {sender, recipient}) > GRAPH_NODES):
                    truncated = True
                    continue
                for key in set(pair):
                    if key not in nodes:
                        record = db.execute("""SELECT node,json_extract(data,'$.name'),
                            json_extract(data,'$.status'),live,updated FROM sessions WHERE key=?""", (key,)).fetchone()
                        node, name, status = key.split('/', 1)[0], key.split('/', 1)[-1], "unknown"
                        if record:
                            node, name = record[0], record[1] or name
                            if record[3] and 0 <= now / 1000 - record[4] <= FRESH:
                                status = record[2]
                        nodes[key] = {"id": key, "title": name, "subTitle": node + " / " + key.split('/', 1)[-1].split(':', 1)[0],
                                      "mainStat": 0, "secondaryStat": status,
                                      "detail__session": key, "detail__node": node}
                    nodes[key]["mainStat"] += 1
                edge = edges.setdefault(pair, {"id": sender + ">" + recipient, "source": sender, "target": recipient,
                    "mainStat": 0, "detail__pending": 0, "detail__uncertain": 0})
                edge["mainStat"] += 1
                edge["detail__pending"] += state == "pending"
                edge["detail__uncertain"] += state == "uncertain"
            for edge in edges.values():
                edge["secondaryStat"] = f"{edge['detail__pending']} pending / {edge['detail__uncertain']} uncertain"
                edge["color"] = "red" if edge["detail__uncertain"] else "orange" if edge["detail__pending"] else "gray"
        summary = {"from": start, "to": end, "bucket_seconds": seconds, "truncated": truncated}
        return {"nodes": list(nodes.values()), "edges": list(edges.values()), "activity": activity,
                **summary, "summary": [summary]}

    def stats(self):
        with self._tx(write=False) as db:
            counts = {row[0]: row[1] for row in db.execute("SELECT status,COUNT(*) FROM messages GROUP BY status")}
            oldest = db.execute("SELECT MIN(updated) FROM messages WHERE status='dispatching'").fetchone()[0]
            storage = {}
            for name, path in (("database", self.path), ("wal", Path(str(self.path) + "-wal"))):
                try:
                    storage[name] = path.stat().st_size
                except FileNotFoundError:
                    storage[name] = 0
            return {"messages": counts, "cursor": self._highwater(db),
                    "oldest_dispatching_age_seconds": max(0, time.time() - oldest) if oldest is not None else 0,
                    "nodes": dict(db.execute("SELECT node,updated FROM nodes")),
                    "storage_bytes": storage, "database_bytes": sum(storage.values())}



def create_app(hub_or_path, tokens_path, *, internal=False):
    owned_hub = not isinstance(hub_or_path, Hub)
    hub = Hub(hub_or_path) if owned_hub else hub_or_path
    tokens = Path(tokens_path)

    async def handle(scope, receive, send):
        if scope["type"] == "lifespan":
            while True:
                message = await receive()
                if message["type"] == "lifespan.startup":
                    await send({"type": "lifespan.startup.complete"})
                elif message["type"] == "lifespan.shutdown":
                    if owned_hub:
                        hub.close()
                    await send({"type": "lifespan.shutdown.complete"})
                    return
        if scope["type"] != "http":
            return
        async def respond(code, payload, *, plain=False):
            body = payload.encode() if plain else encoded(payload).encode()
            await send({"type": "http.response.start", "status": code, "headers": [
                (b"content-type", b"text/plain; version=0.0.4" if plain else b"application/json"),
                (b"cache-control", b"no-store")]})
            await send({"type": "http.response.body", "body": body})
        path = scope.get("path")
        if internal and scope.get("method") == "GET" and path == "/graph":
            try:
                await respond(200, hub.graph(scope.get("query_string", b"")))
            except Unavailable:
                await respond(503, {"ok": False, "error": "hub storage unavailable"})
            except Error:
                await respond(400, {"ok": False, "error": "invalid graph range"})
            return
        if internal and scope.get("method") == "GET" and path in {"/healthz", "/readyz", "/metrics"}:
            try:
                stats = hub.stats()
                if path == "/metrics":
                    await respond(200, hub.metrics(), plain=True)
                else:
                    await respond(200, {"ok": True, "protocol": 2})
            except Unavailable:
                await respond(503, {"ok": False})
            return
        if internal or path != "/v2/rpc" or scope.get("method") != "POST":
            await respond(404, {"ok": False, "error": "not found"})
            return
        auth = [value for key, value in scope.get("headers", []) if key.lower() == b"authorization"]
        node = authenticate_hash_file(tokens, auth[0].decode("latin-1")) if len(auth) == 1 else None
        if node is None:
            await respond(401, {"ok": False, "error": "unauthorized"})
            return
        body = bytearray()
        while True:
            event = await receive()
            if event["type"] == "http.disconnect":
                return
            if event["type"] != "http.request":
                await respond(400, {"ok": False, "error": "invalid request"})
                return
            body.extend(event.get("body", b""))
            if len(body) > MAX_REQUEST:
                await respond(413, {"ok": False, "error": "request too large"})
                return
            if not event.get("more_body", False):
                break
        try:
            request = json.loads(body, parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
            if not isinstance(request, dict) or set(request) - {"op", "data"}:
                raise Error("invalid request")
            data = request.get("data", {})
            if request["op"] == "sync":
                wait = data.get("wait", 0)
                if not number(wait) or not 0 <= wait <= 25:
                    raise Error("invalid sync wait")
                until = time.monotonic() + wait
                while True:
                    result = hub.call(node, "sync", data)
                    if result["events"] or result["reset_required"] or time.monotonic() >= until:
                        break
                    await asyncio.sleep(min(0.1, max(0, until - time.monotonic())))
            else:
                result = hub.call(node, request["op"], data)
            if authenticate_hash_file(tokens, auth[0].decode("latin-1")) != node:
                await respond(401, {"ok": False, "error": "unauthorized"})
                return
            await respond(200, {"ok": True, "result": result})
        except Overloaded:
            await respond(429, {"ok": False, "error": "hub admission quota reached", "code": "overloaded", "retryable": True})
        except Unavailable:
            await respond(503, {"ok": False, "error": "hub storage unavailable", "retryable": True})
        except Error as exc:
            # Hub Error texts are fixed, value-free strings written for the caller.
            await respond(400, {"ok": False, "error": str(exc) or "operation not permitted"})
        except (ValueError, TypeError, KeyError, AttributeError, UnicodeError):
            await respond(400, {"ok": False, "error": "invalid request or operation not permitted"})
        except Exception:
            await respond(500, {"ok": False, "error": "hub operation failed"})

    async def app(scope, receive, send):
        if scope["type"] != "http":
            return await handle(scope, receive, send)
        started, code = time.monotonic(), 500
        async def observed_send(event):
            nonlocal code
            if event["type"] == "http.response.start":
                code = event["status"]
            await send(event)
        try:
            await handle(scope, receive, observed_send)
        finally:
            hub.observe(scope.get("method", "OTHER"), code, time.monotonic() - started,
                        api=not internal and scope.get("path") == "/v2/rpc")

    app.hub = hub
    return app
