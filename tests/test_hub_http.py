import asyncio
import base64
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid

from rookery.conversations import create_conversations_app
from rookery.core import Error
from rookery.hub import Hub, MAX_REQUEST, MESSAGE_BODY_RETENTION, authenticate_hash_file, create_app
from rookery.hub_http import listener, serve, snapshot_once


class HttpTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.hub = Hub(self.root / 'hub.sqlite3')
        self.addCleanup(self.hub.close)
        self.token = 'private-authorization-material-1234567890'
        self.tokens = self.root / 'tokens.json'
        self.tokens.write_text(json.dumps({'server': hashlib.sha256(self.token.encode()).hexdigest()}))
        self.tokens.chmod(0o600)
        self.app = create_app(self.hub, self.tokens)
        self.internal = create_app(self.hub, self.tokens, internal=True)

    async def request(self, value=None, *, raw=None, token=None, auth=True, duplicate=False, path='/v2/rpc', method='POST', app=None, query=b'', extra_headers=()):
        headers = [(b'authorization', ('Bearer ' + (token or self.token)).encode())] if auth else []
        if duplicate:
            headers += headers
        scope = dict(type='http', method=method, path=path, headers=headers + list(extra_headers), query_string=query)
        body = raw if raw is not None else json.dumps(value or {}).encode()
        received, output = False, []
        async def receive():
            nonlocal received
            if received:
                return {'type': 'http.disconnect'}
            received = True
            return {'type': 'http.request', 'body': body}
        async def send(event):
            output.append(event)
        await (app or self.app)(scope, receive, send)
        self.response_headers = dict(output[0]['headers'])
        return output[0]['status'], output[1]['body']

    async def test_authentication_and_errors_do_not_expose_credentials_or_content(self):
        for options in ({'auth': False}, {'token': 'other-secret-credential-1234567890123456'}, {'duplicate': True}):
            status, body = await self.request({'op': 'list'}, **options)
            self.assertEqual(status, 401)
            self.assertNotIn(self.token.encode(), body)
        status, body = await self.request({'op': 'send', 'data': {'body': 'sensitive prompt'}})
        self.assertEqual(status, 400)
        self.assertNotIn(b'sensitive prompt', body)

    async def test_rookery_refusal_text_is_returned(self):
        status, body = await self.request({'op': 'sync', 'data': {'wait': 99}})
        self.assertEqual(status, 400)
        self.assertEqual(json.loads(body)['error'], 'invalid sync wait')

    async def test_malformed_and_oversized_json_is_rejected(self):
        for raw in (b'[]', b'{malformed', b'{"op":"list","data":NaN}', b'{"op":"sync","data":[]}'):
            status, _ = await self.request(raw=raw)
            self.assertEqual(status, 400)
        self.assertEqual((await self.request(raw=b'x' * (MAX_REQUEST + 1)))[0], 413)

    async def test_internal_observability_does_not_expose_public_rpc(self):
        for path in ('/healthz', '/readyz', '/metrics', '/graph'):
            self.assertEqual((await self.request(path=path, method='GET'))[0], 404)
            status, body = await self.request(path=path, method='GET', app=self.internal, auth=False)
            self.assertEqual(status, 200)
            if path == '/metrics':
                for family in (b'rookery_hub_messages{state=', b'rookery_hub_oldest_dispatching_age_seconds',
                               b'rookery_hub_db_bytes', b'rookery_hub_snapshot_last_success_timestamp_seconds',
                               b'rookery_hub_requests_total{method=', b'rookery_hub_request_duration_seconds_bucket'):
                    self.assertIn(family, body)
        self.assertEqual((await self.request({'op': 'list'}, app=self.internal))[0], 404)

    async def test_conversations_are_private_read_only_and_keep_reply_context(self):
        password = 'separate-viewer-password-material-1234567890'
        hashes = self.root / 'viewer.json'
        hashes.write_text(json.dumps({'viewer': hashlib.sha256(password.encode()).hexdigest()}))
        hashes.chmod(0o600)
        app = create_conversations_app(self.hub, hashes)
        host = (b'host', b'127.0.0.1:8789')
        credential = (b'authorization', b'Basic ' + base64.b64encode(f'viewer:{password}'.encode()))
        path = '/api/conversations'
        for existing in (self.app, self.internal):
            self.assertEqual((await self.request(app=existing, path=path, method='GET'))[0], 404)
        for visitor in ((), (credential, credential)):
            self.assertEqual((await self.request(app=app, auth=False, path=path, method='GET',
                                                extra_headers=(host, *visitor)))[0], 401)
        self.assertEqual((await self.request(app=app, path=path, method='GET', extra_headers=(host,)))[0], 401)

        async def viewer(path, *, method='GET', query=b''):
            status, body = await self.request(app=app, auth=False, path=path, method=method,
                                              extra_headers=(host, credential), query=query)
            return status, body

        self.assertEqual((await self.request(app=app, auth=False, path=path, method='GET',
                                            extra_headers=((b'host', b'external.example'), credential)))[0], 400)
        self.assertEqual((await viewer(path, method='POST'))[0], 405)
        self.assertEqual((await viewer('/'))[0], 200)
        self.assertEqual((await viewer(path, query=b'status=bogus'))[0], 400)
        first = str(uuid.uuid4())
        second = str(uuid.uuid4())
        for node, provider, sid, alias in [('server', 'codex', first, 'reviewer'), ('laptop', 'claude', second, 'worker')]:
            self.hub.call(node, 'publish', {'sessions': [{'id': sid, 'provider': provider, 'status': 'idle'}]})
            self.hub.call(node, 'enroll', {'target': f'{node}/{provider}:{sid}', 'alias': alias})
        sender, recipient = f'server/codex:{first}', f'laptop/claude:{second}'
        ask_id, reply_id = str(uuid.uuid4()), str(uuid.uuid4())
        self.hub.call('server', 'send', {'actor': sender, 'to': recipient, 'body': 'Private ask', 'id': ask_id})
        self.hub.call('laptop', 'reply', {'actor': recipient, 'message_id': ask_id, 'body': 'Private reply', 'id': reply_id})
        with patch('rookery.conversations.PAGE_SIZE', 1):
            self.hub.call('server', 'send', {'actor': sender, 'to': recipient, 'body': 'Separate ask', 'id': str(uuid.uuid4())})
            first_page = json.loads((await viewer(path))[1])
            second_page = json.loads((await viewer(path, query=f"before={first_page['next_before']}".encode()))[1])
            self.assertEqual([thread['id'] for thread in second_page['threads']], [ask_id])
            self.assertIsNone(second_page['next_before'])
        status, body = await viewer(path, query=b'host=lap&agent=Reviewer&status=pending')
        self.assertEqual(status, 200)
        self.assertEqual([thread['count'] for thread in json.loads(body)['threads']], [1, 2])
        self.assertNotIn(b'Private', body)
        self.assertEqual(json.loads((await viewer(path, query=b'agent=unknown'))[1])['threads'], [])

        detail = f'{path}/{ask_id}'
        messages = json.loads((await viewer(detail))[1])['messages']
        self.assertEqual([message['id'] for message in messages], [ask_id, reply_id])
        self.assertEqual(messages[1]['in_reply_to'], ask_id)
        self.assertEqual(messages[0]['sender']['name'], 'reviewer')
        self.assertEqual(messages[0]['body'], 'Private ask')
        with patch('rookery.conversations.time.time', return_value=messages[1]['created'] + MESSAGE_BODY_RETENTION + 1):
            self.assertIsNone(json.loads((await viewer(detail))[1])['messages'][0]['body'])
        self.assertEqual((await viewer(f'{path}/{uuid.uuid4()}'))[0], 404)

    async def test_graph_validation_and_api_counter_exclude_diagnostics(self):
        for path in ('/graph', '/metrics', '/healthz'):
            self.assertEqual((await self.request(path=path, method='GET', app=self.internal, auth=False))[0], 200)
        self.assertEqual(self.hub.api_request_counts, {})
        self.assertEqual((await self.request(path='/graph', method='GET', app=self.internal, query=b'interval=0'))[0], 400)
        self.assertEqual((await self.request(path='/graph', method='POST', app=self.internal))[0], 404)
        self.assertEqual((await self.request({'op': 'list'}))[0], 200)
        self.assertEqual(self.hub.api_request_counts, {('POST', 200): 1})
        self.assertIn('rookery_hub_api_requests_total{method="POST",code="200"} 1', self.hub.metrics())
        self.assertGreater(sum(self.hub.request_counts.values()), 1)

    async def test_sessions_requires_viewer_and_includes_unenrolled_remote_and_offline_rows(self):
        hashes = self.root / 'viewer.json'
        hashes.write_text(json.dumps({'viewer': hashlib.sha256(self.token.encode()).hexdigest()}))
        hashes.chmod(0o600)
        app = create_conversations_app(self.hub, hashes)
        host = (b'host', b'127.0.0.1:8789')
        auth = (b'authorization', b'Basic ' + base64.b64encode(f'viewer:{self.token}'.encode()))
        for listener, code in ((app, 401), (self.app, 404), (self.internal, 404)):
            status, _ = await self.request(app=listener, path='/sessions', method='GET', extra_headers=(host,))
            self.assertEqual(status, code)

        claude, codex = str(uuid.uuid4()), str(uuid.uuid4())
        stamp = time.time()
        with patch('rookery.hub.time.time', return_value=stamp):
            self.hub.call('server', 'publish', {'sessions': [{
                'id': claude, 'provider': 'claude', 'status': 'waiting', 'waiting_for': 'permission',
                'tmux': 'work:2', 'cwd': '/home/km/project', 'pid': 42,
            }]})
        self.hub.call('server02', 'publish', {'sessions': [{'id': codex, 'provider': 'codex', 'status': 'busy'}]})
        offline_stamp = self.hub.call('server02', 'list')['sessions'][0]['status_since']
        self.hub.call('server02', 'publish', {'sessions': []})
        self.assertEqual([row['id'] for row in self.hub.call('server02', 'list')['sessions']], [codex])
        for site in (b'cross-site', b'same-site', b'unexpected'):
            status, _ = await self.request(app=app, path='/sessions', method='GET', auth=False,
                                           extra_headers=(host, auth, (b'sec-fetch-site', site)))
            self.assertEqual(status, 403)
        for site in (b'none', b'same-origin'):
            status, body = await self.request(app=app, path='/sessions', method='GET', auth=False,
                                              extra_headers=(host, auth, (b'sec-fetch-site', site)))
            self.assertEqual(status, 200)
            self.assertEqual(self.response_headers[b'cross-origin-resource-policy'], b'same-origin')
            self.assertEqual(self.response_headers[b'cross-origin-opener-policy'], b'same-origin')
        self.assertEqual(json.loads(body), {'sessions': [
            {'node': 'server', 'id': claude, 'provider': 'claude', 'name': claude, 'status': 'waiting', 'online': True,
             'waiting_for': 'permission', 'tmux': 'work:2', 'cwd': '/home/km/project', 'status_since': stamp},
            {'node': 'server02', 'id': codex, 'provider': 'codex', 'name': codex, 'status': 'unknown', 'online': False,
             'status_since': offline_stamp},
        ]})

    async def test_publish_with_skewed_helper_timestamp_keeps_presence_and_delivery(self):
        stamp = time.time()
        sender, target = (str(uuid.uuid4()) for _ in range(2))
        rows = [{'id': sender, 'provider': 'codex', 'status': 'idle'},
                {'id': target, 'provider': 'claude', 'status': 'idle', 'status_since': stamp + 600}]
        with patch('rookery.hub.time.time', return_value=stamp):
            status, data = await self.request({'op': 'publish', 'data': {'sessions': rows}})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(data)['result'], {'published': 2, 'invalid_metadata': 1})
            for row in rows:
                key = f"server/{row['provider']}:{row['id']}"
                self.hub.call('server', 'enroll', {'target': key})
            outgoing = {'actor': f'server/codex:{sender}', 'to': f'server/claude:{target}',
                        'id': str(uuid.uuid4()), 'body': 'delivery remains eligible'}
            self.assertEqual((await self.request({'op': 'send', 'data': outgoing}))[0], 200)

        with patch('rookery.hub.time.time', return_value=stamp + 61):
            rows[1]['status_since'] += 61
            self.assertEqual((await self.request({'op': 'publish', 'data': {'sessions': rows}}))[0], 200)
            status, data = await self.request({'op': 'list'})
            self.assertEqual(status, 200)
            sessions = {row['id']: row for row in json.loads(data)['result']['sessions']}
            self.assertTrue(sessions[target]['online'])
            self.assertEqual(sessions[target]['status_since'], stamp)
            status, data = await self.request({'op': 'claim'})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(data)['result']['delivery']['message']['id'], outgoing['id'])

    async def test_startup_rejects_unusable_viewer_hash_file_before_opening_db(self):
        digest = hashlib.sha256(self.token.encode()).hexdigest()
        hashes = self.root / 'viewer.json'
        db = self.root / 'new.sqlite3'
        args = SimpleNamespace(listen='127.0.0.1:8787', metrics_listen='127.0.0.1:8788',
                               conversations_listen='127.0.0.1:8789', conversations_password_hash_file=hashes,
                               db=db, tokens=self.tokens)
        for value, mode, error in [(None, 0o600, 'is missing'), ('{bad', 0o600, 'not valid JSON'),
                                   (json.dumps({'server': digest}), 0o600, 'no viewer entry'),
                                   (json.dumps({'viewer': digest, 'server': digest}), 0o600, 'duplicate viewer hash'),
                                   (json.dumps({'viewer': 'malformed'}), 0o600, 'must map valid names to SHA-256 hashes'),
                                   (json.dumps({'viewer': digest}), 0o666, 'safe mode and owner')]:
            with self.subTest(value=value, mode=mode):
                if value is not None:
                    hashes.write_text(value)
                    hashes.chmod(mode)
                with self.assertRaisesRegex(Error, f'conversations password hash file .*{error}'):
                    await serve(args)
                self.assertFalse(db.exists())

    async def test_longpoll_yields_to_publication_and_returns_incremental_directory_event(self):
        cursor = self.hub.call('server', 'sync')['cursor']
        waiting = asyncio.create_task(self.request({'op': 'sync', 'data': {'cursor': cursor, 'wait': 1}}))
        await asyncio.sleep(.01)
        sid = str(uuid.uuid4())
        status, _ = await self.request({'op': 'publish', 'data': {'sessions': [{'provider': 'codex', 'id': sid, 'status': 'idle'}]}})
        self.assertEqual(status, 200)
        status, body = await waiting
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)['result']['events'][0]['type'], 'directory')

    async def test_credential_revocation_also_stops_pending_longpoll(self):
        cursor = self.hub.call('server', 'sync')['cursor']
        waiting = asyncio.create_task(self.request({'op': 'sync', 'data': {'cursor': cursor, 'wait': .15}}))
        await asyncio.sleep(.01)
        self.tokens.write_text('{}')
        self.assertEqual((await waiting)[0], 401)

    async def test_root_owned_projected_hash_file_and_private_user_file(self):
        projected = self.root / 'projected-token-hashes'
        projected.symlink_to(self.tokens)
        self.assertEqual(authenticate_hash_file(projected, 'Bearer ' + self.token), 'server')
        fake = SimpleNamespace(st_mode=stat.S_IFREG | 0o440, st_uid=0, st_size=self.tokens.stat().st_size)
        with patch.object(Path, 'stat', return_value=fake):
            self.assertEqual(authenticate_hash_file(projected, 'Bearer ' + self.token), 'server')
        for mode, owner in ((0o460, 0), (0o606, os.getuid()), (0o600, 999999)):
            fake = SimpleNamespace(st_mode=stat.S_IFREG | mode, st_uid=owner, st_size=100)
            with patch.object(Path, 'stat', return_value=fake):
                self.assertIsNone(authenticate_hash_file(projected, 'Bearer ' + self.token))

    async def test_admission_quota_is_retryable_429_not_false_acceptance(self):
        self.hub.pending_limit = 1
        local = []
        for provider in ('codex', 'claude'):
            session = {'provider': provider, 'id': str(uuid.uuid4()), 'status': 'idle'}
            local.append(session)
        self.hub.call('server', 'publish', {'sessions': local})
        for session in local:
            self.hub.call('server', 'enroll', {'target': f'server/{session["provider"]}:{session["id"]}'})
        data = {'actor': f'server/codex:{local[0]["id"]}', 'to': f'server/claude:{local[1]["id"]}', 'body': 'Hello', 'id': str(uuid.uuid4())}
        self.assertEqual((await self.request({'op': 'send', 'data': data}))[0], 200)
        status, body = await self.request({'op': 'send', 'data': {**data, 'id': str(uuid.uuid4())}})
        self.assertEqual(status, 429)
        self.assertTrue(json.loads(body)['retryable'])
        self.assertEqual(len(self.hub.call('server', 'status')['messages']), 1)


