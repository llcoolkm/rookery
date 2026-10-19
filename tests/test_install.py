"""Installer: plans by default, backs up on apply, never touches permissions."""
import io
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
import uuid
from unittest.mock import patch

from rookery import install
from rookery.core import Error


class AgyPluginTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / 'gemini-config'
        self.root.mkdir()
        self.state = Path(self.tmp.name) / 'state'
        env = patch.dict(os.environ, {'ROOKERY_AGY_CONFIG': str(self.root)})
        env.start()
        self.addCleanup(env.stop)
        exe = patch.object(install, 'rookery_command', return_value='/opt/rookery/bin/rookery')
        exe.start()
        self.addCleanup(exe.stop)
        self.cli = {'provider': 'agy', 'configured': True, 'actions': [], 'notes': []}
        cli = patch.object(install, 'plan_cli', side_effect=lambda provider, state: self.cli)
        cli.start()
        self.addCleanup(cli.stop)

    def plugin(self, name):
        return self.root / 'plugins/rookery' / name

    def test_plan_writes_nothing(self):
        result = install.install('agy', self.state)
        self.assertFalse(result['applied'])
        self.assertFalse((self.root / 'plugins').exists())
        self.assertEqual({Path(a['write']).name for a in result['actions']},
                         {'plugin.json', 'hooks.json', 'AGENTS.md'})

    def test_mcp_entry_comes_from_agy_cli_not_the_plugin(self):
        self.cli = {'provider': 'agy', 'configured': False, 'notes': [],
                    'actions': [{'run': ['agy', 'mcp', 'add', 'rookery', '--', '/opt/rookery/bin/rookery']}]}
        with patch.object(install, 'apply_cli', return_value=[{'ran': 'agy mcp add'}]) as ran:
            result = install.install('agy', self.state, apply=True)
        ran.assert_called_once()
        self.assertFalse(self.plugin('mcp_config.json').exists())
        self.assertIn({'ran': 'agy mcp add'}, result['done'])

    def test_apply_is_idempotent_and_backs_up_replaced_files(self):
        install.install('agy', self.state, apply=True)
        hooks = json.loads(self.plugin('hooks.json').read_text())['rookery']
        self.assertIn('session-start', hooks['SessionStart'][0]['command'])
        self.assertIn('pre-invocation', hooks['PreInvocation'][0]['command'])
        self.assertEqual(install.install('agy', self.state)['actions'], [])
        self.plugin('plugin.json').write_text('{"name": "edited"}')
        result = install.install('agy', self.state, apply=True)
        backups = [d['backup'] for d in result['done'] if 'backup' in d]
        self.assertEqual(len(backups), 1)
        self.assertEqual(Path(backups[0]).read_text(), '{"name": "edited"}')

    def test_plan_carries_read_only_grant_migration_note(self):
        home = Path(self.tmp.name) / 'home'
        (home / '.gemini/antigravity-cli').mkdir(parents=True)
        settings = home / '.gemini/antigravity-cli/settings.json'
        settings.write_text(json.dumps({'permissions': {'allow': ['mcp(rookery/rookery_reply)', 'read_url(*)']}}))
        before = settings.read_text()
        with patch.dict(os.environ, {'HOME': str(home)}):
            notes = install.install('agy', self.state, apply=True)['notes']
        self.assertTrue(any('mcp(rookery/rookery_send) -> mcp(rookery/send)' in n for n in notes))
        found = [n for n in notes if n.startswith('Old-name grants found')]
        self.assertEqual(len(found), 1)
        self.assertIn('mcp(rookery/rookery_reply)', found[0])
        self.assertNotIn('read_url', found[0])
        self.assertEqual(settings.read_text(), before)

    def test_no_permission_grants_or_overrides_are_generated(self):
        install.install('agy', self.state, apply=True)
        text = ''.join(p.read_text() for p in (self.root / 'plugins/rookery').rglob('*') if p.is_file())
        for forbidden in ('permissionOverrides', 'PreToolUse', '"allow"', 'decision'):
            self.assertNotIn(forbidden, text)

    def test_stale_plugin_mcp_config_is_reported_not_deleted(self):
        self.plugin('').mkdir(parents=True)
        self.plugin('mcp_config.json').write_text('{}')
        result = install.install('agy', self.state, apply=True)
        self.assertTrue(self.plugin('mcp_config.json').exists())
        self.assertTrue(any('duplicates the MCP server' in note for note in result['notes']))

    def test_symlinked_target_is_refused(self):
        self.plugin('').mkdir(parents=True)
        os.symlink('/etc/hostname', self.plugin('plugin.json'))
        with self.assertRaises(Error):
            install.install('agy', self.state, apply=True)


