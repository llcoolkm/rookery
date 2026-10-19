"""CLI dispatch: every messaging command goes through the unified helper."""
import contextlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
import uuid
from unittest.mock import Mock, patch

from rookery import cli


class CliTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = str(Path(self.tmp.name) / "peer's state with spaces")
        self.b = {"provider": "codex", "id": "b", "name": "beta", "status": "idle", "pid": 20}

    def invoke(self, *args, actor="operator", stdin="", success=True):
        argv = ["rookery", "--state-dir", self.state, "--as", actor, *args]
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(sys, "argv", argv), patch.object(sys, "stdin", io.StringIO(stdin)), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = cli.main()
        if success:
            self.assertEqual(result, 0, stderr.getvalue())
            return json.loads(stdout.getvalue())
        self.assertEqual(result, 1)
        self.assertEqual(stdout.getvalue(), "")
        return stderr.getvalue()

    def test_explicit_agy_polling_registration_is_pid_pinned(self):
        session_id = str(uuid.uuid4())
        result = self.invoke("register-polling", f"agy:{session_id}", "--pid", str(os.getpid()), "--name", "AGY test")
        self.assertEqual(result["registered"], f"agy:{session_id}")
        from rookery.polling import discover
        from rookery.local_store import Store
        self.assertEqual(discover(Store(self.state)), [{"provider": "agy", "id": session_id,
            "pid": os.getpid(), "name": "AGY test", "status": "waiting", "delivery": "polling"}])

    def test_messaging_commands_require_helper_configuration(self):
        mid = str(uuid.uuid4())
        for args in (("list",), ("enroll", "beta"), ("disable", "beta"), ("clear-alias", f"codex:{mid}"),
                     ("send", "--to", "beta", "--body", "hello"), ("reply", mid, "--body", "hi"),
                     ("ack", mid), ("inbox",), ("status",), ("doctor",), ("health",), ("wait", mid)):
            with self.subTest(args=args):
                error = self.invoke(*args, success=False)
                self.assertIn("rookery configure", error)
                self.assertNotIn("Traceback", error)
        self.assertFalse((Path(self.state) / "ledger.jsonl").exists())

    def test_configured_commands_route_through_unified_run(self):
        from rookery.client import configure
        configure(self.state, "server", "https://hub.example", "x" * 40)
        with patch("rookery.cli.unified_run", return_value={"routed": True}) as unified:
            self.assertEqual(self.invoke("status"), {"routed": True})
        self.assertEqual(unified.call_args.args[0].command, "status")

    def test_legacy_commands_are_gone(self):
        for command in ("worker", "flush", "reconcile", "net", "nats"):
            with self.subTest(command=command), contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit):
                cli.parser().parse_args([command])

    def test_partial_discovery_keeps_healthy_provider_and_reports_failure(self):
        # A temporary stand-in avoids requiring the Claude implementation while
        # exercising the real aggregation function and error reporting contract.
        from rookery import providers
        claude = types.ModuleType("rookery.claude_adapter")
        claude.discover = Mock(side_effect=OSError("native inbox unavailable"))
        with patch.dict(sys.modules, {"rookery.claude_adapter": claude}), patch("rookery.claude_adapter", claude, create=True), \
                patch("rookery.codex_adapter.discover", return_value=[self.b]):
            found, errors = providers.discover_all(only={"claude", "codex"})
        self.assertEqual(found, [self.b])
        self.assertEqual(errors, {"claude": "native inbox unavailable"})


if __name__ == "__main__":
    unittest.main()
