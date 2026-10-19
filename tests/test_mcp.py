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
        self.client.send.return_value = {"message_id": "123", "status": "queued"}
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
        self.assertEqual(json.loads(result["result"]["content"][0]["text"])["status"], "queued")

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
        self.assertEqual(value, {'sessions': [{'key': 'server/codex:one', 'name': 'One', 'online': True}], 'stale': False})
        self.client.status.return_value = {'connected': True}
        self.client.identity.return_value = 'server/codex:one'
        response = self.request('tools/call', {'name': 'status'})
        self.assertEqual(json.loads(response['result']['content'][0]['text'])['identity'], 'server/codex:one')

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
