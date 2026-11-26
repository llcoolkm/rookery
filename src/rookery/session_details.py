"""Collect optional session details for the helper's directory publication."""
from __future__ import annotations

from functools import cache
import re
import subprocess

from .mcp_identity import _process


_CONTROLS = re.compile(r'[\x00-\x1f\x7f-\x9f]')
_TMUX_COORDINATES = re.compile(r'[^:]+:[0-9]+\Z')
_TMUX_LIST = ['tmux', 'list-panes', '-a', '-F', '#{pane_pid} #{session_name}:#{window_index}']


def _text(value, limit):
    if not isinstance(value, str) or not value.strip() or _CONTROLS.search(value):
        return None
    try:
        if len(value.encode('utf-8')) <= limit:
            return value
    except UnicodeError:
        pass
    return None


def _list_panes():
    try:
        result = subprocess.run(_TMUX_LIST, stdin=subprocess.DEVNULL, capture_output=True,
                                encoding='utf-8', timeout=2, check=False)
    except (OSError, subprocess.TimeoutExpired, UnicodeError):
        return {}
    panes = {}
    if result.returncode == 0:
        for row in result.stdout.split('\n'):
            pid, _, coordinates = row.partition(' ')
            if (pid.isascii() and pid.isdecimal() and len(pid) <= 10
                    and _text(coordinates, 128) and _TMUX_COORDINATES.fullmatch(coordinates)):
                panes.setdefault(int(pid), coordinates)
    return panes


def _coordinates(pid, panes, process):
    # Choose the nearest matching ancestor when a pane launches another tmux.
    for _ in range(64):
        if pid <= 1:
            break
        if pid in panes:
            return panes[pid]
        parent, _ = process(pid)
        if parent is None or parent == pid:
            break
        pid = parent
    return None


def collect(sessions, previous, now):
    """Return optional wire metadata and the new per-session transition state."""
    @cache
    def process(pid):
        try:
            return _process(pid)
        except (OSError, IndexError, ValueError):
            return None, None

    panes = _list_panes() if any(type(s.get('pid')) is int and s['pid'] > 1 for s in sessions) else {}
    current, details = {}, []
    for session in sessions:
        key = (session['provider'], session['id'])
        pid, status = session.get('pid'), session['status']
        start = None
        tmux = None
        if type(pid) is int and pid > 1:
            _, start = process(pid)
            if panes and start is not None:
                tmux = _coordinates(pid, panes, process)
        prior = previous.get(key)
        identity = (pid, start, status)
        since = min(prior[3], now) if prior and prior[:3] == identity else now
        current[key] = (*identity, since)

        row = {'status_since': since}
        waiting_for = _text(session.get('waiting_for'), 128)
        if status == 'waiting' and waiting_for:
            row['waiting_for'] = waiting_for
        cwd = _text(session.get('cwd'), 512)
        if cwd:
            row['cwd'] = cwd
        if tmux:
            row['tmux'] = tmux
        details.append(row)
    return details, current
