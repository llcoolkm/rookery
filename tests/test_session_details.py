"""Helper-side tmux discovery degrades independently of session publication."""
import subprocess
import unittest
from unittest.mock import patch

from rookery.session_details import SessionDetails


class SessionDetailsTests(unittest.TestCase):
    def test_tmux_mapping_uses_nearest_live_ancestor_and_skips_unreadable_processes(self):
        sessions = [{'pid': pid, 'status': 'idle'} for pid in (120, 220, 320, 420, 620)]
        processes = {120: 121, 121: 122, 122: 1, 220: 221, 221: 222, 222: 1,
                     320: 321, 321: 1, 420: 421, 421: 1}

        def process(pid):
            if pid not in processes:
                raise FileNotFoundError(pid)
            return processes[pid], 1

        with patch('rookery.session_details.os.path.isfile', return_value=True), \
             patch('rookery.session_details._process', side_effect=process), \
             patch('rookery.session_details._list_panes', return_value={
                 122: 'work:2', 221: 'nested:7', 222: 'parent:3', 320: 'direct:4', 620: 'gone:6',
             }):
            self.assertEqual(SessionDetails().collect(sessions), [
                {'tmux': 'work:2'}, {'tmux': 'nested:7'}, {'tmux': 'direct:4'}, {}, {},
            ])

    def test_tmux_skips_bad_output_lines_and_keeps_valid_unicode(self):
        listing = subprocess.CompletedProcess([], 0, stdout='122 arbetet å:2\n220 invalid\ufffd:3\n320 bad:window\n')
        sessions = [{'pid': pid, 'status': 'idle'} for pid in (122, 220, 320)]
        with patch('rookery.session_details.os.path.isfile', return_value=True), \
             patch('rookery.session_details._process', return_value=(1, 10)), \
             patch('rookery.session_details.subprocess.run', return_value=listing) as run:
            self.assertEqual(SessionDetails().collect(sessions), [{'tmux': 'arbetet å:2'}, {}, {}])
        self.assertEqual(run.call_args.kwargs['encoding'], 'utf-8')
        self.assertEqual(run.call_args.kwargs['errors'], 'replace')

    def test_tmux_unavailable_without_proc_or_pid(self):
        observer = SessionDetails()
        session = {'pid': 122, 'status': 'waiting', 'waiting_for': 'permission'}
        with patch('rookery.session_details.os.path.isfile', return_value=False) as probe, \
             patch('rookery.session_details.subprocess.run') as run:
            self.assertEqual(observer.collect([session]), [{'waiting_for': 'permission'}])
            probe.assert_called_once_with('/proc/self/stat')
            probe.return_value = True
            self.assertEqual(observer.collect([{'status': 'idle', 'cwd': '/tmp'}]), [{'cwd': '/tmp'}])
        run.assert_not_called()

    def test_tmux_timeout_suppresses_invocation_for_sixty_seconds(self):
        observer = SessionDetails()
        sessions = [{'pid': 122, 'status': 'idle', 'cwd': '/tmp'}]
        results = [subprocess.TimeoutExpired('tmux', 2), subprocess.CompletedProcess([], 0, stdout='122 work:2\n')]
        with patch('rookery.session_details.os.path.isfile', return_value=True), \
             patch('rookery.session_details._process', return_value=(1, 10)), \
             patch('rookery.session_details.subprocess.run', side_effect=results) as run, \
             patch('rookery.session_details.time.monotonic', return_value=100) as clock:
            self.assertEqual(observer.collect(sessions), [{'cwd': '/tmp'}])
            clock.return_value = 159
            self.assertEqual(observer.collect(sessions), [{'cwd': '/tmp'}])
            run.assert_called_once()
            clock.return_value = 160
            self.assertEqual(observer.collect(sessions), [{'cwd': '/tmp', 'tmux': 'work:2'}])
            self.assertEqual(run.call_count, 2)
