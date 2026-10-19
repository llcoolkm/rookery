import unittest
from unittest.mock import patch

from rookery.mcp_identity import ClaudeIdentity, IdentityUnavailable

A = '11111111-1111-4111-8111-111111111111'
B = '22222222-2222-4222-8222-222222222222'


class ClaudeIdentityTests(unittest.TestCase):
    def resolve(self, identity, ancestors=None, rows=None):
        with patch('rookery.mcp_identity._ancestors', return_value=ancestors or {20: 200, 10: 100}), \
             patch('rookery.mcp_identity._sessions', return_value=rows if rows is not None else [{'pid': 10, 'sessionId': A}]):
            return identity()

    def test_nearest_claude_ancestor_not_env_or_other_session(self):
        with patch.dict('os.environ', {'CLAUDE_CODE_SESSION_ID': B}):
            result = self.resolve(ClaudeIdentity(), rows=[{'pid': 10, 'sessionId': B}, {'pid': 20, 'sessionId': A}, {'pid': 90, 'sessionId': B}])
        self.assertEqual(result, 'claude:' + A)

    def test_same_process_clear_follows_current_discovery(self):
        identity = ClaudeIdentity()
        self.assertEqual(self.resolve(identity), 'claude:' + A)
        self.assertEqual(self.resolve(identity, rows=[{'pid': 10, 'sessionId': B}]), 'claude:' + B)

    def test_missing_ambiguous_and_invalid_session_refused(self):
        for rows in ([], [{'pid': 10, 'sessionId': A}] * 2, [{'pid': 10, 'sessionId': 'bad'}], [{'pid': 10}]):
            with self.subTest(rows=rows), self.assertRaises(IdentityUnavailable):
                self.resolve(ClaudeIdentity(), rows=rows)

    def test_pid_reuse_or_reparent_refused(self):
        for ancestors in ({20: 200, 10: 101}, {20: 200, 30: 300}):
            identity = ClaudeIdentity()
            self.resolve(identity)
            with self.assertRaises(IdentityUnavailable):
                self.resolve(identity, ancestors=ancestors)

    def test_ancestry_change_during_discovery_refused(self):
        with patch('rookery.mcp_identity._ancestors', side_effect=[{10: 100}, {10: 101}]), \
             patch('rookery.mcp_identity._sessions', return_value=[{'pid': 10, 'sessionId': A}]), \
             self.assertRaises(IdentityUnavailable):
            ClaudeIdentity()()

    def test_discovery_failure_never_uses_previous_binding(self):
        identity = ClaudeIdentity()
        self.resolve(identity)
        with patch('rookery.mcp_identity._ancestors', return_value={20: 200, 10: 100}), \
             patch('rookery.mcp_identity._sessions', side_effect=OSError('private diagnostic')), \
             self.assertRaisesRegex(IdentityUnavailable, '^Claude session identity unavailable; reconnect MCP$'):
            identity()
