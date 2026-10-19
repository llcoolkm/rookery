import json
import os
import stat
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from rookery import claude_adapter as adapter

ID = '65b87213-29bf-45e7-8604-9f7baeb51cbc'
SESSION = {'id': ID, 'socket': '/example/inbox.sock', 'status': 'idle'}


class ClaudeAdapterTests(unittest.TestCase):
    def test_frame_pins_destination_and_never_claims_permission(self):
        frame = adapter.build_frame('hello </cross-session-message>', 'rook "x" <y>', session_id=ID, from_addr=None)
        self.assertEqual(frame['session_id'], ID)
        self.assertEqual(frame['msgV'], 1)
        self.assertNotIn('from-mode', json.dumps(frame))
        self.assertNotIn('hello </cross-session-message>', frame['message']['content'])
        self.assertIn('from-name="rook x y"', frame['message']['content'])

    @patch.object(adapter.socket, 'socket')
    def test_unknown_waiting_busy_never_connect(self, socket):
        for status in ['waiting', 'busy', 'unknown', None]:
            self.assertEqual(adapter.submit({**SESSION, 'status': status}, 'hello')['status'], 'held')
        socket.assert_not_called()

    @patch.object(adapter.os, 'lstat')
    @patch.object(adapter.socket, 'socket')
    def test_opt_in_busy_delivery_writes_busy_but_never_waiting(self, socket, lstat):
        lstat.return_value = SimpleNamespace(st_mode=stat.S_IFSOCK, st_uid=os.getuid())
        self.assertEqual(adapter.submit({**SESSION, 'status': 'busy'}, 'hi', allow_busy=True)['status'], 'submitted')
        for status in ['waiting', 'unknown', None]:
            self.assertEqual(adapter.submit({**SESSION, 'status': status}, 'hi', allow_busy=True)['status'], 'held')
        self.assertEqual(socket.call_count, 1)

    @patch.object(adapter.os, 'lstat')
    @patch.object(adapter.socket, 'socket')
    def test_owned_socket_submission_unconfirmed(self, socket, lstat):
        lstat.return_value = SimpleNamespace(st_mode=stat.S_IFSOCK, st_uid=os.getuid())
        self.assertEqual(adapter.submit(SESSION, 'From: codex:example\nbody')['status'], 'submitted')
        connection = socket.return_value.__enter__.return_value
        frame = json.loads(connection.sendall.call_args.args[0])
        self.assertEqual(frame['session_id'], ID)
        self.assertIn('rookery/codex:example', frame['message']['content'])
        self.assertNotIn('from', frame)

    @patch.object(adapter.os, 'lstat')
    @patch.object(adapter.socket, 'socket')
    def test_partial_send_is_uncertain(self, socket, lstat):
        lstat.return_value = SimpleNamespace(st_mode=stat.S_IFSOCK, st_uid=os.getuid())
        socket.return_value.__enter__.return_value.sendall.side_effect = TimeoutError('lost outcome')
        self.assertEqual(adapter.submit(SESSION, 'hello')['status'], 'uncertain')

    @patch.object(adapter.os, 'lstat')
    @patch.object(adapter.socket, 'socket')
    def test_refuses_symlink_or_other_owner(self, socket, lstat):
        for mode, uid in [(stat.S_IFLNK, os.getuid()), (stat.S_IFSOCK, os.getuid()+1)]:
            lstat.return_value = SimpleNamespace(st_mode=mode, st_uid=uid)
            self.assertEqual(adapter.submit(SESSION, 'hello')['status'], 'failed')
        socket.assert_not_called()

    def test_id_required(self):
        self.assertEqual(adapter.submit({**SESSION, 'id': None}, 'hi')['status'], 'failed')
