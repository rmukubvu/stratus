# The emulator clock

Every Stratus service reads time from one clock. It runs with real time
until a test moves it, so a test can cross a wait in one call instead of
sleeping through it:

- an SQS visibility timeout or message delay;
- DynamoDB TTL expiry;
- a CloudWatch alarm's evaluation periods;
- an S3 Object Lock retention date;
- an EventBridge schedule's next run;
- and every timestamp a service records (creation dates, `LastModified`,
  event times, log ingestion times).

```
GET  /_stratus/control/v1/clock           {"now", "real_now", "offset", "frozen"}
POST /_stratus/control/v1/clock/advance   {"by": "30m"} or {"to": "2026-11-23T09:00:00Z"}
POST /_stratus/control/v1/clock/freeze    hold the clock where it stands
POST /_stratus/control/v1/clock/resume    let it run again from where it was held
```

## Advance instead of sleeping

```bash
aws sqs create-queue --queue-name orders
aws sqs send-message --queue-url "$QUEUE" --message-body order-1
aws sqs receive-message --queue-url "$QUEUE" --visibility-timeout 300   # takes it
stratus clock advance 5m1s                                              # no sleep
aws sqs receive-message --queue-url "$QUEUE"                            # it is back
```

`advance` also fires every EventBridge schedule that became due, once each,
in every account and region, before it returns. What a move set off is
listed under `effects`:

```json
{
  "now": "2026-11-22T11:01:00Z",
  "real_now": "2026-11-22T10:00:00Z",
  "offset": "1h1m0s",
  "frozen": false,
  "effects": [
    { "source": "scheduler", "count": 1, "detail": [ { "name": "hourly-report", "target": "arn:aws:sqs:...:reports" } ] }
  ]
}
```

The clock only moves forward. A `by` that is not positive, or a `to` that is
not later than the clock's current time, is refused with `ClockNotForward`.

## Freeze for predictable timestamps

`freeze` holds the clock still: every timestamp Stratus records is the same
until you advance or resume it, which makes snapshot-style assertions on
responses stable. Advancing a frozen clock moves it and keeps it frozen.
`resume` lets it run again from where it was held, so no time passes on the
clock while it was frozen.

## Reset and snapshots

The clock's setting lives in the metadata store, outside any one service. A
full `stratus reset` returns it to real time; resetting a single service
leaves it alone. A snapshot captures it and a restore puts it back, so a
seeded snapshot taken at a fixed moment starts every job at that moment.

## What keeps real time

- **Credentials and signatures.** SDKs sign requests with the machine's
  clock, so IAM access keys, STS sessions and SigV4 checks use real time.
- **Real engines.** Lambda runtime containers, PostgreSQL, MySQL, Valkey and
  Redpanda keep their own system time. A function that reads the time sees
  real time; a database `now()` is the database's.
- **Waiting.** SQS long polling and Lambda retry backoff wait in real time.

The older scheduler-only clock (`/scheduler/advance`, see
[scheduler.md](scheduler.md)) still works and runs on top of this one: the
scheduler's time is the emulator clock plus its own offset.

## From the CLI, MCP and SDKs

```bash
stratus clock                     # status
stratus clock advance 36h         # or an RFC 3339 time
stratus clock freeze
stratus clock resume
```

MCP tools: `stratus_clock_status`, `stratus_clock_advance`,
`stratus_clock_freeze`, `stratus_clock_resume`.

```ts
await stratus.clock.advance("5m");          // TypeScript
```

```python
stratus.clock.advance("5m")                 # Python
```
