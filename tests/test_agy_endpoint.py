"""AGY attached endpoint: strict agentapi results, private socket, pinning and staleness."""
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import threading
import time
import unittest
import uuid
from unittest.mock import patch

from rookery import agy_endpoint
from rookery.core import Error
from rookery.local_store import Store

FAKE_AGENTAPI = '''#!{python}
import json, os, sys, time
mode = os.environ.get("FAKE_MODE", "ok")
conversation = sys.argv[2]
if mode == "ok":
    print(json.dumps({{"response": {{"sendMessage": {{"recipientId": conversation, "content": sys.argv[3]}}}}}}))
elif mode == "other":
    print(json.dumps({{"response": {{"sendMessage": {{"recipientId": "someone-else"}}}}}}))
elif mode == "unset":
    print(json.dumps({{"error": "ANTIGRAVITY_LS_ADDRESS is not set"}})); sys.exit(1)
elif mode == "leak":
    print(json.dumps({{"error": "rejected by " + os.environ["ANTIGRAVITY_LS_ADDRESS"]}})); sys.exit(1)
elif mode == "slow":
    time.sleep(5)
else:
    print("garbage"); sys.exit(2)
'''


class AgentapiTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        exe = Path(self.tmp.name) / 'agentapi'
        exe.write_text(FAKE_AGENTAPI.format(python=sys.executable))
        exe.chmod(0o700)
        self.conversation = str(uuid.uuid4())
        self.env = {'PATH': self.tmp.name, 'ANTIGRAVITY_LS_ADDRESS': '127.0.0.1:4242',
                    'ANTIGRAVITY_CSRF_TOKEN': 'token-value'}

    def send(self, mode, text='hello', **kwargs):
        with patch.dict(os.environ, {**self.env, 'FAKE_MODE': mode}):
            return agy_endpoint.send_message(self.conversation, text, **kwargs)

    def test_matching_acceptance_is_submitted_not_received(self):
        result = self.send('ok')
        self.assertEqual(result['status'], 'submitted')
        self.assertIn('receipt unconfirmed', result['detail'])

    def test_mismatched_or_unparseable_acceptance_is_uncertain(self):
        self.assertEqual(self.send('other')['status'], 'uncertain')
        self.assertEqual(self.send('garbage')['status'], 'uncertain')

    def test_configuration_error_before_sending_is_failed(self):
        self.assertEqual(self.send('unset')['status'], 'failed')

    def test_error_text_never_carries_credential_values(self):
        result = self.send('leak')
        self.assertEqual(result['status'], 'uncertain')
        self.assertNotIn('127.0.0.1:4242', result['detail'])
        self.assertIn('[redacted]', result['detail'])

    def test_timeout_is_uncertain(self):
        self.assertEqual(self.send('slow', timeout=0.5)['status'], 'uncertain')

    def test_limits_and_missing_executable_fail_without_sending(self):
        self.assertEqual(self.send('ok', text='')['status'], 'failed')
        self.assertEqual(self.send('ok', text='x' * (agy_endpoint.MAX_TEXT + 1))['status'], 'failed')
        with patch.dict(os.environ, {'ANTIGRAVITY_AGENTAPI_EXE': '', 'PATH': self.tmp.name + '/none'}):
            self.assertEqual(agy_endpoint.send_message(self.conversation, 'x')['status'], 'failed')

    def test_multicall_binary_receives_agentapi_subcommand(self):
        with patch.dict(os.environ, {'ANTIGRAVITY_AGENTAPI_EXE': '/usr/bin/agy', 'PATH': self.tmp.name + '/none'}), \
             patch('os.access', return_value=True):
            self.assertEqual(agy_endpoint._agentapi(), ['/usr/bin/agy', 'agentapi'])


class EndpointTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.store = Store(Path(self.tmp.name) / 'state')
        self.conversation = str(uuid.uuid4())
        self.agy_pid = 4242
        self.starts = {self.agy_pid: '100', os.getpid(): '200'}

        def pin(pid):
            if pid not in self.starts:
                raise Error('polling process is unavailable')
            return {'starttime': self.starts[pid], 'boot_id': 'boot'}
        patcher = patch.object(agy_endpoint, '_pin', side_effect=pin)
        patcher.start()
        self.addCleanup(patcher.stop)
        for name in ('HEARTBEAT', 'METADATA_INTERVAL'):
            beat = patch.object(agy_endpoint, name, 0.2)
            beat.start()
            self.addCleanup(beat.stop)
        self.sent = []

        def sender(conversation, text):
            self.sent.append((conversation, text))
            return {'status': 'submitted', 'detail': 'accepted'}
        self.exists = True
        self.endpoint = agy_endpoint.Endpoint(self.store, self.conversation, self.agy_pid, 'AGY test', sender=sender,
                                              checker=lambda conversation: self.exists)
        self.assertTrue(self.endpoint.acquire())
        self.endpoint.bind()
        self.endpoint.write_record()
        self.thread = threading.Thread(target=self.endpoint.serve, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop)

    def stop(self):
        self.endpoint.stopping = True
        self.thread.join(timeout=5)

    def session(self):
        sessions = agy_endpoint.discover(self.store)
        self.assertEqual(len(sessions), 1)
        return sessions[0]

    def test_discovered_session_is_attached_and_submit_reaches_sender(self):
        session = self.session()
        self.assertEqual((session['id'], session['pid'], session['delivery']), (self.conversation, self.agy_pid, 'attached'))
        result = agy_endpoint.submit(session, 'peer text')
        self.assertEqual(result['status'], 'submitted')
        self.assertEqual(self.sent, [(self.conversation, 'peer text')])
        self.assertEqual(agy_endpoint.ping(session)['conversation'], self.conversation)

    def test_record_socket_and_log_are_private_and_log_has_no_text(self):
        agy_endpoint.submit(self.session(), 'confidential body')
        paths = agy_endpoint._paths(self.store, self.conversation)
        for name in ('record', 'socket', 'log'):
            self.assertEqual(paths[name].stat().st_mode & 0o077, 0, name)
        self.assertNotIn('confidential body', paths['log'].read_text())

    def test_request_for_another_conversation_is_refused(self):
        forged = {**self.session(), 'id': str(uuid.uuid4())}
        result = agy_endpoint.submit(forged, 'x')
        self.assertEqual(result['status'], 'failed')
        self.assertEqual(self.sent, [])

    def test_detach_records_a_sticky_detached_marker(self):
        # The endpoint here runs in this test process; never signal ourselves.
        with patch.object(agy_endpoint, '_terminate', return_value=True) as kill:
            agy_endpoint.detach(self.store, self.conversation)
        kill.assert_called_once()
        self.assertTrue(agy_endpoint._paths(self.store, self.conversation)['record'].with_suffix('.detached').exists())

    def test_second_endpoint_for_same_conversation_cannot_start(self):
        other = agy_endpoint.Endpoint(self.store, self.conversation, self.agy_pid, 'dup')
        self.assertFalse(other.acquire())

    def test_stale_heartbeat_is_not_deliverable(self):
        record = agy_endpoint._paths(self.store, self.conversation)['record']
        data = json.loads(record.read_text())
        data['heartbeat'] = time.time() - agy_endpoint.STALE - 5
        self.endpoint.stopping = True
        self.thread.join(timeout=5)
        record.write_text(json.dumps(data))
        record.chmod(0o600)
        states = [entry['state'] for entry in agy_endpoint.status(self.store)]
        self.assertNotIn('attached', states)
        self.assertEqual(agy_endpoint.discover(self.store), [])

    def test_conversation_not_found_stops_endpoint(self):
        self.exists = False
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())
        self.assertEqual(agy_endpoint.discover(self.store), [])

    def test_unknown_metadata_answer_keeps_endpoint(self):
        self.exists = None
        time.sleep(0.5)
        self.assertEqual(len(agy_endpoint.discover(self.store)), 1)

    def second(self, agy_pid=None):
        other = str(uuid.uuid4())
        endpoint = agy_endpoint.Endpoint(self.store, other, agy_pid or self.agy_pid, 'AGY other',
                                         checker=lambda conversation: True)
        self.assertTrue(endpoint.acquire())
        endpoint.bind()
        endpoint.write_record()
        self.addCleanup(endpoint.cleanup)
        return other

    def test_switch_in_same_agy_process_supersedes_the_previous_endpoint(self):
        current = self.second()
        # Both endpoints run in this test process; never signal ourselves.
        with patch.object(agy_endpoint, '_terminate', return_value=True) as kill:
            stopped = agy_endpoint.supersede(self.store, current, self.agy_pid, confirm=lambda: True)
        self.assertEqual(stopped, [self.conversation])
        kill.assert_called_once_with(os.getpid(), {'starttime': '200', 'boot_id': 'boot'})
        # Withdrawn from discovery at once, before the endpoint has exited.
        self.assertEqual([s['id'] for s in agy_endpoint.discover(self.store)], [current])
        states = {e['id']: e['state'] for e in agy_endpoint.status(self.store)}
        self.assertEqual(states[self.conversation], 'superseded')
        # The fenced endpoint refuses new input and stops on its own heartbeat.
        old = {'provider': 'agy', 'id': self.conversation, 'pid': self.agy_pid,
               'socket': str(agy_endpoint._paths(self.store, self.conversation)['socket'])}
        self.assertEqual(agy_endpoint.submit(old, 'late')['status'], 'failed')
        self.assertEqual(self.sent, [])
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())
        log = agy_endpoint._paths(self.store, self.conversation)['log'].read_text()
        self.assertIn('"superseded"', log)
        self.assertIn(current, log)
        # Not sticky: returning to the old conversation may attach again.
        self.assertFalse(agy_endpoint._paths(self.store, self.conversation)['record'].with_suffix('.detached').exists())

    def test_endpoint_of_another_agy_process_is_left_alone(self):
        self.starts[5151] = '300'
        current = self.second(agy_pid=5151)
        with patch.object(agy_endpoint, '_terminate') as kill:
            self.assertEqual(agy_endpoint.supersede(self.store, current, 5151), [])
        kill.assert_not_called()

    def test_reused_agy_pid_from_an_earlier_process_is_left_alone(self):
        current = str(uuid.uuid4())
        self.starts[self.agy_pid] = '999'  # same pid, a different process now
        with patch.object(agy_endpoint, '_terminate') as kill:
            self.assertEqual(agy_endpoint.supersede(self.store, current, self.agy_pid), [])
        kill.assert_not_called()

    def test_own_conversation_is_never_superseded(self):
        with patch.object(agy_endpoint, '_terminate') as kill:
            self.assertEqual(agy_endpoint.supersede(self.store, self.conversation, self.agy_pid), [])
        kill.assert_not_called()

    def test_unconfirmed_classification_supersedes_nothing(self):
        current = self.second()
        with patch.object(agy_endpoint, '_terminate') as kill:
            with self.assertRaises(agy_endpoint.Unconfirmed):
                agy_endpoint.supersede(self.store, current, self.agy_pid, confirm=lambda: False)
        kill.assert_not_called()
        self.assertIn(self.conversation, [s['id'] for s in agy_endpoint.discover(self.store)])

    def test_no_candidates_is_not_a_failed_confirmation(self):
        confirm = unittest.mock.Mock(return_value=False)
        self.assertEqual(agy_endpoint.supersede(self.store, self.conversation, self.agy_pid, confirm=confirm), [])
        confirm.assert_not_called()

    def test_dead_endpoint_record_replaced_meanwhile_is_kept(self):
        current = self.second()
        record = agy_endpoint._paths(self.store, self.conversation)['record']

        def replaced(pid, pin):
            data = json.loads(record.read_text())
            data['endpoint_pid'], data['endpoint_starttime'] = 7777, '888'  # a newer generation
            agy_endpoint.common.atomic(record, data)
            return False  # the old endpoint was already gone
        with patch.object(agy_endpoint, '_terminate', side_effect=replaced):
            agy_endpoint.supersede(self.store, current, self.agy_pid)
        self.assertTrue(record.exists())
        self.assertEqual(json.loads(record.read_text())['endpoint_pid'], 7777)

    def test_record_of_a_live_lock_holder_is_never_unlinked(self):
        # The running endpoint in this test holds the conversation lock.
        record = agy_endpoint._paths(self.store, self.conversation)['record']
        stale = json.loads(record.read_text())
        agy_endpoint._unlink_if_unchanged(self.store, self.conversation, stale)
        self.assertTrue(record.exists())

    def test_dead_endpoint_record_is_unlinked_under_the_lock(self):
        self.endpoint.stopping = True
        self.thread.join(timeout=5)
        record = agy_endpoint._paths(self.store, self.conversation)['record']
        data = {'endpoint_pid': 4, 'endpoint_starttime': '5'}
        agy_endpoint.common.atomic(record, {**json.loads('{}'), **data})
        os.close(self.endpoint.lock_fd)  # the stopped endpoint releases its lock
        agy_endpoint._unlink_if_unchanged(self.store, self.conversation, {'endpoint_pid': 9, 'endpoint_starttime': '9'})
        self.assertTrue(record.exists())   # a different generation's record is kept
        agy_endpoint._unlink_if_unchanged(self.store, self.conversation, data)
        self.assertFalse(record.exists())

    def test_transient_cleanup_lock_is_not_reported_as_attached(self):
        # Codex repro: cleanup holds the lock, attach must not claim a live endpoint.
        self.endpoint.stopping = True
        self.thread.join(timeout=5)
        os.close(self.endpoint.lock_fd)
        other = str(uuid.uuid4())
        lock = agy_endpoint._paths(self.store, other)['lock']
        fd = os.open(lock, os.O_CREAT | os.O_RDWR, 0o600)
        agy_endpoint.fcntl.flock(fd, agy_endpoint.fcntl.LOCK_EX)
        self.addCleanup(os.close, fd)
        with patch.object(agy_endpoint, 'LOCK_WAIT', 0.3):
            with self.assertRaisesRegex(Error, 'lock is busy'):
                agy_endpoint._run(self.store, other, self.agy_pid, 'x', None)

    def test_lock_released_during_wait_lets_attach_proceed(self):
        other = str(uuid.uuid4())
        lock = agy_endpoint._paths(self.store, other)['lock']
        fd = os.open(lock, os.O_CREAT | os.O_RDWR, 0o600)
        agy_endpoint.fcntl.flock(fd, agy_endpoint.fcntl.LOCK_EX)
        threading.Timer(0.3, os.close, (fd,)).start()
        started = threading.Event()
        def serve(endpoint_self):
            started.set()
        with patch.object(agy_endpoint.Endpoint, 'serve', serve), \
                patch.object(agy_endpoint.Endpoint, 'bind'), patch.object(agy_endpoint.Endpoint, 'write_record'), \
                patch.object(agy_endpoint.signal, 'signal'):
            result = agy_endpoint._run(self.store, other, self.agy_pid, 'x', None)
        self.assertTrue(started.is_set())
        self.assertNotIn('already', result)

    def test_slow_submission_in_flight_keeps_its_outcome_and_queued_one_is_refused(self):
        started, release = threading.Event(), threading.Event()

        def slow(conversation, text):
            self.sent.append((conversation, text))
            started.set()
            release.wait(5)
            return {'status': 'submitted', 'detail': 'accepted'}
        self.endpoint.sender = slow
        session = self.session()
        results = {}
        first = threading.Thread(target=lambda: results.setdefault('first', agy_endpoint.submit(session, 'one')))
        first.start()
        self.assertTrue(started.wait(5))
        second = threading.Thread(target=lambda: results.setdefault('second', agy_endpoint.submit(session, 'two')))
        second.start()
        time.sleep(0.2)  # the second request is queued behind the first
        current = self.second()

        def terminate(pid, pin):
            self.endpoint.stopping = True
            return True
        with patch.object(agy_endpoint, '_terminate', side_effect=terminate):
            self.assertEqual(agy_endpoint.supersede(self.store, current, self.agy_pid), [self.conversation])
        self.assertNotIn(self.conversation, [s['id'] for s in agy_endpoint.discover(self.store)])
        release.set()
        first.join(5)
        second.join(5)
        self.assertEqual(results['first']['status'], 'submitted')
        self.assertEqual(results['second']['status'], 'failed')
        self.assertEqual(self.sent, [(self.conversation, 'one')])

    def test_agy_exit_stops_endpoint_and_withdraws_record(self):
        del self.starts[self.agy_pid]
        self.thread.join(timeout=5)
        self.assertFalse(self.thread.is_alive())
        self.assertFalse(agy_endpoint._paths(self.store, self.conversation)['record'].exists())
        self.assertEqual(agy_endpoint.discover(self.store), [])