class AgyHookTests(unittest.TestCase):
    def setUp(self):
        exe = patch.object(install, 'rookery_command', return_value='/opt/rookery/bin/rookery')
        exe.start()
        self.addCleanup(exe.stop)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = Path(self.tmp.name) / 'state'
        self.conversation = str(uuid.uuid4())
        self.record = (None, None)
        st = patch.object(install, '_endpoint_state', side_effect=lambda state, conversation: self.record)
        st.start()
        self.addCleanup(st.stop)
        self.home = Path(self.tmp.name) / 'home'
        home = patch.dict(os.environ, {'HOME': str(self.home)})
        home.start()
        self.addCleanup(home.stop)
        from rookery import agy_endpoint
        (self.home / agy_endpoint.BRAIN / self.conversation).mkdir(parents=True)  # a top-level conversation

    def hook(self, event, invocation=1):
        payload = {'conversationId': self.conversation, 'invocationNum': invocation}
        return install.agy_hook(event, self.state, io.StringIO(json.dumps(payload)))

    def test_asks_once_per_conversation_so_a_declined_prompt_is_respected(self):
        first = self.hook('session-start')['injectSteps'][0]['systemMessage']['systemMessage']
        self.assertIn('attach agy', first)
        # The agent declined or the user refused the prompt: later turns stay quiet.
        for event in ('pre-invocation', 'pre-invocation', 'session-start'):
            self.assertEqual(self.hook(event), {})

    def test_session_started_before_install_is_asked_on_its_next_turn(self):
        self.assertIn('ephemeralMessage', self.hook('pre-invocation')['injectSteps'][0])
        self.assertEqual(self.hook('pre-invocation'), {})

    def test_attached_conversation_gets_nothing(self):
        self.record = ('attached', 77)
        self.assertEqual(self.hook('session-start'), {})
        self.assertEqual(self.hook('pre-invocation'), {})

    def test_dead_endpoint_is_reported_once_per_lost_endpoint(self):
        self.hook('session-start')
        self.record = ('detached', 77)
        message = self.hook('pre-invocation')['injectSteps'][0]['ephemeralMessage']
        self.assertIn('has stopped', message)
        self.assertEqual(self.hook('pre-invocation'), {})
        self.record = ('stale', 78)
        self.assertIn('injectSteps', self.hook('pre-invocation'))

    def marker(self, suffix):
        from rookery import agy_endpoint
        from rookery.local_store import Store
        (agy_endpoint.directory(Store(self.state)) / f'agy-{self.conversation}.{suffix}').touch()

    def test_resumed_conversation_reattaches_in_each_new_agy_process(self):
        self.assertIn('injectSteps', self.hook('session-start'))      # first ask
        self.marker('attached')                                        # attach succeeded
        self.record = ('attached', 77)
        self.assertEqual(self.hook('pre-invocation'), {})
        self.record = (None, None)                                     # agy exited, endpoint cleaned up
        with patch.object(install, '_agy_process', return_value='900-111'):
            self.assertIn('injectSteps', self.hook('session-start'))   # resumed in a new process
            self.assertEqual(self.hook('pre-invocation'), {})          # declined there: quiet
        with patch.object(install, '_agy_process', return_value='901-222'):
            self.assertIn('injectSteps', self.hook('session-start'))   # yet another process

    def test_refusal_without_any_attachment_stays_quiet_across_processes(self):
        self.hook('session-start')
        for owner in ('900-111', '901-222'):
            with patch.object(install, '_agy_process', return_value=owner):
                self.assertEqual(self.hook('session-start'), {})

    def test_explicit_detach_stays_quiet_until_reattached(self):
        self.marker('attached')
        self.marker('detached')
        with patch.object(install, '_agy_process', return_value='900-111'):
            self.assertEqual(self.hook('session-start'), {})
            self.record = ('detached', 77)
            self.assertEqual(self.hook('pre-invocation'), {})

    def test_subagent_conversation_gets_nothing_and_displaces_nothing(self):
        from rookery import agy_endpoint
        from rookery.local_store import Store
        brain = self.home / agy_endpoint.BRAIN
        record = brain / str(uuid.uuid4()) / '.system_generated' / 'subagents'
        record.mkdir(parents=True)  # the child's own store already exists (setUp)
        (record / f'{self.conversation}.json').write_text('{}')
        with patch.object(install, '_supersede_others') as supersede:
            payload = {'conversationId': self.conversation, 'artifactDirectoryPath': str(brain / self.conversation)}
            for event in ('session-start', 'pre-invocation'):
                self.assertEqual(install.agy_hook(event, self.state, io.StringIO(json.dumps(payload))), {})
        supersede.assert_not_called()
        self.assertFalse(list(agy_endpoint.directory(Store(self.state)).glob('*.asked*')))

    def test_unconfirmed_conversation_gets_nothing_and_displaces_nothing(self):
        from rookery import agy_endpoint
        (self.home / agy_endpoint.BRAIN / self.conversation).rmdir()  # store layout unknown
        with patch.object(install, '_supersede_others') as supersede:
            self.assertEqual(self.hook('session-start'), {})
            self.assertEqual(self.hook('pre-invocation'), {})
        supersede.assert_not_called()

    def test_unavailable_reconfirmation_stops_nothing(self):
        from rookery import agy_endpoint
        with patch.object(agy_endpoint, 'find_agy', return_value=900), \
                patch.object(agy_endpoint, '_pin', return_value={'starttime': '111', 'boot_id': 'b'}), \
                patch.object(agy_endpoint, 'confirmed_top_level', return_value=False), \
                patch.object(agy_endpoint, 'inspect', return_value=[{'id': str(uuid.uuid4()), 'agy_pid': 900}]), \
                patch.object(agy_endpoint, '_read_private',
                             return_value={'starttime': '111', 'boot_id': 'b', 'endpoint_pid': 5,
                                           'endpoint_starttime': '6'}), \
                patch.object(agy_endpoint, '_terminate') as kill:
            self.assertEqual(self.hook('session-start'), {})   # disproved: no prompt either
        kill.assert_not_called()
        from rookery.local_store import Store
        self.assertFalse(list(agy_endpoint.directory(Store(self.state)).glob('*.asked*')))

    def test_top_level_hook_retires_other_conversations_and_reoffers_on_return(self):
        from rookery import agy_endpoint
        from rookery.local_store import Store
        previous = str(uuid.uuid4())
        marker = agy_endpoint.directory(Store(self.state)) / f'agy-{previous}.asked-900-111'
        marker.touch()
        with patch.object(agy_endpoint, 'find_agy', return_value=900), \
                patch.object(agy_endpoint, '_pin', return_value={'starttime': '111', 'boot_id': 'b'}), \
                patch.object(agy_endpoint, 'supersede', return_value=[previous]) as supersede:
            self.hook('session-start')
        supersede.assert_called_once()
        self.assertEqual(supersede.call_args.args[1:], (self.conversation, 900))
        self.assertFalse(marker.exists())

    def test_missing_conversation_or_bad_payload_yields_no_steps(self):
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(install.agy_hook('session-start', self.state, io.StringIO('not json')), {})
            self.assertEqual(install.agy_hook('pre-invocation', self.state,
                                              io.StringIO('{"conversationId": "not-a-uuid"}')), {})


