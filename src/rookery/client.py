"""Small agent-facing interface: atomic requests and bounded local reads."""
import json
import os
import sqlite3
from pathlib import Path
import time
import uuid

from .core import Error, MAX_BYTES
from .common import canonical_id
from .local_store import admission_lock, state_path
from .common import atomic, qualify, validate_url
from .request_ids import new_request_id

MAX_PENDING_COMMANDS = 1000
MAX_PENDING_BYTES = 32 * 1024 * 1024


def configure(path, node, url, token, allow_http=False):
    from .common import NODE
    path = state_path(path)
    if not NODE.fullmatch(node) or len(token) < 32 or any(c.isspace() for c in token):
        raise Error('invalid node or token (minimum 32 non-whitespace characters)')
    config = {'node':node,'url':validate_url(url,allow_http),'token':token,'allow_http_loopback':allow_http}
    if (path/'config.json').exists():
        old = json.loads((path/'config.json').read_text())
        if old['node'] != node or old['url'] != config['url']:
            raise Error('use a fresh state directory to change hub or node')
        # Operator options survive reconfiguration.
        config.update({k: v for k, v in old.items() if k == 'busy_delivery'})
    atomic(path/'config.json',config)
    for name in ('spool','results'):
        (path/name).mkdir(mode=0o700,exist_ok=True)
    return {'node':node,'url':config['url']}


def set_busy_delivery(path, provider, enabled):
    """Opt a host provider in or out of delivery during an active turn."""
    from . import providers
    path = state_path(path)
    config = read_config(path)
    target = providers.get(provider)
    if target is None or not target.capabilities.busy_optin:
        raise Error(f'{provider} does not offer busy delivery')
    chosen = [name for name in providers.busy_choice(config) if name != provider]
    if enabled:
        chosen.append(provider)
    config['busy_delivery'] = sorted(chosen)
    atomic(path/'config.json', config)
    return {'busy_delivery': config['busy_delivery'], 'restart_helper': True,
            'detail': 'takes effect, on or off, only after the helper restarts; until then the old policy applies'}


def read_config(path):
    path = state_path(path)/'config.json'
    if path.is_symlink() or path.stat().st_mode & 0o077:
        raise Error('config must be a private regular file')
    config = json.loads(path.read_text())
    validate_url(config['url'],config.get('allow_http_loopback',False))
    return config


# Replies carry results, not work: a long life lets a busy or briefly
# unreachable recipient still receive them. Sends (asks and informs) keep the
# short default so stale work is not started late.
REPLY_TTL = 86400

