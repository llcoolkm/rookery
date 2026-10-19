"""Host integration and a small supervised fallback for WSL without systemd."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pwd
import selectors
import shlex
import shutil
import signal
import subprocess
import sys
import time

from .core import Error

UNIT = 'rookery.service'
LOG_LIMIT = 1024 * 1024


def add_parser(sub):
    p = sub.add_parser('service', help='install and manage the Rookery helper')
    commands = p.add_subparsers(dest='service_action', required=True)
    install = commands.add_parser('install', help='install startup integration without starting it')
    install.add_argument('--backend', choices=['auto', 'systemd', 'wsl', 'manual'], default='auto')
    install.add_argument('--user', default=os.environ.get('SUDO_USER') or pwd.getpwuid(os.getuid()).pw_name)
    install.add_argument('--executable', help='absolute path to the installed rookery executable')
    install.add_argument('--path', help='PATH for provider executables (defaults to target home bin directories plus current PATH)')
    install.add_argument('--dry-run', action='store_true')
    for name in ('start', 'stop', 'status'):
        commands.add_parser(name)
    supervise = commands.add_parser('supervise', help=argparse.SUPPRESS)
    supervise.add_argument('--detach', action='store_true')


def detect_backend():
    if Path('/run/systemd/system').is_dir():
        return 'systemd'
    if 'microsoft' in Path('/proc/sys/kernel/osrelease').read_text().lower():
        return 'wsl'
    return 'manual'


def unit_quote(value, *, command=False):
    # systemd performs percent and environment expansion even inside double quotes.
    value = str(value).replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%')
    return '"' + (value.replace('$', '$$') if command else value) + '"'


def unit_directory(value):
    # WorkingDirectory is one raw path, not an ExecStart-style quoted word.
    # Percent specifiers still expand here; spaces and backslashes are literal.
    value = str(value)
    if not value.startswith('/') or '\n' in value or '\r' in value:
        raise Error('service working directory must be an absolute single-line path')
    return value.replace('%', '%%')


def systemd_unit(config):
    argv = [config['executable'], '--state-dir', config['state_dir'], 'daemon']
    return '\n'.join([
        '[Unit]', 'Description=Rookery message helper', 'After=network-online.target',
        'Wants=network-online.target', '', '[Service]', 'Type=simple',
        'User=' + config['user'], 'WorkingDirectory=' + unit_directory(config['home']),
        'Environment=' + unit_quote('HOME=' + config['home']),
        'Environment=' + unit_quote('PATH=' + config['path']),
        'ExecStart=' + ' '.join(unit_quote(item, command=True) for item in argv), 'Restart=always',
        'RestartSec=5', 'TimeoutStopSec=60', 'KillMode=mixed', 'UMask=0077',
        'NoNewPrivileges=true', '', '[Install]', 'WantedBy=multi-user.target', '',
    ])


def wsl_boot_command(config):
    argv = ['/usr/sbin/runuser', '-u', config['user'], '--', '/usr/bin/env',
            'HOME=' + config['home'], 'PATH=' + config['path'], config['executable'],
            '--state-dir', config['state_dir'], 'service', 'supervise', '--detach']
    return shlex.join(argv)


def merge_wsl_config(original, command, previous=None):
    """Change only boot.command, retaining all unrelated text and existing commands."""
    lines = original.splitlines(keepends=True)
    start = end = None
    for index, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith('[') and stripped.endswith(']'):
            if start is not None:
                end = index
                break
            if stripped.lower() == '[boot]':
                start = index
    if start is None:
        return original.rstrip('\n') + '\n\n[boot]\ncommand=' + command + '\n'
    end = len(lines) if end is None else end
    for index in range(start + 1, end):
        key, separator, value = lines[index].partition('=')
        if separator and key.strip().lower() == 'command':
            old = value.strip()
            if old == command or old.endswith('; ' + command):
                return original
            if previous and (old == previous or old.endswith('; ' + previous)):
                old = old[:-len(previous)].removesuffix('; ').rstrip()
            lines[index] = 'command=' + (old + '; ' if old else '') + command + '\n'
            return ''.join(lines)
    lines.insert(end, 'command=' + command + '\n')
    return ''.join(lines)


def _read_config(state):
    try:
        config = json.loads((state / 'service.json').read_text())
    except (OSError, ValueError) as exc:
        raise Error('service is not configured; run service install first') from exc
    if config['state_dir'] != str(state):
        raise Error('service configuration belongs to another state directory')
    return config


def install(args, state):
    account = pwd.getpwnam(args.user)
    executable = args.executable or shutil.which('rookery')
    if not executable or not Path(executable).is_absolute() or not os.access(executable, os.X_OK):
        raise Error('specify --executable with an absolute installed rookery executable path')
    backend = detect_backend() if args.backend == 'auto' else args.backend
    if backend == 'systemd' and not Path('/run/systemd/system').is_dir():
        raise Error('systemd is not running; select wsl or manual')
    if any('\n' in value or '\r' in value for value in (str(executable), str(state), account.pw_dir, os.environ.get('PATH', ''))):
        raise Error('service paths must not contain newlines')
    service_path = getattr(args, 'path', None) or ':'.join([str(Path(account.pw_dir) / '.local/bin'), str(Path(account.pw_dir) / '.npm-global/bin'), os.environ.get('PATH', '/usr/local/bin:/usr/bin:/bin')])
    if '\n' in service_path or '\r' in service_path:
        raise Error('service PATH must not contain newlines')
    config = dict(backend=backend, user=account.pw_name, home=account.pw_dir,
                  executable=str(Path(executable).absolute()), state_dir=str(state),
                  path=service_path,
                  unit='rookery-helper@' + account.pw_name + '.service')
    content = systemd_unit(config) if backend == 'systemd' else wsl_boot_command(config) if backend == 'wsl' else shlex.join([executable, '--state-dir', str(state), 'daemon'])
    if args.dry_run:
        return {'backend': backend, 'content': content, 'started': False}
    if backend != 'manual' and os.geteuid() != 0:
        raise Error('system startup installation requires root; use sudo with explicit --user, --executable and --state-dir, or --backend manual')
    previous = None
    if (state / 'service.json').exists():
        old = _read_config(state)
        if old['backend'] != backend:
            raise Error('remove the previous startup integration before changing service backend')
        if backend == 'wsl':
            previous = wsl_boot_command(old)
    if backend == 'systemd':
        Path('/etc/systemd/system/' + config['unit']).write_text(content)
        subprocess.run(['systemctl', 'daemon-reload'], check=True)
        subprocess.run(['systemctl', 'enable', config['unit']], check=True)
    elif backend == 'wsl':
        target = Path('/etc/wsl.conf')
        original = target.read_text() if target.exists() else ''
        target.write_text(merge_wsl_config(original, content, previous))
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    target = state / 'service.json'
    target.write_text(json.dumps(config, indent=2) + '\n')
    target.chmod(0o600)
    if os.geteuid() == 0:
        os.chown(state, account.pw_uid, account.pw_gid)
        os.chown(target, account.pw_uid, account.pw_gid)
    return {'backend': backend, 'installed': True, 'started': False}


def process_identity(pid):
    try:
        stat = Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()
        return {'pid': pid, 'start': stat[19], 'boot': Path('/proc/sys/kernel/random/boot_id').read_text().strip()}
    except (OSError, IndexError):
        return None


def running(state):
    try:
        record = json.loads((state / 'supervisor.json').read_text())
        return record if process_identity(record['pid']) == record else None
    except (OSError, ValueError, KeyError):
        return None


def _append_log(state, chunk):
    path = state / 'service.log'
    if path.exists() and path.stat().st_size + len(chunk) > LOG_LIMIT:
        path.replace(state / 'service.log.1')
    with path.open('ab') as log:
        os.chmod(path, 0o600)
        log.write(chunk)


def supervise(state):
    config = _read_config(state)
    if os.getuid() != pwd.getpwnam(config['user']).pw_uid:
        raise Error('run the supervisor as its configured user')
    lock = (state / 'supervisor.lock').open('a')
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        lock.close()
        return {'running': True}
    stop = False
    def terminate(signum, frame):
        nonlocal stop
        stop = True
    handlers = {sig: signal.signal(sig, terminate) for sig in (signal.SIGTERM, signal.SIGINT)}
    pidfile = state / 'supervisor.json'
    pidfile.write_text(json.dumps(process_identity(os.getpid())))
    delay = 1
    try:
        while not stop:
            started = time.monotonic()
            child = subprocess.Popen([config['executable'], '--state-dir', str(state), 'daemon'],
                                     stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                     stderr=subprocess.STDOUT, start_new_session=True,
                                     env={**os.environ, 'HOME': config['home'], 'PATH': config['path']})
            selector = selectors.DefaultSelector()
            selector.register(child.stdout, selectors.EVENT_READ)
            shutdown = None
            while child.poll() is None:
                if stop and shutdown is None:
                    try:
                        os.killpg(child.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                    shutdown = time.monotonic()
                if shutdown is not None and time.monotonic() - shutdown > 55:
                    try:
                        os.killpg(child.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                for key, _ in selector.select(.2):
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if chunk:
                        _append_log(state, chunk)
                    else:
                        selector.unregister(key.fileobj)
            os.set_blocking(child.stdout.fileno(), False)
            while True:
                try:
                    chunk = os.read(child.stdout.fileno(), 65536)
                except BlockingIOError:
                    break
                if not chunk:
                    break
                _append_log(state, chunk)
            selector.close()
            child.stdout.close()
            child.wait()
            if stop:
                break
            _append_log(state, f'helper exited {child.returncode}; restarting in {delay}s\n'.encode())
            until = time.monotonic() + delay
            while not stop and time.monotonic() < until:
                time.sleep(.2)
            delay = 1 if time.monotonic() - started > 60 else min(delay * 2, 60)
    finally:
        pidfile.unlink(missing_ok=True)
        lock.close()
        for sig, handler in handlers.items():
            signal.signal(sig, handler)
    return {'stopped': True}


def _libc_pidfd(name, arguments, argtypes):
    # Some standalone/uv Python builds omit both pidfd bindings even though
    # the host libc and kernel support them. Keep the same kernel handle API;
    # never fall back to signaling a numeric PID that could have been reused.
    import ctypes
    libc = ctypes.CDLL(None, use_errno=True)
    try:
        function = getattr(libc, name)
    except AttributeError:
        raise Error('safe service stop requires Linux pidfd support in Python or libc') from None
    function.argtypes = argtypes
    function.restype = ctypes.c_int
    result = function(*arguments)
    if result < 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))
    return result


def _pidfd_open(pid):
    native = getattr(os, 'pidfd_open', None)
    if native is not None:
        return native(pid)
    import ctypes
    return _libc_pidfd('pidfd_open', (pid, 0), (ctypes.c_int, ctypes.c_uint))


def _pidfd_signal(fd, sig):
    native = getattr(signal, 'pidfd_send_signal', None)
    if native is not None:
        return native(fd, sig)
    import ctypes
    return _libc_pidfd('pidfd_send_signal', (fd, sig, None, 0),
                      (ctypes.c_int, ctypes.c_int, ctypes.c_void_p, ctypes.c_uint))


def run(args, state_dir):
    state = Path(state_dir).expanduser().absolute()
    if args.service_action == 'install':
        return install(args, state)
    config = _read_config(state)
    action = args.service_action
    if action == 'supervise' and not args.detach:
        return supervise(state)
    if config['backend'] == 'systemd':
        if action == 'supervise':
            raise Error('systemd manages this service; use service start')
        result = subprocess.run(['systemctl', action if action != 'status' else 'is-active', config.get('unit', UNIT)], capture_output=True, text=True)
        if action != 'status' and result.returncode:
            raise Error(result.stderr.strip() or 'systemctl failed')
        return {'backend': 'systemd', 'running': result.returncode == 0} if action == 'status' else {'action': action, 'ok': True}
    record = running(state)
    if action == 'status':
        return {'backend': config['backend'], 'running': bool(record), 'pid': record['pid'] if record else None}
    if os.getuid() != pwd.getpwnam(config['user']).pw_uid:
        raise Error('run service commands as the configured user')
    if action == 'stop':
        if record:
            try:
                fd = _pidfd_open(record['pid'])
            except ProcessLookupError:
                return {'stopped': True}
            try:
                if process_identity(record['pid']) == record:
                    try:
                        _pidfd_signal(fd, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
            finally:
                os.close(fd)
            deadline = time.monotonic() + 60
            while running(state) and time.monotonic() < deadline:
                time.sleep(.1)
            if running(state):
                raise Error('supervisor did not stop within 60 seconds')
        return {'stopped': True}
    if record:
        return {'running': True, 'pid': record['pid']}
    child = subprocess.Popen([config['executable'], '--state-dir', str(state), 'service', 'supervise'],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        record = running(state)
        if record:
            return {'running': True, 'pid': record['pid']}
        if child.poll() is not None:
            raise Error('supervisor failed to start; run service supervise in foreground for diagnostics')
        time.sleep(.1)
    raise Error('supervisor startup timed out; inspect service status')