class ConversationExistenceTests(AgentapiTests):
    def check(self, body):
        exe = Path(self.tmp.name) / 'agentapi'
        exe.write_text('#!' + sys.executable + '\nimport sys\nprint(' + repr(body) + ')\n')
        with patch.dict(os.environ, self.env):
            return agy_endpoint.conversation_exists(self.conversation)

    def test_metadata_found_not_found_and_unknown(self):
        self.assertIs(self.check(json.dumps({'response': {'conversationMetadata': {'metadata': {'createdAt': 'x'}}}})), True)
        self.assertIs(self.check(json.dumps({'error': 'rpc error: code = NotFound desc = not_found'})), False)
        self.assertIsNone(self.check(json.dumps({'error': 'rpc error: code = Unavailable'})))
        self.assertIsNone(self.check('garbage'))


class SocketPathTests(unittest.TestCase):
    def test_long_state_paths_use_a_short_private_runtime_socket(self):
        with tempfile.TemporaryDirectory() as tmp:
            deep = Path(tmp) / ('d' * 90) / 'state'
            store = Store(deep)
            with patch.dict(os.environ, {'XDG_RUNTIME_DIR': os.path.join(tmp, 'run')}):
                paths = agy_endpoint._paths(store, str(uuid.uuid4()))
                self.assertLessEqual(len(str(paths['socket'])), agy_endpoint.SOCKET_PATH_MAX)
                self.assertEqual(paths['socket'].parent.stat().st_mode & 0o077, 0)
                self.assertEqual(paths['record'].parent, agy_endpoint.directory(store))


