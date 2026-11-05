"""Offline administration of the hub's node token-hash map."""
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import tempfile

from .common import NODE
from .core import Error


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Error('duplicate node name in token-hash file')
        result[key] = value
    return result


def validate(records):
    if not isinstance(records, dict):
        raise Error('token-hash file must contain a JSON object')
    hashes = set()
    for node, digest in records.items():
        if not NODE.fullmatch(node):
            raise Error('bad node name in token-hash file')
        if not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{64}', digest):
            raise Error('token-hash file must contain lowercase SHA-256 hex hashes only')
        if digest in hashes:
            raise Error('token hash is already used by another node')
        hashes.add(digest)
    return records


def manage(path, command, node=None, digest=None):
    path = Path(path)
    # A stable lock survives atomic replacement of the token-hash file.
    with open(str(path) + '.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode):
            raise Error('token-hash file must be a regular file, not a symlink')
        try:
            records = validate(json.loads(path.read_text(), object_pairs_hook=_pairs))
        except (ValueError, UnicodeError):
            raise Error('invalid token-hash JSON file') from None
        if command == 'list':
            return {'nodes': sorted(records)}
        if not isinstance(node, str) or not NODE.fullmatch(node):
            raise Error('bad node name: expected [a-z][a-z0-9-]{0,31}')
        updated = dict(records)
        if command == 'add':
            if not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{64}', digest):
                raise Error('hash must be a lowercase SHA-256 hex digest; raw tokens are not accepted')
            if node in records:
                raise Error('node already exists; remove it first to rotate its token')
            if digest in records.values():
                raise Error('token hash is already used by another node')
            updated[node] = digest
        elif command == 'remove':
            if node not in records:
                raise Error('node is not a hub node')
            del updated[node]
            if not updated:
                raise Error('refusing to remove the last node')
        else:
            raise Error('unknown hub node command')
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(mode='w', dir=path.parent, prefix='.' + path.name + '-', delete=False) as output:
                temporary = output.name
                current = os.fstat(output.fileno())
                if (current.st_uid, current.st_gid) != (info.st_uid, info.st_gid):
                    os.fchown(output.fileno(), info.st_uid, info.st_gid)
                os.fchmod(output.fileno(), stat.S_IMODE(info.st_mode))
                json.dump(updated, output, indent=2, sort_keys=True)
                output.write('\n')
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, path)
            temporary = None
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if temporary is not None:
                os.unlink(temporary)
        return {'nodes': sorted(updated), 'changed': node}
