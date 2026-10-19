"""Provider registry: built-ins, plugin boundaries and readiness rules."""
import contextlib
import io
import unittest
from unittest.mock import patch

from rookery import providers


class EchoProvider(providers.Provider):
    name = 'echo'

    def discover(self, state=None):
        return [{'provider': 'echo', 'id': 'one', 'status': 'idle'},
                {'provider': 'claude', 'id': 'other'}]

    def submit(self, session, text):
        return {'status': 'submitted', 'detail': 'echo'}


class EntryPoint:
    def __init__(self, name, value):
        self.name = name
        self.value = value

    def load(self):
        if isinstance(self.value, Exception):
            raise self.value
        return self.value


class RegistryTests(unittest.TestCase):
    def setUp(self):
        saved = dict(providers._REGISTRY)

        def restore():
            providers._REGISTRY.clear()
            providers._REGISTRY.update(saved)
            providers._LOADED = False
        self.addCleanup(restore)
        providers._LOADED = False

    def load(self, points):
        stderr = io.StringIO()
        with patch('importlib.metadata.entry_points', return_value=points), contextlib.redirect_stderr(stderr):
            names = providers.names()
        return names, stderr.getvalue()

    def test_builtins_are_registered(self):
        names, _ = self.load([])
        self.assertTrue({'claude', 'codex', 'agy'} <= names)
        self.assertEqual(providers.get('agy').capabilities.topology, 'attached')
        self.assertFalse(providers.get('claude').capabilities.accepts_busy)

    def test_plugin_adds_provider_but_cannot_replace_builtin(self):
        class Impostor(EchoProvider):
            name = 'claude'
        names, errors = self.load([EntryPoint('echo', EchoProvider), EntryPoint('claude', Impostor),
                                   EntryPoint('broken', ImportError('missing'))])
        self.assertIn('echo', names)
        self.assertIsNot(type(providers.get('claude')), Impostor)
        self.assertIn('broken', errors)
        self.assertIn('claude', errors)

    def test_discovery_keeps_only_own_provider_rows_and_isolates_failures(self):
        self.load([EntryPoint('echo', EchoProvider)])
        with patch.object(providers.get('claude'), 'discover', side_effect=OSError('down')), \
             patch.object(providers.get('codex'), 'discover', return_value=[]):
            sessions, errors = providers.discover_all()
        self.assertEqual([s['id'] for s in sessions if s['provider'] == 'echo'], ['one'])
        self.assertNotIn('other', [s['id'] for s in sessions])
        self.assertEqual(errors['claude'], 'down')

    def test_readiness_rules(self):
        self.load([])
        self.assertTrue(providers.ready({'provider': 'codex', 'status': 'idle'}))
        self.assertFalse(providers.ready({'provider': 'codex', 'status': 'busy'}))
        self.assertFalse(providers.ready({'provider': 'claude', 'status': 'idle', 'delivery': 'polling'}))
        self.assertFalse(providers.ready({'provider': 'agy', 'status': 'idle', 'delivery': 'polling'}))
        self.assertTrue(providers.ready({'provider': 'agy', 'status': 'unknown', 'delivery': 'attached', 'socket': '/x'}))
        self.assertFalse(providers.ready({'provider': 'nobody', 'status': 'idle'}))

    def test_admission_is_the_single_policy_behind_readiness(self):
        self.load([])
        busy_codex = {'provider': 'codex', 'status': 'busy'}
        self.assertEqual(providers.admission(busy_codex), 'idle')
        self.assertEqual(providers.admission({'provider': 'claude', 'delivery': 'polling'}), 'none')
        self.assertEqual(providers.admission({'provider': 'agy', 'delivery': 'attached', 'socket': '/x'}), 'busy')
        self.assertEqual(providers.admission({'provider': 'agy', 'delivery': 'polling'}), 'none')
        with patch.object(type(providers.get('codex')), 'capabilities',
                          providers.Capabilities(topology='host', accepts_busy=True)):
            self.assertEqual(providers.admission(busy_codex), 'busy')
            self.assertTrue(providers.ready(busy_codex))

    def test_plugin_admission_errors_fail_closed_locally(self):
        class Odd(EchoProvider):
            def admission(self, session):
                return 'whenever'

        class Broken(EchoProvider):
            name = 'broken-adm'

            def admission(self, session):
                raise RuntimeError('bug')
        self.load([EntryPoint('echo', Odd), EntryPoint('broken-adm', Broken)])
        self.assertEqual(providers.admission({'provider': 'echo'}), 'none')
        self.assertEqual(providers.admission({'provider': 'broken-adm'}), 'none')
        for value in ([], {}, None, 1, 1.5, b'busy', ['busy']):
            with self.subTest(value=value), patch.object(providers.get('agy'), 'admission', return_value=value):
                self.assertEqual(providers.admission({'provider': 'agy'}), 'none')

    def test_claude_busy_delivery_is_default_on_and_never_into_a_dialog(self):
        self.load([])
        self.addCleanup(providers.configure, {})
        busy, waiting = {'provider': 'claude', 'status': 'busy'}, {'provider': 'claude', 'status': 'waiting'}
        # Never configured: Claude receives while busy by default.
        self.assertEqual(providers.configure({}), ['claude'])
        self.assertEqual(providers.admission(busy), 'busy')
        # An explicit empty list (busy-delivery claude off) is respected.
        self.assertEqual(providers.configure({'busy_delivery': []}), [])
        self.assertEqual(providers.admission(busy), 'idle')
        self.assertEqual(providers.configure({'busy_delivery': ['claude', 'codex', 'nobody', 7]}), ['claude'])
        self.assertEqual(providers.admission(busy), 'busy')
        self.assertTrue(providers.ready(busy))
        self.assertEqual(providers.admission(waiting), 'idle')
        self.assertFalse(providers.ready(waiting))
        for status in ('unknown', None):
            self.assertEqual(providers.admission({'provider': 'claude', 'status': status}), 'idle')
            self.assertFalse(providers.ready({'provider': 'claude', 'status': status}))
        self.assertEqual(providers.admission({'provider': 'codex', 'status': 'busy'}), 'idle')
        with patch('rookery.claude_adapter.submit', return_value={'status': 'submitted'}) as submit:
            providers.submit(busy, 'x')
        self.assertTrue(submit.call_args.kwargs['allow_busy'])
        providers.configure({'busy_delivery': 'claude'})
        self.assertEqual(providers.admission(busy), 'idle')

    def test_submit_normalizes_unknown_and_invalid_results(self):
        self.load([])
        self.assertEqual(providers.submit({'provider': 'nobody'}, 'x')['status'], 'held')
        with patch.object(providers.get('codex'), 'submit', return_value={'status': 'delivered'}):
            self.assertEqual(providers.submit({'provider': 'codex'}, 'x')['status'], 'uncertain')
        held = providers.submit({'provider': 'agy', 'delivery': 'polling'}, 'x')
        self.assertEqual(held['status'], 'held')