class CrossEnvironmentSocketTests(unittest.TestCase):
    """The endpoint's shell and the helper's supervisor may disagree on XDG_RUNTIME_DIR."""

    def test_reader_with_different_runtime_dir_still_accepts_the_recorded_socket(self):
        with tempfile.TemporaryDirectory() as tmp:
            store = Store(Path(tmp) / ('d' * 90) / 'state')
            conversation = str(uuid.uuid4())
            starts = {4242: '100', os.getpid(): '200'}
            pin = lambda pid: {'starttime': starts[pid], 'boot_id': 'boot'}
            with patch.object(agy_endpoint, '_pin', side_effect=pin):
                with patch.dict(os.environ, {'XDG_RUNTIME_DIR': os.path.join(tmp, 'run')}):
                    endpoint = agy_endpoint.Endpoint(store, conversation, 4242, 'cross', sender=lambda c, t: {})
                    self.assertTrue(endpoint.acquire())
                    endpoint.bind()
                    endpoint.write_record()
                try:
                    with patch.dict(os.environ, {'XDG_RUNTIME_DIR': os.path.join(tmp, 'different-run')}):
                        self.assertEqual([e['state'] for e in agy_endpoint.status(store)], ['attached'])
                    with patch.dict(os.environ, {}, clear=False):
                        os.environ.pop('XDG_RUNTIME_DIR', None)
                        self.assertEqual(len(agy_endpoint.discover(store)), 1)
                finally:
                    endpoint.cleanup()

    def test_recorded_socket_elsewhere_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            store = Store(Path(tmp) / 'state')
            conversation = str(uuid.uuid4())
            foreign = Path(tmp) / 'elsewhere'
            foreign.mkdir(mode=0o700)
            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            sock.bind(str(foreign / f'agy-{conversation}.sock'))
            self.addCleanup(sock.close)
            self.assertFalse(agy_endpoint._valid_socket(store, conversation, str(foreign / f'agy-{conversation}.sock')))
            self.assertFalse(agy_endpoint._valid_socket(store, conversation, 'relative.sock'))


class HelperSideTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, 'endpoint.sock')
        self.conversation = str(uuid.uuid4())

    def serve_once(self, reply):
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(self.path)
        server.listen(1)

        def run():
            connection, _ = server.accept()
            with connection:
                connection.recv(65536)
                if reply is not None:
                    connection.sendall(reply)
            server.close()
        thread = threading.Thread(target=run, daemon=True)
        thread.start()
        return thread

    def session(self):
        return {'provider': 'agy', 'id': self.conversation, 'delivery': 'attached', 'socket': self.path}

    def test_missing_socket_fails_before_sending(self):
        self.assertEqual(agy_endpoint.submit(self.session(), 'x')['status'], 'failed')

    def test_closed_without_reply_after_send_is_uncertain(self):
        thread = self.serve_once(None)
        self.assertEqual(agy_endpoint.submit(self.session(), 'x')['status'], 'uncertain')
        thread.join(timeout=5)

    def test_reply_for_another_request_is_uncertain(self):
        reply = json.dumps({'request_id': str(uuid.uuid4()), 'status': 'submitted'}).encode() + b'\n'
        thread = self.serve_once(reply)
        self.assertEqual(agy_endpoint.submit(self.session(), 'x')['status'], 'uncertain')
        thread.join(timeout=5)


class ClassifyTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.brain = Path(self.tmp.name) / 'brain'
        self.parent, self.child = str(uuid.uuid4()), str(uuid.uuid4())
        (self.brain / self.parent / '.system_generated' / 'subagents').mkdir(parents=True)
        (self.brain / self.parent / '.system_generated' / 'subagents' / f'{self.child}.json').write_text('{}')
        (self.brain / self.child).mkdir()

    def test_recorded_child_is_a_subagent_and_its_parent_is_top_level(self):
        self.assertEqual(agy_endpoint.classify(self.child, self.brain), 'subagent')
        self.assertEqual(agy_endpoint.classify(self.parent, self.brain), 'top-level')

    def test_missing_store_or_own_directory_is_unknown_not_top_level(self):
        self.assertEqual(agy_endpoint.classify(self.parent, self.brain / 'missing'), 'unknown')
        self.assertEqual(agy_endpoint.classify(str(uuid.uuid4()), self.brain), 'unknown')

    def test_unreadable_store_is_unknown(self):
        if os.geteuid() == 0:
            self.skipTest('root reads everything')
        self.brain.chmod(0o100)  # traversable but not listable
        self.addCleanup(self.brain.chmod, 0o700)
        self.assertEqual(agy_endpoint.classify(self.parent, self.brain), 'unknown')

    def test_delayed_child_record_is_caught_by_the_recheck(self):
        late = str(uuid.uuid4())
        (self.brain / late).mkdir()
        record = self.brain / self.parent / '.system_generated' / 'subagents' / f'{late}.json'
        real = agy_endpoint.time.sleep
        with patch.object(agy_endpoint.time, 'sleep', side_effect=lambda s: record.write_text('{}')):
            self.assertFalse(agy_endpoint.confirmed_top_level(late, self.brain))
        self.assertTrue(agy_endpoint.confirmed_top_level(self.parent, self.brain))
        del real

    def test_brain_directory_follows_the_hook_payload_only_for_its_own_conversation(self):
        own = str(self.brain / self.child)
        self.assertEqual(agy_endpoint.brain_directory(own, self.child), self.brain)
        with patch.dict(os.environ, {'HOME': self.tmp.name}):
            default = Path(self.tmp.name) / agy_endpoint.BRAIN
            self.assertEqual(agy_endpoint.brain_directory(own, self.parent), default)
            self.assertEqual(agy_endpoint.brain_directory('relative/' + self.child, self.child), default)
            self.assertEqual(agy_endpoint.brain_directory(None, self.child), default)

    def test_malformed_id_is_rejected(self):
        with self.assertRaises(Error):
            agy_endpoint.classify('*', self.brain)


class AttachValidationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.store = Store(Path(self.tmp.name) / 'state')

    def test_attach_requires_agy_shell_credentials(self):
        clean = {k: v for k, v in os.environ.items() if not k.startswith('ANTIGRAVITY_')}
        with patch.dict(os.environ, clean, clear=True):
            with self.assertRaises(Error):
                agy_endpoint.attach(self.store, str(uuid.uuid4()))

    def test_attach_refuses_a_different_conversation(self):
        env = {'ANTIGRAVITY_LS_ADDRESS': 'a', 'ANTIGRAVITY_CSRF_TOKEN': 'b',
               'ANTIGRAVITY_CONVERSATION_ID': str(uuid.uuid4())}
        with patch.dict(os.environ, env):
            with self.assertRaises(Error):
                agy_endpoint.attach(self.store, str(uuid.uuid4()))

    def test_attach_refuses_a_subagent_conversation(self):
        conversation = str(uuid.uuid4())
        record = Path(self.tmp.name) / 'home' / agy_endpoint.BRAIN / str(uuid.uuid4()) / '.system_generated' / 'subagents'
        record.mkdir(parents=True)
        (record / f'{conversation}.json').write_text('{}')
        env = {'ANTIGRAVITY_LS_ADDRESS': 'a', 'ANTIGRAVITY_CSRF_TOKEN': 'b',
               'ANTIGRAVITY_CONVERSATION_ID': conversation, 'HOME': str(Path(self.tmp.name) / 'home')}
        with patch.dict(os.environ, env), patch.object(agy_endpoint, 'find_agy') as find:
            with self.assertRaisesRegex(Error, 'subagent'):
                agy_endpoint.attach(self.store, conversation)
        find.assert_not_called()

    def attach_env(self, conversation):
        return {'ANTIGRAVITY_LS_ADDRESS': 'a', 'ANTIGRAVITY_CSRF_TOKEN': 'b',
                'ANTIGRAVITY_CONVERSATION_ID': conversation, 'HOME': str(Path(self.tmp.name) / 'home')}

    def test_attach_refuses_an_unconfirmed_conversation(self):
        conversation = str(uuid.uuid4())  # no AGY store at all
        with patch.dict(os.environ, self.attach_env(conversation)), \
                patch.object(agy_endpoint, 'find_agy') as find, patch.object(agy_endpoint, 'supersede') as supersede:
            with self.assertRaisesRegex(Error, 'top-level'):
                agy_endpoint.attach(self.store, conversation)
        find.assert_not_called()
        supersede.assert_not_called()

    def test_disproved_recheck_aborts_attach_and_stops_nothing(self):
        conversation = str(uuid.uuid4())
        home = Path(self.tmp.name) / 'home'
        (home / agy_endpoint.BRAIN / conversation).mkdir(parents=True)
        parent = str(uuid.uuid4())
        with patch.dict(os.environ, self.attach_env(conversation)), \
                patch.object(agy_endpoint, 'find_agy', return_value=1), \
                patch.object(agy_endpoint, '_pin', return_value={'starttime': '1', 'boot_id': 'b'}), \
                patch.object(agy_endpoint, 'inspect', return_value=[{'id': parent, 'agy_pid': 1}]), \
                patch.object(agy_endpoint, '_read_private',
                             return_value={'starttime': '1', 'boot_id': 'b', 'endpoint_pid': 5, 'endpoint_starttime': '6'}), \
                patch.object(agy_endpoint, 'confirmed_top_level', return_value=False), \
                patch.object(agy_endpoint, '_terminate') as kill, \
                patch.object(agy_endpoint, '_run') as run, patch.object(agy_endpoint.os, 'fork') as fork, \
                patch.object(agy_endpoint.common, 'atomic') as fence:
            with self.assertRaises(agy_endpoint.Unconfirmed):
                agy_endpoint.attach(self.store, conversation, foreground=True)
        run.assert_not_called()
        fork.assert_not_called()
        kill.assert_not_called()
        fence.assert_not_called()

    def test_invalid_name_fails_before_any_side_effect(self):
        conversation = str(uuid.uuid4())
        (Path(self.tmp.name) / 'home' / agy_endpoint.BRAIN / conversation).mkdir(parents=True)
        with patch.dict(os.environ, self.attach_env(conversation)), \
                patch.object(agy_endpoint, 'find_agy', return_value=1), \
                patch.object(agy_endpoint, 'supersede') as supersede:
            for name in (' ', 'x' * 257):
                with self.assertRaisesRegex(Error, 'name'):
                    agy_endpoint.attach(self.store, conversation, name)
        supersede.assert_not_called()

    def test_find_agy_requires_an_agy_ancestor(self):
        with self.assertRaises(Error):
            agy_endpoint.find_agy()


if __name__ == '__main__':
    unittest.main()
