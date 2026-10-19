"""Runtime messaging, transaction and node isolation contract for the v2 hub."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing, contextmanager
import hashlib
import json
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest
from unittest.mock import patch
import uuid

from rookery.core import Error
from rookery.hub import Hub, Overloaded, RETENTION, DISPATCH_TIMEOUT
from rookery.request_ids import new_request_id, request_created, REQUEST_WINDOW


def uid():
    return str(uuid.uuid4())


class HubTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / 'hub.sqlite3'
        self.hub = Hub(self.path)
        self.addCleanup(lambda: self.hub.close())
        self.a = dict(provider='claude', id=uid(), name='A', status='idle', pid=101)
        self.b = dict(provider='codex', id=uid(), name='B', status='idle', pid=202)
        self.ak = 'server/claude:' + self.a['id']
        self.bk = 'laptop/codex:' + self.b['id']
        for node, session, key, alias in [('server', self.a, self.ak, 'reviewer'), ('laptop', self.b, self.bk, 'worker')]:
            self.hub.call(node, 'publish', {'sessions': [session]})
            self.hub.call(node, 'enroll', {'target': key, 'alias': alias})

    def send(self, **fields):
        return self.hub.call('server', 'send', {'actor': self.ak, 'to': self.bk, 'body': 'A summons', 'id': uid(), **fields})

    def result(self, delivery, status):
        return self.hub.call('laptop', 'result', dict(message_id=delivery['message']['id'], claim_id=delivery['claim_id'], status=status))

    def messages(self, node='server'):
        return self.hub.call(node, 'status')['messages']

    def test_answering_a_reply_sends_one_threaded_follow_up_ask(self):
        ask = self.send()
        reply = self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=ask['id'], id=uid(), body='Answer'))
        follow = self.hub.call('server', 'reply', dict(actor=self.ak, message_id=reply['id'], id=uid(), body='Follow-up'))
        self.assertEqual((follow['kind'], follow['recipient'], follow['in_reply_to']), ('ask', self.bk, reply['id']))
        self.assertEqual((follow['conversation'], follow['hops']), (ask['id'], 2))
        parent = next(m for m in self.messages() if m['id'] == reply['id'])
        self.assertEqual(parent['followup_id'], follow['id'])
        self.assertNotEqual(parent['status'], 'replied')
        with self.assertRaisesRegex(Error, 'already has a follow-up'):
            self.hub.call('server', 'reply', dict(actor=self.ak, message_id=reply['id'], id=uid(), body='Again'))
        from rookery.daemon import render
        envelope = json.loads(render(follow).splitlines()[1])
        self.assertEqual((envelope['in_reply_to'], envelope['conversation']), (reply['id'], ask['id']))
        self.assertNotIn('in_reply_to', json.loads(render(ask).splitlines()[1]))
        first = json.loads(render(reply).splitlines()[1])  # conversation equals in_reply_to
        self.assertEqual(first['in_reply_to'], ask['id'])
        self.assertNotIn('conversation', first)
        answer = self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=follow['id'], id=uid(), body='Second answer'))
        self.assertEqual((answer['kind'], answer['hops']), ('reply', 3))

    def test_thread_stops_at_max_hops_with_a_clear_error(self):
        from rookery.core import MAX_HOPS
        message, actors = self.send(), [(self.bk, 'laptop'), (self.ak, 'server')]
        for hop in range(1, MAX_HOPS):
            actor, node = actors[(hop - 1) % 2]
            message = self.hub.call(node, 'reply', dict(actor=actor, message_id=message['id'], id=uid(), body=f'hop {hop}'))
        actor, node = actors[(MAX_HOPS - 1) % 2]
        with self.assertRaisesRegex(Error, 'message limit'):
            self.hub.call(node, 'reply', dict(actor=actor, message_id=message['id'], id=uid(), body='too far'))

    def test_reply_refusals_name_the_reason(self):
        inform = self.send(kind='inform')
        with self.assertRaisesRegex(Error, 'inform needs a new message'):
            self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=inform['id'], id=uid(), body='x'))
        ask = self.send()
        self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=ask['id'], id=uid(), body='first'))
        with self.assertRaisesRegex(Error, 'already answered'):
            self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=ask['id'], id=uid(), body='second'))

    def test_graph_metadata_creation_counts_and_current_states(self):
        now = int(time.time()) // 60 * 60
        with patch('rookery.hub.time.time', return_value=now - 150):
            ask = self.send(body='SECRET BODY')
        with patch('rookery.hub.time.time', return_value=now - 90):
            self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=ask['id'], id=uid(), body='SECRET REPLY'))
        with patch('rookery.hub.time.time', return_value=now - 30):
            self.send(kind='inform')
        query = f'from={(now-180)*1000}&to={now*1000}&interval=60'.encode()
        graph = self.hub.graph(query)
        self.assertEqual([(b['ask'], b['reply'], b['inform']) for b in graph['activity']], [(1,0,0),(0,1,0),(0,0,1)])
        self.assertEqual({n['title'] for n in graph['nodes']}, {'A', 'B'})
        edge = next(e for e in graph['edges'] if e['source'] == self.ak)
        self.assertEqual(edge['mainStat'], 2)
        self.assertEqual(edge['detail__pending'], 1)  # ask is replied now
        self.assertNotIn('SECRET', json.dumps(graph))
        self.assertNotIn('body', json.dumps(graph))
        self.assertFalse(graph['truncated'])
        self.assertEqual(sum(n['mainStat'] for n in graph['nodes']), 6)
        self.assertEqual(self.hub.graph(f'from={(now-240)*1000}&to={now*1000}&interval=60'.encode())['activity'][0]['ask'], 0)

    def test_graph_activity_buckets_are_utc_aligned_across_refreshes(self):
        now = int(time.time()) // 300 * 300
        with patch('rookery.hub.time.time', return_value=now - 90):
            self.send()
        first = self.hub.graph(f'from={(now-3600+10)*1000}&to={now*1000}&interval=300'.encode())
        second = self.hub.graph(f'from={(now-3600+20)*1000}&to={(now+10)*1000}&interval=300'.encode())
        self.assertTrue(all(row['time'] % 300000 == 0 for row in first['activity'] + second['activity']))
        self.assertEqual(next(row for row in first['activity'] if row['ask']),
                         next(row for row in second['activity'] if row['ask']))
        self.assertEqual(first['activity'][0]['time'], (now-3600)*1000)
        default = self.hub.graph(f'from={(now-3600)*1000}&to={now*1000}'.encode())
        self.assertEqual(default['bucket_seconds'], 60)

    def test_graph_bounds_truncation_and_exact_activity(self):
        for _ in range(3):
            self.send()
        # Default window ends at millisecond precision; advance past all writes.
        with patch('rookery.hub.GRAPH_MESSAGES', 1), patch('rookery.hub.time.time', return_value=time.time() + 1):
            graph = self.hub.graph()
        self.assertTrue(graph['truncated'])
        self.assertEqual(sum(e['mainStat'] for e in graph['edges']), 1)
        self.assertEqual(sum(b['ask'] for b in graph['activity']), 3)
        with patch('rookery.hub.GRAPH_NODES', 1), patch('rookery.hub.time.time', return_value=time.time() + 1):
            self.assertEqual(self.hub.graph()['nodes'], [])
            self.assertTrue(self.hub.graph()['truncated'])
        with patch('rookery.hub.GRAPH_EDGES', 0), patch('rookery.hub.time.time', return_value=time.time() + 1):
            self.assertEqual(self.hub.graph()['edges'], [])
            self.assertTrue(self.hub.graph()['truncated'])
        now = int(time.time() * 1000)
        with patch('rookery.hub.time.time', return_value=now / 1000):
            graph = self.hub.graph(f'from=0&to={now+100000}&interval=1'.encode())
        self.assertEqual(graph['from'], now - RETENTION * 1000)
        self.assertEqual(graph['to'], now)
        self.assertLessEqual(len(graph['activity']), 300)
        self.assertEqual(graph['summary'][0]['bucket_seconds'], graph['bucket_seconds'])
        for query in (b'from=nan', b'from=-1', b'from=2&to=1', b'interval=0', b'from=1&from=2', b'bad=1', b'from=', b'from=0&to=1', b'interval=1.5'):
            with self.subTest(query=query), self.assertRaises(Error):
                self.hub.graph(query)

    def test_storage_metrics_split_database_and_wal_preserve_total(self):
        # Keep a reader connected so SQLite does not remove the WAL on last close.
        with closing(sqlite3.connect(self.path)) as reader:
            reader.execute('SELECT COUNT(*) FROM messages').fetchone()
            self.send()
            values = self.hub.stats()
            self.assertEqual(values['storage_bytes']['database'], self.path.stat().st_size)
            self.assertEqual(values['storage_bytes']['wal'], Path(str(self.path) + '-wal').stat().st_size)
            self.assertGreater(values['storage_bytes']['wal'], 0)
            self.assertEqual(values['database_bytes'], sum(values['storage_bytes'].values()))
            metrics = self.hub.metrics()
            for name in ('database', 'wal'):
                self.assertIn(f'rookery_hub_storage_bytes{{file="{name}"}} ', metrics)
            self.assertIn('rookery_hub_db_bytes ', metrics)
        self.assertEqual(self.hub.stats()['storage_bytes']['wal'], 0)

    def test_send_alias_binding_request_dedupe_and_content_conflicts(self):
        request, mid = uid(), uid()
        data = dict(actor=self.ak, to='laptop/worker', body='Nonce', id=mid, _request_id=request)
        first = self.hub.call('server', 'send', data)
        self.assertEqual(first['recipient'], self.bk)
        self.assertEqual(self.hub.call('server', 'send', data), first)
        self.assertEqual(self.send(id=mid, body='Nonce'), first)
        for changes in ({'body': 'Other'}, {'ttl': 1}, {'kind': 'inform'}, {'to': self.ak}):
            with self.subTest(changes=changes), self.assertRaises(Error):
                self.send(id=mid, body='Nonce', **changes) if 'body' not in changes else self.send(id=mid, **changes)
        with self.assertRaises(Error):
            self.hub.call('server', 'send', {**data, 'body': 'Changed'})
        self.assertEqual(len(self.messages()), 1)

    def test_node_isolation_and_enrollment_required(self):
        message = self.send()
        self.assertEqual(self.messages('third'), [])
        for op, data in [('send', dict(actor=self.bk, to=self.ak, body='Forgery', id=uid())),
                         ('disable', dict(target=self.bk)), ('ack', dict(actor=self.ak, message_id=message['id'])),
                         ('received', dict(message_id=message['id']))]:
            with self.subTest(op=op), self.assertRaises(Error):
                self.hub.call('server', op, data)
        self.hub.call('laptop', 'disable', {'target': self.bk})
        with self.assertRaises(Error):
            self.send()

    def test_clear_offline_disabled_alias_preserves_history_and_releases_name(self):
        message = self.send()
        self.hub.call('laptop', 'disable', {'target': self.bk})
        replacement = {**self.b, 'id': uid(), 'pid': 303}
        replacement_key = 'laptop/codex:' + replacement['id']
        self.hub.call('laptop', 'publish', {'sessions': [replacement]})
        with self.assertRaisesRegex(Error, 'alias already belongs'):
            self.hub.call('laptop', 'enroll', {'target': replacement_key, 'alias': 'worker'})
        request = {'target': self.bk, '_request_id': uid()}
        result = self.hub.call('laptop', 'clear-alias', request)
        self.assertEqual(result, {'target': self.bk, 'cleared_alias': 'worker'})
        self.assertEqual(self.hub.call('laptop', 'clear-alias', request), result)
        old = next(s for s in self.hub.call('laptop', 'list')['sessions'] if s['key'] == self.bk)
        self.assertFalse(old['enrolled'])
        self.assertFalse(old['online'])
        self.assertIsNone(old['alias'])
        self.assertEqual(self.messages()[0]['id'], message['id'])
        self.assertEqual(self.messages()[0]['recipient'], self.bk)
        self.hub.call('laptop', 'enroll', {'target': replacement_key, 'alias': 'worker'})
        self.assertEqual(self.send(to='laptop/worker')['recipient'], replacement_key)

    def test_clear_alias_requires_exact_owned_existing_session(self):
        for target in (self.bk, 'reviewer', 'server/reviewer', 'server/claude:' + uid()):
            with self.subTest(target=target), self.assertRaises(Error):
                self.hub.call('server', 'clear-alias', {'target': target})
        self.hub.call('server', 'clear-alias', {'target': self.ak})
        row = next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)
        self.assertTrue(row['enrolled'])
        self.assertIsNone(row['alias'])

    def test_resumed_session_with_new_pid_is_repinned_by_its_own_node(self):
        # A same-UUID resume used to leave the session enrolled but silently
        # refusing every claim until a manual re-enroll, so mail expired.
        message = self.send()
        self.hub.call('laptop', 'publish', {'sessions': [{**self.b, 'pid': 303}]})
        row = next(s for s in self.hub.call('laptop', 'list')['sessions'] if s['key'] == self.bk)
        self.assertTrue(row['online'])
        self.assertTrue(row['enrollment_current'])
        self.assertNotIn('delivery_blocker', row)
        self.assertEqual(row['alias'], 'worker')
        self.assertIsNotNone(self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'])

    def test_publish_never_enrolls_a_disabled_session(self):
        self.hub.call('laptop', 'disable', {'target': self.bk})
        self.hub.call('laptop', 'publish', {'sessions': [{**self.b, 'pid': 303}]})
        row = next(s for s in self.hub.call('laptop', 'list')['sessions'] if s['key'] == self.bk)
        self.assertFalse(row['enrolled'])

    def test_another_node_cannot_repin_a_session(self):
        self.hub.call('server', 'publish', {'sessions': [self.a, {**self.b, 'pid': 303}]})
        row = next(s for s in self.hub.call('laptop', 'list')['sessions'] if s['key'] == self.bk)
        self.assertTrue(row['enrolled'])
        self.assertEqual(row['pid'], 202)

    def test_durable_host_receipt_is_not_agent_ack(self):
        message = self.send()
        received = self.hub.call('laptop', 'received', {'message_id': message['id']})
        self.assertTrue(received['received_by_host'])
        self.assertEqual(received['status'], 'pending')
        result = self.hub.call('laptop', 'ack', {'message_id': message['id'], 'actor': self.bk})
        self.assertEqual(result['status'], 'acknowledged')
        self.assertIsNone(self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'])

    def test_competing_claims_are_one_use_and_held_is_definite_pre_io(self):
        message = self.send()
        with ThreadPoolExecutor(max_workers=4) as pool:
            claims = list(pool.map(lambda _: self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'], range(8)))
        delivery = next(value for value in claims if value)
        self.assertEqual(sum(value is not None for value in claims), 1)
        self.assertEqual(self.result(delivery, 'held')['status'], 'pending')
        second = self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery']
        self.assertNotEqual(second['claim_id'], delivery['claim_id'])
        with self.assertRaises(Error):
            self.result(delivery, 'submitted')
        self.assertEqual(self.result(second, 'uncertain')['status'], 'uncertain')
        self.assertIsNone(self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'])

    def test_lost_claim_response_times_out_uncertain_without_restart_or_retry(self):
        message = self.send()
        self.hub.call('laptop', 'claim', {'message_id': message['id']})
        with patch('rookery.hub.time.time', return_value=time.time() + DISPATCH_TIMEOUT + 1):
            self.hub.maintain()
            self.assertEqual(self.messages()[0]['status'], 'uncertain')
            self.assertIsNone(self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'])
        self.assertIsNone(self.hub.call('laptop', 'claim', {'message_id': message['id']})['delivery'])

    def test_restart_marks_unresolved_claim_uncertain_without_native_replay(self):
        message = self.send()
        self.hub.call('laptop', 'claim', {'message_id': message['id']})
        self.hub.close()
        self.hub = Hub(self.path)
        self.assertEqual(self.messages()[0]['status'], 'uncertain')
        self.assertIsNone(self.hub.call('laptop', 'claim')['delivery'])

    def test_late_result_cannot_downgrade_explicit_ack_or_reply(self):
        message = self.send()
        delivery = self.hub.call('laptop', 'claim')['delivery']
        self.hub.call('laptop', 'ack', {'actor': self.bk, 'message_id': message['id']})
        reply = self.hub.call('laptop', 'reply', dict(actor=self.bk, message_id=message['id'], id=uid(), body='Answer'))
        self.assertEqual(self.result(delivery, 'submitted')['status'], 'replied')
        self.assertEqual(reply['recipient'], self.ak)
        self.assertEqual(reply['conversation'], message['conversation'])
        self.assertEqual(reply['in_reply_to'], message['id'])
        self.hub.call('server', 'ack', {'actor': self.ak, 'message_id': reply['id']})
        self.assertEqual(self.messages()[1]['status'], 'acknowledged')

    def test_same_node_lifecycle_uses_one_authority_row(self):
        other = dict(provider='codex', id=uid(), status='idle', name='Local')
        key = 'server/codex:' + other['id']
        self.hub.call('server', 'publish', {'sessions': [self.a, other]})
        self.hub.call('server', 'enroll', {'target': key})
        message = self.send(to=key)
        self.hub.call('server', 'received', {'message_id': message['id']})
        self.hub.call('server', 'ack', {'actor': key, 'message_id': message['id']})
        reply = self.hub.call('server', 'reply', dict(actor=key, message_id=message['id'], body='Local answer', id=uid()))
        self.hub.call('server', 'ack', {'actor': self.ak, 'message_id': reply['id']})
        self.assertEqual([m['status'] for m in self.messages()], ['replied', 'acknowledged'])

    def test_claim_checks_live_pid_and_polling_registration(self):
        message = self.send()
        for session in ({**self.b, 'status': 'busy'}, {**self.b, 'delivery': 'polling'}):
            self.hub.call('laptop', 'publish', {'sessions': [session]})
            self.assertIsNone(self.hub.call('laptop', 'claim')['delivery'])
        self.hub.call('laptop', 'publish', {'sessions': [self.b]})
        self.assertEqual(self.hub.call('laptop', 'claim')['delivery']['message']['id'], message['id'])

    def test_attached_endpoint_is_claimable_while_busy_but_pid_still_pinned(self):
        message = self.send()
        attached = {**self.b, 'status': 'busy', 'delivery': 'attached', 'admission': 'busy'}
        self.hub.call('laptop', 'publish', {'sessions': [{**attached, 'admission': 'none'}]})
        self.assertIsNone(self.hub.call('laptop', 'claim')['delivery'])
        self.hub.call('laptop', 'publish', {'sessions': [attached]})
        delivery = self.hub.call('laptop', 'claim')['delivery']
        self.assertEqual(delivery['message']['id'], message['id'])
        self.assertEqual(delivery['session']['delivery'], 'attached')

    def test_admission_policy_is_applied_as_published(self):
        message = self.send()
        cases = [({**self.b, 'status': 'busy'}, None),                                   # older helper: idle-only
                 ({**self.b, 'status': 'busy', 'admission': 'idle'}, None),
                 ({**self.b, 'status': 'idle', 'admission': 'none'}, None),
                 ({**self.b, 'status': 'busy', 'admission': 'busy', 'delivery': 'polling'}, None),
                 ({**self.b, 'status': 'busy', 'admission': 'busy'}, message['id'])]     # busy-capable host route
        for session, expected in cases:
            with self.subTest(session=session):
                self.hub.call('laptop', 'publish', {'sessions': [session]})
                delivery = self.hub.call('laptop', 'claim')['delivery']
                self.assertEqual(delivery and delivery['message']['id'], expected)

    def test_invalid_admission_is_rejected_atomically_not_read_as_absent(self):
        self.send()
        before = self.hub.call('laptop', 'list')
        for bad in ('bogus', None, 1, ['busy'], {'busy': True}):
            with self.subTest(admission=bad), self.assertRaises(Error):
                self.hub.call('laptop', 'publish', {'sessions': [{**self.b, 'status': 'idle', 'admission': bad}]})
        self.assertEqual(self.hub.call('laptop', 'list'), before)
        self.hub.call('laptop', 'publish', {'sessions': [{**self.b, 'status': 'idle'}]})   # legacy absent: idle
        self.assertIsNotNone(self.hub.call('laptop', 'claim')['delivery'])

    def test_fourth_provider_publishes_enrolls_sends_and_claims(self):
        echo = dict(provider='echo-cli', id=uid(), name='Echo', status='idle', pid=303, admission='idle')
        key = 'laptop/echo-cli:' + echo['id']
        self.hub.call('laptop', 'publish', {'sessions': [self.b, echo]})
        self.hub.call('laptop', 'enroll', {'target': key})
        message = self.hub.call('server', 'send', {'actor': self.ak, 'to': key, 'body': 'hello', 'id': uid()})
        delivery = self.hub.call('laptop', 'claim')['delivery']
        self.assertEqual((delivery['message']['id'], delivery['session']['provider']), (message['id'], 'echo-cli'))
        self.assertEqual(self.result(delivery, 'submitted')['status'], 'submitted')

    def test_unknown_delivery_mode_is_not_published_as_attached(self):
        self.send()
        self.hub.call('laptop', 'publish', {'sessions': [{**self.b, 'status': 'busy', 'delivery': 'teleport'}]})
        self.assertIsNone(self.hub.call('laptop', 'claim')['delivery'])

    def test_sync_is_private_incremental_and_cursor_pages_do_not_skip(self):
        cursor = self.hub.call('server', 'bootstrap')['cursor']
        first, second = self.send(), self.send()
        page = self.hub.call('server', 'sync', {'cursor': cursor, 'limit': 1})
        self.assertTrue(page['more'])
        self.assertEqual(page['events'][0]['data']['id'], first['id'])
        following = self.hub.call('server', 'sync', {'cursor': page['cursor']})
        self.assertEqual(following['events'][0]['data']['id'], second['id'])
        third = self.hub.call('third', 'sync', {'cursor': cursor})
        self.assertEqual(third['events'], [])
        self.assertEqual(third['cursor'], following['cursor'])

    def test_bootstrap_preserves_first_cursor_across_concurrent_updates(self):
        first, second = self.send(), self.send()
        page = self.hub.call('server', 'bootstrap', {'limit': 1})
        cursor = page['cursor']
        self.hub.call('laptop', 'ack', {'actor': self.bk, 'message_id': first['id']})
        next_page = self.hub.call('server', 'bootstrap', {'after': page['next_after'], 'limit': 1, 'cursor': cursor})
        self.assertEqual(next_page['cursor'], cursor)
        self.assertEqual(next_page['messages'][0]['id'], second['id'])
        changes = self.hub.call('server', 'sync', {'cursor': cursor})
        self.assertEqual(changes['events'][0]['data']['status'], 'acknowledged')

    def test_validation_failure_cannot_poison_directory_or_partially_publish(self):
        before = self.hub.call('server', 'list')
        for value in (['bad'], [{**self.a, 'provider': 'Bad Provider!'}], [self.a, self.a], [{**self.a, 'status': []}]):
            with self.subTest(value=value), self.assertRaises(Error):
                self.hub.call('server', 'publish', {'sessions': value})
        self.assertEqual(self.hub.call('server', 'list'), before)
        self.hub.call('laptop', 'publish', {'sessions': []})
        cursor = self.hub.call('server', 'bootstrap')['cursor']
        self.hub.call('laptop', 'publish', {'sessions': []})
        self.assertEqual(self.hub.call('server', 'sync', {'cursor': cursor})['events'], [])

    def test_terminal_body_retention_redacts_requests_and_events_and_keeps_binding(self):
        message = self.send(_request_id=uid())
        self.hub.call('laptop', 'ack', {'actor': self.bk, 'message_id': message['id']})
        with patch('rookery.hub.time.time', return_value=time.time() + RETENTION + 1):
            remaining = self.messages()[0]
            self.assertEqual(remaining['body'], '')
            self.assertFalse(remaining['body_retained'])
            reset = self.hub.call('server', 'sync', {'cursor': 0})
            self.assertTrue(reset['reset_required'])
        with closing(sqlite3.connect(self.path)) as db:
            self.assertNotIn('A summons', ''.join(row[0] for row in db.execute('SELECT result FROM requests')))
            self.assertNotIn('A summons', ''.join(row[0] for row in db.execute('SELECT data FROM events')))
        self.assertEqual(self.send(id=message['id'])['body'], '')
        with self.assertRaises(Error):
            self.send(id=message['id'], body='Different')

    def test_expiry_and_pending_quota_are_explicit(self):
        self.hub.pending_limit = 1
        message = self.send(ttl=1)
        with self.assertRaises(Overloaded):
            self.send()
        with patch('rookery.hub.time.time', return_value=time.time() + 2):
            self.assertIsNone(self.hub.call('laptop', 'claim')['delivery'])
            self.assertEqual(self.messages()[0]['status'], 'expired')
        self.assertEqual(self.send()['status'], 'pending')

    def test_timestamped_request_expires_without_replaying_after_cache_prune(self):
        request_id = new_request_id()
        self.assertEqual(uuid.UUID(request_id).version, 7)
        self.assertAlmostEqual(request_created(request_id), time.time(), delta=1)
        data = {'target': self.ak, '_request_id': request_id}
        disabled = self.hub.call('server', 'disable', data)
        self.assertEqual(self.hub.call('server', 'disable', data), disabled)
        self.hub.call('server', 'enroll', {'target': self.ak})
        with patch('rookery.hub.time.time', return_value=time.time() + REQUEST_WINDOW + 1):
            self.hub.maintain()
            with closing(sqlite3.connect(self.path)) as db:
                self.assertEqual(db.execute('SELECT COUNT(*) FROM requests').fetchone()[0], 0)
            with self.assertRaisesRegex(Error, 'expired'):
                self.hub.call('server', 'disable', data)
        row = next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)
        self.assertTrue(row['enrolled'])

    def test_future_request_id_rejected_without_writing(self):
        with patch('rookery.request_ids.time.time', return_value=time.time() + 600):
            request_id = new_request_id()
        with self.assertRaisesRegex(Error, 'future clock'):
            self.hub.call('server', 'disable', {'target': self.ak, '_request_id': request_id})
        self.assertTrue(next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)['enrolled'])

    def test_legacy_request_cache_is_capped_and_grace_cannot_reopen_after_clock_step(self):
        data = {'target': self.ak, '_request_id': uid()}
        with patch('rookery.hub.MAX_REQUESTS_PER_NODE', 1):
            result = self.hub.call('server', 'disable', data)
            with self.assertRaisesRegex(Overloaded, 'request-identity quota'):
                self.hub.call('server', 'disable', {**data, '_request_id': uid()})
            self.assertEqual(self.hub.call('server', 'disable', data), result)
        with patch('rookery.hub.time.time', return_value=time.time() + REQUEST_WINDOW + 1):
            self.hub.maintain()
        with closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(db.execute('SELECT COUNT(*) FROM requests').fetchone()[0], 0)
            self.assertEqual(db.execute('SELECT COUNT(*) FROM request_expiry').fetchone()[0], 0)
        # The wall clock is now back before the deadline, but the persisted
        # pruning boundary keeps the forgotten legacy operation forbidden.
        with self.assertRaisesRegex(Error, 'legacy request grace period ended'):
            self.hub.call('server', 'disable', data)

    def test_expiry_is_checked_after_waiting_for_transaction_lock(self):
        request_id = new_request_id()
        data = {'target': self.ak, '_request_id': request_id}
        self.hub.call('server', 'disable', data)
        self.hub.call('server', 'enroll', {'target': self.ak})
        expires = request_created(request_id) + REQUEST_WINDOW
        transaction = self.hub._tx
        with patch('rookery.hub.time.time', return_value=expires - 1) as clock:
            @contextmanager
            def waited(*args, **kwargs):
                with transaction(*args, **kwargs) as db:
                    clock.return_value = expires + 1
                    yield db
            with patch.object(self.hub, '_tx', waited):
                with self.assertRaisesRegex(Error, 'expired'):
                    self.hub.call('server', 'disable', data)
        self.assertTrue(next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)['enrolled'])

    def test_pruned_uuid7_request_cannot_reexecute_after_backwards_clock_step(self):
        data = {'target': self.ak, '_request_id': new_request_id()}
        self.hub.call('server', 'disable', data)
        self.hub.call('server', 'enroll', {'target': self.ak})
        expires = request_created(data['_request_id']) + REQUEST_WINDOW
        with patch('rookery.hub.time.time', return_value=expires + 1):
            self.hub.maintain()
        with self.assertRaisesRegex(Error, 'expired'):
            self.hub.call('server', 'disable', data)
        self.hub.close()
        self.hub = Hub(self.path)
        with self.assertRaisesRegex(Error, 'expired'):
            self.hub.call('server', 'disable', data)
        self.assertTrue(next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)['enrolled'])

    def test_request_count_reserve_accepts_ack_after_admission_quota(self):
        message = self.send()
        with patch('rookery.hub.MAX_REQUESTS_PER_NODE', 5):
            for _ in range(4):
                self.hub.call('laptop', 'enroll', {'target': self.bk, '_request_id': new_request_id()})
            with self.assertRaises(Overloaded):
                self.hub.call('laptop', 'enroll', {'target': self.bk, '_request_id': new_request_id()})
            self.assertEqual(self.hub.call('laptop', 'ack', {'actor': self.bk, 'message_id': message['id'], '_request_id': new_request_id()})['status'], 'acknowledged')

    def test_session_quota_and_pruning_preserve_enrollments_and_aliases(self):
        unowned = {**self.a, 'id': uid()}
        with patch('rookery.hub.MAX_SESSIONS_PER_NODE', 2):
            self.hub.call('server', 'publish', {'sessions': [self.a, unowned]})
            self.assertEqual(self.hub.call('server', 'publish', {'sessions': [self.a, {**self.a, 'id': uid()}]}), {'published': 1, 'refused': 1})
            self.assertTrue(next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)['online'])
        self.hub.call('laptop', 'disable', {'target': self.bk})
        with patch('rookery.hub.time.time', return_value=time.time() + RETENTION + 1):
            self.hub.maintain()
        with closing(sqlite3.connect(self.path)) as db:
            rows = dict(db.execute('SELECT key,alias FROM sessions'))
        self.assertEqual(rows, {self.ak: 'reviewer', self.bk: 'worker'})

    def test_new_session_byte_refusal_keeps_existing_node_published(self):
        # Admission headroom is exhausted while completion reserve still fits
        # existing heartbeats and directory updates.
        with closing(sqlite3.connect(self.path)) as db:
            used = (db.execute('PRAGMA page_count').fetchone()[0] - db.execute('PRAGMA freelist_count').fetchone()[0]) * 4096
        self.hub.max_bytes = used + 16384
        fresh = {**self.a, 'id': uid()}
        self.assertEqual(self.hub.call('server', 'publish', {'sessions': [self.a, fresh]}), {'published': 1, 'refused': 1})
        self.assertTrue(next(s for s in self.hub.call('server', 'list')['sessions'] if s['key'] == self.ak)['online'])

    def test_metadata_writes_reach_byte_quota_and_rollback_atomically(self):
        self.hub.max_bytes = 262144
        for count in range(1000):
            try:
                self.hub.call('server', 'disable', {'target': self.ak, '_request_id': new_request_id()})
            except Overloaded:
                break
        else:
            self.fail('metadata writes did not reach the byte admission quota')
        with closing(sqlite3.connect(self.path)) as db:
            before = tuple(db.execute('SELECT COUNT(*) FROM ' + table).fetchone()[0] for table in ('requests', 'events'))
        self.assertGreater(count, 0)
        self.assertLess(count, 1000)
        with self.assertRaises(Overloaded):
            self.hub.call('server', 'disable', {'target': self.ak, '_request_id': new_request_id()})
        with closing(sqlite3.connect(self.path)) as db:
            after = tuple(db.execute('SELECT COUNT(*) FROM ' + table).fetchone()[0] for table in ('requests', 'events'))
        self.assertEqual(before, after)

    def test_old_request_table_migrates_without_forgetting_legacy_identity(self):
        data = {'target': self.ak, '_request_id': uid()}
        expected = self.hub.call('server', 'disable', data)
        self.hub.close()
        with closing(sqlite3.connect(self.path)) as db:
            db.executescript('DROP TABLE request_expiry;')
        self.hub = Hub(self.path)
        self.assertEqual(self.hub.call('server', 'disable', data), expected)
        with closing(sqlite3.connect(self.path)) as db:
            deadline = float(db.execute("SELECT value FROM metadata WHERE key='legacy_request_deadline'").fetchone()[0])
            self.assertEqual(db.execute('SELECT expires FROM request_expiry WHERE id=?', (data['_request_id'],)).fetchone()[0], deadline)
        self.hub.close()
        with patch('rookery.hub.time.time', return_value=time.time() + 86400):
            self.hub = Hub(self.path)
        with closing(sqlite3.connect(self.path)) as db:
            self.assertEqual(float(db.execute("SELECT value FROM metadata WHERE key='legacy_request_deadline'").fetchone()[0]), deadline)

    def test_failed_snapshot_never_exposes_partial_final_file(self):
        self.send()
        destination = self.path.parent / 'snapshot.sqlite3'
        with patch('rookery.hub.os.replace', side_effect=OSError('rename failed')):
            with self.assertRaises(OSError):
                self.hub.backup(destination)
        self.assertFalse(destination.exists())
        self.assertEqual(list(self.path.parent.glob('.tmp-hub-*')), [])
        self.assertEqual(self.hub.snapshot_last_success, 0)

    def test_only_one_process_owner_and_online_backup_is_consistent(self):
        message = self.send()
        with self.assertRaisesRegex(Error, 'another hub'):
            Hub(self.path)
        snapshot = self.path.parent / 'snapshot.sqlite3'
        self.hub.backup(snapshot)
        self.assertEqual(snapshot.stat().st_mode & 0o777, 0o600)
        with closing(sqlite3.connect(snapshot)) as db:
            self.assertEqual(db.execute('PRAGMA integrity_check').fetchone()[0], 'ok')
            self.assertEqual(json.loads(db.execute('SELECT data FROM messages').fetchone()[0])['id'], message['id'])
        self.assertGreater(self.hub.snapshot_last_success, 0)
