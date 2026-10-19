"""Claude MCP identity from supported session discovery and Linux ancestry.

No provider record files, environment tokens, or model-controlled identity hints.
Process identity is pinned; the current session UUID is read anew on every call.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import uuid


class IdentityUnavailable(ValueError):
    """The MCP caller cannot be bound without guessing."""


def _process(pid: int) -> tuple[int, int]:
    """Return parent PID and Linux start ticks, handling spaces in comm."""
    path = Path('/proc') / str(pid)
    raw = (path / 'stat').read_text()
    fields = raw[raw.rfind(')') + 2:].split()
    return int(fields[1]), int(fields[19])


def _ancestors() -> dict[int, int]:
    result = {}
    pid = os.getppid()
    for _ in range(64):
        if pid <= 1 or pid in result:
            break
        parent, started = _process(pid)
        result[pid] = started
        pid = parent
    return result


def _sessions() -> list[dict]:
    proc = subprocess.run(['claude', 'agents', '--json'], capture_output=True,
                          text=True, timeout=10, check=False)
    if proc.returncode or len(proc.stdout) > 1024 * 1024:
        raise IdentityUnavailable('Claude session discovery unavailable')
    rows = json.loads(proc.stdout)
    if not isinstance(rows, list) or any(not isinstance(row, dict) for row in rows):
        raise IdentityUnavailable('Claude session discovery returned an invalid list')
    return rows


class ClaudeIdentity:
    """Resolve the nearest Claude ancestor through ``claude agents --json``.

    The environment session ID may be stale after /clear or implicit resume,
    so it is deliberately not an authority. An orphaned, reparented, or reused
    process is refused. A session switch within the same live Claude process
    follows the current authoritative discovery row, never the stale env ID.
    """
    def __init__(self):
        self._owner: tuple[int, int] | None = None

    def __call__(self) -> str:
        try:
            before = _ancestors()
            rows = _sessions()
            after = _ancestors()
            if before != after:
                raise IdentityUnavailable('Claude process ancestry changed; reconnect MCP')
            if self._owner is None:
                pid = next((pid for pid in before
                            if any(type(r.get('pid')) is int and r['pid'] == pid for r in rows)), None)
                if pid is None:
                    raise IdentityUnavailable('No live Claude ancestor found; reconnect MCP')
                owner = (pid, before[pid])
            else:
                owner = self._owner
            pid, started = owner
            if before.get(pid) != started:
                raise IdentityUnavailable('Claude process identity changed; reconnect MCP')
            matches = [r for r in rows if type(r.get('pid')) is int and r['pid'] == pid]
            if len(matches) != 1:
                raise IdentityUnavailable('Claude ancestor session is missing or ambiguous')
            value = matches[0].get('sessionId')
            if not isinstance(value, str) or str(uuid.UUID(value)) != value.lower():
                raise IdentityUnavailable('Claude session UUID is unavailable')
            self._owner = owner
            return 'claude:' + value.lower()
        except IdentityUnavailable:
            raise
        except (OSError, ValueError, IndexError, subprocess.SubprocessError):
            raise IdentityUnavailable('Claude session identity unavailable; reconnect MCP') from None
