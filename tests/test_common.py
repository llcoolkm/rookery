"""Shared address, URL and file helpers used by the hub, helper and CLI."""
import json
import os
from pathlib import Path
import tempfile
import unittest
import uuid

from rookery.common import RemoteError, address, atomic, canonical_id, owned, qualify, validate_url
from rookery.core import Error


class CommonTests(unittest.TestCase):
    def test_canonical_id_requires_lowercase_uuid(self):
        value = str(uuid.uuid4())
        self.assertEqual(canonical_id(value), value)
        for bad in (value.upper(), "{" + value + "}", "nope", None, 7):
            with self.subTest(bad=bad), self.assertRaises(Error):
                canonical_id(bad)

    def test_address_and_ownership(self):
        sid = str(uuid.uuid4())
        self.assertEqual(address(f"server/my_plugin:{sid}"), ("server", "my_plugin", sid))
        for bad in (None, "server", f"server/codex{sid}", f"Server/codex:{sid}", f"server/Codex:{sid}", "server/codex:x"):
            with self.subTest(bad=bad), self.assertRaises(Error):
                address(bad)
        self.assertEqual(owned(f"server/codex:{sid}", "server"), f"server/codex:{sid}")
        with self.assertRaises(Error):
            owned(f"laptop/codex:{sid}", "server")

    def test_qualify_binds_actor_to_local_node(self):
        sid = str(uuid.uuid4())
        self.assertEqual(qualify(f"codex:{sid}", "server"), f"server/codex:{sid}")
        self.assertEqual(qualify(f"server/codex:{sid}", "server"), f"server/codex:{sid}")
        for bad in ("operator", f"laptop/codex:{sid}"):
            with self.subTest(bad=bad), self.assertRaises(Error):
                qualify(bad, "server")

    def test_https_required_and_url_cannot_embed_credentials(self):
        for url in ("http://hub.example", "https://u:p@hub.example", "https://hub.example/path",
                    "https://hub.example?q=1", "https://hub.example#f", "https://", "http://example.com:80"):
            with self.subTest(url=url), self.assertRaises(Error):
                validate_url(url, True)
        self.assertEqual(validate_url("https://hub.example/"), "https://hub.example")
        self.assertEqual(validate_url("http://127.0.0.1:9765", True), "http://127.0.0.1:9765")
        with self.assertRaises(Error):
            validate_url("http://127.0.0.1:9765")

    def test_atomic_replaces_whole_file_and_leaves_no_temporary(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "value.json"
            atomic(path, {"a": 1})
            atomic(path, {"b": "ä"})
            self.assertEqual(json.loads(path.read_text()), {"b": "ä"})
            self.assertEqual(os.listdir(directory), ["value.json"])
            with self.assertRaises(ValueError):
                atomic(path, {"x": float("nan")})
            self.assertEqual(os.listdir(directory), ["value.json"])

    def test_remote_error_is_an_error(self):
        self.assertTrue(issubclass(RemoteError, Error))


if __name__ == "__main__":
    unittest.main()
