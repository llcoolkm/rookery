"""Test-only Python helper peer. All state/configuration arrives over stdin.

Never invokes host discovery. Its sessions are synthetic polling registrations;
no provider executable, user state, or external service is consulted.
"""
import json
import os
from pathlib import Path
import sys

from rookery.client import Client, configure
from rookery.daemon import Helper
from rookery import polling

SESSION = "00000000-0000-4000-8000-000000000002"


def main():
    client = helper = None
    for line in sys.stdin:
        try:
            request = json.loads(line)
            operation = request["op"]
            if operation == "setup":
                root = Path(request["root"])
                configure(root, "beta", request["url"], request["token"], allow_http=True)
                client = Client(root, actor="codex:" + SESSION)
                original = client.request
                client.request = lambda op, data, timeout=0: original(op, data, timeout=0)
                helper = Helper(root, discover=lambda: (polling.discover(helper.store), {}),
                                submit=lambda *_: {"status": "failed", "detail": "test rejected unexpected native submission"})
                polling.register(helper.store, "codex", SESSION, os.getpid(), name="synthetic polling peer")
                result = {"ready": True}
            elif operation == "cycle":
                helper.publish()
                helper.sync_once(wait=0)
                helper.cycle()
                helper.sync_once(wait=0)
                result = {"cycle": "complete"}
            elif operation == "invoke":
                result = getattr(client, request["method"])(**(request.get("args") or {}))
            else:
                raise ValueError()
            print(json.dumps({"ok": True, "result": result}, allow_nan=False), flush=True)
        except Exception:
            # Never return potentially credential-bearing exception strings.
            print(json.dumps({"ok": False, "error": "isolated Python peer operation failed"}), flush=True)


if __name__ == "__main__":
    main()
