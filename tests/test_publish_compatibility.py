"""Optional cross-version publish regression against the 0.3.2 source tree.

Point ROOKERY_COMPAT_BASELINE at an unpacked git archive of ace2346 and run:
    uv run --frozen python -m unittest discover -s tests -p test_publish_compatibility.py
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


BASELINE = os.environ.get('ROOKERY_COMPAT_BASELINE')
SESSION = {'provider': 'claude', 'id': '00000000-0000-4000-8000-000000000001',
           'name': 'synthetic approval', 'status': 'waiting',
           'waiting_for': 'permission', 'cwd': '/synthetic/work'}


def helper_payload(root):
    from rookery.client import configure
    from rookery.daemon import Helper

    class Recorder:
        def call(self, op, data=None):
            if op != 'publish':
                raise AssertionError(f'unexpected helper call {op}')
            self.payload = data
            return {'published': 1}

    recorder = Recorder()
    state = root / 'state'
    configure(state, 'test', 'http://127.0.0.1:9999', 'x' * 32, allow_http=True)
    Helper(state, discover=lambda: ([SESSION.copy()], {}), client=recorder).publish()
    return recorder.payload


def hub_publish(root, payload):
    from rookery.hub import Hub

    hub = Hub(root / 'hub.sqlite3')
    try:
        result = hub.call('test', 'publish', payload)
        rows = hub.call('test', 'list')['sessions']
        return {'result': result, 'sessions': rows}
    finally:
        hub.close()


def baseline_probe(source):
    # The legacy package and current package share a module name. Run the
    # baseline in an isolated process with its exact source first on sys.path.
    sys.path.insert(0, str(Path(source) / 'src'))
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        current_payload = json.load(sys.stdin)
        response = {'legacy_hub': hub_publish(root, current_payload), 'legacy_payload': helper_payload(root)}
        json.dump(response, sys.stdout)


@unittest.skipUnless(BASELINE, 'set ROOKERY_COMPAT_BASELINE to the unpacked ace2346 source tree')
class PublishCompatibilityTests(unittest.TestCase):
    def test_helpers_and_hubs_interoperate_with_032(self):
        baseline = Path(BASELINE).resolve()
        self.assertTrue((baseline / 'src' / 'rookery' / 'hub.py').is_file(), f'no baseline hub at {baseline}')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            current = helper_payload(root)
            self.assertEqual(current['sessions'][0]['waiting_for'], SESSION['waiting_for'])
            self.assertEqual(current['sessions'][0]['cwd'], SESSION['cwd'])
            run = subprocess.run([sys.executable, '-I', str(Path(__file__).resolve()), '--baseline-probe', str(baseline)],
                                 input=json.dumps(current), capture_output=True, text=True, check=True, timeout=15)
            exchange = json.loads(run.stdout)
            legacy_hub = exchange['legacy_hub']
            self.assertEqual(legacy_hub['result'], {'published': 1})
            self.assertEqual(len(legacy_hub['sessions']), 1)
            legacy_row = legacy_hub['sessions'][0]
            self.assertEqual((legacy_row['id'], legacy_row['status'], legacy_row['online']),
                             (SESSION['id'], SESSION['status'], True))
            self.assertNotIn('waiting_for', legacy_row)
            self.assertNotIn('cwd', legacy_row)

            legacy_payload = exchange['legacy_payload']
            self.assertNotIn('waiting_for', legacy_payload['sessions'][0])
            self.assertNotIn('cwd', legacy_payload['sessions'][0])
            current_hub = hub_publish(root, legacy_payload)
            self.assertEqual(current_hub['result'], {'published': 1})
            self.assertEqual(len(current_hub['sessions']), 1)
            current_row = current_hub['sessions'][0]
            self.assertEqual((current_row['id'], current_row['status'], current_row['online']),
                             (SESSION['id'], SESSION['status'], True))
            self.assertGreater(current_row['status_since'], 0)


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--baseline-probe':
        baseline_probe(sys.argv[2])
    else:
        unittest.main()
