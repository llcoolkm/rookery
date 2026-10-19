"""Codex adapter for an existing managed app-server; no daemon or policy changes."""
from __future__ import annotations

import json
import os
import subprocess
import time
import uuid
from typing import Any
from pathlib import Path


class AdapterError(RuntimeError):
    pass


class Proxy:
    """JSON-RPC over the existing daemon's WebSocket Unix socket.

    ``codex app-server proxy`` forwards raw socket bytes; it is NOT a JSONL
    adapter. LocalWebSocket supplies bounded HTTP Upgrade and RFC 6455 framing.
    """

    def __init__(self, timeout: float = 8.0, socket_path: str | None = None):
        self.timeout = timeout
        self.counter = 0
        self.connection = None
        from .local_ws import LocalWebSocket, LocalWebSocketError
        self.transport_errors = (OSError, TimeoutError, LocalWebSocketError)
        home = Path(os.environ.get('CODEX_HOME', str(Path.home() / '.codex')))
        path = socket_path or str(home / 'app-server-control' / 'app-server-control.sock')
        try:
            self.connection = LocalWebSocket(path, timeout=timeout)
            self.call('initialize', {'clientInfo': {'name': 'rookery', 'version': '0.1.0'},
                                    'capabilities': {'experimentalApi': True}})
            self.notify('initialized', {})
        except self.transport_errors as exc:
            self.close()
            raise AdapterError('Codex daemon connection failed: ' + str(exc)) from exc
        except Exception:
            self.close()
            raise

    def _write(self, message: dict[str, Any]) -> None:
        try:
            self.connection.send(json.dumps(message, ensure_ascii=False))
        except self.transport_errors as exc:
            raise AdapterError('Codex daemon send failed: ' + str(exc)) from exc

    def notify(self, method: str, params: dict) -> None:
        self._write({'method': method, 'params': params})

    def call(self, method: str, params: dict) -> dict:
        self.counter += 1
        request_id = self.counter
        self._write({'id': request_id, 'method': method, 'params': params})
        deadline = time.monotonic() + self.timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AdapterError('Codex daemon request timed out')
            try:
                payload = self.connection.recv(timeout=remaining)
                message = json.loads(payload)
            except self.transport_errors as exc:
                raise AdapterError('Codex daemon receive failed: ' + str(exc)) from exc
            except (ValueError, UnicodeError) as exc:
                raise AdapterError('Invalid JSON from Codex daemon') from exc
            if not isinstance(message, dict):
                raise AdapterError('Invalid Codex RPC message')
            # Never answer provider approval requests or confuse their IDs with
            # our requests. Connection-local counters can legitimately collide.
            if message.get('id') != request_id or 'method' in message:
                continue
            if 'error' in message:
                raise AdapterError('Codex RPC error: ' + str(message['error']))
            if not isinstance(message.get('result'), dict):
                raise AdapterError('Invalid Codex RPC result')
            return message['result']

    def close(self) -> None:
        if self.connection is not None:
            self.connection.close()
            self.connection = None

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


def normalize(thread: dict) -> dict:
    raw = thread.get('status', {})
    kind = raw.get('type') if isinstance(raw, dict) else None
    flags = raw.get('activeFlags', []) if isinstance(raw, dict) else []
    status = 'unknown'
    # A failed turn (for example model capacity) leaves systemError until the
    # next turn starts; the thread still accepts input, so treat it as idle.
    if kind in ('idle', 'systemError') and thread.get('canAcceptDirectInput') is True:
        status = 'idle'
    elif kind == 'active':
        status = 'waiting' if flags else 'busy'
    return {'provider': 'codex', 'id': thread['id'],
            'name': thread.get('name') or thread['id'], 'status': status,
            'raw_status': raw, 'cwd': thread.get('cwd'),
            'can_accept_direct_input': thread.get('canAcceptDirectInput')}


def discover() -> list[dict]:
    # thread/list is persisted history and can omit live, not-yet-persisted
    # sessions. Loaded IDs plus thread/read are the runtime source of truth.
    sessions = []
    cursor = None
    seen_cursors = set()
    seen_ids = set()
    with Proxy() as proxy:
        for _ in range(100):
            params = {'limit': 100}
            if cursor:
                params['cursor'] = cursor
            result = proxy.call('thread/loaded/list', params)
            for thread_id in result.get('data', []):
                if thread_id in seen_ids:
                    continue
                seen_ids.add(thread_id)
                thread = proxy.call('thread/read', {'threadId': thread_id, 'includeTurns': False})['thread']
                if thread.get('id') != thread_id:
                    raise AdapterError('Codex returned a mismatched thread identity')
                if thread.get('status', {}).get('type') != 'notLoaded':
                    sessions.append(normalize(thread))
            cursor = result.get('nextCursor')
            if not cursor:
                return sessions
            if cursor in seen_cursors:
                raise AdapterError('Codex loaded-thread listing repeated pagination cursor')
            seen_cursors.add(cursor)
    raise AdapterError('Codex loaded-thread listing exceeded pagination limit')


def submit(session: dict, text: str) -> dict:
    """Recheck exact live identity; queue only idle. An exit code is not a receipt."""
    try:
        session_id = str(uuid.UUID(session['id']))
    except (KeyError, ValueError, TypeError, AttributeError):
        return {'status': 'failed', 'detail': 'Codex destination must be an exact UUID'}
    try:
        with Proxy() as proxy:
            thread = proxy.call('thread/read', {'threadId': session_id, 'includeTurns': False})['thread']
        current = normalize(thread)
        if current['id'] != session_id or current['status'] != 'idle':
            return {'status': 'held', 'detail': 'Codex recipient is not confirmed idle; no submission attempted'}
    except (AdapterError, OSError, KeyError, TypeError) as exc:
        return {'status': 'failed', 'detail': str(exc)}
    try:
        result = subprocess.run(['codex', 'queue', '--thread', session_id, '--message', text],
                                capture_output=True, text=True, timeout=20)
    except subprocess.TimeoutExpired:
        return {'status': 'uncertain', 'detail': 'Codex queue timed out; submission may have occurred; do not blindly retry'}
    except OSError as exc:
        return {'status': 'failed', 'detail': str(exc)}
    if result.returncode:
        # Even a nonzero client exit can follow server acceptance.
        return {'status': 'uncertain', 'detail': 'Codex queue exited unsuccessfully; reconcile before retrying'}
    return {'status': 'submitted', 'detail': 'Codex queue accepted input; recipient receipt unconfirmed'}
