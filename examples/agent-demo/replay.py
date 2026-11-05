#!/usr/bin/env python3
"""Replays the loop a coding agent runs with Stratus over MCP, step by step.

No LLM is involved: the decisions an agent would make are scripted, so the
run is deterministic and CI can check every claim the README makes. The
tool calls, their arguments and the responses are real.

    python replay.py            # needs `stratus` on PATH and boto3 installed
    STRATUS_BIN=./stratus python replay.py
"""

from __future__ import annotations

import difflib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent


class MCPClient:
    """Minimal MCP client over stdio: newline-delimited JSON-RPC 2.0."""

    def __init__(self, command: list[str]) -> None:
        self.proc = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
        self.next_id = 0

    def request(self, method: str, params: dict[str, Any] | None = None) -> Any:
        self.next_id += 1
        msg = {"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params or {}}
        assert self.proc.stdin and self.proc.stdout
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()
        line = self.proc.stdout.readline()
        if not line:
            raise RuntimeError("MCP server exited")
        reply = json.loads(line)
        if "error" in reply:
            raise RuntimeError(f"{method}: {reply['error']}")
        return reply["result"]

    def notify(self, method: str) -> None:
        assert self.proc.stdin
        self.proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": method}) + "\n")
        self.proc.stdin.flush()

    def call(self, tool: str, **args: Any) -> Any:
        """Calls a tool and returns its decoded JSON result."""
        say("tool", f"{tool}({json.dumps(args) if args else ''})")
        result = self.request("tools/call", {"name": tool, "arguments": args})
        text = result["content"][0]["text"]
        if result.get("isError"):
            raise RuntimeError(f"{tool} failed: {text}")
        try:
            return json.loads(text)
        except ValueError:
            return text

    def close(self) -> None:
        assert self.proc.stdin
        self.proc.stdin.close()
        self.proc.wait(timeout=30)


def say(who: str, text: str) -> None:
    prefix = {"agent": "agent >", "tool": "  tool >", "out": "       "}[who]
    for i, line in enumerate(text.splitlines() or [""]):
        print(f"{prefix if i == 0 else ' ' * len(prefix)} {line}")
    sys.stdout.flush()


def expect(cond: bool, what: str) -> None:
    if not cond:
        raise SystemExit(f"replay check failed: {what}")


def run_app(workdir: Path, env: dict[str, str], script: str) -> subprocess.CompletedProcess[str]:
    say("agent", f"Running `python {script}`.")
    res = subprocess.run([sys.executable, script], cwd=workdir, env=env, capture_output=True, text=True, timeout=120)
    tail = (res.stdout + res.stderr).strip().splitlines()[-3:]
    say("out", "\n".join(tail) or f"exit {res.returncode}")
    return res


def main() -> int:
    stratus = os.environ.get("STRATUS_BIN") or shutil.which("stratus")
    expect(bool(stratus), "stratus is not on PATH; set STRATUS_BIN")

    workdir = Path(tempfile.mkdtemp(prefix="stratus-agent-demo-"))
    shutil.copytree(HERE / "app", workdir, dirs_exist_ok=True)
    mcp = MCPClient([str(stratus), "mcp", "--start"])
    try:
        info = mcp.request("initialize", {
            "protocolVersion": "2025-06-18", "capabilities": {},
            "clientInfo": {"name": "replay", "version": "1"},
        })
        mcp.notify("notifications/initialized")
        tools = mcp.request("tools/list")["tools"]
        say("agent", f"Connected to {info['serverInfo']['name']} {info['serverInfo']['version']}: {len(tools)} tools.")

        conn = mcp.call("stratus_connection_info")
        env = {**os.environ, **conn["env"]}
        say("out", f"endpoint {conn['endpoint']}")

        print("\n--- 1. Seed once, then snapshot so every attempt starts clean")
        expect(run_app(workdir, env, "setup_fixture.py").returncode == 0, "setup failed")
        mcp.call("stratus_snapshot_save", name="seeded")

        print("\n--- 2. Run the test; it fails")
        first = run_app(workdir, env, "check_orders.py")
        expect(first.returncode != 0, "the buggy app should fail")

        print("\n--- 3. Ask Stratus why, instead of guessing from a stack trace")
        report = mcp.call("stratus_diagnose", since="5m")
        group = report["groups"][0]
        say("out", f"{report['summary']}\nnext step: {group['next_step']}\nyour_bug: {group['your_bug']}")
        expect(group["category"] == "missing_resource" and group["your_bug"], f"unexpected diagnosis {group}")

        say("agent", "Following the next step: list the tables that do exist.")
        listing = subprocess.run(
            [sys.executable, "-c", "import boto3; print(boto3.client('dynamodb').list_tables()['TableNames'])"],
            env=env, capture_output=True, text=True, timeout=60, check=True,
        )
        say("out", listing.stdout.strip())
        expect("'Orders'" in listing.stdout, "setup should have created Orders")

        print("\n--- 4. Fix the code: the table is 'Orders', not 'orders'")
        src = workdir / "orders.py"
        before = src.read_text()
        after = before.replace('TABLE_NAME = "orders"', 'TABLE_NAME = "Orders"')
        src.write_text(after)
        diff = difflib.unified_diff(before.splitlines(), after.splitlines(), "a/orders.py", "b/orders.py", lineterm="", n=0)
        say("out", "\n".join(diff))

        print("\n--- 5. Roll back to the seeded state and run again")
        mcp.call("stratus_snapshot_restore", name="seeded")
        expect(run_app(workdir, env, "check_orders.py").returncode == 0, "the fixed app should pass")

        print("\n--- 6. Prove it survives throttling: seeded chaos on every PutItem")
        mcp.call("stratus_snapshot_restore", name="seeded")
        chaos_began = datetime.now(timezone.utc).isoformat()
        chaos = {"seed": 42, "rules": [{"id": "throttle-puts", "service": "dynamodb", "operation": "PutItem", "action": "throttle", "probability": 0.3}]}
        mcp.call("stratus_chaos_start", **chaos)
        under_chaos = run_app(workdir, env, "check_orders.py")
        stopped = mcp.call("stratus_chaos_stop")
        say("out", f"chaos injected {stopped['total_injected']} throttles (seed {stopped['seed']})")
        expect(under_chaos.returncode == 0, "the app should absorb throttling through SDK retries")
        expect(stopped["total_injected"] > 0, "chaos should have injected faults")

        real = mcp.call("stratus_diagnose", since=chaos_began, exclude_injected=True)
        everything = mcp.call("stratus_diagnose", since=chaos_began)
        injected = [g for g in everything["groups"] if g["category"] == "injected_fault"]
        say("out", f"real failures: {real['failures']}  |  injected and retried: {sum(g['count'] for g in injected)}")
        expect(real["healthy"], f"no real failures expected, got {real['summary']}")
        expect(injected and not injected[0]["your_bug"], "injected faults should not be blamed on the code")

        print("\n--- 7. Leave the environment as we found it")
        mcp.call("stratus_snapshot_restore", name="seeded")
        say("agent", "Done: the bug is fixed, the fix holds under seeded throttling, and the state is clean.")
        return 0
    finally:
        mcp.close()
        shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
