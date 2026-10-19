"""Shared limits and the error type every Rookery component raises."""
from __future__ import annotations

MAX_BYTES = 16384
MAX_HOPS = 6


class Error(Exception):
    pass
