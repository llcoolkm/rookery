"""Collect optional session details for the helper's directory publication."""
from __future__ import annotations

from functools import cache
import os
import re
import subprocess
import time

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
                                encoding='utf-8', errors='replace', timeout=2, check=False)
    except OSError:
        return {}
    panes = {}
    if result.returncode == 0:
        for row in result.stdout.split('\n'):
            pid, _, coordinates = row.partition(' ')
            if ('\ufffd' not in row and pid.isascii() and pid.isdecimal() and len(pid) <= 10
                    and _text(coordinates, 128) and _TMUX_COORDINATES.fullmatch(coordinates)):
                panes.setdefault(int(pid), coordinates)
    return panes


def _coordinates(pid, panes, process):
    # Choose the nearest matching ancestor when a pane launches another tmux.
    for _ in range(64):
        if pid <= 1:
            break
        parent = process(pid)
        if parent is None or parent == pid:
            break
        if pid in panes:
            return panes[pid]
        pid = parent
    return None


class SessionDetails:
    def __init__(self):
        self._retry_tmux_at = 0

    def _panes(self, sessions):
        if (time.monotonic() < self._retry_tmux_at or not os.path.isfile('/proc/self/stat')
                or not any(type(s.get('pid')) is int and s['pid'] > 1 for s in sessions)):
            return {}
        try:
            return _list_panes()
        except subprocess.TimeoutExpired:
            self._retry_tmux_at = time.monotonic() + 60
            return {}

    def collect(self, sessions):
        """Return optional wire metadata in the same order as sessions."""
        @cache
        def process(pid):
            try:
                return _process(pid)[0]
            except (OSError, IndexError, ValueError):
                return None

        panes = self._panes(sessions)
        details = []
        for session in sessions:
            row = {}
            waiting_for = _text(session.get('waiting_for'), 128)
            if session['status'] == 'waiting' and waiting_for:
                row['waiting_for'] = waiting_for
            cwd = _text(session.get('cwd'), 512)
            if cwd:
                row['cwd'] = cwd
            pid = session.get('pid')
            if panes and type(pid) is int and pid > 1:
                tmux = _coordinates(pid, panes, process)
                if tmux:
                    row['tmux'] = tmux
            details.append(row)
        return details
