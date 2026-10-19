"""Time-bound request identities, compatible with UUIDv7 on Python 3.11+."""
import secrets
import time
import uuid

REQUEST_WINDOW = 7 * 86400
CLOCK_SKEW = 300


def new_request_id():
    # RFC 9562: 48-bit Unix milliseconds, version 7, 74 random bits, RFC variant.
    milliseconds = int(time.time() * 1000)
    random = secrets.randbits(74)
    value = (milliseconds << 80) | (7 << 76) | ((random >> 62) << 64)
    value |= (2 << 62) | (random & ((1 << 62) - 1))
    return str(uuid.UUID(int=value))


def request_created(request_id):
    """Legacy UUIDs have no safe expiry; never evict their dedupe records."""
    value = uuid.UUID(request_id)
    return (value.int >> 80) / 1000 if value.version == 7 else None
