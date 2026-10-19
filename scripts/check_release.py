"""Release metadata and installed CLI/MCP checks; no live providers used."""
import argparse
import importlib.metadata
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import tomllib


def metadata_check():
    root = Path(__file__).resolve().parents[1]
    project = tomllib.loads((root / "pyproject.toml").read_text())["project"]
    tag = os.environ.get("GITHUB_REF", "")
    if tag.startswith("refs/tags/"):
        expected = "refs/tags/v" + project["version"]
        if tag != expected:
            raise SystemExit(f"release tag must equal {expected}")
    print("Release metadata verified:", project["name"], project["version"])


def smoke_check():
    version = importlib.metadata.version("rookery-agents")
    executable = Path(sys.executable).parent / "rookery"
    with tempfile.TemporaryDirectory(prefix="rookery-release-") as temporary:
        root = Path(temporary)
        state = root / "state"
        token = root / "token"
        token.write_text(secrets.token_urlsafe(32))
        token.chmod(0o600)
        env = {key: value for key, value in os.environ.items()
               if key not in {"PYTHONPATH", "ROOKERY_HOME", "ROOKERY_SESSION"}}

        def run(*arguments, **kwargs):
            return subprocess.run([str(executable), "--state-dir", str(state), *arguments],
                                  cwd=root, env=env, text=True, capture_output=True,
                                  check=True, timeout=20, **kwargs)

        run("--help")
        run("configure", "--node", "release-smoke", "--url", "https://hub.example.invalid",
            "--token-file", str(token))
        requests = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize",
             "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                        "clientInfo": {"name": "release-smoke", "version": "1"}}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
        ]
        response = run("mcp", "--session", "codex:00000000-0000-4000-8000-000000000001",
                       input="".join(json.dumps(request) + "\n" for request in requests))
        replies = [json.loads(line) for line in response.stdout.splitlines()]
        assert len(replies) == 2, replies
        assert replies[0]["result"]["serverInfo"]["version"] == version, replies[0]
        names = {tool["name"] for tool in replies[1]["result"]["tools"]}
        assert names == {"peers", "send", "reply", "inbox", "status"}, names
    print("Installed CLI and MCP verified:", version)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--smoke", action="store_true")
    arguments = parser.parse_args()
    smoke_check() if arguments.smoke else metadata_check()
