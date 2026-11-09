# EventBridge Scheduler with a fast-forward clock

Stratus emulates Amazon EventBridge Scheduler (`aws scheduler`): schedules,
schedule groups, `at()`, `rate()` and `cron()` expressions with time zones,
and delivery to SQS queues, SNS topics, Lambda functions and Step Functions
state machines. Its clock is yours to move. A test creates an hourly
schedule, advances the scheduler by an hour, and reads the message from the
queue, all in well under a second.

## What is emulated

- `CreateSchedule`, `GetSchedule`, `UpdateSchedule`, `DeleteSchedule`,
  `ListSchedules` (filters: group, name prefix, state).
- `CreateScheduleGroup`, `GetScheduleGroup`, `DeleteScheduleGroup` (removes
  its schedules), `ListScheduleGroups`, and tagging of groups. The `default`
  group exists implicitly and cannot be deleted.
- Expressions:
  - `at(2026-11-10T09:00:00)` runs once, in `ScheduleExpressionTimezone`.
  - `rate(5 minutes)`, `rate(1 hour)`, `rate(2 days)`. Without a start date
    the first run is one interval after creation; with one, it is the start
    date.
  - `cron(minutes hours day-of-month month day-of-week year)` with `*`, `?`,
    lists, ranges, steps, month and day names, and day-of-week `1`=SUN as in
    AWS. `L`, `W` and `#` are rejected with a clear message.
- `StartDate`, `EndDate`, `State` (`ENABLED`/`DISABLED`),
  `ActionAfterCompletion: DELETE` and `FlexibleTimeWindow` (accepted; Stratus
  fires at the scheduled time so tests stay deterministic).
- `Target.Input` with the context attributes
  `<aws.scheduler.schedule-arn>`, `<aws.scheduler.scheduled-time>`,
  `<aws.scheduler.execution-id>` and `<aws.scheduler.attempt-number>`. An
  empty input delivers `{}`.
- Deliveries run through the chaos gate like every other event path, so a
  rule with `source: scheduler` can delay, drop or fail them (see
  [chaos.md](chaos.md)).

Targets other than SQS, SNS, Lambda and Step Functions (universal targets,
Kinesis, EventBridge buses, ECS tasks) are rejected at creation with
`ValidationException` naming the supported kinds. `RoleArn` is accepted but
not checked.

## The clock

Schedules fire against the scheduler clock: real time plus an offset that
starts at zero. A ticker checks every 250 ms, so a `rate(1 minute)` schedule
also fires on its own if you wait. The control API moves the clock:

```
GET  /_stratus/control/v1/scheduler/clock     {"now": "...", "offset": "1h0m0s"}
POST /_stratus/control/v1/scheduler/advance   {"by": "1h"} or {"to": "2026-11-10T09:00:00Z"}
POST /_stratus/control/v1/scheduler/run       {"name": "nightly", "group": "default"}
```

`advance` fires every schedule that became due, once each, before it
returns, and reports what fired:

```json
{
  "now": "2026-11-09T13:00:00Z",
  "offset": "1h0m0s",
  "fired": [
    {
      "name": "nightly",
      "group": "default",
      "schedule_arn": "arn:aws:scheduler:us-east-1:000000000000:schedule/default/nightly",
      "target": "arn:aws:sqs:us-east-1:000000000000:jobs",
      "scheduled_time": "2026-11-09T13:00:00Z",
      "execution_id": "2f1c...",
      "input": "{\"job\":\"nightly\"}"
    }
  ]
}
```

Each schedule fires at most once per advance and its next run is planned
from the new time. Advancing a `rate(1 minute)` schedule by an hour yields
one delivery, not sixty; advance in smaller steps when you want each one.
A delivery that fails, because the queue does not exist or a chaos rule
failed it, is reported in `fired[].error` rather than lost.

`run` fires one schedule immediately, whatever its expression or state, and
returns the same invocation record. It does not touch the plan.

The offset is stored with the service's state: `stratus reset` (or resetting
`scheduler`) returns the clock to real time, and a snapshot restores it along
with the schedules.

```bash
aws sqs create-queue --queue-name jobs
aws scheduler create-schedule --name nightly --schedule-expression 'rate(1 hour)' \
  --flexible-time-window Mode=OFF \
  --target '{"Arn":"arn:aws:sqs:us-east-1:000000000000:jobs","RoleArn":"arn:aws:iam::000000000000:role/x","Input":"{\"job\":\"nightly\"}"}'

curl -s -X POST localhost:4566/_stratus/control/v1/scheduler/advance -d '{"by":"1h"}'
aws sqs receive-message --queue-url http://localhost:4566/000000000000/jobs
```

From the SDKs:

```ts
const { fired } = await stratus.scheduler.advance("1h");
expect(fired.map((f) => f.name)).toEqual(["nightly"]);
```

```python
fired = stratus.scheduler.advance("1h")["fired"]
assert [f["name"] for f in fired] == ["nightly"]
```

The MCP server exposes the clock as `stratus_scheduler_advance` and
`stratus_scheduler_run`.

## Not emulated

Flexible windows (runs happen at the scheduled time), retries and dead-letter
queues (a failed delivery is reported once), KMS encryption, universal
targets and pagination tokens on list calls.
