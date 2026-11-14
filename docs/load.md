# Load runs

A load run is a recording window around a load test. While it records,
Stratus counts every AWS request per second and per operation: how many
succeeded, were throttled, failed on purpose because of chaos, or failed
for any other reason, and how long they took. A run can also apply a
[limits profile](limits.md) and start a [chaos experiment](chaos.md) for
its duration, so one command sets up the whole test and tears it down.

Stratus does not generate load. Use k6, Gatling, Locust or anything else;
Stratus plays AWS at its limits and tells you what happened.

## Quick start

```sh
stratus serve &
stratus load run --name checkout --limits strict --chaos chaos.json -- k6 run load.js
```

`stratus load run` starts a run, runs the command, stops the run (even if
the command fails), prints a report and exits with the command's exit
code. The report shows the totals, latency percentiles, the busiest
operations and what the limits and chaos did; `--json` prints the full
summary, per-second timeline included, instead. The command gets `STRATUS_RUN_ID` and `STRATUS_ENDPOINT` in its
environment. [examples/k6-load](../examples/k6-load) is a complete example.

To drive the run yourself instead:

```sh
stratus load start --name checkout --limits aws-default
k6 run load.js
stratus load stop                 # undoes the limits and chaos, prints the report
```

`stratus load status [run-id]` shows the current run (or `latest`, or a
past run by ID) while it records, and `stratus load list` lists recent runs.

## What a run records

| Field | Meaning |
| --- | --- |
| `totals` | Requests by outcome for the whole run, mean `per_second`, `throttle_rate` and `error_rate`, and latency percentiles. |
| `timeline` | One entry per second (`t` is seconds since the start), with empty seconds filled in, so a stall shows as zeros. Capped at an hour; totals keep counting after that. |
| `operations` | The 50 busiest service operations, each with the same counts. |
| `limits` | The limits profile's status: live while the run records, final once it stops. |
| `chaos` | The chaos experiment's report, likewise. |

Each request lands in exactly one outcome:

- `ok`: an HTTP status below 400.
- `throttled`: rejected by a limits rule, or answered with an AWS throttling
  code (`ThrottlingException`, `SlowDown`, `ProvisionedThroughputExceededException`,
  `TooManyRequestsException` and the like), whether a limits profile or a
  chaos rule caused it.
- `faults`: failed by any other chaos action, including dropped connections.
- `errors`: everything else. These are the ones to look at first, since the
  test did not plan for them; `stratus diagnose` explains them.

Latency percentiles come from a fixed histogram whose buckets grow by 25%,
so a percentile is at most a quarter above the true value. Recording costs
no allocation, and memory stays flat however long a run lasts.

One run records at a time; starting another answers `RunInProgress` (409).
The last 20 finished runs are kept in memory until Stratus restarts.

## Control API

```
POST /_stratus/control/v1/runs              start a run: {"name", "limits", "chaos"}, all optional
GET  /_stratus/control/v1/runs              list runs, newest first
GET  /_stratus/control/v1/runs/{id}         a run's summary and timeline; id may be current or latest
POST /_stratus/control/v1/runs/{id}/stop    stop a run, undo its limits and chaos, return the summary
```

`limits` takes a [limits profile](limits.md#profiles) and `chaos` a
[chaos experiment](chaos.md). A run only clears what it set up: if `limits`
was not given, a profile applied separately stays active.

## SDKs and agents

```ts
const { summary } = await stratus.runs.during({ name: "checkout", limits: { profile: "strict" } }, () => runLoad());
console.log(summary.totals.throttle_rate);
```

```python
with stratus.runs.recording({"name": "checkout", "limits": {"profile": "strict"}}) as rec:
    run_load()
print(rec.summary["totals"]["throttle_rate"])
```

Both SDKs also have `runs.start()`, `runs.status()`, `runs.stop()` and
`runs.list()`. Agents use the `stratus_load_start`, `stratus_load_status`
and `stratus_load_stop` MCP tools.
