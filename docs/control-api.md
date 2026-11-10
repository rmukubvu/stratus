# Control API

The control API manages the emulator itself: health, clean slates and
snapshots. It is meant for CI pipelines and coding agents that need fast,
repeatable environments. All endpoints live under `/_stratus/control/v1` on
the normal Stratus port and speak JSON.

| Method | Path | What it does |
| --- | --- | --- |
| GET | `/health` | Full health report. Always 200; read `status` and `ready`. |
| GET | `/ready` | 200 when ready for traffic, 503 otherwise. |
| POST | `/reset` | `{"services": ["s3", "sqs"]}` resets those services; an empty body resets everything. |
| GET | `/snapshots` | List snapshots, oldest first. |
| POST | `/snapshots` | `{"name": "baseline"}` captures all state. The name is optional. |
| GET | `/snapshots/{name}` | Describe a snapshot. |
| DELETE | `/snapshots/{name}` | Delete a snapshot. Live state is untouched. |
| POST | `/snapshots/{name}/restore` | Replace all live state with the snapshot. |

## Health

`status` is `ok`, `degraded` or `down`. The state store is critical: if it
fails, Stratus is `down` and not ready. Docker is not: without it Stratus is
`degraded` but still ready, because everything except Lambda execution works.

## Reset

A per-service reset drops that service's metadata and blobs and nothing
else. Services are reset independently, so resetting `dynamodb` leaves
`dynamodbstreams` alone; name both to reset both. Resetting `lambda`, or
everything, also stops warm Lambda containers so the next invocation starts
clean. Resetting `rds` or `elasticache`, or everything, also stops the
database and cache engines that backed the dropped instances; the response
lists those services under `engines_stopped`.

## Snapshots

A snapshot is a transactionally consistent copy of the metadata database
plus the blob tree, stored under `<data-dir>/snapshots/<name>`. Blob files
are never modified in place, so snapshots hard-link them: taking one is fast
and costs little disk even with large S3 objects or Lambda bundles. A
snapshot can be restored any number of times.

Names are 1-63 characters of lowercase letters, digits, `.`, `_` and `-`.

A snapshot also holds the data inside RDS instances and ElastiCache
clusters: each PostgreSQL, MySQL, MariaDB and Valkey engine is paused for a
moment while its data directory is copied into the snapshot. A restore
stops every engine and starts each one from that copy on the same port, so
endpoints stay stable and the rows and keys are back. The response lists
captured engines under `engines`, for example `["rds/orders-db",
"elasticache/sessions"]`. Memcached keeps no data, as on AWS, and comes back
empty. See [engines.md](engines.md#what-a-snapshot-captures).

## Consistency

Reset, snapshot and restore wait for in-flight AWS API requests to finish,
then run while new requests wait briefly. Nested requests (a Lambda calling
back into Stratus) are still admitted while a control operation waits, so
they cannot deadlock. If requests do not drain within 30 seconds the
operation fails with `Busy`. Asynchronous work started outside a request,
such as async Lambda invocations or event source polling, is not paused.

## SES inbox

```
GET    /_stratus/control/v1/ses/messages          emails sent through SES, oldest first
GET    /_stratus/control/v1/ses/messages/{id}     one email by MessageId
DELETE /_stratus/control/v1/ses/messages          empty the inbox; identities and templates stay
```

List filters: `to`, `from`, `subject` (substring), `since` (RFC 3339 or a
duration such as `5m`) and `limit` (newest N). See [ses.md](ses.md).

## Scheduler clock

```
GET  /_stratus/control/v1/scheduler/clock      current scheduler time and offset
POST /_stratus/control/v1/scheduler/advance    {"by": "1h"} or {"to": "<RFC 3339>"}; fires due schedules, once each
POST /_stratus/control/v1/scheduler/run        {"name": "nightly", "group": "default"}; fires one schedule now
```

The offset is part of the `scheduler` service's state: a reset returns the
clock to real time and a snapshot restores it. See
[scheduler.md](scheduler.md).

## Bedrock fixtures

```
GET    /_stratus/control/v1/bedrock                  mode, fixture directory and upstream
PUT    /_stratus/control/v1/bedrock/mode             {"mode": "record" | "replay"}; lasts until reset
GET    /_stratus/control/v1/bedrock/fixtures         every fixture in match order
POST   /_stratus/control/v1/bedrock/fixtures         add a fixture: {"model", "match": {"contains"}, "text"} or {"kind", "response"}
DELETE /_stratus/control/v1/bedrock/fixtures         remove fixtures added here; fixture files stay
DELETE /_stratus/control/v1/bedrock/fixtures/{id}    remove one
GET    /_stratus/control/v1/bedrock/invocations      model calls with prompt, answer and outcome, oldest first
DELETE /_stratus/control/v1/bedrock/invocations      empty the log
```

Invocation filters: `model` (pattern with `*`), `contains` (prompt
substring), `outcome` (`replayed`, `recorded`, `synthesized`, `missed`,
`failed`) and `limit`. See [bedrock.md](bedrock.md).

## MSK clusters

```
GET    /_stratus/control/v1/kafka/clusters           every cluster with its bootstrap brokers (host:port)
```

See [msk.md](msk.md).

## DNS

```
GET    /_stratus/control/v1/dns/resolve?name=api.corp.local&type=A
```

Resolves a name against the emulated Route 53 hosted zones, including the
zones and records Cloud Map creates. `type` defaults to `A`. The answer lists
CNAME and alias steps first, then the records of the requested type;
`found: false` means NXDOMAIN or no records of that type. See
[route53.md](route53.md) and [cloudmap.md](cloudmap.md).

## Errors

Every error has a stable code and a hint:

```json
{"error": {"code": "SnapshotNotFound",
           "message": "snapshot \"baseline\" does not exist",
           "hint": "list snapshots with GET /_stratus/control/v1/snapshots"}}
```

Codes: `UnknownService`, `InvalidSnapshotName`, `InvalidRequestBody`,
`SnapshotNotFound`, `SnapshotAlreadyExists`, `Busy`,
`UnknownControlEndpoint`, `InternalError`.

## CLI

The `stratus` binary wraps the API. Output is the JSON response; errors go
to stderr with a non-zero exit code. Use `--endpoint` or `STRATUS_ENDPOINT`
to target a non-default instance.

```sh
stratus serve &
stratus wait --timeout 30s

# seed once, then reuse the seeded state for every test
./seed.sh
stratus snapshot save seeded

for suite in orders payments; do
  stratus snapshot restore seeded
  go test ./tests/$suite/...
done

stratus reset sqs            # or reset everything with: stratus reset
```
