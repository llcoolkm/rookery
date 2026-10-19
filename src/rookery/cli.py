"""Command line front end for the unified helper, its MCP tools and the hub."""
import argparse
import json
import os
import sys
from .core import Error


def parser():
    p = argparse.ArgumentParser(description="Rookery: saved messages between Codex and Claude sessions")
    p.add_argument("--state-dir", help="private state directory (default ROOKERY_HOME or ~/.local/state/rookery)")
    p.add_argument("--as", dest="actor", default=os.environ.get("ROOKERY_SESSION", "operator"), help="enrolled provider:session-id; identity is a same-user assertion")
    sub = p.add_subparsers(dest="command", required=True)
    sub.add_parser("list", help="discover live sessions")
    e = sub.add_parser("enroll", help="opt a live session into receiving")
    e.add_argument("target")
    e.add_argument("--alias", help="friendly name bound to this exact session")
    d = sub.add_parser("disable")
    d.add_argument("target")
    a = sub.add_parser("clear-alias", help="release an exact local session's alias, including offline sessions")
    a.add_argument("target", help="provider:UUID or node/provider:UUID; aliases are not accepted")
    for name in ("send", "reply"):
        s = sub.add_parser(name)
        if name == "send":
            s.add_argument("--to", required=True)
            s.add_argument("--kind", choices=["ask", "inform"], default="ask")
        else:
            s.add_argument("message_id")
        s.add_argument("--body", required=True, help="text or - to read stdin")
        s.add_argument("--ttl", type=int, default=None, help="seconds; default 3600 for send, 86400 for reply")
        s.add_argument("--id", dest="message_id_override", help="UUID idempotency key")
    a = sub.add_parser("ack")
    a.add_argument("message_id")
    inbox = sub.add_parser("inbox")
    inbox.add_argument("--limit", type=int, default=20)
    inbox.add_argument("--peek", action="store_true", help="inspect without marking messages read or suppressing future native delivery")
    status = sub.add_parser("status")
    status.add_argument("message_id", nargs="?")
    sub.add_parser("doctor", help="helper connection and provider diagnostics")
    for command in ("register-polling", "unregister-polling"):
        polling_command = sub.add_parser(command, help="register or remove an existing process for cooperative inbox polling")
        polling_command.add_argument("target", help="local provider:UUID")
        if command == "register-polling":
            polling_command.add_argument("--pid", type=int, required=True)
            polling_command.add_argument("--name")
    for command in ("attach", "detach"):
        attach = sub.add_parser(command, help=("start this conversation's delivery endpoint from the harness's own shell"
                                               if command == "attach" else "stop an attached delivery endpoint"))
        attach.add_argument("provider", choices=("agy",))
        attach.add_argument("--conversation", help="conversation UUID (attach defaults to the shell's own)")
        if command == "attach":
            attach.add_argument("--name", help="friendly session name shown to peers")
            attach.add_argument("--status", action="store_true", help="report endpoint states without attaching")
            attach.add_argument("--foreground", action="store_true", help="serve in the foreground (diagnostics)")
    busy = sub.add_parser("busy-delivery", help="opt a host provider in or out of delivery during an active turn; "
                                                "on and off both take effect only after a helper restart")
    busy.add_argument("provider")
    busy.add_argument("state", choices=("on", "off"))
    install = sub.add_parser("install", help="plan (default) or apply Rookery MCP integration for one harness")
    install.add_argument("provider", choices=("claude", "codex", "agy"))
    install.add_argument("--apply", action="store_true", help="make the planned changes, backing up replaced files")
    install.add_argument("--accept-inbound", action="store_true",
                         help="claude only: also set crossSessionInbound to accept in Claude's settings.json, so "
                              "every session of the account accepts incoming cross-session messages without asking")
    hook = sub.add_parser("agy-hook", help="AGY plugin hook entry point (reads the hook payload on stdin)")
    hook.add_argument("event", choices=("session-start", "pre-invocation"))
    c = sub.add_parser("configure", help="configure the unified helper")
    c.add_argument("--node", required=True)
    c.add_argument("--url", required=True)
    c.add_argument("--token-file", required=True)
    c.add_argument("--allow-http-loopback", action="store_true")
    daemon = sub.add_parser("daemon", help="run the unified helper")
    daemon.add_argument("--once", action="store_true")
    mcp = sub.add_parser("mcp", help="session-bound MCP messaging tools over stdio")
    binding = mcp.add_mutually_exclusive_group()
    binding.add_argument("--session", help="exact local provider:UUID or node/provider:UUID")
    binding.add_argument("--provider", choices=("codex", "claude", "agy"), help="derive session identity from the calling harness")
    from .service import add_parser as add_service_parser
    add_service_parser(sub)
    from .hub_http import add_parser as add_hub_parser
    add_hub_parser(sub)
    sub.add_parser("health", help="helper connection health")
    w = sub.add_parser("wait", help="wait for a reply to a sent message")
    w.add_argument("message_id")
    w.add_argument("--timeout", type=float, default=60)
    w.add_argument("--receipt", action="store_true", help="return on acknowledgment instead of requiring a reply")
    return p