class ManagedMcpTests(unittest.TestCase):
    def test_managed_claude_mcp_is_reported_not_attempted(self):
        with tempfile.TemporaryDirectory() as tmp:
            managed = Path(tmp) / 'managed-mcp.json'
            managed.write_text('{"mcpServers": {}}')
            with patch.object(install, 'MANAGED_MCP', (managed,)), \
                    patch.object(install.shutil, 'which', return_value='/usr/bin/claude'), \
                    patch.object(install, '_mcp_entry', return_value=None):
                plan = install.plan_cli('claude', Path(tmp))
        self.assertEqual(plan['actions'], [])
        self.assertTrue(plan['managed'])
        self.assertTrue(any('managed' in n for n in plan['notes']))

    def test_unmanaged_claude_still_plans_the_add(self):
        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(install, 'MANAGED_MCP', (Path(tmp) / 'absent.json',)), \
                    patch.object(install.shutil, 'which', return_value='/usr/bin/claude'), \
                    patch.object(install, '_mcp_entry', return_value=None):
                plan = install.plan_cli('claude', Path(tmp))
        self.assertEqual(plan['actions'][0]['run'][:3], ['/usr/bin/claude', 'mcp', 'add'])


class ClaudeInboundTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        env = patch.dict(os.environ, {'CLAUDE_CONFIG_DIR': self.tmp.name})
        env.start()
        self.addCleanup(env.stop)
        self.settings = Path(self.tmp.name) / 'settings.json'

    def test_missing_setting_is_added_keeping_every_other_key(self):
        self.settings.write_text(json.dumps({'permissions': {'allow': ['Bash(ls)']}, 'theme': 'dark'}))
        action = install.plan_claude_inbound()
        self.assertTrue(action['replaces'])
        done = install.apply_plan({'actions': [action]})
        written = json.loads(self.settings.read_text())
        self.assertEqual(written, {'permissions': {'allow': ['Bash(ls)']}, 'theme': 'dark',
                                   'crossSessionInbound': 'accept'})
        self.assertTrue(any('backup' in d for d in done))

    def test_already_accepting_needs_nothing_and_absent_file_is_created(self):
        self.settings.write_text(json.dumps({'crossSessionInbound': 'accept'}))
        self.assertIsNone(install.plan_claude_inbound())
        self.settings.unlink()
        install.apply_plan({'actions': [install.plan_claude_inbound()]})
        self.assertEqual(json.loads(self.settings.read_text()), {'crossSessionInbound': 'accept'})

    def test_private_settings_stay_private_and_new_ones_are_created_private(self):
        self.settings.write_text('{}')
        self.settings.chmod(0o600)
        install.apply_plan({'actions': [install.plan_claude_inbound()]})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)
        self.settings.unlink()
        install.apply_plan({'actions': [install.plan_claude_inbound()]})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)

    def test_explicit_non_accept_choice_is_kept_and_reported(self):
        for value in ('hold', 'refuse'):
            with self.subTest(value=value):
                self.settings.write_text(json.dumps({'crossSessionInbound': value, 'theme': 'dark'}))
                self.assertIsNone(install.plan_claude_inbound())
                with patch.object(install, 'plan_cli', return_value={'provider': 'claude', 'actions': [], 'notes': []}):
                    plan = install.install('claude', Path(self.tmp.name) / 'state')
                self.assertEqual(plan['actions'], [])
                self.assertTrue(any(repr(value) in note for note in plan['notes']))

    def run_install(self, **kwargs):
        with patch.object(install, 'plan_cli', return_value={'provider': 'claude', 'actions': [], 'notes': []}):
            return install.install('claude', Path(self.tmp.name) / 'state', **kwargs)

    def test_default_install_leaves_settings_untouched_and_explains_the_flag(self):
        original = json.dumps({'theme': 'dark'})
        self.settings.write_text(original)
        for apply in (False, True):
            with self.subTest(apply=apply):
                plan = self.run_install(apply=apply)
                self.assertEqual(plan['actions'], [])
                self.assertEqual(self.settings.read_text(), original)
                note = next(n for n in plan['notes'] if 'crossSessionInbound' in n)
                self.assertIn('ask for approval on each incoming Rookery message', note)
                self.assertIn('--accept-inbound', note)
                self.assertIn('whole account', note)
        self.settings.unlink()
        self.run_install(apply=True)
        self.assertFalse(self.settings.exists())

    def test_accept_inbound_writes_the_key_and_keeps_or_sets_a_private_mode(self):
        plan = self.run_install(accept_inbound=True)
        self.assertEqual([a['setting'] for a in plan['actions']], ['crossSessionInbound=accept'])
        self.assertFalse(self.settings.exists())
        self.run_install(apply=True, accept_inbound=True)
        self.assertEqual(json.loads(self.settings.read_text()), {'crossSessionInbound': 'accept'})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)
        self.settings.write_text(json.dumps({'theme': 'dark'}))
        self.settings.chmod(0o640)
        self.run_install(apply=True, accept_inbound=True)
        self.assertEqual(json.loads(self.settings.read_text()), {'theme': 'dark', 'crossSessionInbound': 'accept'})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o640)

    def test_accept_inbound_never_overwrites_an_explicit_value(self):
        self.settings.write_text(json.dumps({'crossSessionInbound': 'refuse'}))
        plan = self.run_install(apply=True, accept_inbound=True)
        self.assertEqual(plan['actions'], [])
        self.assertEqual(json.loads(self.settings.read_text()), {'crossSessionInbound': 'refuse'})
        self.assertTrue(any("'refuse'" in note for note in plan['notes']))
        self.settings.write_text(json.dumps({'crossSessionInbound': 'accept'}))
        plan = self.run_install(apply=True, accept_inbound=True)
        self.assertEqual(plan['actions'], [])
        self.assertFalse(any('crossSessionInbound' in note for note in plan['notes']))

    def test_accept_inbound_with_invalid_settings_is_refused(self):
        self.settings.write_text('[1]')
        with self.assertRaises(Error):
            self.run_install(apply=True, accept_inbound=True)
        self.assertEqual(self.settings.read_text(), '[1]')

    def test_accept_inbound_is_refused_for_other_providers(self):
        for provider in ('codex', 'agy'):
            with self.subTest(provider=provider):
                with patch.object(install, 'plan_cli') as plan_cli, patch.object(install, 'plan_agy') as plan_agy:
                    with self.assertRaisesRegex(Error, 'only to claude'):
                        install.install(provider, Path(self.tmp.name) / 'state', apply=True, accept_inbound=True)
                plan_cli.assert_not_called()
                plan_agy.assert_not_called()

    def test_cli_passes_the_flag_through(self):
        from rookery.cli import parser, run
        with patch('rookery.install.install', return_value={}) as fake:
            run(parser().parse_args(['--state-dir', str(Path(self.tmp.name) / 'state'),
                                     'install', 'claude', '--accept-inbound']))
        self.assertTrue(fake.call_args.kwargs['accept_inbound'])
        self.assertFalse(fake.call_args.kwargs['apply'])

    def test_invalid_settings_are_refused_not_overwritten(self):
        self.settings.write_text('{not json')
        with self.assertRaises(Error):
            install.plan_claude_inbound()
        self.assertEqual(self.settings.read_text(), '{not json')


class SafeWriteTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.target = self.dir / 'plugin.json'

    def plan(self, text):
        return {'actions': [{'write': str(self.target), 'text': text}]}

    def test_predictable_temporary_symlink_cannot_redirect_the_write(self):
        unrelated = self.dir / 'unrelated.txt'
        unrelated.write_text('KEEP')
        os.symlink(unrelated, self.dir / '.plugin.json.tmp')
        install.apply_plan(self.plan('NEW_PLUGIN'))
        self.assertEqual(unrelated.read_text(), 'KEEP')
        self.assertFalse(self.target.is_symlink())
        self.assertEqual(self.target.read_text(), 'NEW_PLUGIN')

    def test_backups_with_the_same_timestamp_never_overwrite_each_other(self):
        self.target.write_text('ORIGINAL')
        with patch('time.strftime', return_value='20261017-120000'):
            os.symlink(self.dir / 'elsewhere', self.dir / 'plugin.json.bak.20261017-120000.1')
            first = install.apply_plan(self.plan('FIRST'))
            second = install.apply_plan(self.plan('SECOND'))
        backups = [Path(d['backup']) for d in first + second if 'backup' in d]
        self.assertEqual([b.read_text() for b in backups], ['ORIGINAL', 'FIRST'])
        self.assertTrue(all(not b.is_symlink() for b in backups))
        self.assertFalse((self.dir / 'elsewhere').exists())
        self.assertEqual(self.target.read_text(), 'SECOND')


class CliProviderTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.bin = Path(self.tmp.name)
        self.log = self.bin / 'calls.log'
        exe = patch.object(install, 'rookery_command', return_value='/opt/rookery/bin/rookery')
        exe.start()
        self.addCleanup(exe.stop)

    def fake(self, name, configured):
        script = self.bin / name
        body = ('import sys\n'
                f'open({str(self.log)!r}, "a").write(" ".join(sys.argv[1:]) + "\\n")\n'
                'if sys.argv[1:3] == ["mcp", "get"]:\n'
                f'    print({"/opt/rookery/bin/rookery --state-dir /s mcp --provider " + name!r}) if {configured} else sys.exit(1)\n')
        script.write_text('#!' + sys.executable + '\n' + body)
        script.chmod(0o700)

    def run_install(self, name, apply):
        with patch.dict(os.environ, {'PATH': str(self.bin)}):
            return install.install(name, '/s', apply=apply)

    def test_absent_entry_is_added_through_the_provider_cli_only_on_apply(self):
        for name in ('claude', 'codex'):
            with self.subTest(provider=name):
                self.fake(name, False)
                plan = self.run_install(name, False)
                self.assertFalse(plan['configured'])
                self.assertNotIn('mcp add', self.log.read_text())
                self.run_install(name, True)
                self.assertIn('mcp add', self.log.read_text())
                self.log.unlink()

    def test_agy_entry_is_found_in_mcp_list(self):
        script = self.bin / 'agy'
        script.write_text('#!' + sys.executable + '\nimport sys\n'
                          f'open({str(self.log)!r}, "a").write(" ".join(sys.argv[1:]) + "\\n")\n'
                          'print("NAME TYPE STATUS COMMAND/URL")\n'
                          'print("rookery stdio enabled /opt/rookery/bin/rookery --state-dir /s mcp --provider agy")\n')
        script.chmod(0o700)
        with patch.dict(os.environ, {'PATH': str(self.bin)}):
            plan = install.plan_cli('agy', Path('/s'))
        self.assertTrue(plan['configured'])
        self.assertIsNone(plan['matches'])
        self.assertEqual(plan['actions'], [])

    def test_entry_for_another_state_directory_does_not_match(self):
        for lossless in (True, False):
            self.assertIs(install._matches(('/opt/rookery/bin/rookery', ['--state-dir', '/state-other', 'mcp']),
                                           '/opt/rookery/bin/rookery', ['--state-dir', '/state', 'mcp'],
                                           lossless=lossless), False)
            self.assertIsNone(install._matches('unknown', '/r', [], lossless=lossless))
        self.assertTrue(install._matches(('/r', ['--state-dir', '/state']), '/r', ['--state-dir', '/state'], lossless=True))
        # A space-separated listing cannot prove argv boundaries, in either direction.
        listed = ('/r', ['--state-dir', '/state', 'mcp', '--provider', 'agy'])
        self.assertIsNone(install._matches(listed, '/r', ['--state-dir', '/state mcp', '--provider', 'agy'], lossless=False))
        self.assertIsNone(install._matches(listed, '/r', listed[1], lossless=False))

    def test_codex_entry_is_read_from_json(self):
        script = self.bin / 'codex'
        payload = json.dumps({'transport': {'command': '/opt/rookery/bin/rookery',
                                            'args': ['--state-dir', '/state-other', 'mcp', '--provider', 'codex']}})
        script.write_text('#!' + sys.executable + '\nprint(' + repr(payload) + ')\n')
        script.chmod(0o700)
        with patch.dict(os.environ, {'PATH': str(self.bin)}):
            plan = install.plan_cli('codex', Path('/s'))
        self.assertTrue(plan['configured'])
        self.assertIs(plan['matches'], False)

    def test_existing_entry_is_never_replaced(self):
        self.fake('codex', True)
        result = self.run_install('codex', True)
        self.assertTrue(result['configured'])
        self.assertEqual(result['done'], [])
        self.assertNotIn('add', self.log.read_text().replace('mcp get', ''))


if __name__ == '__main__':
    unittest.main()
