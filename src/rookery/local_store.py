"""Private, indexed helper state. The daemon owns writes except atomic request spooling."""
from __future__ import annotations

import contextlib
import fcntl
import json
import os
from pathlib import Path
import sqlite3
import time
import uuid

from .core import Error, SEND_TTL
from .common import atomic


@contextlib.contextmanager
def admission_lock(path):
    """Serialize admission and spool-to-database moves across local processes."""
    fd = os.open(path / 'admission.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def state_path(value=None):
    path = Path(value or os.environ.get('ROOKERY_HOME', Path.home() / '.local/state/rookery')).expanduser().absolute()
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if path.is_symlink() or path.stat().st_uid != os.getuid() or path.stat().st_mode & 0o077:
        raise Error('state directory must be private (0700), owned by this user, and not a symlink')
    return path


class Store:
    def __init__(self, path):
        self.path = state_path(path)
        for name in ('spool', 'results'):
            directory = self.path / name
            directory.mkdir(mode=0o700, exist_ok=True)
            if directory.is_symlink():
                raise Error('spool directories must not be symlinks')
        self.database = self.path / 'helper.sqlite3'
        fd = os.open(self.database, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        os.close(fd)
        with self.connect() as db:
            db.execute('PRAGMA journal_mode=WAL')
            db.executescript('''
                CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS commands(id TEXT PRIMARY KEY,op TEXT NOT NULL,data TEXT NOT NULL,created REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY,recipient TEXT NOT NULL,sender TEXT NOT NULL,status TEXT NOT NULL,expires REAL NOT NULL,updated REAL NOT NULL,data TEXT NOT NULL);
                CREATE INDEX IF NOT EXISTS message_pending ON messages(status,recipient,expires);
                CREATE INDEX IF NOT EXISTS message_inbox ON messages(recipient,updated);
                CREATE TABLE IF NOT EXISTS attempts(claim_id TEXT PRIMARY KEY,message_id TEXT NOT NULL,result TEXT,reported INTEGER NOT NULL DEFAULT 0,created REAL NOT NULL);
                CREATE INDEX IF NOT EXISTS attempt_pending ON attempts(reported);
                CREATE TABLE IF NOT EXISTS notices(message_id TEXT PRIMARY KEY,result TEXT,created REAL NOT NULL);
            ''')
            # Expiry notices cover only messages that expire after this store
            # first supported them, so an upgrade does not replay old expiries.
            db.execute('INSERT OR IGNORE INTO metadata VALUES (?,?)', ('notices_since', json.dumps(time.time())))

    @contextlib.contextmanager
    def connect(self):
        db = sqlite3.connect(self.database, timeout=10)
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA busy_timeout=10000')
        db.execute('PRAGMA synchronous=FULL')
        try:
            with db:
                yield db
        finally:
            db.close()

    def get(self, key, default=None):
        with self.connect() as db:
            row = db.execute('SELECT value FROM metadata WHERE key=?', (key,)).fetchone()
            return json.loads(row[0]) if row else default

    def put(self, key, value):
        with self.connect() as db:
            db.execute('INSERT INTO metadata VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value', (key,json.dumps(value)))

    def import_spool(self):
        with admission_lock(self.path):
            self._import_spool()

    def _import_spool(self):
        for file in sorted((self.path / 'spool').glob('*.json'))[:64]:
            try:
                if file.is_symlink() or file.stat().st_size > 1048576:
                    raise ValueError('invalid request file')
                request = json.loads(file.read_text())
                if str(uuid.UUID(request['id'])) != file.stem or not isinstance(request['data'], dict):
                    raise ValueError('invalid request identity')
                if (self.path / 'results' / file.name).exists():
                    file.unlink()
                    continue
                with self.connect() as db:
                    db.execute('INSERT OR IGNORE INTO commands VALUES (?,?,?,?)',
                               (request['id'],request['op'],json.dumps(request['data']),request['created']))
            except (ValueError, KeyError, TypeError) as exc:
                atomic(self.path / 'results' / file.name, {'ok':False,'error':str(exc)})
            file.unlink()

    def commands(self):
        with self.connect() as db:
            return [dict(row) | {'data':json.loads(row['data'])} for row in db.execute('SELECT * FROM commands ORDER BY created LIMIT 64')]

    def pending_reads(self):
        """Local unconfirmed reads delay claims, never replace hub delivery state.

        Include unimported spool files: a read may be behind a full command batch.
        The admission lock makes spool-to-SQLite moves atomic to this inspection.
        """
        requests = []
        with admission_lock(self.path):
            with self.connect() as db:
                requests.extend(json.loads(row[0]) for row in db.execute("SELECT data FROM commands WHERE op='read'"))
            for file in (self.path / 'spool').glob('*.json'):
                try:
                    if file.is_symlink() or file.stat().st_size > 1048576:
                        continue
                    request = json.loads(file.read_text())
                    if isinstance(request, dict) and request.get('op') == 'read':
                        requests.append(request.get('data', {}))
                except (OSError, ValueError):
                    continue
        result = set()
        for data in requests:
            if not isinstance(data, dict) or not isinstance(data.get('actor'), str):
                continue
            mids = data.get('message_ids')
            if isinstance(mids, list) and 1 <= len(mids) <= 100:
                result.update((data['actor'], mid) for mid in mids if isinstance(mid, str))
        return result

    def complete(self, request_id, result):
        atomic(self.path / 'results' / (request_id+'.json'), result)
        with self.connect() as db:
            db.execute('DELETE FROM commands WHERE id=?', (request_id,))

    @staticmethod
    def save_message(db, msg):
        db.execute('INSERT INTO messages VALUES (?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,expires=excluded.expires,updated=excluded.updated,data=excluded.data',
                   (msg['id'],msg['recipient'],msg['sender'],msg['status'],msg['expires'],time.time(),json.dumps(msg)))

    def apply(self, events, cursor):
        with self.connect() as db:
            for event in events:
                if event['type'] == 'message':
                    self.save_message(db,event['data'])
            db.execute('INSERT INTO metadata VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value', ('cursor',json.dumps(cursor)))

    def messages(self, actor=None, pending=False, limit=100):
        sql, params = 'SELECT data FROM messages WHERE 1=1', []
        if actor:
            sql += ' AND recipient=?'; params.append(actor)
        if pending:
            sql += " AND status='pending' AND expires>?"; params.append(time.time())
        sql += ' ORDER BY updated DESC LIMIT ?'; params.append(limit)
        with self.connect() as db:
            return [json.loads(row[0]) for row in db.execute(sql,params)]

    def message(self, message_id):
        with self.connect() as db:
            row = db.execute('SELECT data FROM messages WHERE id=?',(message_id,)).fetchone()
            return json.loads(row[0]) if row else None

    def start_attempt(self, mid, claim_id):
        with self.connect() as db:
            db.execute('INSERT INTO attempts VALUES (?,?,NULL,0,?)',(claim_id,mid,time.time()))

    def finish_attempt(self, claim_id, result):
        with self.connect() as db:
            db.execute('UPDATE attempts SET result=? WHERE claim_id=?',(json.dumps(result),claim_id))

    def recover_attempts(self):
        with self.connect() as db:
            db.execute('UPDATE attempts SET result=? WHERE result IS NULL',
                       (json.dumps({'status':'uncertain','detail':'helper stopped during native submission; no automatic retry'}),))
            db.execute('UPDATE notices SET result=? WHERE result IS NULL',
                       (json.dumps({'status':'uncertain','detail':'helper stopped during notice submission; no automatic retry'}),))

    def expired_unnoticed(self, node, limit=32):
        """Sent from this node, expired without the recipient ever reading it,
        and not yet noticed to the sender. Only expiries within one send
        lifetime count; older ones are no longer worth interrupting for."""
        since = max(self.get('notices_since', 0), time.time() - SEND_TTL)
        with self.connect() as db:
            return [json.loads(row[0]) for row in db.execute(
                "SELECT data FROM messages WHERE status='expired' AND substr(sender,1,?)=? AND expires>=? "
                "AND json_extract(data,'$.read_at') IS NULL AND id NOT IN (SELECT message_id FROM notices) "
                "ORDER BY expires LIMIT ?", (len(node)+1, node+'/', since, limit))]

    def start_notice(self, message_id):
        with self.connect() as db:
            db.execute('INSERT INTO notices VALUES (?,NULL,?)', (message_id, time.time()))

    def finish_notice(self, message_id, result):
        with self.connect() as db:
            if result.get('status') == 'held':
                # Nothing was typed: try again when the sender is ready.
                db.execute('DELETE FROM notices WHERE message_id=?', (message_id,))
            else:
                db.execute('UPDATE notices SET result=? WHERE message_id=?', (json.dumps(result), message_id))

    def reports(self):
        with self.connect() as db:
            return [dict(r) | {'result':json.loads(r['result'])} for r in db.execute('SELECT * FROM attempts WHERE reported=0 AND result IS NOT NULL')]

    def reported(self, claim_id):
        with self.connect() as db:
            db.execute('UPDATE attempts SET reported=1 WHERE claim_id=?',(claim_id,))

    def prune(self):
        cutoff = time.time()-7*86400
        with self.connect() as db:
            db.execute("DELETE FROM messages WHERE updated<? AND status IN ('submitted','acknowledged','replied','expired','cancelled','failed')",(cutoff,))
            db.execute('DELETE FROM attempts WHERE reported=1 AND created<?',(time.time()-30*86400,))
            db.execute('DELETE FROM notices WHERE created<?',(time.time()-30*86400,))
        for file in (self.path/'results').glob('*.json'):
            if file.stat().st_mtime < cutoff:
                file.unlink()
