# Agent Demo: Find a Bug, Fix It, Prove It Under Chaos

This is the loop a coding agent runs with Stratus over MCP. `app/` is a tiny
order service with a bug: it writes to a table called `orders`, but setup
creates `Orders`.

`replay.py` drives the real `stratus mcp` server step by step, making the
decisions an agent would make, so the run is deterministic. Every tool call
and response is real, and the script fails if any step stops behaving as
described. CI runs it on every push.

```sh
pip install boto3
go build -o stratus ./cmd/stratus
STRATUS_BIN=./stratus python examples/agent-demo/replay.py
```

## What happens

1. **Seed once and snapshot.** The agent runs the setup script, then calls
   `stratus_snapshot_save`, so every attempt starts from the same state.
2. **The test fails** with `ResourceNotFoundException` from deep inside
   botocore.
3. **The agent asks Stratus why** instead of guessing from the stack trace.
   `stratus_diagnose` answers:
   > 1 failed request in 1 group(s): 1 missing resource. Start with dynamodb
   > PutItem (ResourceNotFoundException) ...
   > next step: create it in your setup step before use, and check the name,
   > region and account in the request; list what exists with
   > `aws dynamodb list-tables` · your_bug: true

   Following that next step shows `['Orders']`.
4. **Fix:** `TABLE_NAME = "orders"` becomes `"Orders"`.
5. **Restore and rerun:** `stratus_snapshot_restore`, and the test passes.
6. **Prove it under chaos.** `stratus_chaos_start` throttles 30% of `PutItem`
   calls with seed 42. The test still passes, because the SDK retries.
   Diagnostics separates the two kinds of failure:
   ```
   chaos injected 11 throttles (seed 42)
   real failures: 0  |  injected and retried: 11
   ```
   Every injected fault is marked `your_bug: false`, so the agent doesn't
   "fix" working code. Reusing seed 42 replays the same faults.
7. **Clean up:** restore the snapshot so the environment is as it was found.

## With a real agent

```sh
claude mcp add stratus -- stratus mcp --start
```

Then ask something like: *"Run examples/agent-demo/app/check_orders.py
against Stratus. If it fails, use stratus_diagnose to find out why, fix it,
and prove the fix holds with 30% throttling on PutItem. Snapshot first and
leave the environment clean."* The agent has the same tools the replay uses.
