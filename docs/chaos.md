# Deterministic chaos

Stratus can inject faults into AWS API traffic: errors, throttling, latency,
dropped connections, Lambda function errors and cold starts. It can also
fault the events Stratus delivers internally (SQS-to-Lambda, SNS fan-out,
EventBridge targets, Step Functions tasks and more): lose them, delay them,
duplicate them or fail them. You need no service mesh, sidecar or code
changes, because every AWS call and every delivery already goes through
Stratus.

If you already write AWS Fault Injection Service templates, Stratus runs
them too; see [fis.md](fis.md).

Faults are **deterministic**. Whether the nth request that matches a rule
is faulted depends only on the experiment's seed, the rule and n. Run the
same test with the same seed and you get the same faults, so a failure
caused by chaos can be reproduced and debugged instead of dismissed as a
flake.

## Quick start

```json
{
  "seed": 1234,
  "duration_seconds": 300,
  "rules": [
    {"id": "throttle-orders", "service": "dynamodb", "operation": "PutItem",
     "resource": "orders", "action": "throttle", "probability": 0.2},
    {"id": "slow-s3", "service": "s3", "action": "latency",
     "latency": {"delay_ms": 400, "jitter_ms": 200}},
    {"id": "cold-checkout", "service": "lambda", "operation": "Invoke",
     "resource": "checkout", "action": "cold_start", "max_hits": 1}
  ]
}
```

```sh
stratus chaos start experiment.json   # or: PUT /_stratus/control/v1/chaos
go test ./...
stratus chaos status                  # GET    /_stratus/control/v1/chaos
stratus chaos stop                    # DELETE /_stratus/control/v1/chaos
```

Starting an experiment replaces any running one. One experiment runs at a
time.

## Experiment

| Field | Meaning |
| --- | --- |
| `seed` | Makes the run reproducible. If omitted, Stratus picks one and reports it so you can replay the run. |
| `duration_seconds` | Optional. Once it elapses the experiment stops injecting faults, and its status becomes `expired`. |
| `rules` | 1 to 64 rules, evaluated in order. |

## Rules

