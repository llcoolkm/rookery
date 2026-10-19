"""Provider registry: how each harness's sessions are found, admitted and reached.

Providers differ in real guarantees (wake on idle, busy-turn delivery, receipt),
so each declares them as capabilities rather than pretending to be equal. Every
provider reports only held, submitted, failed or uncertain; submitted never
means the model received the input.
"""
from __future__ import annotations

from dataclasses import dataclass
import sys

from ..common import PROVIDER as NAME  # one grammar for local registry and wire addresses
RESULTS = frozenset({"held", "submitted", "failed", "uncertain"})
BUILTIN = ("claude", "codex", "agy")


@dataclass(frozen=True)
class Capabilities:
    # host: the helper writes into the harness directly.
    # attached: a session-launched endpoint holds the harness credentials.
    topology: str
    # True only where delivery during an active turn is proven for the route.
    accepts_busy: bool
    receipt: str = "submitted input is not model receipt; only ack or reply proves receipt"
    # The route is documented to accept input during a turn but is not yet
    # accepted as default: the operator may opt in per provider.
    busy_optin: bool = False


class Provider:
    """Base provider. Subclasses override discover, ready and submit."""
    name = ""
    capabilities = Capabilities(topology="host", accepts_busy=False)

    def discover(self, state=None):
        return []

    def admission(self, session):
        """The single admission policy shared by helper and hub.

        "busy" admits during an active turn, "idle" only when idle, "none"
        never (polling sessions are read by the session itself).
        """
        if session.get("delivery") == "polling":
            return "none"
        if self.capabilities.accepts_busy or (self.capabilities.busy_optin and self.name in _BUSY_OPTIN):
            return "busy"
        return "idle"

    def ready(self, session):
        """Whether the helper may claim and submit to this session now."""
        policy = self.admission(session)
        return policy == "busy" or (policy == "idle" and session.get("status") == "idle")

    def submit(self, session, text):
        return {"status": "held", "detail": f"{self.name} has no native submit route"}


class _Claude(Provider):
    name = "claude"
    # Claude Code documents that peer input arriving during a turn is read
    # between tool calls. Busy delivery stays opt-in until accepted live.
    capabilities = Capabilities(topology="host", accepts_busy=False, busy_optin=True)

    def discover(self, state=None):
        from .. import claude_adapter
        return claude_adapter.discover()

    def admission(self, session):
        # A session waiting on a dialog (permission, question) is never pushed
        # mid-turn; publishing "idle" makes the hub hold it until it is idle.
        policy = super().admission(session)
        # Busy admission only for a session positively reported busy or idle;
        # waiting (a dialog is open) or unknown falls back to idle-only.
        if policy == "busy" and session.get("status") not in {"busy", "idle"}:
            return "idle"
        return policy

    def submit(self, session, text):
        from .. import claude_adapter
        return claude_adapter.submit(session, text, allow_busy=self.admission(session) == "busy")


class _Codex(Provider):
    name = "codex"
    capabilities = Capabilities(topology="host", accepts_busy=False)

    def discover(self, state=None):
        from .. import codex_adapter
        return codex_adapter.discover()

    def submit(self, session, text):
        from .. import codex_adapter
        return codex_adapter.submit(session, text)


class _Agy(Provider):
    name = "agy"
    # Proven: agentapi send-message wakes an idle conversation and is accepted
    # during an active turn. Only an attached endpoint holds the credentials.
    capabilities = Capabilities(topology="attached", accepts_busy=True)

    def discover(self, state=None):
        if state is None:
            return []
        from .. import agy_endpoint
        return agy_endpoint.discover(state)

    def admission(self, session):
        # Only a live attached endpoint holds the agentapi credentials.
        return "busy" if session.get("delivery") == "attached" and session.get("socket") else "none"

    def submit(self, session, text):
        from .. import agy_endpoint
        if session.get("delivery") != "attached":
            return {"status": "held", "detail": "AGY session has no attached endpoint; run `rookery attach agy` from that conversation"}
        return agy_endpoint.submit(session, text)


_REGISTRY = {p.name: p for p in (_Claude(), _Codex(), _Agy())}
# Providers that receive during an active turn unless the operator turns it
# off. Orchestrating Claude sessions are busy while subagents run, so
# idle-only delivery starved them.
DEFAULT_BUSY_DELIVERY = ("claude",)
_BUSY_OPTIN = frozenset(DEFAULT_BUSY_DELIVERY)


def busy_choice(config):
    """The configured busy-delivery list, or the default when never set."""
    if isinstance(config, dict) and "busy_delivery" in config:
        return config["busy_delivery"]
    return list(DEFAULT_BUSY_DELIVERY)


def configure(config):
    """Apply operator options from the helper config (currently busy_delivery)."""
    global _BUSY_OPTIN
    chosen = busy_choice(config)
    if not isinstance(chosen, list):
        chosen = []
    _BUSY_OPTIN = frozenset(name for name in chosen if isinstance(name, str)
                            and get(name) is not None and get(name).capabilities.busy_optin)
    return sorted(_BUSY_OPTIN)


def busy_optin():
    return sorted(_BUSY_OPTIN)
_LOADED = False


def _load_plugins():
    """Load third-party providers from the `rookery.providers` entry-point group once.

    Plugins may add names but never replace a built-in provider, and a broken
    plugin is reported rather than disabling the built-ins.
    """
    global _LOADED
    if _LOADED:
        return
    _LOADED = True
    try:
        from importlib.metadata import entry_points
        points = entry_points(group="rookery.providers")
    except Exception:
        return
    for point in points:
        try:
            provider = point.load()
            provider = provider() if isinstance(provider, type) else provider
            name = getattr(provider, "name", None)
            if (not isinstance(provider, Provider) or not isinstance(name, str)
                    or not NAME.fullmatch(name) or name in _REGISTRY or point.name != name):
                raise ValueError("invalid or conflicting provider")
            _REGISTRY[name] = provider
        except Exception as exc:
            print(f"rookery: ignored provider plugin {point.name!r}: {type(exc).__name__}", file=sys.stderr)


def names():
    _load_plugins()
    return frozenset(_REGISTRY)


def get(name):
    _load_plugins()
    return _REGISTRY.get(name)


def discover_all(state=None, only=None):
    """Discover sessions from every provider; one failing provider never hides others."""
    sessions, errors = [], {}
    for name in sorted(names()):
        if only is not None and name not in only:
            continue
        try:
            found = get(name).discover(state)
            sessions.extend(s for s in found if s.get("provider") == name)
        except Exception as exc:
            errors[name] = str(exc)
    return sessions, errors


def ready(session):
    provider = get(session.get("provider"))
    return bool(provider and provider.ready(session))


def admission(session):
    """A plugin returning anything else gets "none", so it cannot make the
    node's whole publication rejected by the hub."""
    provider = get(session.get("provider"))
    try:
        value = provider.admission(session) if provider else "none"
    except Exception:
        return "none"
    return value if isinstance(value, str) and value in {"idle", "busy", "none"} else "none"


def submit(session, text):
    provider = get(session.get("provider"))
    if provider is None:
        return {"status": "held", "detail": "unknown provider; no input submitted"}
    result = provider.submit(session, text)
    if not isinstance(result, dict) or result.get("status") not in RESULTS:
        return {"status": "uncertain", "detail": "unrecognized provider result"}
    return result
