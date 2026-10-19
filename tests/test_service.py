import argparse
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from rookery import service


class ServiceTests(unittest.TestCase):
    def config(self):
        return dict(user='alpha', home='/home/alice', path='/home/alice/.local/bin:/usr/bin:/bin',
                    executable='/home/alice/.local/bin/rookery', state_dir='/home/alice/.local/state/rookery')

    def test_wsl_preserves_existing_daemons_and_sections(self):
        original = '# keep\n[boot]\nsystemd=false\ncommand=/usr/sbin/sshd; /usr/sbin/cron\n\n[user]\ndefault=alpha\n'
        command = service.wsl_boot_command(self.config())
        merged = service.merge_wsl_config(original, command)
        self.assertIn('command=/usr/sbin/sshd; /usr/sbin/cron; ' + command, merged)
        self.assertIn('[user]\ndefault=alpha', merged)
        self.assertIn('HOME=/home/alice', merged)
        self.assertEqual(service.merge_wsl_config(merged, command), merged)

    def test_wsl_shell_executes_existing_nested_command_and_launcher(self):
        import shlex
        with tempfile.TemporaryDirectory() as tmp:
            existing = Path(tmp) / 'existing service'
            appended = Path(tmp) / 'helper marker'
            # WSL itself executes boot.command using /bin/sh -c. Existing
            # boot scripts may already wrap their command with another sh -c.
            prior = shlex.join(['/bin/sh', '-c',
                'printf existing > ' + shlex.quote(str(existing)) + '; exit 1'])
            launcher = shlex.join(['/bin/sh', '-c',
                'printf helper > ' + shlex.quote(str(appended))])
            merged = service.merge_wsl_config('[boot]\ncommand=' + prior + '\n', launcher)
            command = merged.split('command=', 1)[1].strip()
            result = subprocess.run(['/bin/sh', '-c', command], capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(existing.read_text(), 'existing')
            self.assertEqual(appended.read_text(), 'helper')

    def test_wsl_upgrade_replaces_only_previous_launcher(self):
        original = '[boot]\ncommand=cron; old-launcher\n[network]\nhostname=x\n'
        self.assertEqual(service.merge_wsl_config(original, 'new-launcher', 'old-launcher'),
                         '[boot]\ncommand=cron; new-launcher\n[network]\nhostname=x\n')

    def test_wsl_adds_missing_boot_section_or_command(self):
        self.assertIn('[boot]\ncommand=run\n', service.merge_wsl_config('[user]\ndefault=x\n', 'run'))
        self.assertEqual(service.merge_wsl_config('[boot]\nsystemd=false\n', 'run'), '[boot]\nsystemd=false\ncommand=run\n')

    def test_command_injection_is_quoted(self):
        import shlex
        config = self.config()
        config['state_dir'] = '/home/alice/space ; $(touch dangerous)'
        words = shlex.split(service.wsl_boot_command(config))
        self.assertEqual(words[words.index('--state-dir') + 1], config['state_dir'])
        unit = service.systemd_unit({**config, 'executable': '/opt/100%/$bad/rookery'})
        self.assertIn('100%%/$$bad', unit)
        self.assertIn('User=alpha', unit)
        self.assertIn('HOME=/home/alice', unit)
        self.assertNotIn('/home/alpha', unit)

    @unittest.skipUnless(shutil.which('systemd-analyze'), 'systemd-analyze unavailable')
    def test_generated_unit_passes_systemd_verification(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp) / 'home with spaces 100%'
            home.mkdir()
            config = {**self.config(), 'user': pwd.getpwuid(os.getuid()).pw_name,
                      'home': str(home), 'executable': '/usr/bin/true'}
            unit = Path(tmp) / 'rookery-test.service'
            unit.write_text(service.systemd_unit(config))
            result = subprocess.run([shutil.which('systemd-analyze'), 'verify', str(unit)],
                                    capture_output=True, text=True, timeout=20)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn('WorkingDirectory=' + str(home).replace('%', '%%') + '\n', unit.read_text())
            self.assertIn('Restart=always\n', unit.read_text())

    def test_manual_install_does_not_start_anything(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(service.subprocess, 'run') as run, patch.object(service.subprocess, 'Popen') as popen:
            args = argparse.Namespace(user=pwd.getpwuid(os.getuid()).pw_name, executable='/usr/bin/true', backend='manual', dry_run=False)
            result = service.install(args, Path(tmp))
            self.assertFalse(result['started'])
            self.assertTrue((Path(tmp) / 'service.json').exists())
            run.assert_not_called()
            popen.assert_not_called()

    def test_dry_run_does_not_write(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp) / 'absent'
            args = argparse.Namespace(user=pwd.getpwuid(os.getuid()).pw_name, executable='/usr/bin/true', backend='wsl', dry_run=True)
            result = service.install(args, state)
            self.assertEqual(result['backend'], 'wsl')
            self.assertFalse(state.exists())

    def test_supervisor_restarts_failed_child_and_stops_gracefully(self):
        import subprocess
        import sys
        import time
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)
            executable = state / 'fake-helper'
            executable.write_text("#!/usr/bin/python3\nimport pathlib, sys, time\np = pathlib.Path(sys.argv[2]) / 'runs'\nn = int(p.read_text()) + 1 if p.exists() else 1\np.write_text(str(n))\nprint('started', flush=True)\nif n == 1: sys.exit(7)\ntime.sleep(30)\n")
            executable.chmod(0o700)
            config = {**self.config(), 'user': pwd.getpwuid(os.getuid()).pw_name,
                      'executable': str(executable), 'state_dir': str(state)}
            (state / 'service.json').write_text(json.dumps(config))
            proc = subprocess.Popen([sys.executable, '-c',
                'from pathlib import Path; from rookery.service import supervise; import sys; supervise(Path(sys.argv[1]))', str(state)])
            try:
                deadline = time.monotonic() + 6
                while time.monotonic() < deadline:
                    if (state / 'runs').exists() and (state / 'runs').read_text() == '2':
                        break
                    time.sleep(.05)
                self.assertEqual((state / 'runs').read_text(), '2')
                self.assertIsNotNone(service.running(state))
                # A competing supervisor exits without starting another helper.
                self.assertEqual(service.supervise(state), {'running': True})
                proc.terminate()
                self.assertEqual(proc.wait(timeout=5), 0)
                self.assertIsNone(service.running(state))
                self.assertIn(b'helper exited 7', (state / 'service.log').read_bytes())
            finally:
                if proc.poll() is None:
                    proc.terminate()
                    proc.wait(timeout=5)

    def test_supervisor_restarts_clean_child_exit_but_service_stop_stays_down(self):
        import signal
        import sys
        import time
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)
            executable = state / 'fake-helper'
            executable.write_text("#!/usr/bin/python3\nimport os, pathlib, signal, sys, time\nstate = pathlib.Path(sys.argv[2])\nsignal.signal(signal.SIGTERM, lambda *_: sys.exit(0))\np = state / 'runs'\nn = int(p.read_text()) + 1 if p.exists() else 1\n(state / 'child.pid').write_text(str(os.getpid()))\np.write_text(str(n))\ntime.sleep(30)\n")
            executable.chmod(0o700)
            config = {**self.config(), 'backend': 'manual',
                      'user': pwd.getpwuid(os.getuid()).pw_name,
                      'executable': str(executable), 'state_dir': str(state)}
            (state / 'service.json').write_text(json.dumps(config))
            proc = subprocess.Popen([sys.executable, '-c',
                'from pathlib import Path; from rookery.service import supervise; import sys; supervise(Path(sys.argv[1]))', str(state)])
            def wait_for_runs(count):
                deadline = time.monotonic() + 6
                while time.monotonic() < deadline:
                    if (state / 'runs').exists() and (state / 'runs').read_text() == str(count):
                        return
                    time.sleep(.05)
                self.fail('helper did not reach run ' + str(count))
            try:
                wait_for_runs(1)
                # Model the live failure: the daemon handles an external SIGTERM
                # gracefully and exits zero while the supervisor remains alive.
                os.kill(int((state / 'child.pid').read_text()), signal.SIGTERM)
                wait_for_runs(2)
                child_pid = int((state / 'child.pid').read_text())
                self.assertIn(b'helper exited 0', (state / 'service.log').read_bytes())
                result = service.run(argparse.Namespace(service_action='stop'), state)
                self.assertTrue(result['stopped'])
                self.assertEqual(proc.wait(timeout=5), 0)
                time.sleep(1.2)
                self.assertEqual((state / 'runs').read_text(), '2')
                self.assertIsNone(service.running(state))
                self.assertIsNone(service.process_identity(child_pid))
            finally:
                if proc.poll() is None:
                    proc.terminate()
                    proc.wait(timeout=5)

    def test_pidfd_stop_uses_libc_when_python_bindings_are_missing(self):
        import signal
        proc = subprocess.Popen(['/bin/sleep', '30'])
        try:
            with patch.object(os, 'pidfd_open', None, create=True), \
                 patch.object(signal, 'pidfd_send_signal', None, create=True):
                fd = service._pidfd_open(proc.pid)
                try:
                    service._pidfd_signal(fd, signal.SIGTERM)
                finally:
                    os.close(fd)
            self.assertEqual(proc.wait(timeout=5), -signal.SIGTERM)
        finally:
            if proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=5)

    def test_stale_pid_never_counts_as_running(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)
            record = service.process_identity(os.getpid())
            (state / 'supervisor.json').write_text(json.dumps(record))
            self.assertEqual(service.running(state), record)
            record['start'] = 'wrong'
            (state / 'supervisor.json').write_text(json.dumps(record))
            self.assertIsNone(service.running(state))

    def test_logs_have_two_bounded_files(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(service, 'LOG_LIMIT', 10):
            state = Path(tmp)
            service._append_log(state, b'12345678')
            service._append_log(state, b'abcdefgh')
            service._append_log(state, b'87654321')
            self.assertEqual((state / 'service.log').read_bytes(), b'87654321')
            self.assertEqual((state / 'service.log.1').read_bytes(), b'abcdefgh')

if __name__ == '__main__':
    unittest.main()
