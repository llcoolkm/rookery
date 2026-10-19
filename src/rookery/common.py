"""Small helpers shared by the hub, the helper and the CLI."""
import json
import os
from pathlib import Path
import re
import tempfile
import urllib.parse
import urllib.request
import uuid

from .core import Error

NODE = re.compile(r"[a-z][a-z0-9-]{0,31}\Z")
# Wire addresses accept any well-formed provider name: the hub must never need
# a node's locally installed adapters to route that node's sessions.
PROVIDER = re.compile(r"[a-z][a-z0-9_-]{0,31}\Z")


class RemoteError(Error):
    """A definite rejection, rather than an unknown network outcome."""


def canonical_id(value):
    try:
        result = str(uuid.UUID(value))
    except (ValueError, TypeError, AttributeError) as exc:
        raise Error("ID must be a UUID") from exc
    if result != value:
        raise Error("ID must be a canonical UUID")
    return result


def address(value):
    if not isinstance(value, str):
        raise Error("address must be node/provider:UUID")
    try:
        node, local = value.split("/", 1)
        provider, session = local.split(":", 1)
    except ValueError as exc:
        raise Error("address must be node/provider:UUID") from exc
    if not NODE.fullmatch(node) or not PROVIDER.fullmatch(provider):
        raise Error("invalid node or provider")
    canonical_id(session)
    return node, provider, session


def owned(value, node):
    if address(value)[0] != node:
        raise Error("actor or target belongs to another node")
    return value


def atomic(path, value):
    path = Path(path)
    fd, name = tempfile.mkstemp(prefix=".tmp-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, ensure_ascii=False, allow_nan=False)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        dfd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(dfd)
        finally:
            os.close(dfd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def directory(ledger):
    path = ledger.path / "network"
    for item in (path, path / "outbox", path / "results", path / "attempts", path / "rejected-attempts"):
        item.mkdir(mode=0o700, exist_ok=True)
        if item.is_symlink() or item.stat().st_uid != os.getuid():
            raise Error("network state must be owned by this user and not symlinked")
    return path


def validate_url(url, allow_http=False):
    parsed = urllib.parse.urlsplit(url)
    if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in {"", "/"}:
        raise Error("hub URL must be an origin without credentials, path, query or fragment")
    if not parsed.hostname:
        raise Error("hub URL needs a hostname")
    if parsed.scheme != "https":
        if not (allow_http and parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "::1", "localhost"}):
            raise Error("HTTPS required (HTTP allowed only for explicit loopback development)")
    return url.rstrip("/")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def qualify(actor, node):
    if actor == "operator":
        raise Error("network messaging requires --as provider:UUID of an enrolled local session")
    key = actor if "/" in actor else node + "/" + actor
    if not key.startswith(node + "/"):
        raise Error("actor must belong to this connector node")
    return key