class Client:
    def __init__(self, state_dir=None, actor=None):
        self.path = state_path(state_dir)
        self.config = read_config(self.path)
        self.actor = actor or os.environ.get('ROOKERY_SESSION')
        if self.actor:
            self.identity()

    def identity(self):
        if not self.actor:
            raise Error('session binding required: --as or ROOKERY_SESSION')
        from .common import address
        actor = qualify(self.actor,self.config['node'])
        address(actor)
        return actor

    def _admit(self, request):
        payload_size = len(json.dumps(request, allow_nan=False).encode())
        if payload_size > 1048576:
            raise Error('request exceeds local size limit')
        with admission_lock(self.path):
            count, size = 0, 0
            database = self.path / 'helper.sqlite3'
            if database.exists():
                db = sqlite3.connect(database.as_uri() + '?mode=ro', uri=True, timeout=5)
                try:
                    count, size = db.execute('SELECT count(*),coalesce(sum(length(cast(data AS BLOB))),0) FROM commands').fetchone()
                finally:
                    db.close()
            for file in (self.path / 'spool').glob('*.json'):
                count += 1
                size += file.lstat().st_size
                if count >= MAX_PENDING_COMMANDS or size + payload_size > MAX_PENDING_BYTES:
                    break
            if count >= MAX_PENDING_COMMANDS or size + payload_size > MAX_PENDING_BYTES:
                raise Error('local outgoing queue is full; wait for the helper to reconnect')
            atomic(self.path/'spool'/(request['id']+'.json'), request)

    def request(self, op, data, timeout=5):
        request_id = new_request_id()
        data = dict(data)
        if op in ('send', 'reply'):
            body = data.get('body')
            if not isinstance(body, str) or not body.strip() or len(body.encode()) > MAX_BYTES:
                raise Error(f'body must contain text and be at most {MAX_BYTES} UTF-8 bytes')
            ttl = data.get('ttl', 3600)
            if type(ttl) is not int or not 1 <= ttl <= 86400:
                raise Error('ttl must be an integer between 1 and 86400 seconds')
            canonical_id(data['id'])
            if op == 'send' and data.get('kind', 'ask') not in ('ask', 'inform'):
                raise Error('send kind must be ask or inform')
            if op == 'reply':
                canonical_id(data['message_id'])
        data['_request_id'] = request_id
        self._admit({'id':request_id,'op':op,'data':data,'created':time.time()})
        deadline = time.monotonic()+timeout
        result_path = self.path/'results'/(request_id+'.json')
        while time.monotonic() < deadline:
            if result_path.exists():
                result = json.loads(result_path.read_text())
                if not result['ok']:
                    raise Error(result['error'])
                return result['result']
            time.sleep(0.1)
        return {'status':'queued_local','request_id':request_id,'id':data.get('id'),
                'detail':'saved locally; helper will upload when connected'}

    def peers(self):
        path = self.path/'directory.json'
        if not path.exists():
            return {'sessions':[],'detail':'helper has not connected yet'}
        value = json.loads(path.read_text())
        value['stale'] = time.time()-value.get('updated',0)>45
        if value['stale']:
            value['sessions'] = [{**s,'online':False,'status':'unknown'} for s in value.get('sessions',[])]
        return value

    @staticmethod
    def compact(msg):
        return {k:msg[k] for k in ('id','request_id','sender','recipient','kind','body','status','received_by_host','read_at','detail','reply_id','in_reply_to','followup_id','expires') if k in msg}

    def send(self, to, body, kind='ask', ttl=3600, message_id=None):
        return self.compact(self.request('send',{'actor':self.identity(),'to':to,'body':body,'kind':kind,
            'ttl':ttl,'expires':time.time()+ttl,'id':message_id or str(uuid.uuid4())}))

    def reply(self, message_id, body, ttl=REPLY_TTL, reply_id=None):
        return self.compact(self.request('reply',{'actor':self.identity(),'message_id':message_id,'body':body,
            'ttl':ttl,'expires':time.time()+ttl,'id':reply_id or str(uuid.uuid4())}))

    def _read(self, sql, args=()):
        import sqlite3
        database = self.path/'helper.sqlite3'
        if not database.exists():
            return []
        db = sqlite3.connect(database.as_uri()+'?mode=ro',uri=True,timeout=5)
        try:
            return [json.loads(row[0]) for row in db.execute(sql,args)]
        finally:
            db.close()

    def inbox(self, limit=20, peek=False):
        if type(peek) is not bool:
            raise Error('peek must be a boolean')
        limit = max(1,min(int(limit),100))
        actor = self.identity()
        # Mail that expired before it was ever delivered or read stays listed
        # (as expired) until read once; otherwise a session that was unreachable
        # would never learn it was sent.
        messages = self._read("SELECT data FROM messages WHERE recipient=? AND status NOT IN ('acknowledged','replied','cancelled') AND NOT (status='expired' AND json_extract(data,'$.read_at') IS NOT NULL) ORDER BY (json_extract(data,'$.read_at') IS NOT NULL),updated DESC LIMIT ?",(actor,limit))
        unread = [m['id'] for m in messages if not m.get('read_at')]
        if unread and not peek:
            # Persist one batch before returning. Failure is surfaced; an inbox
            # result is never presented as consumed without a durable request.
            self.request('read', {'actor': actor, 'message_ids': unread}, timeout=0)
        return [self.compact(m) for m in messages]

    def status(self, message_id=None):
        if message_id:
            messages = self._read('SELECT data FROM messages WHERE id=?',(message_id,))
            if not messages:
                return {'id':message_id,'status':'unknown_local'}
            msg = messages[0]
            if self.actor and self.identity() not in (msg['sender'],msg['recipient']):
                raise Error('message belongs to another session')
            return self.compact(msg)
        path = self.path/'health.json'
        if not path.exists():
            return {'running':False,'connected':False,'detail':'helper has not started'}
        value = json.loads(path.read_text())
        value['stale'] = time.time()-value['updated'] > 45
        if value['stale']:
            value['connected'] = False
        return value

    def enroll(self, target, alias=None):
        target = target if '/' in target else self.config['node']+'/'+target
        return self.request('enroll',{'target':target,**({'alias':alias} if alias else {})})

    def ack(self, message_id):
        return self.request('ack',{'actor':self.identity(),'message_id':message_id})
