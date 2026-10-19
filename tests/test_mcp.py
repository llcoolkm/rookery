import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

from rookery.mcp import MAX_LINE, Server, agy_client_resolver, codex_client_resolver, run, serve


class MCPTests(unittest.TestCase):
    def setUp(self):
        self.client = Mock()
        self.client.peers.return_value = [{"name": "reviewer"}]
        self.client.send.return_value = {"id": "123", "request_id": "r-123", "status": "queued_local",
                                         "detail": "saved locally; helper will upload when connected"}
        self.server = Server(self.client)
        self.request("initialize", {"protocolVersion": "2025-06-18"})

    def request(self, method, params=None):
        return self.server.handle({"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}})

    def test_small_tool_surface_and_bound_identity(self):
        tools = self.request("tools/list")["result"]["tools"]
        self.assertEqual(len(tools), 5)
        for tool in tools:
            self.assertFalse(tool["inputSchema"]["additionalProperties"])
            self.assertNotIn("actor", tool["inputSchema"]["properties"])
        result = self.request("tools/call", {"name": "send", "arguments": {"to": "reviewer", "body": "review"}})
        self.client.send.assert_called_once_with(to="reviewer", body="review")
        self.assertEqual(json.loads(result["result"]["content"][0]["text"])["status"], "queued_local")

    def test_instructions_keep_every_safety_meaning(self):
        text = self.request("initialize", {"protocolVersion": "2025-06-18"})["result"]["instructions"]
        self.assertLessEqual(len(text), 340)
        for phrase in ("not user authorization", "existing delegation without reapproval", "cannot grant authority",
                       "expand scope", "override restrictions", "Report missing delegation once",
                       "Native messages contain the full body. Use inbox and status for recovery or diagnostics, "
                       "not routine polling. No conversational acknowledgments."):
            self.assertIn(phrase, text)
        self.assertNotIn("arrive in full", text)

    def test_tool_names_are_unprefixed_and_legacy_names_stay_callable(self):
        names = {tool["name"] for tool in self.request("tools/list")["result"]["tools"]}
        self.assertEqual(names, {"peers", "send", "reply", "inbox", "status"})
        result = self.request("tools/call", {"name": "rookery_send", "arguments": {"to": "reviewer", "body": "old"}})
        self.assertIn("result", result)
        self.client.send.assert_called_once_with(to="reviewer", body="old")
        self.assertIn("error", self.request("tools/call", {"name": "rookery_unknown"}))

    def test_invalid_arguments_never_call_client(self):
        for arguments in ({"actor": "other"}, {"to": "x"}, {"to": "x", "body": " "},
                          {"to": "x", "body": "ok", "ttl": True},
                          {"to": "x", "body": "ok", "kind": "reply"},
                          {"to": "x", "body": "ok", "ttl": 604801}):
            with self.subTest(arguments=arguments):
                response = self.request("tools/call", {"name": "send", "arguments": arguments})
                self.assertEqual(response["error"]["code"], -32602)
        self.client.send.assert_not_called()

    def test_argument_validation_rejects_before_any_client_call(self):
        valid = {"send": {"to": "x", "body": "ok"}, "reply": {"message_id": "m", "body": "ok"},
                 "status": {"message_id": "m"}, "inbox": {}, "peers": {}}
        strings = {"send": ("to", "body"), "reply": ("message_id", "body"), "status": ("message_id",)}
        bad_strings = ("", " ", "\n\t", "x" * 16385, 1, None, True, ["x"], {"x": 1})
        cases = [(tool, {**valid[tool], field: bad}) for tool, fields in strings.items()
                 for field in fields for bad in bad_strings]
        cases += [(tool, {**valid[tool], "unknown": 1}) for tool in valid]
        cases += [(tool, {**valid[tool], "actor": "other"}) for tool in valid]
        cases += [("send", {"to": "x"}), ("send", {"body": "x"}), ("reply", {"message_id": "m"}),
                  ("send", {**valid["send"], "kind": "reply"}), ("send", {**valid["send"], "kind": 1}),
                  ("send", {**valid["send"], "ttl": 0}), ("send", {**valid["send"], "ttl": 86401}),
                  ("send", {**valid["send"], "ttl": 1.5}), ("send", {**valid["send"], "ttl": True}),
                  ("inbox", {"limit": 0}), ("inbox", {"limit": 101}), ("inbox", {"limit": "5"}),
                  ("inbox", {"peek": 1}), ("inbox", {"include_read": "yes"}), ("inbox", {"verbose": True}),
                  ("send", {**valid["send"], "verbose": 1}), ("reply", {**valid["reply"], "verbose": None}),
                  ("status", {"message_id": "m", "verbose": "true"}), ("status", {"include_read": True}),
                  ("peers", {"verbose": True})]
        for tool, arguments in cases:
            with self.subTest(tool=tool, arguments=str(arguments)[:80]):
                response = self.request("tools/call", {"name": tool, "arguments": arguments})
                self.assertEqual(response["error"]["code"], -32602)
        for method in ("send", "reply", "status", "inbox", "peers"):
            getattr(self.client, method).assert_not_called()
        self.client.send.return_value = {"id": "m", "status": "pending"}
        for arguments in ({"to": "x" * 16384, "body": "y" * 16384}, {"to": "x", "body": "ok", "kind": "inform", "ttl": 86400}):
            self.assertNotIn("error", self.request("tools/call", {"name": "send", "arguments": arguments}))
        self.assertEqual(self.client.send.call_count, 2)

    def test_unknown_method_tool_and_initialization(self):
        self.assertEqual(self.request("unknown")["error"]["code"], -32601)
        self.assertEqual(self.request("tools/call", {"name": "unknown"})["error"]["code"], -32602)
        fresh = Server(self.client)
        self.assertEqual(fresh.handle({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})["error"]["code"], -32000)

    def test_inbox_peek_boolean_is_forwarded_and_validated(self):
        self.client.inbox.return_value = []
        result = self.request("tools/call", {"name": "inbox", "arguments": {"peek": True}})
        self.assertNotIn("error", result)
        self.client.inbox.assert_called_once_with(peek=True)
        self.client.inbox.reset_mock()
        self.request("tools/call", {"name": "inbox", "arguments": {"include_read": True, "peek": False}})
        self.client.inbox.assert_called_once_with(peek=False)
        self.assertEqual(self.request("tools/call", {"name": "inbox", "arguments": {"include_read": 1}})["error"]["code"], -32602)
        for invalid in (1, "true", None):
            result = self.request("tools/call", {"name": "inbox", "arguments": {"peek": invalid}})
            self.assertEqual(result["error"]["code"], -32602)

    def test_operation_exception_has_no_secret(self):
        self.client.send.side_effect = RuntimeError("Authorization Bearer SECRET")
        response = self.request("tools/call", {"name": "send", "arguments": {"to": "reviewer", "body": "review"}})
        self.assertTrue(response["result"]["isError"])
        self.assertNotIn("SECRET", json.dumps(response))

    def test_rookery_error_text_reaches_the_caller(self):
        from rookery.core import Error
        self.client.reply.side_effect = Error("this ask was already answered; reply to the reply for a follow-up")
        response = self.request("tools/call", {"name": "reply", "arguments": {"message_id": "m", "body": "b"}})
        self.assertTrue(response["result"]["isError"])
        self.assertIn("already answered", response["result"]["content"][0]["text"])

    def test_stdio_recovers_after_bad_and_oversized_frames(self):
        frames = [b"{bad}\n", b"x" * (MAX_LINE + 1) + b"\n", b"\xff\n",
                  b'[]\n', b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n',
                  b'{"jsonrpc":"2.0","id":8,"method":"ping"}\n']
        output = io.BytesIO()
        serve(self.client, io.BytesIO(b"".join(frames)), output)
        responses = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual(len(responses), 5)
        self.assertEqual([r["error"]["code"] for r in responses[:4]], [-32700, -32700, -32700, -32600])
        self.assertEqual(responses[-1], {"jsonrpc": "2.0", "id": 8, "result": {}})

    def test_protocol_round_trip(self):
        frames = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "future-version"}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": "two", "method": "tools/call", "params": {"name": "peers"}},
        ]
        output = io.BytesIO()
        serve(self.client, io.BytesIO(b"".join((json.dumps(f) + "\n").encode() for f in frames)), output)
        responses = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual(len(responses), 2)
        self.assertEqual(responses[0]["result"]["protocolVersion"], "2025-06-18")
        self.assertEqual(responses[1]["id"], "two")
        self.client.peers.assert_called_once_with()

    def test_codex_auto_identity_is_resolved_per_call(self):
        ids = ["00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"]
        with patch('rookery.client.Client') as factory:
            factory.return_value.peers.return_value = []
            self.server = Server(client_resolver=codex_client_resolver('/private-state'))
            self.request('initialize', {'protocolVersion': '2025-06-18'})
            for thread in (ids[0], ids[1], ids[0]):
                response = self.request('tools/call', {'name': 'peers', '_meta': {'threadId': thread}})
                self.assertNotIn('error', response)
            self.assertEqual([call.kwargs['actor'] for call in factory.call_args_list],
                             ['codex:' + ids[0], 'codex:' + ids[1], 'codex:' + ids[0]])
            self.assertEqual(factory.call_count, 3)

    def test_codex_auto_identity_missing_invalid_or_conflicting_fails_closed(self):
        thread = "00000000-0000-4000-8000-000000000001"
        with patch('rookery.client.Client') as factory:
            self.server = Server(client_resolver=codex_client_resolver('/private-state'))
            self.request('initialize', {'protocolVersion': '2025-06-18'})
            for meta in (None, [], {}, {'threadId': 'SECRET'}, {'threadId': 42},
                         {'threadId': thread, 'sessionId': 'SECRET'},
                         {'threadId': thread, 'x-codex-turn-metadata': {'thread_id': 'SECRET'}}):
                with self.subTest(meta=meta):
                    response = self.request('tools/call', {'name': 'peers', '_meta': meta})
                    self.assertEqual(response['error']['code'], -32602)
                    self.assertNotIn('SECRET', json.dumps(response))
            for arguments in ({'actor': 'SECRET'}, {'_meta': {'threadId': thread}}, {'session': thread}):
                response = self.request('tools/call', {'name': 'peers', '_meta': {'threadId': thread}, 'arguments': arguments})
                self.assertEqual(response['error']['code'], -32602)
            factory.assert_not_called()

    def test_agy_identity_is_bound_per_call_from_metadata(self):
        ids = ["00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"]
        with patch('rookery.client.Client') as factory:
            factory.return_value.peers.return_value = []
            self.server = Server(client_resolver=agy_client_resolver('/private-state'))
            self.request('initialize', {'protocolVersion': '2025-06-18'})
            for conversation_id in (ids[0], ids[1], ids[0]):
                response = self.request('tools/call', {'name': 'peers', '_meta': {
                    'antigravity.google/conversation_id': conversation_id,
                    'antigravity.google/artifacts_dir': '/tmp/private',
                }})
                self.assertNotIn('error', response)
            self.assertEqual([call.kwargs['actor'] for call in factory.call_args_list],
                             ['agy:' + ids[0], 'agy:' + ids[1], 'agy:' + ids[0]])
            self.assertEqual(factory.call_count, 3)

    def test_agy_identity_missing_or_invalid_fails_closed(self):
        valid = "00000000-0000-4000-8000-00000000abcd"
        with patch('rookery.client.Client') as factory:
            self.server = Server(client_resolver=agy_client_resolver('/private-state'))
            self.request('initialize', {'protocolVersion': '2025-06-18'})
            for meta in (None, [], {}, {'antigravity.google/conversation_id': 'SECRET'},
                         {'antigravity.google/conversation_id': 42},
                         {'antigravity.google/conversation_id': valid.upper()}):
                with self.subTest(meta=meta):
                    response = self.request('tools/call', {'name': 'peers', '_meta': meta})
                    self.assertEqual(response['error']['code'], -32602)
                    self.assertNotIn('SECRET', json.dumps(response))
            factory.assert_not_called()

    def test_peer_list_is_compact_and_health_identifies_caller(self):
        self.client.peers.return_value = {'sessions': [
            {'key': 'server/codex:one', 'name': 'One', 'enrolled': True, 'pid': 42, 'socket_path': '/private', 'online': True},
            {'key': 'server/codex:two', 'name': 'Two', 'enrolled': False}], 'stale': False}
        response = self.request('tools/call', {'name': 'peers'})
        value = json.loads(response['result']['content'][0]['text'])
        self.assertEqual(value, {'sessions': [{'key': 'server/codex:one', 'status': 'unknown', 'name': 'One'}], 'stale': False})
        self.client.status.return_value = {'connected': True}
        self.client.identity.return_value = 'server/codex:one'
        response = self.request('tools/call', {'name': 'status'})
        self.assertEqual(json.loads(response['result']['content'][0]['text'])['identity'], 'server/codex:one')

    def call(self, name, arguments=None):
        response = self.request("tools/call", {"name": name, "arguments": arguments or {}})
        self.assertNotIn("error", response)
        return json.loads(response["result"]["content"][0]["text"])

    def test_send_result_is_compact_and_names_the_resolved_recipient(self):
        record = {"id": "m1", "sender": "node/claude:me", "recipient": "node/codex:canonical", "kind": "ask",
                  "body": "a long body", "status": "pending", "received_by_host": False,
                  "in_reply_to": None, "expires": 1234.5}
        self.client.send.return_value = record
        value = self.call("send", {"to": "worker", "body": "a long body"})
        self.assertEqual(value, {"id": "m1", "to": "node/codex:canonical", "kind": "ask", "status": "pending",
                                 "received_by_host": False})
        self.assertEqual(list(value)[:4], ["id", "to", "kind", "status"])
        self.client.send.assert_called_with(to="worker", body="a long body")
        self.assertEqual(self.call("send", {"to": "worker", "body": "b", "verbose": True}), record)
        self.client.send.assert_called_with(to="worker", body="b")

    def test_send_result_never_fabricates_fields(self):
        queued = {"status": "queued_local", "request_id": "r1", "id": "m1",
                  "detail": "saved locally; helper will upload when connected"}
        self.client.send.return_value = queued
        value = self.call("send", {"to": "worker", "body": "b"})
        self.assertEqual(value, queued)
        self.assertNotIn("to", value)
        self.assertNotIn("received_by_host", value)
        for record in ({"id": "m2", "recipient": "n/codex:x", "kind": "ask", "status": "uncertain",
                        "received_by_host": True, "detail": "outcome unknown; inspect status"},
                       {"id": "m3", "status": "failed", "error": "refused by recipient policy",
                        "detail": "refused", "stale": True}):
            self.client.send.return_value = record
            value = self.call("send", {"to": "worker", "body": "b"})
            expected = {("to" if k == "recipient" else k): v for k, v in record.items()}
            self.assertEqual(value, expected)

    def test_reply_result_keeps_actual_kind_and_thread_fields(self):
        follow = {"id": "f1", "request_id": "r2", "sender": "n/claude:me", "recipient": "n/codex:peer",
                  "kind": "ask", "body": "Follow-up", "status": "pending", "received_by_host": False,
                  "read_at": None, "in_reply_to": "reply-1", "expires": 99.0}
        self.client.reply.return_value = follow
        value = self.call("reply", {"message_id": "reply-1", "body": "Follow-up"})
        self.assertEqual(value, {"id": "f1", "to": "n/codex:peer", "kind": "ask", "status": "pending",
                                 "request_id": "r2", "received_by_host": False, "in_reply_to": "reply-1"})
        self.client.reply.assert_called_with(message_id="reply-1", body="Follow-up")
        answer = dict(follow, kind="reply", reply_id="x", followup_id="y", read_at=5.0)
        self.client.reply.return_value = answer
        value = self.call("reply", {"message_id": "ask-1", "body": "b"})
        self.assertEqual((value["kind"], value["reply_id"], value["followup_id"], value["read_at"]),
                         ("reply", "x", "y", 5.0))
        self.assertEqual(self.call("reply", {"message_id": "ask-1", "body": "b", "verbose": True}), answer)

    def test_message_status_is_compact_unless_verbose(self):
        record = {"id": "m1", "request_id": "r1", "sender": "n/claude:me", "recipient": "n/codex:peer",
                  "kind": "ask", "body": "secret-sized body", "status": "replied", "received_by_host": True,
                  "read_at": 10.0, "detail": "frame written", "reply_id": "r9", "in_reply_to": None,
                  "followup_id": "f9", "expires": 99.0}
        self.client.status.return_value = record
        value = self.call("status", {"message_id": "m1"})
        self.assertEqual(value, {"id": "m1", "kind": "ask", "status": "replied", "request_id": "r1",
                                 "received_by_host": True, "read_at": 10.0, "detail": "frame written",
                                 "reply_id": "r9", "followup_id": "f9"})
        self.client.status.assert_called_with(message_id="m1")
        self.assertEqual(self.call("status", {"message_id": "m1", "verbose": True}), record)
        self.client.status.return_value = {"id": "m2", "status": "unknown_local"}
        self.assertEqual(self.call("status", {"message_id": "m2"}), {"id": "m2", "status": "unknown_local"})
        self.client.status.return_value = {"running": True, "connected": False, "stale": True, "detail": None}
        self.client.identity.return_value = "n/claude:me"
        self.assertEqual(self.call("status"), {"running": True, "connected": False, "stale": True,
                                               "detail": None, "identity": "n/claude:me"})
        self.client.status.assert_called_with()

    def test_peer_rows_drop_redundant_fields_and_never_look_available_when_offline(self):
        uid = "65b87213-29bf-45e7-8604-9f7baeb51cbc"
        self.client.peers.return_value = {'sessions': [
            {'key': f'server/claude:{uid}', 'node': 'server', 'provider': 'claude', 'id': uid, 'name': uid,
             'status': 'idle', 'online': True, 'alias': None, 'enrolled': True, 'pid': 1},
            {'key': 'laptop/codex:two', 'node': 'laptop', 'provider': 'codex', 'name': 'Reviewer',
             'status': 'busy', 'online': True, 'alias': 'rev', 'enrolled': True},
            {'key': 'laptop/agy:three', 'name': 'three', 'status': 'idle', 'online': False, 'enrolled': True},
            {'key': 'laptop/agy:four', 'enrolled': True},
            {'key': 'laptop/agy:five', 'status': '', 'online': True, 'enrolled': True}], 'stale': False}
        value = self.call('peers')
        self.assertEqual(value, {'sessions': [
            {'key': f'server/claude:{uid}', 'status': 'idle'},
            {'key': 'laptop/codex:two', 'status': 'busy', 'name': 'Reviewer', 'alias': 'rev'},
            {'key': 'laptop/agy:three', 'status': 'unknown', 'online': False},
            {'key': 'laptop/agy:four', 'status': 'unknown'},
            {'key': 'laptop/agy:five', 'status': 'unknown'}], 'stale': False})

    def test_stale_peer_directory_stays_stale_and_unknown(self):
        from rookery.client import Client
        with tempfile.TemporaryDirectory() as directory:
            from rookery.client import configure
            configure(directory, "test", "http://127.0.0.1:9999", "x" * 32, allow_http=True)
            with open(os.path.join(directory, "directory.json"), "w") as handle:
                json.dump({"updated": 0, "sessions": [
                    {"key": "test/codex:one", "name": "one", "status": "idle", "online": True, "enrolled": True}]}, handle)
            self.server = Server(Client(directory, actor="test/codex:00000000-0000-4000-8000-000000000001"))
            self.request("initialize", {"protocolVersion": "2025-06-18"})
            value = self.call("peers")
        self.assertEqual(value, {"sessions": [{"key": "test/codex:one", "status": "unknown", "online": False}],
                                 "stale": True})
        self.client.peers.return_value = {"sessions": [], "detail": "helper has not connected yet"}
        self.server = Server(self.client)
        self.request("initialize", {"protocolVersion": "2025-06-18"})
        self.assertEqual(self.call("peers"), {"sessions": [], "stale": True})

    def test_provider_and_explicit_binding_are_exclusive(self):
        with patch.dict('os.environ', {}, clear=True):
            with self.assertRaisesRegex(ValueError, 'cannot be combined'):
                run(actor='codex:00000000-0000-4000-8000-000000000001', provider='codex')
        with patch.dict('os.environ', {'ROOKERY_SESSION': 'codex:00000000-0000-4000-8000-000000000001'}):
            with self.assertRaisesRegex(ValueError, 'cannot be combined'):
                run(provider='codex')

    def test_real_subprocess_provider_codex_resolves_metadata(self):
        from rookery.client import configure
        with tempfile.TemporaryDirectory() as directory:
            configure(directory, "test", "http://127.0.0.1:9999", "x" * 32, allow_http=True)
            frames = [
                {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}},
                {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "status"}},
                {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "status", "_meta": {"threadId": "00000000-0000-4000-8000-000000000001"}}},
            ]
            env = {k: v for k, v in os.environ.items() if k != 'ROOKERY_SESSION'}
            process = subprocess.run([sys.executable, '-m', 'rookery.cli', '--state-dir', directory, 'mcp', '--provider', 'codex'],
                input=''.join(json.dumps(f) + '\n' for f in frames), capture_output=True, text=True, timeout=10, env=env)
            self.assertEqual(process.returncode, 0, process.stderr)
            responses = [json.loads(line) for line in process.stdout.splitlines()]
            self.assertEqual(responses[1]['error']['code'], -32602)
            self.assertFalse(json.loads(responses[2]['result']['content'][0]['text'])['running'])

    def test_requires_explicit_session(self):
        with patch.dict("os.environ", {}, clear=True):
            with self.assertRaisesRegex(ValueError, "requires --session"):
                run()

    def test_real_subprocess_with_bound_client(self):
        from rookery.client import configure
        with tempfile.TemporaryDirectory() as directory:
            configure(directory, "test", "http://127.0.0.1:9999", "x" * 32, allow_http=True)
            frames = [
                {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}},
                {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "status"}},
                {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "inbox"}},
            ]
            process = subprocess.run(
                [sys.executable, "-c", "from rookery.mcp import run; run()"],
                input="".join(json.dumps(frame) + "\n" for frame in frames),
                capture_output=True, text=True, timeout=10,
                env={**os.environ, "ROOKERY_HOME": directory, "ROOKERY_SESSION": "test/codex:00000000-0000-4000-8000-000000000001"})
            self.assertEqual(process.returncode, 0, process.stderr)
            responses = [json.loads(line) for line in process.stdout.splitlines()]
            self.assertEqual(len(responses), 3)
            health = json.loads(responses[1]["result"]["content"][0]["text"])
            self.assertFalse(health["running"])
            self.assertEqual(json.loads(responses[2]["result"]["content"][0]["text"]), [])


if __name__ == "__main__":
    unittest.main()