class BusyDeliveryConfigTests(unittest.TestCase):
    def test_switch_persists_survives_reconfigure_and_refuses_unsupported(self):
        import json
        import tempfile
        from pathlib import Path
        from rookery.client import configure, read_config, set_busy_delivery
        from rookery.core import Error
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            configure(path, 'test', 'http://127.0.0.1:9999', 'x' * 32, allow_http=True)
            self.assertEqual(set_busy_delivery(path, 'claude', True)['busy_delivery'], ['claude'])
            configure(path, 'test', 'http://127.0.0.1:9999', 'y' * 32, allow_http=True)
            self.assertEqual(read_config(path)['busy_delivery'], ['claude'])
            self.assertEqual((path / 'config.json').stat().st_mode & 0o077, 0)
            for provider in ('codex', 'agy', 'nobody'):
                with self.subTest(provider=provider), self.assertRaises(Error):
                    set_busy_delivery(path, provider, True)
            self.assertEqual(set_busy_delivery(path, 'claude', False)['busy_delivery'], [])
            # Turned off explicitly, it stays off and does not fall back to the default.
            from rookery import providers
            self.assertEqual(providers.busy_choice(read_config(path)), [])
            self.assertEqual(providers.busy_choice({}), ['claude'])

    def test_doctor_separates_configured_from_effective_policy(self):
        import json
        import tempfile
        import time
        from argparse import Namespace
        from pathlib import Path
        from rookery.cli import unified_run
        from rookery.client import configure, set_busy_delivery
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            configure(path, 'test', 'http://127.0.0.1:9999', 'x' * 32, allow_http=True)
            (path / 'health.json').write_text(json.dumps({'updated': time.time(), 'running': True,
                                                          'connected': True, 'busy_delivery': []}))
            set_busy_delivery(path, 'claude', True)
            report = unified_run(Namespace(command='doctor', actor='operator'), path)['busy_delivery']
            self.assertEqual(report, {'configured': ['claude'], 'effective': [], 'pending_restart': True})


if __name__ == '__main__':
    unittest.main()
