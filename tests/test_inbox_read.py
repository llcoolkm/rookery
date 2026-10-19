"""Polling consumes native eligibility without pretending the model acknowledged."""
import json
from pathlib import Path
import tempfile
import time
import unittest
import uuid
from unittest.mock import Mock, patch

from rookery.client import Client, configure
from rookery.core import Error
from rookery.daemon import Helper
from rookery.hub import Hub, Overloaded, RETENTION
from rookery.mcp import Server


class InboxReadTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.hub = Hub(root / 'hub.sqlite3', pending_limit=1000)
        self.addCleanup(self.hub.close)
        self.path = root / 'helper'
        configure(self.path, 'laptop', 'http://127.0.0.1:9999', 'x' * 32, allow_http=True)
        self.sender = {'provider': 'claude', 'id': str(uuid.uuid4()), 'status': 'idle', 'pid': 11}
        self.recipient = {'provider': 'codex', 'id': str(uuid.uuid4()), 'status': 'idle', 'pid': 22}
        self.actor = 'laptop/codex:' + self.recipient['id']
        self.sender_actor = 'server/claude:' + self.sender['id']
        for node, session, actor in [('server', self.sender, self.sender_actor), ('laptop', self.recipient, self.actor)]:
            self.hub.call(node, 'publish', {'sessions': [session]})
            self.hub.call(node, 'enroll', {'target': actor})
        self.transport = Mock()
        self.transport.call.side_effect = lambda op, data=None: self.hub.call('laptop', op, data)
        self.submit = Mock(return_value={'status': 'submitted', 'detail': 'test adapter'})
        self.helper = self.new_helper()
        self.client = Client(self.path, self.actor)

    def new_helper(self):
        helper = Helper(self.path, client=self.transport,
                        discover=lambda: ([dict(self.recipient)], {}), submit=self.submit)
        helper.sessions = [dict(self.recipient)]
        return helper

    def send(self, **fields):
        message = self.hub.call('server', 'send', {'actor': self.sender_actor, 'to': self.actor,
            'id': str(uuid.uuid4()), 'body': 'read test', **fields})
        self.helper.sync_once()
        return message

    def message(self, mid):
        return next(m for m in self.hub.call('laptop', 'status')['messages'] if m['id'] == mid)

    def read(self, mids, actor=None, node='laptop'):
        return self.hub.call(node, 'read', {'actor': actor or self.actor, 'message_ids': mids})

    def test_inbox_then_idle_does_not_native_submit(self):
        message = self.send()
        self.assertEqual(self.client.inbox()[0]['id'], message['id'])
        self.helper.requests()
        self.helper.deliver()  # Local pending snapshot intentionally not synced yet.
        self.submit.assert_not_called()
        value = self.message(message['id'])
        self.assertEqual(value['status'], 'read')
        self.assertIn('model receipt unconfirmed', value['detail'])
        self.assertGreater(value['read_at'], 0)

    def test_peek_then_idle_still_submits(self):
        message = self.send()
        self.client.inbox(peek=True)
        self.assertEqual(list((self.path / 'spool').glob('*.json')), [])
        self.helper.deliver()
        self.submit.assert_called_once()
        self.assertEqual(self.message(message['id'])['status'], 'submitted')

    def test_offline_read_survives_helper_restart_without_native_replay(self):
        message = self.send()
        self.client.inbox()
        self.transport.call.side_effect = Error('offline')
        with self.assertRaises(Error):
            self.helper.requests()
        restarted = self.new_helper()
        self.transport.call.side_effect = lambda op, data=None: self.hub.call('laptop', op, data)
        restarted.requests()
        restarted.deliver()
        self.submit.assert_not_called()
        self.assertEqual(self.message(message['id'])['status'], 'read')

    def test_unimported_read_behind_full_batch_delays_native_claim(self):
        message = self.send()
        self.client.inbox()
        # requests() imports only 64 commands; delivery must also see reads that
        # remain in spool and have not yet received an authoritative hub result.
        self.helper.deliver()
        self.submit.assert_not_called()
        self.assertEqual(self.message(message['id'])['status'], 'pending')
        self.helper.requests()
        self.assertEqual(self.message(message['id'])['status'], 'read')

    def test_read_during_dispatch_preserves_claim_and_held_returns_read(self):
        message = self.send()
        delivery = self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery']
        self.read([message['id']])
        value = self.message(message['id'])
        self.assertEqual(value['status'], 'dispatching')
        self.assertEqual(value['claim_id'], delivery['claim_id'])
        self.hub.call('laptop', 'result', {'message_id': message['id'], 'claim_id': delivery['claim_id'], 'status': 'held'})
        self.assertEqual(self.message(message['id'])['status'], 'read')
        self.assertIsNone(self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'])

    def test_lost_mcp_result_can_be_fetched_again_without_acknowledgment(self):
        message = self.send()
        server = Server(self.client)
        server.handle({'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2025-06-18'}})
        request = {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {'name': 'inbox'}}
        server.handle(request)  # Simulate losing the successful tool response.
        self.helper.requests()
        self.helper.sync_once()
        second = server.handle({**request, 'id': 3})
        returned = json.loads(second['result']['content'][0]['text'])
        self.assertEqual(returned[0]['id'], message['id'])
        self.assertEqual(returned[0]['status'], 'read')
        self.assertEqual(list((self.path / 'spool').glob('*.json')), [])

    def test_read_batch_authentication_is_atomic(self):
        message = self.send()
        reverse = self.hub.call('laptop', 'send', {'actor': self.actor, 'to': self.sender_actor,
                            'id': str(uuid.uuid4()), 'body': 'other recipient'})
        with self.assertRaises(Error):
            self.read([message['id'], reverse['id']])
        self.assertEqual(self.message(message['id'])['status'], 'pending')
        self.assertNotIn('read_at', self.message(message['id']))
        with self.assertRaises(Error):
            self.read([message['id']], node='server')
        for ids in ([], [message['id']] * 101, [message['id'], 'invalid']):
            with self.assertRaises(Error):
                self.read(ids)
        self.assertEqual(self.message(message['id'])['status'], 'pending')

    def test_read_keeps_quota_until_expiry_then_body_is_retained_temporarily(self):
        message = self.send(ttl=60)
        self.read([message['id']])
        self.hub.pending_limit = 1
        with self.assertRaises(Overloaded):
            self.send()
        with patch('rookery.hub.time.time', return_value=message['expires'] + 1):
            self.assertEqual(self.message(message['id'])['status'], 'expired')
        with patch('rookery.hub.time.time', return_value=message['expires'] + RETENTION + 2):
            value = self.message(message['id'])
            self.assertFalse(value['body_retained'])
            self.assertEqual(value['body'], '')

    def test_mail_that_expired_unseen_stays_listed_until_read_once(self):
        # A recipient that was unreachable until expiry must still see the mail.
        message = self.send(ttl=60)
        with patch('rookery.hub.time.time', return_value=message['expires'] + 1):
            self.assertEqual(self.message(message['id'])['status'], 'expired')
            self.helper.sync_once()
            listed = self.client.inbox()
            self.assertEqual([m['id'] for m in listed], [message['id']])
            self.helper.requests()
            value = self.message(message['id'])
            self.assertEqual(value['status'], 'expired')   # read does not revive it
            self.assertGreater(value['read_at'], 0)
            self.helper.sync_once()
            self.assertEqual(self.client.inbox(), [])

    def test_replies_default_to_a_day_and_asks_to_an_hour(self):
        from rookery import client as client_module
        self.assertEqual(client_module.REPLY_TTL, 86400)
        import inspect
        self.assertEqual(inspect.signature(Client.reply).parameters['ttl'].default, 86400)
        self.assertEqual(inspect.signature(Client.send).parameters['ttl'].default, 3600)

    def test_later_read_does_not_downgrade_native_or_model_states(self):
        for status in ('submitted', 'uncertain', 'failed', 'acknowledged', 'replied'):
            with self.subTest(status=status):
                message = self.send()
                if status in ('submitted', 'uncertain', 'failed'):
                    delivery = self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery']
                    self.hub.call('laptop', 'result', {'message_id': message['id'], 'claim_id': delivery['claim_id'], 'status': status})
                elif status == 'acknowledged':
                    self.hub.call('laptop', 'ack', {'message_id': message['id'], 'actor': self.actor})
                else:
                    self.hub.call('laptop', 'reply', {'message_id': message['id'], 'actor': self.actor,
                                  'id': str(uuid.uuid4()), 'body': 'answer'})
                self.read([message['id']])
                self.assertEqual(self.message(message['id'])['status'], status)

    def test_failed_local_read_admission_does_not_return_consumed_inbox(self):
        self.send()
        with patch('rookery.client.MAX_PENDING_COMMANDS', 0):
            with self.assertRaisesRegex(Error, 'queue is full'):
                self.client.inbox()
            self.assertEqual(len(self.client.inbox(peek=True)), 1)

    def test_large_inbox_queues_one_batch(self):
        for _ in range(70):
            self.send()
        messages = self.client.inbox(limit=100)
        self.assertEqual(len(messages), 70)
        spool = list((self.path / 'spool').glob('*.json'))
        self.assertEqual(len(spool), 1)
        self.assertEqual(len(json.loads(spool[0].read_text())['data']['message_ids']), 70)
        self.helper.requests()
        self.helper.deliver()
        self.submit.assert_not_called()

    def test_inbox_prefers_older_unread_over_recently_consumed_batch(self):
        older = self.send(body='older unread')
        newer = self.send(body='newer unread')
        first = self.client.inbox(limit=1)
        self.assertEqual(first[0]['id'], newer['id'])
        self.helper.requests()
        self.helper.sync_once()
        # The read receipt just bumped newer.updated; it must not starve older.
        second = self.client.inbox(limit=1)
        self.assertEqual(second[0]['id'], older['id'])
        self.helper.requests()
        self.helper.sync_once()
        recovery = self.client.inbox(limit=2)
        self.assertEqual({m['id'] for m in recovery}, {older['id'], newer['id']})
        self.assertTrue(all(m['status'] == 'read' for m in recovery))
        self.assertEqual(list((self.path / 'spool').glob('*.json')), [])


if __name__ == '__main__':
    unittest.main()
