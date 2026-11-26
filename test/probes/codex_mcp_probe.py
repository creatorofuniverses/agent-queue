#!/usr/bin/env python3
"""Probe installed Codex MCP identity without model turns or live sessions.

Run after just build. All Codex state is temporary under ignored bin/scratch.
"""
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import tempfile
import threading
import time

repo = Path(__file__).resolve().parents[2]
scratch = repo / "bin/scratch"
scratch.mkdir(parents=True, exist_ok=True)
aq = str(repo / "bin/aq")


def served_tools(env):
    """The tool names aq mcp lists over stdio: what Codex has to see as well."""
    process = subprocess.Popen([aq, "mcp"], env=env, text=True, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    try:
        def request(number, method, params):
            process.stdin.write(json.dumps(dict(jsonrpc="2.0", id=number, method=method, params=params)) + "\n")
            process.stdin.flush()
            for line in process.stdout:
                try:
                    response = json.loads(line)
                except ValueError:
                    continue
                # An error has id null when aq could not name the request: fail at
                # once rather than wait for a reply that does not come.
                if "error" in response:
                    raise RuntimeError(response["error"])
                if response.get("id") == number:
                    return response["result"]
            raise RuntimeError("aq mcp closed its output")
        request(1, "initialize", {"protocolVersion": "2025-06-18", "clientInfo": {"name": "aq_probe", "version": "1"},
                                  "capabilities": {}})
        process.stdin.write(json.dumps(dict(jsonrpc="2.0", method="notifications/initialized")) + "\n")
        process.stdin.flush()
        return sorted(tool["name"] for tool in request(2, "tools/list", {})["tools"])
    finally:
        process.stdin.close()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


for forward in (False, True):
    with tempfile.TemporaryDirectory(prefix="codex-mcp-", dir=scratch) as tmp:
        root = Path(tmp)
        home = root / "home"
        home.mkdir()
        identity_log = root / "identity.json"
        wrapper = root / "logger.py"
        wrapper.write_text(
            "import os,json\nfrom pathlib import Path\n"
            f"Path({str(identity_log)!r}).write_text(json.dumps({{k:os.environ.get(k) "
            'for k in ["CODEX_THREAD_ID","CLAUDE_CODE_SESSION_ID"]}))\n'
            f"os.execv({aq!r},[{aq!r},'mcp'])\n"
        )
        values = {"AQ_CONFIG": str(root / "aq.toml"),
                  "AQ_STATE_DIR": str(root / "state"),
                  "AQ_RUNTIME_DIR": str(root / "run")}
        if forward:
            values["CODEX_THREAD_ID"] = "aq-explicit-thread"
        (home / "config.toml").write_text(
            '[mcp_servers.aq_probe]\ncommand="/usr/bin/python3"\n'
            f'args=[{json.dumps(str(wrapper))}]\n[mcp_servers.aq_probe.env]\n'
            + "".join(k + "=" + json.dumps(v) + "\n" for k, v in values.items())
        )
        env = os.environ.copy()
        env.update(CODEX_HOME=str(home), CODEX_THREAD_ID="aq-probe-parent",
                   CLAUDE_CODE_SESSION_ID="aq-probe-claude")
        # Codex must see the tools aq mcp serves, compared with aq mcp's own list
        # rather than a count that a new tool would leave behind.
        expected = served_tools({**os.environ, **values})
        messages = queue.Queue()
        with (root / "stderr.log").open("w") as stderr:
            process = subprocess.Popen(
                ["codex", "app-server", "--stdio"], env=env, text=True,
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr,
                start_new_session=True,
            )
            def read_messages():
                for line in process.stdout:
                    try:
                        messages.put(json.loads(line))
                    except ValueError:
                        pass
            reader = threading.Thread(target=read_messages, daemon=True)
            reader.start()
            def rpc(number, method, params):
                process.stdin.write(json.dumps(dict(id=number, method=method, params=params)) + "\n")
                process.stdin.flush()
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    try:
                        response = messages.get(timeout=.5)
                    except queue.Empty:
                        if process.poll() is not None:
                            raise RuntimeError("app-server exited; see " + str(root))
                        continue
                    if response.get("id") == number:
                        if "error" in response:
                            raise RuntimeError(response["error"])
                        return response["result"]
                raise TimeoutError(method)
            try:
                rpc(1, "initialize", {"clientInfo": {"name": "aq_probe", "version": "1"},
                                      "capabilities": {"experimentalApi": True}})
                process.stdin.write('{"method":"initialized"}\n')
                process.stdin.flush()
                rpc(2, "thread/start", {"cwd": str(root), "ephemeral": True,
                                       "approvalPolicy": "never", "sandbox": "read-only"})
                status = rpc(3, "mcpServerStatus/list", {})["data"]
                assert len(status) == 1, [s["name"] for s in status]
                tools = sorted(status[0]["tools"])
                assert tools == expected, (tools, expected)
                identity = json.loads(identity_log.read_text())
                expected_thread = "aq-explicit-thread" if forward else None
                assert identity["CODEX_THREAD_ID"] == expected_thread, identity
                assert identity["CLAUDE_CODE_SESSION_ID"] is None, identity
                print(json.dumps({"explicit_forwarding": forward, "tools": len(tools), **identity}))
            finally:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                reader.join(timeout=2)
                process.stdin.close()
                process.stdout.close()
