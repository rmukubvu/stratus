# Deterministic chaos

Stratus can inject faults into AWS API traffic: errors, throttling, latency,
dropped connections and Lambda cold starts. You need no service mesh,
sidecar or code changes, because every AWS call already goes through
Stratus.

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
| `service` | AWS service name (`s3`, `sqs`, `dynamodb`, `lambda`, ...), or `*` for every service. |
| `operation` | Optional API operation, such as `PutItem` or `Invoke`. Case-insensitive. |
| `resource` | Optional substring matched against the request path, the S3 bucket and the Lambda function name. |
| `probability` | Optional, in (0, 1]. Defaults to 1. |
| `max_hits` | Optional cap on how many times the rule fires. Use it for "fail the first N calls, then recover". |
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

Errors are shaped for the service's protocol (JSON, Query or S3 XML), so
AWS SDKs parse and retry them exactly as they would against AWS.

When rules overlap, latency and cold starts add up, and the first error or
drop wins. Rules after a terminal fault don't see the request.

## Diagnosing a failure

- Every faulted response carries `X-Stratus-Fault: <rule-id>[,<rule-id>]`.
- `stratus chaos status` reports, for each rule, how many requests it
  matched and how many it faulted. It also lists the latest 500 hits with
  request ID, service, operation, path and match number.
- Faults also appear in the operator portal's activity and error views,
  under the same request ID.

## Scope and limits

- Faults apply to AWS API requests. Work that Stratus does internally
  without an API request is not faulted. That includes SQS-to-Lambda event
  source mappings, SNS fan-out, EventBridge targets and DynamoDB streams.
- Health and readiness endpoints are never faulted, even by a `*` rule.
- Determinism is per rule and per match order. If concurrent requests
  reach the same rule in a different order, different requests may be
  faulted, but the number of faults and the decision for each match
  number stay the same.
