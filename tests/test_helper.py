"""Runtime boundaries: durable request IDs, submission uncertainty and busy peers."""
import copy
import http.client
import json
from pathlib import Path
import tempfile
import time
import unittest
import uuid
from unittest.mock import Mock, patch

from rookery.client import Client, configure
from rookery.core import Error
from rookery.daemon import Helper, HubClient, lock
from rookery.local_store import Store


class FakeHub:
    def __init__(self):
        self.messages = {}
        self.calls = []
        self.lose_send_response = False
        self.claims = {}

    def call(self, op, data=None):
        data = data or {}
        self.calls.append((op, copy.deepcopy(data)))
        if op == 'send':
            message = self.messages.setdefault(data['id'], {
                'id': data['id'], 'sender': data['actor'], 'recipient': data['to'],
                'kind': data['kind'], 'body': data['body'], 'expires': data['expires'],
                'status': 'pending', 'received_by_host': False,
            })
            if self.lose_send_response:
                self.lose_send_response = False
                raise Error('simulated connection lost after hub commit')
            return copy.deepcopy(message)
        if op == 'received':
            self.messages[data['message_id']]['received_by_host'] = True
            return {}
        if op == 'claim':
            message = self.messages[data['message_id']]
            if message['status'] != 'pending':
                return {'delivery': None}
            claim_id = str(uuid.uuid4())
            self.claims[claim_id] = data['message_id']
            message['status'] = 'dispatching'
            return {'delivery': {'claim_id': claim_id}}
        if op == 'result':
            message = self.messages[data['message_id']]
            assert self.claims[data['claim_id']] == message['id']
            message['status'] = 'pending' if data['status'] == 'held' else data['status']
            return {}
        if op == 'sync':
            return {'cursor': 1, 'events': [{'type': 'message', 'data': copy.deepcopy(m)} for m in self.messages.values()]}
        raise AssertionError('unexpected call: ' + op)


class RealHub:
    """Adapter giving a real Hub the HubClient call(op, data) shape for one node."""

    def __init__(self, hub, node):
        self.hub, self.node = hub, node

    def call(self, op, data=None):
        return self.hub.call(self.node, op, data)


class HelperHubAdmissionTests(unittest.TestCase):
    """Helper publication and hub admission apply one policy end to end."""

    def setUp(self):
        from rookery.hub import Hub
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name)
        self.hub = Hub(root / 'hub.sqlite3')
        self.addCleanup(self.hub.close)
        configure(root / 'state', 'test', 'http://127.0.0.1:9999', 'x' * 32, allow_http=True)
        self.sender = {'provider': 'claude', 'id': str(uuid.uuid4()), 'pid': 101, 'status': 'idle'}
        self.target = {'provider': 'codex', 'id': str(uuid.uuid4()), 'pid': 202, 'status': 'busy'}
        self.submit = Mock(return_value={'status': 'submitted', 'detail': 'accepted'})
        discover = lambda: ([copy.deepcopy(self.sender), copy.deepcopy(self.target)], {})
        self.helper = Helper(root / 'state', client=RealHub(self.hub, 'test'), discover=discover, submit=self.submit)
        self.helper.publish()
        for session in (self.sender, self.target):
            self.hub.call('test', 'enroll', {'target': f"test/{session['provider']}:{session['id']}"})

    def deliver(self):
        self.hub.call('test', 'send', {'actor': 'test/claude:' + self.sender['id'], 'id': str(uuid.uuid4()),
                                       'to': 'test/codex:' + self.target['id'], 'body': 'work'})
        self.helper.sync_once()
        self.helper.deliver()

    def test_busy_host_provider_is_held_by_default(self):
        self.deliver()
        self.submit.assert_not_called()

    def test_claude_opt_in_is_published_and_claimed_end_to_end(self):
        from rookery import providers
        self.addCleanup(providers.configure, {})
        self.target = {'provider': 'claude', 'id': str(uuid.uuid4()), 'pid': 303, 'status': 'busy'}
        self.helper.discover = lambda: ([copy.deepcopy(self.sender), copy.deepcopy(self.target)], {})
        providers.configure({'busy_delivery': ['claude']})
        self.helper.publish()
        self.hub.call('test', 'enroll', {'target': 'test/claude:' + self.target['id']})
        self.hub.call('test', 'send', {'actor': 'test/claude:' + self.sender['id'], 'id': str(uuid.uuid4()),
                                       'to': 'test/claude:' + self.target['id'], 'body': 'work'})
        self.helper.sync_once()
        self.helper.deliver()
        self.submit.assert_called_once()
        self.target['status'] = 'waiting'          # a dialog opened: published as idle-only
        self.helper.publish()
        live = next(s for s in self.hub.call('test', 'list')['sessions'] if s['id'] == self.target['id'])
        self.assertEqual(live['status'], 'waiting')

    def test_busy_capable_host_provider_is_claimed_and_submitted(self):
        from rookery import providers
        with patch.object(type(providers.get('codex')), 'capabilities',
                          providers.Capabilities(topology='host', accepts_busy=True)):
            self.helper.publish()
            self.deliver()
        self.submit.assert_called_once()


class HelperTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name)
        configure(self.path, 'test', 'http://127.0.0.1:9999', 'x' * 32, allow_http=True)
        self.hub = FakeHub()
        self.session = {'provider': 'codex', 'id': str(uuid.uuid4()), 'pid': 999, 'status': 'idle'}
        self.submit = Mock(return_value={'status': 'submitted', 'detail': 'accepted'})
        self.discover = Mock(side_effect=lambda: ([copy.deepcopy(self.session)], {}))
        self.helper = Helper(self.path, client=self.hub, discover=self.discover, submit=self.submit)
        self.helper.sessions = [copy.deepcopy(self.session)]

    def test_helper_publishes_explicit_agy_polling_registration(self):
        agy = {'provider': 'agy', 'id': str(uuid.uuid4()), 'name': 'Antigravity CLI',
               'pid': 999, 'status': 'waiting', 'delivery': 'polling'}
        with patch('rookery.providers.discover_all', return_value=([self.session], {})), \
             patch('rookery.polling.discover', return_value=[agy]):
            sessions, errors = self.helper.discover_sessions()
        self.assertEqual(sessions, [self.session, agy])
        self.assertEqual(errors, {})

    def test_explicit_polling_registration_overrides_native_discovery(self):
        polling = {'provider': 'codex', 'id': self.session['id'], 'name': 'Codex polling',
                   'pid': 999, 'status': 'waiting', 'delivery': 'polling'}
        with patch('rookery.providers.discover_all', return_value=([self.session], {})), \
             patch('rookery.polling.discover', return_value=[polling]):
            sessions, _ = self.helper.discover_sessions()
        self.assertEqual(sessions, [polling])

    def test_live_attached_endpoint_wins_over_polling_registration(self):
        attached = {'provider': 'agy', 'id': str(uuid.uuid4()), 'pid': 999, 'status': 'unknown',
                    'delivery': 'attached', 'socket': '/x'}
        polling = {**attached, 'status': 'waiting', 'delivery': 'polling'}
        del polling['socket']
        with patch('rookery.providers.discover_all', return_value=([attached], {})), \
             patch('rookery.polling.discover', return_value=[polling]):
            sessions, _ = self.helper.discover_sessions()
        self.assertEqual(sessions, [attached])

    def test_uuid_held_by_two_live_processes_is_not_published(self):
        # The hub re-pins an enrolled UUID to the published PID; with two live
        # processes it could pick the wrong one, so neither is published.
        twin = {**self.session, 'pid': self.session.get('pid', 1) + 1}
        other = {**self.session, 'id': str(uuid.uuid4())}
        with patch('rookery.providers.discover_all', return_value=([self.session, twin, other], {})), \
             patch('rookery.polling.discover', return_value=[]), patch('sys.stderr') as err:
            sessions, _ = self.helper.discover_sessions()
            self.helper.discover_sessions()
        self.assertEqual(sessions, [other])
        # Logged once when it appears, not on every discovery cycle.
        self.assertEqual(sum('more than one live process' in str(c) for c in err.write.call_args_list), 1)

    def incoming(self):
        message = {'id': str(uuid.uuid4()), 'sender': 'remote/claude:sender',
                   'recipient': 'test/codex:' + self.session['id'], 'kind': 'ask',
                   'body': 'Review this', 'expires': time.time() + 60,
                   'status': 'pending', 'received_by_host': False}
        self.hub.messages[message['id']] = copy.deepcopy(message)
        self.helper.store.apply([{'type': 'message', 'data': message}], 1)
        return message

    def test_lost_send_response_retries_same_id_after_helper_restart(self):
        client = Client(self.path, actor='test/codex:' + self.session['id'])
        mid = str(uuid.uuid4())
        queued = client.request('send', {'actor': client.identity(), 'id': mid,
                                'to': 'remote/claude:receiver', 'body': 'test',
                                'kind': 'ask', 'expires': time.time() + 60}, timeout=0)
        self.hub.lose_send_response = True
        with self.assertRaises(Error):
            self.helper.requests()
        self.assertEqual(len(self.hub.messages), 1)
        restarted = Helper(self.path, client=self.hub, discover=self.discover, submit=self.submit)
        restarted.requests()
        sends = [data for op, data in self.hub.calls if op == 'send']
        self.assertEqual(len(sends), 2)
        self.assertEqual(sends[0], sends[1])
        self.assertEqual(sends[0]['id'], mid)
        self.assertEqual(len(self.hub.messages), 1)
        self.assertEqual(restarted.store.commands(), [])
        result = json.loads((self.path / 'results' / (queued['request_id'] + '.json')).read_text())
        self.assertTrue(result['ok'])
        self.assertEqual(result['result']['id'], mid)

    def test_crash_during_submission_becomes_uncertain_and_is_not_replayed(self):
        message = self.incoming()

        class SimulatedProcessDeath(BaseException):
            pass

        self.submit.side_effect = SimulatedProcessDeath()
        with self.assertRaises(SimulatedProcessDeath):
            self.helper.deliver()
        restarted = Helper(self.path, client=self.hub, discover=self.discover, submit=self.submit)
        restarted.sessions = [copy.deepcopy(self.session)]
        restarted.store.recover_attempts()
        restarted.report_attempts()
        self.assertEqual(self.hub.messages[message['id']]['status'], 'uncertain')
        # Even an old local pending row cannot obtain a new claim from the hub.
        restarted.deliver()
        self.submit.assert_called_once()
        self.assertEqual(restarted.store.reports(), [])

    def test_busy_session_is_received_but_not_claimed(self):
        self.incoming()
        self.helper.sessions[0]['status'] = 'busy'
        self.helper.deliver()
        self.submit.assert_not_called()
        self.assertEqual([op for op, _ in self.hub.calls], ['received'])

    def test_session_becomes_busy_after_claim_is_held_without_input(self):
        message = self.incoming()
        self.session['status'] = 'busy'
        self.helper.deliver()
        self.submit.assert_not_called()
        reports = [data for op, data in self.hub.calls if op == 'result']
        self.assertEqual(reports[0]['status'], 'held')
        self.assertEqual(self.hub.messages[message['id']]['status'], 'pending')

    def test_attached_agy_session_is_submitted_while_busy(self):
        self.session = {'provider': 'agy', 'id': str(uuid.uuid4()), 'pid': 999, 'status': 'busy',
                        'delivery': 'attached', 'socket': '/x'}
        self.helper.sessions = [copy.deepcopy(self.session)]
        message = self.incoming()
        message['recipient'] = 'test/agy:' + self.session['id']
        self.hub.messages[message['id']]['recipient'] = message['recipient']
        self.helper.store.apply([{'type': 'message', 'data': copy.deepcopy(self.hub.messages[message['id']])}], 2)
        self.helper.deliver()
        self.submit.assert_called_once()
        self.assertEqual(self.hub.messages[message['id']]['status'], 'submitted')

    def test_replacement_process_is_held_not_written_to(self):
        from rookery import providers
        self.addCleanup(providers.configure, {})
        for opt_in, provider, status in ((False, 'codex', 'idle'), (True, 'claude', 'busy')):
            with self.subTest(provider=provider):
                providers.configure({'busy_delivery': ['claude']} if opt_in else {'busy_delivery': []})
                self.submit.reset_mock()
                self.session = {'provider': provider, 'id': str(uuid.uuid4()), 'pid': 999, 'status': status, 'socket': '/x'}
                self.helper.sessions = [copy.deepcopy(self.session)]
                message = self.incoming()
                message['recipient'] = f'test/{provider}:' + self.session['id']
                self.hub.messages[message['id']]['recipient'] = message['recipient']
                self.helper.store.apply([{'type': 'message', 'data': copy.deepcopy(self.hub.messages[message['id']])}], 2)
                self.session['pid'] = 1000      # same UUID, resumed in a new process
                self.helper.deliver()
                self.submit.assert_not_called()
                result = [data for op, data in self.hub.calls if op == 'result'][-1]
                self.assertEqual(result['status'], 'held')
                self.assertEqual(self.helper.sessions[0]['pid'], 1000)   # the cache keeps the fresh view

    def test_session_found_not_ready_is_not_reclaimed_before_rediscovery(self):
        # Live case: an opted-in busy Claude opens a dialog; the cached view
        # still says busy (admissible) while fresh discovery says waiting.
        from rookery import providers
        providers.configure({'busy_delivery': ['claude']})
        self.addCleanup(providers.configure, {})
        self.session = {'provider': 'claude', 'id': self.session['id'], 'pid': 999, 'status': 'busy', 'socket': '/x'}
        self.helper.sessions = [copy.deepcopy(self.session)]
        message = self.incoming()
        message['recipient'] = 'test/claude:' + self.session['id']
        self.hub.messages[message['id']]['recipient'] = message['recipient']
        self.helper.store.apply([{'type': 'message', 'data': copy.deepcopy(self.hub.messages[message['id']])}], 2)
        self.session['status'] = 'waiting'
        for _ in range(3):
            self.helper.deliver()
        self.submit.assert_not_called()
        self.assertEqual([op for op, _ in self.hub.calls].count('claim'), 1)

    def test_provider_exception_is_uncertain_not_retried(self):
        message = self.incoming()
        self.submit.side_effect = OSError('socket disappeared after write')
        self.helper.deliver()
        self.assertEqual(self.hub.messages[message['id']]['status'], 'uncertain')
        self.helper.sessions = [copy.deepcopy(self.session)]
        self.helper.deliver()
        self.submit.assert_called_once()

    def test_success_is_submitted_not_model_acknowledged(self):
        message = self.incoming()
        self.helper.deliver()
        self.assertEqual(self.hub.messages[message['id']]['status'], 'submitted')
        self.assertIn('not user authorization', self.submit.call_args.args[1])
        self.helper.sync_once()
        client = Client(self.path, actor=message['recipient'])
        self.assertEqual(client.status(message['id'])['status'], 'submitted')

    def test_sync_cursor_and_messages_rollback_together(self):
        message = self.incoming()
        malformed = {'id': 'malformed'}
        with self.assertRaises(KeyError):
            self.helper.store.apply([{'type': 'message', 'data': {**message, 'status': 'submitted'}},
                                     {'type': 'message', 'data': malformed}], 99)
        self.assertEqual(self.helper.store.get('cursor'), 1)
        self.assertEqual(self.helper.store.message(message['id'])['status'], 'pending')

    def test_single_instance_lock(self):
        with lock(self.path):
            with self.assertRaisesRegex(Error, 'already owns'):
                with lock(self.path):
                    self.fail('second helper acquired lock')
        with lock(self.path):
            pass

    def test_spool_import_is_idempotent(self):
        request_id = str(uuid.uuid4())
        spool = self.path / 'spool' / (request_id + '.json')
        payload = json.dumps({'id': request_id, 'op': 'list', 'data': {}, 'created': time.time()})
        spool.write_text(payload)
        self.helper.store.import_spool()
        spool.write_text(payload)
        Store(self.path).import_spool()
        self.assertEqual(len(self.helper.store.commands()), 1)

    def test_old_command_response_does_not_overwrite_new_sync_state(self):
        message = self.incoming()
        request_id = str(uuid.uuid4())
        self.helper.store.apply([{'type': 'message', 'data': {**message, 'status': 'replied'}}], 2)
        with self.helper.store.connect() as db:
            db.execute('INSERT INTO commands VALUES (?,?,?,?)',
                       (request_id, 'send', json.dumps({'id': message['id']}), time.time()))
        self.helper.client = Mock()
        self.helper.client.call.return_value = message
        self.helper.requests()
        self.assertEqual(self.helper.store.message(message['id'])['status'], 'replied')
        self.assertTrue((self.path / 'results' / (request_id + '.json')).exists())

    def test_local_admission_counts_database_and_spool(self):
        client = Client(self.path, actor='test/codex:' + self.session['id'])
        with patch('rookery.client.MAX_PENDING_COMMANDS', 2):
            client.request('list', {}, timeout=0)
            self.helper.store.import_spool()
            client.request('list', {}, timeout=0)
            with self.assertRaisesRegex(Error, 'queue is full'):
                client.request('list', {}, timeout=0)
        self.assertEqual(len(self.helper.store.commands()), 1)
        self.assertEqual(len(list((self.path / 'spool').glob('*.json'))), 1)

    def test_local_admission_bytes_and_body_validation(self):
        client = Client(self.path, actor='test/codex:' + self.session['id'])
        with patch('rookery.client.MAX_PENDING_BYTES', 10):
            with self.assertRaisesRegex(Error, 'queue is full'):
                client.request('list', {}, timeout=0)
        with self.assertRaisesRegex(Error, 'UTF-8 bytes'):
            client.send('remote/claude:receiver', 'ü' * 10000)
        with self.assertRaisesRegex(Error, 'ttl'):
            client.send('remote/claude:receiver', 'test', ttl=True)
        self.assertEqual(list((self.path / 'spool').glob('*.json')), [])

    def test_session_binding_requires_uuid_and_own_node(self):
        with self.assertRaises(Error):
            Client(self.path, actor='test/codex:not-a-uuid')
        with self.assertRaises(Error):
            Client(self.path, actor='other/codex:' + self.session['id'])

    def test_interrupted_http_body_is_retryable(self):
        from unittest.mock import MagicMock
        client = HubClient({'url': 'http://127.0.0.1:9999', 'token': 'x' * 32})
        response = MagicMock()
        response.read.side_effect = http.client.IncompleteRead(b'partial', 100)
        client.opener = Mock()
        client.opener.open.return_value = response
        with self.assertRaisesRegex(Error, 'original request retained'):
            client.call('sync', {'cursor': 1})

    def test_maximum_escaped_message_batch_fits_bounded_http_response(self):
        from io import BytesIO
        from rookery.core import MAX_BYTES
        from rookery.daemon import MAX_RESPONSE
        client = HubClient({'url': 'https://hub.example', 'token': 'x' * 32})
        sid = '00000000-0000-4000-8000-000000000001'
        key = f'{"a" * 32}/{"p" * 32}:{sid}'
        # Bound every serializable field, not only ordinary body text. NUL
        # costs six wire bytes per UTF-8 byte with the hub's JSON encoding.
        message = dict(v=1, id=sid, sender=key, recipient=key, kind='reply',
                       body='\0' * MAX_BYTES, detail='\0' * 4096, status='acknowledged',
                       in_reply_to=sid, conversation=sid, reply_id=sid, followup_id=sid,
                       claim_id=sid, ttl=86400, requested_expires=10**308, created=1e308,
                       expires=1e308, updated=1e308, received_at=1e308, read_at=1e308,
                       hops=6, received_by_host=True, body_retained=True)
        session = dict(name='\0' * 256, key=key, node='a' * 32, provider='p' * 32,
                       id=sid, alias='a' * 64, pid=2147483647, status='unknown',
                       delivery='attached', admission='busy', enrolled=True, online=False,
                       enrollment_current=False,
                       delivery_blocker='session PID changed; re-enroll this exact session')
        messages, sessions = [message] * 128, [session] * 4096
        raw = json.dumps({'ok': True, 'result': {'messages': messages, 'sessions': sessions,
                                               'cursor': 2**63 - 1, 'next_after': 2**63 - 1}},
                         ensure_ascii=False).encode()
        self.assertGreater(len(raw), 4 * 1024 * 1024)
        self.assertLess(len(raw), MAX_RESPONSE)
        client.opener = Mock()
        client.opener.open.return_value = BytesIO(raw)
        result = client.call('bootstrap')
        self.assertEqual(len(result['messages']), 128)
        self.assertEqual(len(result['sessions']), 4096)

    def test_http_response_size_ceiling_still_refuses_excess(self):
        from io import BytesIO
        client = HubClient({'url': 'https://hub.example', 'token': 'x' * 32})
        client.opener = Mock()
        client.opener.open.return_value = BytesIO(b'x' * 1025)
        with patch('rookery.daemon.MAX_RESPONSE', 1024):
            with self.assertRaisesRegex(Error, 'response too large'):
                client.call('bootstrap')

    def test_bootstrap_failure_preserves_cursor_until_final_page(self):
        message = self.incoming()
        second = {**message, 'id': str(uuid.uuid4()), 'body': 'Second page'}
        client = Mock()
        client.call.side_effect = [
            {'messages': [message], 'cursor': 200, 'next_after': 128},
            Error('connection failed between pages'),
        ]
        with self.assertRaises(Error):
            self.helper.bootstrap(client)
        self.assertEqual(self.helper.store.get('cursor'), 1)
        restarted = Helper(self.path, client=client, discover=self.discover, submit=self.submit)
        client.call.side_effect = [
            {'messages': [message], 'cursor': 200, 'next_after': 128},
            {'messages': [second], 'cursor': 200, 'next_after': None},
        ]
        restarted.bootstrap(client)
        self.assertEqual(restarted.store.get('cursor'), 200)
        self.assertEqual(len(restarted.store.messages()), 2)
        self.assertEqual(client.call.call_args.args[1]['cursor'], 200)


if __name__ == '__main__':
    unittest.main()
