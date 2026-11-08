# Stratus Python SDK

Control a local [Stratus](https://github.com/rmukubvu/stratus) from tests and agents: wait for it,
reset it, snapshot and roll back state, run seeded chaos, and ask why calls
failed. Standard library only; Python 3.11+.

```sh
pip install stratus-emulator
```

```python
import boto3
from stratus_emulator import StratusClient, StratusError

stratus = StratusClient()  # $STRATUS_ENDPOINT or http://localhost:4566
stratus.wait_until_ready()

s3 = boto3.client("s3", **stratus.boto3_kwargs())

# Everything created inside is rolled back afterwards, even on failure.
with stratus.snapshot_scope():
    run_my_test(s3)

# Fail the first PutItem, then recover; the experiment always stops.
spec = {"seed": 42, "rules": [{"service": "dynamodb", "operation": "PutItem", "action": "throttle", "max_hits": 1}]}
with stratus.chaos_experiment(spec) as run:
    run_my_test(s3)
print(run.report["total_injected"], run.report["recent_hits"])

# Why did things fail? Groups, likely cause, next step, and whether chaos did it.
print(stratus.diagnose(since="5m", exclude_injected=True)["summary"])
```

| Method | Control API |
| --- | --- |
| `health()`, `is_ready()`, `wait_until_ready()` | `GET /health`, `GET /ready` |
| `reset(*services)` | `POST /reset` |
| `snapshots.create/list/get/restore/delete` | `/snapshots` |
| `chaos.start/status/stop` | `/chaos` |
| `diagnose(...)` | `GET /diagnostics` |
| `snapshot_scope()`, `chaos_experiment(spec)`, `boto3_kwargs()`, `aws_env()` | helpers |

Every failure is a `StratusError` with a stable `code` (such as
`SnapshotNotFound`), a `hint` and the HTTP `status` (0 when Stratus could not
be reached). Responses are plain dicts, typed with `TypedDict`s in
`stratus_emulator.types`.

## pytest

Installing the package registers two fixtures:

- `stratus`: a session-wide client, waited until ready.
- `stratus_isolated`: the same client, with all state rolled back after the test.

```python
def test_upload(stratus_isolated):
    s3 = boto3.client("s3", **stratus_isolated.boto3_kwargs())
    s3.create_bucket(Bucket="uploads")  # gone before the next test
```

## Development

```sh
pip install -e . pytest && pytest tests
STRATUS_ENDPOINT=http://localhost:4566 pytest tests   # also runs the integration tests
```
