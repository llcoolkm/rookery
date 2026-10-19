"""CLI runner for the public RPC and private observability listeners."""
import asyncio
import contextlib
import signal
import shutil
import logging
import time
from pathlib import Path
from urllib.parse import urlsplit

from .core import Error
from .hub import Hub, create_app


def listener(value):
    try:
        parsed = urlsplit('http://' + value)
        if not parsed.hostname or parsed.port is None or parsed.path or parsed.username or parsed.password:
            raise ValueError()
        return parsed.hostname, parsed.port
    except ValueError:
        raise Error('listener must be HOST:PORT') from None


def snapshot_once(hub, directory, keep=3):
    """At most keep copies, including a new snapshot being constructed."""
    for old in sorted(directory.glob('hub-*.sqlite3'), reverse=True)[keep - 1:]:
        old.unlink()
    needed = hub.stats()['database_bytes'] + 16 * 1024 * 1024
    if shutil.disk_usage(directory).free < needed:
        raise Error('insufficient free space for a consistent hub snapshot')
    return hub.backup(directory / f'hub-{time.time_ns()}.sqlite3')


def add_parser(sub):
    parser = sub.add_parser('hub', help='run the unified durable messaging authority')
    commands = parser.add_subparsers(dest='hub_command', required=True)
    serve = commands.add_parser('serve')
    serve.add_argument('--db', required=True)
    serve.add_argument('--tokens', required=True)
    serve.add_argument('--listen', default='127.0.0.1:8787')
    serve.add_argument('--metrics-listen', default='127.0.0.1:8788')
    serve.add_argument('--max-bytes', type=int, default=100_000_000)
    serve.add_argument('--pending-limit', type=int, default=1000)
    serve.add_argument('--snapshot-dir')
    serve.add_argument('--snapshot-interval', type=float, default=86400)
    serve.add_argument('--snapshot-keep', type=int, default=3)


async def serve(args):
    try:
        import uvicorn
    except ImportError:
        raise Error("hub serving requires the project's hub extra") from None
    public, internal = listener(args.listen), listener(args.metrics_listen)
    if public == internal or args.snapshot_interval < 1 or not 1 <= args.snapshot_keep <= 3:
        raise Error('listeners must differ; snapshot interval >=1 and keep=1..3')
    tokens = Path(args.tokens)
    if not tokens.is_file():
        raise Error('private node token-hash file is required')
    hub = Hub(args.db, max_bytes=args.max_bytes, pending_limit=args.pending_limit)
    snapshots = Path(args.snapshot_dir) if args.snapshot_dir else hub.path.parent / 'backups'
    snapshots.mkdir(mode=0o700, parents=True, exist_ok=True)
    if snapshots.is_symlink() or snapshots.stat().st_mode & 0o077:
        hub.close()
        raise Error('snapshot directory must be private')

    class Server(uvicorn.Server):
        @contextlib.contextmanager
        def capture_signals(self):
            yield

        def install_signal_handlers(self):
            pass

    servers = [Server(uvicorn.Config(create_app(hub, tokens, internal=private), host=host, port=port,
                                    workers=1, access_log=False, timeout_graceful_shutdown=15,
                                    server_header=False, proxy_headers=False))
               for (host, port), private in ((public, False), (internal, True))]
    loop = asyncio.get_running_loop()
    def stop():
        for server in servers:
            server.should_exit = True
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop)

    async def housekeeping():
        next_snapshot = 0.0
        while True:
            try:
                await asyncio.to_thread(hub.checkpoint)
                await asyncio.to_thread(hub.maintain)
            except Exception as exc:
                logging.getLogger(__name__).warning('hub maintenance failed (%s)', type(exc).__name__)
            if time.monotonic() >= next_snapshot:
                try:
                    await asyncio.to_thread(snapshot_once, hub, snapshots, args.snapshot_keep)
                    next_snapshot = time.monotonic() + args.snapshot_interval
                except Exception as exc:
                    logging.getLogger(__name__).warning('hub snapshot failed (%s)', type(exc).__name__)
                    next_snapshot = time.monotonic() + min(60, args.snapshot_interval)
            await asyncio.sleep(30)

    upkeep = asyncio.create_task(housekeeping())
    tasks = [asyncio.create_task(server.serve()) for server in servers]
    try:
        await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        stop()
        await asyncio.gather(*tasks)
    finally:
        stop()
        upkeep.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await upkeep
        await asyncio.gather(*tasks, return_exceptions=True)
        with contextlib.suppress(Exception):
            hub.checkpoint()
        hub.close()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.remove_signal_handler(sig)
    return {'stopped': True}


def run(args, ledger=None):
    if args.hub_command != 'serve':
        raise Error('unknown hub command')
    return asyncio.run(serve(args))
