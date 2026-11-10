"""Shared limits and the error type every Rookery component raises."""
from __future__ import annotations

MAX_BYTES = 16384
MAX_HOPS = 6
# Sends (asks and informs) live 12 hours: long enough to outlast a long turn
# or a dialog left open overnight, short enough that yesterday's work is not
# started as today's. Replies carry results, not work, so they live a day.
SEND_TTL = 43200
REPLY_TTL = 86400


class Error(Exception):
    pass