| Field | Meaning |
| --- | --- |
| `id` | Optional. Defaults to `rule-N`. Shown in reports and in the `X-Stratus-Fault` response header. |
| `scope` | Optional. `api` (the default) for AWS API requests, `event` for internal event deliveries, `all` for both. See [Event deliveries](#event-deliveries). |
| `service` | AWS service name (`s3`, `sqs`, `dynamodb`, `lambda`, ...), or `*` for every service. For an event delivery, the service receiving it. |
| `source` | Optional. For event deliveries, the service making the delivery (`sqs`, `sns`, `events`, ...). Setting it makes the scope default to `event`. |
| `operation` | Optional API operation, such as `PutItem` or `Invoke`. Case-insensitive. |
| `resource` | Optional substring matched against the request path, the S3 bucket and the Lambda function name, or for a delivery against its source and target ARNs. |
| `probability` | Optional, in (0, 1]. Defaults to 1. |
| `max_hits` | Optional cap on how many times the rule fires. Use it for "fail the first N calls, then recover". |
| `start_after_seconds` | Optional. The rule only becomes active this many seconds after the experiment starts. |
| `duration_seconds` | Optional. Once active, the rule stays active for this long; by default until the experiment ends. |
| `action` | One of the actions in the next table. |

| Action | Effect |
| --- | --- |
| `throttle` | The service's throttling error: `ThrottlingException` (400), or `SlowDown` (503) for S3. |
| `internal_error` | A 500 `InternalFailure` error, or `InternalError` for S3. |
| `unavailable` | A 503 `ServiceUnavailable` error. |
| `error` | A custom error, for example `{"status_code": 400, "code": "ProvisionedThroughputExceededException"}`. |
| `latency` | Delays the request by `delay_ms` plus deterministic jitter of up to `jitter_ms`, then handles it normally. The total is capped at 5 minutes. |
| `drop` | Closes the connection without a response. |
| `cold_start` | Lambda only. Discards the function's warm container, so the invocation pays a cold start. |
| `function_error` | Lambda `Invoke` only. The function is not run and the invocation fails the way a throwing function does: `200` with `X-Amz-Function-Error: Unhandled` and an `InjectedFunctionError` payload. |
| `duplicate` | Event scope only. Delivers the event twice, as at-least-once delivery can. |

Errors are shaped for the service's protocol (JSON, Query or S3 XML), so
AWS SDKs parse and retry them exactly as they would against AWS.

When rules overlap, latency and cold starts add up, and the first error or
drop wins. Rules after a terminal fault don't see the request.

## Event deliveries

Much of an event-driven system runs between API calls: a message sent to a
queue is handed to a Lambda function by an event source mapping, an SNS
publish fans out to subscribers, an EventBridge rule invokes its targets.
Stratus makes those deliveries itself, so rules with `scope: "event"` can
fault them. In AWS no fault injector can reach these paths.

```json
{
  "seed": 7,
  "rules": [
    {"id": "lose-audit", "source": "sns", "service": "sqs", "resource": "audit",
     "action": "drop", "probability": 0.1},
    {"id": "redeliver", "source": "sqs", "service": "lambda", "resource": "ship-order",
     "action": "duplicate", "max_hits": 3},
    {"id": "late-events", "source": "events", "service": "*",
     "action": "latency", "latency": {"delay_ms": 2000, "jitter_ms": 1000}},
    {"id": "flaky-charge", "source": "stepfunctions", "service": "lambda",
     "resource": "charge", "action": "function_error", "max_hits": 1}
  ]
}
```

A delivery is described by where it comes from and where it goes:

| Path | `source` | `service` | `operation` | Async |
| --- | --- | --- | --- | --- |
| SQS event source mapping | `sqs` | `lambda` | `Invoke` | yes |
| Kinesis event source mapping | `kinesis` | `lambda` | `Invoke` | yes |
| DynamoDB Streams event source mapping | `dynamodb` | `lambda` | `Invoke` | yes |
| SNS subscription | `sns` | `sqs` or `lambda` | `SendMessage` or `Invoke` | yes |
| EventBridge rule target | `events` | `lambda`, `sqs`, `sns` or `stepfunctions` | `Invoke`, `SendMessage`, `Publish` or `StartExecution` | yes |
| Step Functions Task state | `stepfunctions` | `lambda` | `Invoke` | no |
| Lambda destination (async invoke) | `lambda` | `sqs` or `sns` | `SendMessage` or `Publish` | yes |

`resource` matches the source ARN (queue, topic, stream, rule, state
machine or function) and the target ARN, so `"resource": "orders"` catches
deliveries from or to anything with `orders` in its ARN.

What each action does to a delivery depends on whether the source waits for
it. In AWS, SNS, EventBridge, event source mappings and destinations hand
the event off and move on (async); a Step Functions task waits for its
function (sync).

| Action | Async delivery | Sync delivery |
| --- | --- | --- |
| `drop` | The event is lost. The source carries on. | The target is skipped and the source sees success. |
| `duplicate` | The target receives the event twice. | The target is called twice; the second result counts. |
| `latency` | The event arrives late, in the background. Later events can overtake it, so this also tests ordering. | The source waits for the delay. |
| `throttle`, `internal_error`, `unavailable`, `error`, `function_error` | The event is not delivered. The source carries on (Stratus does not model AWS retries or dead-letter queues). | The source gets the error. For a Task state, its `Retry` and `Catch` apply. |
| `cold_start` | The target function starts cold. | Same. |

Use `scope: "all"` to fault a function however it is invoked, directly or
by an event: `{"scope": "all", "service": "lambda", "operation": "Invoke",
"resource": "checkout", "action": "function_error"}`.

## Phased experiments

`start_after_seconds` and `duration_seconds` give each rule a window, so one
experiment can run phases: a minute of latency, then a minute of errors.

```json
{
  "rules": [
    {"id": "slow", "service": "dynamodb", "action": "latency",
     "latency": {"delay_ms": 300}, "duration_seconds": 60},
    {"id": "throttled", "service": "dynamodb", "action": "throttle",
     "start_after_seconds": 60, "duration_seconds": 60}
  ]
}
```

A rule outside its window neither fires nor counts matches. Windows are
measured in wall-clock time, so they are less exact than the per-match
determinism of `probability` and `max_hits`.

## Diagnosing a failure

- Every faulted response carries `X-Stratus-Fault: <rule-id>[,<rule-id>]`.
- `stratus chaos status` reports, for each rule, how many requests it
  matched and how many it faulted. It also lists the latest 500 hits with
  scope, request ID, service, operation, path and match number. A delivery
  hit has scope `event`, its event ID (when there is one) as the request ID,
  the target ARN as the path, and its `source` and `source_arn`.
- Faults also appear in the operator portal's activity and error views,
  under the same request ID.

## Scope and limits

- Event-scope faults cover the delivery paths in the table above. API
  Gateway integrations are reached through the API Gateway request itself,
  which API-scope rules can fault.
- Stratus does not model AWS's retries for failed async deliveries, so a
  failed async delivery is simply not delivered.
- Health and readiness endpoints are never faulted, even by a `*` rule.
- Determinism is per rule and per match order. If concurrent requests
  reach the same rule in a different order, different requests may be
  faulted, but the number of faults and the decision for each match
  number stay the same.
