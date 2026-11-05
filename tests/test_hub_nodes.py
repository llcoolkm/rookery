import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from rookery.cli import parser, run
from rookery.core import Error
from rookery.hub_nodes import manage


class HubNodeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / 'tokens.json'
        self.original = {'alpha': 'a' * 64}
        self.path.write_text(json.dumps(self.original))
        self.path.chmod(0o640)

    def test_cli_add_list_remove_preserves_owner_mode_and_other_entries(self):
        original_stat = self.path.stat()
        def cli(*args):
            return run(parser().parse_args(['hub', 'node', *args, '--tokens', str(self.path)]))
        self.assertEqual(cli('list'), {'nodes': ['alpha']})
        cli('add', 'beta', 'b' * 64)
        self.assertEqual(json.loads(self.path.read_text()), {'alpha': 'a' * 64, 'beta': 'b' * 64})
        info = self.path.stat()
        self.assertEqual((info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)),
                         (original_stat.st_uid, original_stat.st_gid, 0o640))
        cli('remove', 'beta')
        self.assertEqual(json.loads(self.path.read_text()), self.original)

    def test_invalid_changes_leave_file_unchanged(self):
        before = self.path.read_bytes()
        for args in [('add', 'alpha', 'b' * 64), ('add', 'beta', 'a' * 64),
                     ('add', 'Bad', 'b' * 64), ('add', 'beta', 'B' * 64),
                     ('add', 'beta', 'raw-token'), ('remove', 'alpha'), ('remove', 'beta')]:
            with self.subTest(args=args), self.assertRaises(Error):
                manage(self.path, *args)
            self.assertEqual(self.path.read_bytes(), before)

    def test_first_node_can_be_added_to_empty_map(self):
        self.path.write_text('{}')
        manage(self.path, 'add', 'alpha', 'a' * 64)
        self.assertEqual(json.loads(self.path.read_text()), self.original)

    def test_invalid_existing_maps_and_symlinks_are_refused(self):
        for contents in ['[]', '{', '{"Bad":"' + 'a' * 64 + '"}',
                         '{"alpha":"raw-token"}', json.dumps({'alpha': 'a' * 64, 'beta': 'a' * 64}),
                         '{"alpha":"' + 'a' * 64 + '","alpha":"' + 'b' * 64 + '"}']:
            self.path.write_text(contents)
            with self.subTest(contents=contents), self.assertRaises(Error):
                manage(self.path, 'list')
        link = self.path.with_name('link.json')
        link.symlink_to(self.path)
        with self.assertRaises(Error):
            manage(link, 'list')

    def test_failed_replace_leaves_original_and_cleans_temp_file(self):
        before = self.path.read_bytes()
        with patch('rookery.hub_nodes.os.replace', side_effect=OSError('replace failed')):
            with self.assertRaises(OSError):
                manage(self.path, 'add', 'beta', 'b' * 64)
        self.assertEqual(self.path.read_bytes(), before)
        self.assertFalse(list(self.path.parent.glob('.tokens.json-*')))

    def test_atomic_replace_follows_data_fsync_and_is_in_same_directory(self):
        actual_replace, actual_fsync = os.replace, os.fsync
        calls = []
        def sync(fd):
            calls.append('fsync')
            return actual_fsync(fd)
        def replace(source, target):
            self.assertEqual(Path(source).parent, self.path.parent)
            self.assertEqual(calls, ['fsync'])
            calls.append('replace')
            actual_replace(source, target)
        with patch('rookery.hub_nodes.os.fsync', side_effect=sync), patch('rookery.hub_nodes.os.replace', side_effect=replace):
            manage(self.path, 'add', 'beta', 'b' * 64)
        self.assertEqual(calls, ['fsync', 'replace', 'fsync'])


class KubernetesHubNodeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.script = Path(__file__).resolve().parents[1] / 'deploy/kubernetes/hub-node.sh'
        self.state = self.root / 'secret.json'
        self.backups = self.root / 'backups'
        import base64
        self.state.write_text(json.dumps({'metadata': {'resourceVersion': '1'}, 'data': {
            'tokens.json': base64.b64encode(json.dumps({'alpha': 'a' * 64}).encode()).decode(),
            'unrelated': 'preserved'}}))
        kubectl = self.root / 'kubectl'
        kubectl.write_text('''#!/usr/bin/env python3
import base64, json, os, pathlib, sys
args = sys.argv[1:]
if args[:2] != ['-n', 'test-namespace'] or args[3:5] != ['secret', 'test-secret']:
    sys.exit('unexpected namespace or secret')
path = pathlib.Path(os.environ['MOCK_SECRET'])
secret = json.loads(path.read_text())
if args[2] == 'get':
    print(json.dumps(secret))
elif args[2] == 'patch':
    patch = json.loads(pathlib.Path(args[-1]).read_text())
    if patch['metadata']['resourceVersion'] != secret['metadata']['resourceVersion']:
        sys.exit('resourceVersion conflict')
    secret['data'].update(patch['data'])
    if os.environ.get('MOCK_TAMPER'):
        records = json.loads(base64.b64decode(secret['data']['tokens.json']))
        records['alpha'] = 'c' * 64
        secret['data']['tokens.json'] = base64.b64encode(json.dumps(records).encode()).decode()
    secret['metadata']['resourceVersion'] = str(int(secret['metadata']['resourceVersion']) + 1)
    path.write_text(json.dumps(secret))
else:
    sys.exit('unexpected command')
''')
        kubectl.chmod(0o755)

    def invoke(self, *args, tamper=False):
        import subprocess
        env = {**os.environ, 'PATH': str(self.root) + os.pathsep + os.environ['PATH'],
               'MOCK_SECRET': str(self.state)}
        if tamper:
            env['MOCK_TAMPER'] = '1'
        return subprocess.run(['bash', str(self.script), '--namespace', 'test-namespace',
                               '--secret', 'test-secret', '--backup-dir', str(self.backups), *args],
                              text=True, capture_output=True, env=env)

    def test_add_remove_list_and_backups(self):
        before = self.state.read_bytes()
        result = self.invoke('list')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'alpha\n')
        self.assertFalse(self.backups.exists())
        result = self.invoke('add', 'beta', 'b' * 64)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('verified', result.stdout)
        backup = next(self.backups.glob('*.json'))
        self.assertEqual(json.loads(backup.read_text()), json.loads(before))
        self.assertEqual(stat.S_IMODE(backup.stat().st_mode), 0o600)
        self.assertEqual(json.loads(self.state.read_text())['data']['unrelated'], 'preserved')
        self.assertEqual(self.invoke('remove', 'beta').returncode, 0)
        self.assertEqual(len(list(self.backups.glob('*.json'))), 2)

    def test_invalid_operations_do_not_patch_or_create_backup(self):
        before = self.state.read_bytes()
        for args in [('remove', 'alpha'), ('add', 'beta', 'raw-token'),
                     ('add', 'beta', 'a' * 64), ('add', 'alpha', 'b' * 64),
                     ('add', 'Bad', 'b' * 64), ('remove', 'beta'), ('list', 'extra')]:
            with self.subTest(args=args):
                self.assertNotEqual(self.invoke(*args).returncode, 0)
                self.assertEqual(self.state.read_bytes(), before)
        self.assertFalse(self.backups.exists())

    def test_tampered_untouched_entry_fails_verification_and_keeps_backup(self):
        before = self.state.read_bytes()
        result = self.invoke('add', 'beta', 'b' * 64, tamper=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('untouched entries changed', result.stderr)
        self.assertEqual(json.loads(next(self.backups.glob('*.json')).read_text()), json.loads(before))
