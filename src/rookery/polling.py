"""Explicit process-pinned inbox identities; never advertise native push readiness."""
from __future__ import annotations

import json
import os
from pathlib import Path
import stat
import uuid

from .core import Error
from . import common

PROC = Path('/proc')
MAX_RECORD = 4096


def _identity(provider, session_id):
    from . import providers
    if provider not in providers.names():
        raise Error('polling provider must be a registered provider: ' + ', '.join(sorted(providers.names())))
    try:
        canonical = str(uuid.UUID(session_id))
    except (ValueError, TypeError, AttributeError) as exc:
        raise Error('polling session ID must be a canonical UUID') from exc
    if canonical != session_id:
        raise Error('polling session ID must be a canonical UUID')
    return canonical


def parse_stat(value):
    """Linux comm may contain spaces and parentheses; fields start after final )."""
    end = value.rfind(')')
    if end < 0 or '(' not in value[:end]:
        raise Error('invalid process stat')
    fields = value[end + 1:].split()
    if len(fields) < 20 or not fields[19].isdigit():
        raise Error('invalid process stat')
    # fields[0] is Linux field 3 (state); fields[19] is field 22 (starttime).
    return fields[0], fields[19]


def _pin(pid):
    if isinstance(pid, bool) or not isinstance(pid, int) or pid <= 0:
        raise Error('polling PID must be a positive integer')
    proc = PROC / str(pid)
    try:
        if proc.stat().st_uid != os.getuid():
            raise Error('polling process must be owned by this user')
        state, start = parse_stat((proc / 'stat').read_text())
        if state not in {'R', 'S', 'D', 'I'}:
            raise Error('polling process is stopped, dead or unavailable')
        boot = (PROC / 'sys/kernel/random/boot_id').read_text().strip()
        if str(uuid.UUID(boot)) != boot:
            raise Error('invalid boot identity')
        return {'starttime': start, 'boot_id': boot}
    except (OSError, ValueError) as exc:
        raise Error('polling process is unavailable') from exc


def _directory(ledger):
    path = common.directory(ledger) / 'polling'
    path.mkdir(mode=0o700, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise Error('polling directory must be private and owned by this user')
    return path


def register(ledger, provider, session_id, pid, name=None):
    session_id = _identity(provider, session_id)
    if name is None:
        name = provider + ' polling ' + session_id
    if not isinstance(name, str) or not name.strip() or len(name) > 256:
        raise Error('polling name must be nonempty text of at most 256 characters')
    pin = _pin(pid)
    record = {'provider': provider, 'id': session_id, 'pid': pid, 'name': name, **pin}
    common.atomic(_directory(ledger) / f'{provider}-{session_id}.json', record)
    return {'registered': f'{provider}:{session_id}', 'pid': pid, 'delivery': 'polling'}


def unregister(ledger, provider, session_id):
    session_id = _identity(provider, session_id)
    path = _directory(ledger) / f'{provider}-{session_id}.json'
    path.unlink(missing_ok=True)
    return {'unregistered': f'{provider}:{session_id}'}


def discover(ledger):
    sessions = []
    for path in sorted(_directory(ledger).glob('*.json')):
        try:
            # No following symlinks, unbounded reads, or foreign-user records.
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd, 'rb') as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                    continue
                data = stream.read(MAX_RECORD + 1)
            if len(data) > MAX_RECORD:
                continue
            record = json.loads(data)
            provider, session_id = record['provider'], record['id']
            _identity(provider, session_id)
            if path.name != f'{provider}-{session_id}.json':
                continue
            name = record['name']
            if not isinstance(name, str) or not name.strip() or len(name) > 256:
                continue
            pin = _pin(record['pid'])
            if any(record.get(key) != value for key, value in pin.items()):
                continue
            sessions.append({'provider': provider, 'id': session_id, 'pid': record['pid'],
                             'name': name, 'status': 'waiting', 'delivery': 'polling'})
        except (Error, OSError, ValueError, TypeError, KeyError, AttributeError):
            continue
    return sessions