def run(args):
    if args.command == "hub":
        from .hub_http import run as hub_run
        return hub_run(args)
    if args.command == "service":
        from pathlib import Path
        from .service import run as service_run
        path = Path(args.state_dir or os.environ.get('ROOKERY_HOME',Path.home()/'.local/state/rookery')).expanduser().absolute()
        return service_run(args,path)
    if args.command == "agy-hook":
        # A hook must never break the agent loop: any failure yields no steps.
        from .install import agy_hook
        try:
            return agy_hook(args.event, args.state_dir or os.environ.get('ROOKERY_HOME') or
                            os.path.expanduser('~/.local/state/rookery'))
        except Exception:
            return {}
    from .local_store import state_path
    path = state_path(args.state_dir)
    if args.command == "busy-delivery":
        from .client import set_busy_delivery
        return set_busy_delivery(path, args.provider, args.state == "on")
    if args.command == "install":
        from .install import install
        return install(args.provider, path, apply=args.apply, accept_inbound=args.accept_inbound)
    if args.command in {"register-polling", "unregister-polling"}:
        try:
            provider, session_id = args.target.split(":", 1)
        except ValueError:
            raise Error("polling target must be local provider:UUID") from None
        from .local_store import Store
        from . import polling
        store = Store(path)
        if args.command == "register-polling":
            return polling.register(store, provider, session_id, args.pid, args.name)
        return polling.unregister(store, provider, session_id)
    if args.command in {"attach", "detach"}:
        from . import agy_endpoint
        from .local_store import Store
        store = Store(path)
        if args.command == "detach":
            if not args.conversation:
                raise Error("detach requires --conversation")
            return agy_endpoint.detach(store, args.conversation)
        if args.status:
            return agy_endpoint.status(store)
        return agy_endpoint.attach(store, args.conversation, args.name, foreground=args.foreground)
    if args.command == "configure":
        from .client import configure
        from pathlib import Path
        return configure(path,args.node,args.url,Path(args.token_file).read_text().strip(),args.allow_http_loopback)
    if args.command == "daemon":
        from .daemon import Helper
        return Helper(path).run(once=args.once)
    if args.command == "mcp":
        from .mcp import run as mcp_run
        mcp_run(path,args.session or (args.actor if args.actor != 'operator' else None), provider=args.provider)
        return None
    if not (path/'config.json').exists():
        raise Error(f"no helper configuration in {path}; run `rookery configure` first")
    return unified_run(args,path)


def unified_run(args,path):
    from .client import Client
    client = Client(path,args.actor if args.actor!='operator' else None)
    command = args.command
    if command == 'list':
        return client.peers()
    if command == 'health':
        return client.status()
    if command == 'doctor':
        from . import agy_endpoint
        from .local_store import Store
        # MCP connectivity is not delivery: attached endpoints are reported apart.
        health = client.status()
        from . import providers
        configured = sorted(providers.busy_choice(client.config))
        # Effective is what the running helper applied at start; the setting
        # takes effect (on or off) only after a helper restart.
        effective = health.get('busy_delivery') if health.get('running') else None
        return {**health, 'attachments': agy_endpoint.status(Store(path)),
                'busy_delivery': {'configured': configured, 'effective': effective,
                                  'pending_restart': effective is None or sorted(effective) != configured}}
    if command == 'status':
        return client.status(args.message_id)
    if command == 'inbox':
        return client.inbox(args.limit, peek=args.peek)
    if command == 'enroll':
        return client.enroll(args.target,args.alias)
    if command in ('disable', 'clear-alias'):
        target = args.target if '/' in args.target else client.config['node']+'/'+args.target
        return client.request(command,{'target':target})
    if command in ('send','reply'):
        body = sys.stdin.read() if args.body=='-' else args.body
        if command == 'send':
            return client.send(args.to,body,args.kind,3600 if args.ttl is None else args.ttl,args.message_id_override)
        return client.reply(args.message_id,body,86400 if args.ttl is None else args.ttl,args.message_id_override)
    if command == 'ack':
        return client.ack(args.message_id)
    if command == 'wait':
        import time
        deadline = time.monotonic()+args.timeout
        while time.monotonic()<deadline:
            msg = client.status(args.message_id)
            if msg.get('reply_id'):
                return {'message':msg,'reply':client.status(msg['reply_id'])}
            if args.receipt and msg.get('status') in ('acknowledged','replied'):
                return msg
            if msg.get('status') in ('failed','uncertain','expired','cancelled'):
                raise Error('message '+msg['status'])
            time.sleep(.2)
        raise Error('wait timed out; message was not resent')
    raise Error('unknown command: '+command)


def main():
    try:
        result = run(parser().parse_args())
        if result is not None:
            print(json.dumps(result, ensure_ascii=False, indent=2))
    except (Error, OSError, ValueError) as exc:
        print(f"rookery: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
