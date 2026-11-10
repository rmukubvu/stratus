# Stratus for AI Coding Agents

Stratus gives coding agents (Claude Code, Cursor, Codex and any other MCP
client) a local AWS they can drive safely: check it is up, save and roll back
state, inject faults, and ask *why* a call failed in plain language.

## MCP server

`stratus mcp` speaks the Model Context Protocol over stdio (JSON-RPC 2.0, one
message per line). It is a thin client of the [control API](control-api.md).

```sh
# Let the agent start its own throwaway Stratus (temporary data dir, removed on exit)
claude mcp add stratus -- stratus mcp --start

# Or attach to a Stratus you already run
claude mcp add stratus -- stratus mcp --endpoint http://localhost:4566
```

For other clients, use the same command in their MCP config:

```json
{
  "mcpServers": {
    "stratus": { "command": "stratus", "args": ["mcp", "--start"] }
  }
}
```

| Flag | Meaning |
| --- | --- |
| `--endpoint` | Stratus to attach to (default `$STRATUS_ENDPOINT`, then `http://localhost:4566`) |
| `--start` | Start an in-process Stratus on a free port instead of attaching |
| `--data-dir` | With `--start`, keep data here instead of a temporary directory |
| `--seed` | With `--start`, seed from a snapshot directory |

Logs go to stderr; stdout carries only protocol messages.

### Tools

| Tool | What it does |
| --- | --- |
| `stratus_connection_info` | Endpoint, region and the env vars to point an SDK or the AWS CLI at Stratus |
| `stratus_status` | Health and readiness |
| `stratus_diagnose` | Grouped recent failures with likely cause and next step |
| `stratus_reset` | Wipe all state (or one service) |
| `stratus_snapshot_save` / `_restore` / `_list` / `_delete` | Save and roll back state |
| `stratus_chaos_start` / `_status` / `_stop` | Seeded fault injection (see [chaos.md](chaos.md)) |
| `stratus_ses_inbox` | Emails the application sent through SES, with filters (see [ses.md](ses.md)) |
| `stratus_scheduler_advance` / `_run` | Fast-forward the EventBridge Scheduler clock, or fire one schedule now (see [scheduler.md](scheduler.md)) |
| `stratus_bedrock_fixture_add` | Make Bedrock answer matching model calls with a fixed reply (see [bedrock.md](bedrock.md)) |
| `stratus_bedrock_invocations` | The prompts the application sent to Bedrock, the replies, and which calls had no fixture |
| `stratus_kafka_clusters` | MSK clusters with the bootstrap broker address of each (see [msk.md](msk.md)) |

A failing tool returns `isError: true` with the control API's error code and a
hint, so the agent can correct itself instead of guessing.

A typical agent loop: `stratus_snapshot_save` before an experiment, run the
code, `stratus_diagnose` if anything failed, `stratus_snapshot_restore` to get
back to a known state.

## SDKs

For test code, the [TypeScript](../sdk/typescript/README.md) and
[Python](../sdk/python/README.md) SDKs wrap the same API with typed errors and
helpers that always clean up: `withSnapshot` / `snapshot_scope` roll state
back after a block, and `withChaos` / `chaos_experiment` always stop the
experiment. The Python package also registers `stratus` and
`stratus_isolated` pytest fixtures. Neither has runtime dependencies.

```sh
npm install --save-dev stratus-emulator
pip install stratus-emulator
```

## Diagnostics

```
GET /_stratus/control/v1/diagnostics?since=5m&service=sqs&exclude_injected=true&max_groups=10
stratus diagnose --since 5m --service sqs --exclude-injected
```

Recent failed requests are grouped by service, operation and error code. Each
group says:

- `category`: `missing_resource`, `conflict`, `invalid_request`, `throttling`,
  `auth`, `connection`, `unsupported_operation`, `emulator_error`,
  `injected_fault` or `client_error`.
- `likely_cause` and `next_step`: one sentence each, written for an agent.
- `your_bug`: `false` when the failure is not the caller's fault (a chaos
  fault, an operation Stratus does not implement, or an emulator error).
- `examples`: up to three request IDs and resources to look at.

Failures caused by an active or just-stopped chaos experiment are attributed
to the rule that injected them, so an agent does not "fix" code that was
working. Latency and cold-start faults are never blamed, because the request
that followed them may have failed for its own reasons.

Real problems are listed before injected faults, most recent first, and the
`summary` field gives a one-paragraph answer with where to start.

| Parameter | Meaning |
| --- | --- |
| `since` | A duration (`5m`) or RFC 3339 time; default all retained failures |
| `service` | Only this service |
| `exclude_injected` | Leave out chaos faults |
| `max_groups` | 1 to 100, default 20 |
