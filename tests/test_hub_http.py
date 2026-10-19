import asyncio
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import uuid

from rookery.hub import Hub, MAX_REQUEST, authenticate_hash_file, create_app
from rookery.hub_http import listener, snapshot_once


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

    async def request(self, value=None, *, raw=None, token=None, auth=True, duplicate=False, path='/v2/rpc', method='POST', app=None, query=b''):
        headers = [(b'authorization', ('Bearer ' + (token or self.token)).encode())] if auth else []
        if duplicate:
            headers += headers
        scope = dict(type='http', method=method, path=path, headers=headers, query_string=query)
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