class ListenerTests(unittest.TestCase):
    def test_bounded_explicit_listener_address(self):
        self.assertEqual(listener('127.0.0.1:8787'), ('127.0.0.1', 8787))
        self.assertEqual(listener('[::1]:8788'), ('::1', 8788))
        for value in ('localhost', 'user:secret@example.org:8787', '127.0.0.1:8787/path'):
            with self.subTest(value=value), self.assertRaises(Exception):
                listener(value)


class SnapshotRotationTests(unittest.TestCase):
    def test_rotation_counts_inflight_copy_and_space_refusal_preserves_completed_snapshots(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root)
            for stamp in (1, 2, 3):
                (directory / f'hub-{stamp}.sqlite3').write_text('completed')
            class FakeHub:
                def stats(self):
                    return {'database_bytes': 100}
                def backup(self, destination):
                    assert len(list(directory.glob('hub-*.sqlite3'))) == 2
                    destination.write_text('new completed')
                    return {'snapshot': str(destination)}
            with patch('rookery.hub_http.shutil.disk_usage', return_value=SimpleNamespace(free=100_000_000)):
                snapshot_once(FakeHub(), directory)
            self.assertEqual(len(list(directory.glob('hub-*.sqlite3'))), 3)
            with patch('rookery.hub_http.shutil.disk_usage', return_value=SimpleNamespace(free=1)):
                with self.assertRaises(Exception):
                    snapshot_once(FakeHub(), directory)
            self.assertEqual(len(list(directory.glob('hub-*.sqlite3'))), 2)
