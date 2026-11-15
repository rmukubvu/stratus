# AWS limits

Stratus can enforce AWS service quotas, so an application under load hits
its limits locally, in the same order it would in AWS, and gets the same
errors back: `SlowDown` from S3, `ThrottlingException` from JSON services,
`Throttling` from query services, `TooManyRequestsException` from Lambda.
AWS SDKs treat every one of these as a throttle and retry with backoff, so
you see whether the application's retries, batching and caching hold up.

Limits are off by default, so existing test suites are not affected. They
are part of load and limits mode: run a load generator such as k6 against
the application while Stratus plays AWS at its limits.

Limits that are part of an API's contract, such as ten entries per SQS
batch, are always enforced and are not covered here; see [sqs.md](sqs.md).

## Quick start

```sh
stratus serve --limits aws-default        # or STRATUS_LIMITS=aws-default
stratus limits set strict                  # change it on a running instance
stratus limits set ./limits.json
stratus limits status                      # per-rule counts and recent throttles
stratus limits clear                       # turn limits off
```

`stratus ci --limits strict -- go test ./...` applies a profile to a CI run,
and `stratus load run --limits strict -- k6 run load.js` to a
[load run](load.md), which also records what the profile did.
Over the control API it is `PUT`, `GET` and `DELETE` on
`/_stratus/control/v1/limits`; an empty `PUT` applies `aws-default`. Agents
get the same through the `stratus_limits_set`, `stratus_limits_status` and
`stratus_limits_clear` MCP tools, and the SDKs through `limits.set()`,
`limits.status()` and `limits.clear()`.

## Profiles

```json
{
  "profile": "aws-default",
  "scale": 0.02,
  "rates": [
    {"id": "ssm-get-parameters", "service": "ssm",
     "operations": ["GetParameter", "GetParameters", "GetParametersByPath"], "per_second": 1000},
    {"id": "kms-cryptographic-operations", "disabled": true},
    {"id": "orders-queue", "service": "sqs", "operations": ["SendMessage"], "per_second": 300}
  ]
}
```

| Field | Meaning |
| --- | --- |
| `profile` | The base: `aws-default` (when omitted), `strict`, or `none` for only your own rules. |
| `scale` | Multiplies every rate. Defaults to 1. |
| `rates` | Rules to add. A rule with the `id` of a base rule replaces it; `"disabled": true` turns a base rule off. |

### Rate rules

| Field | Meaning |
| --- | --- |
| `id` | Shown in errors, the `X-Stratus-Limit` header, `stratus limits status` and diagnostics. Defaults to `rate-N`. |
| `service` | AWS service name, e.g. `ssm`, `s3`, `kms`. |
| `operations` | Operations that share the rule's bucket. Empty means every operation of the service. |
| `per_second` | Sustained rate, before scaling. |
| `region_per_second` | Optional rates for named regions, e.g. `{"us-east-1": 100000}`, overriding `per_second` there. |
| `burst_seconds` | How many seconds of `per_second` may arrive at once. Defaults to 1. |
| `per` | What shares one bucket: `account` (default; one per account and region), `resource` (one per S3 bucket or Lambda function) or `prefix` (one per S3 key prefix). |

Each rule is a token bucket that starts full. A request that would exceed
any rule covering it is throttled and consumes nothing, as in AWS. Accounts
and regions always have separate buckets.

### Base profiles

`aws-default` holds the default quotas AWS documents for the APIs most
often called on a hot path (checked against the AWS documentation in
November 2026):

| Rule | Service | Operations | Rate | Per |
| --- | --- | --- | --- | --- |
| `s3-reads-per-prefix` | s3 | GetObject, HeadObject | 5,500/s | prefix |
| `s3-writes-per-prefix` | s3 | PutObject, CopyObject, DeleteObject(s), multipart create and complete | 3,500/s | prefix |
| `ssm-get-parameters` | ssm | GetParameter, GetParameters, GetParametersByPath | 40/s | account |
| `kms-cryptographic-operations` | kms | Encrypt, Decrypt, ReEncrypt, GenerateDataKey(WithoutPlaintext), GenerateRandom, GenerateMac, VerifyMac on symmetric keys | 10,000/s; 20,000/s in us-east-2, ap-southeast-1, ap-southeast-2, ap-northeast-1, eu-central-1, eu-west-2 and eu-south-2; 100,000/s in us-east-1, us-west-2 and eu-west-1 | account |
| `secretsmanager-get-secret-value` | secretsmanager | GetSecretValue | 10,000/s | account |
| `secretsmanager-describe-secret` | secretsmanager | DescribeSecret | 40,000/s | account |
| `secretsmanager-batch-get-secret-value` | secretsmanager | BatchGetSecretValue | 100/s | account |

DynamoDB partitions serve 3,000 read and 1,000 write units a second, tables
bank 300 seconds of unused capacity and on-demand tables are capped at
40,000 units a second; Lambda allows 1,000 concurrent executions per region
and keeps 100 unreserved. These match AWS's documented defaults too.

Many quotas can be raised in AWS, and new AWS accounts start with lower
Lambda concurrency; override the rules to match your account. Parameter
Store's higher throughput setting, for example, is
`{"id": "ssm-get-parameters", "service": "ssm", "operations": ["GetParameter"], "per_second": 10000}`. `strict` is the same set at 1% (`scale` 0.01), so
missing retries and backoff show up with very little load.

DynamoDB table capacity and Lambda concurrency are not request rates and
have their own models, below.

## DynamoDB capacity

With `aws-default` or `strict`, every DynamoDB read and write draws read
or write capacity units (the same units `ReturnConsumedCapacity` reports)
from two token buckets:

- **The table.** A provisioned table gets its `ProvisionedThroughput` per
  second and banks up to 300 seconds of unused capacity for bursts, as in
  AWS. An on-demand table is capped at its maximum throughput, 40,000
  units per second by default. `UpdateTable` changes take effect on the
  next request.
- **The partition.** Each item's partition key hashes to one of the
  table's partitions, and a partition serves 3,000 read units or 1,000
  write units per second. A provisioned table has as many partitions as
  it needs to serve what it was provisioned; an on-demand table has four.
  A hot partition key is throttled while the table still has room.

A request that doesn't fit gets what DynamoDB returns:

| Operation | Throttled |
| --- | --- |
| Single-item reads and writes, Query, Scan | `ProvisionedThroughputExceededException` with `ThrottlingReasons` (`TableWriteKeyRangeThroughputExceeded`, `TableReadProvisionedThroughputExceeded`, ...); `ThrottlingException` when an on-demand table hits its maximum |
| BatchWriteItem, BatchGetItem | The items that don't fit come back in `UnprocessedItems` / `UnprocessedKeys`; the call fails only when none fit |
| TransactWriteItems, TransactGetItems | `TransactionCanceledException` with `ThrottlingError` for the throttled items; nothing is written |

Scan spreads over every partition, so only the table cap applies to it.
A query on a secondary index draws on that index's partition, against
the table's capacity. A write larger than a whole bucket is admitted
when the bucket is full and leaves it in debt, so it is slowed, never
refused forever.

`scale` applies to partition and on-demand capacity. Provisioned capacity
is what you chose for the table, so it is never scaled. Tune the model in
the profile:

```json
{
  "profile": "aws-default",
  "scale": 0.01,
  "dynamodb": {
    "burst_seconds": 1,
    "tables": {"orders": {"partitions": 8}}
  }
}
```

| Field | Meaning |
| --- | --- |
| `disabled` | Turns capacity off while the other limits apply. |
| `partition_read_units`, `partition_write_units` | What one partition serves per second, before scaling. Default 3,000 and 1,000. |
| `on_demand_read_units`, `on_demand_write_units` | An on-demand table's maximum per second, before scaling. Default 40,000 each. |
| `burst_seconds` | Seconds of unused provisioned capacity a table banks. Default 300; 0 banks none, so a table throttles as soon as it goes over its rate. |
| `tables` | Fixes the partition count of named tables. |

The `none` base enforces capacity only when the profile has a `dynamodb`
section. `stratus limits status` lists each table's partitions, the units
it consumed and the reads and writes it throttled. `stratus diagnose`
explains partition throttles as a hot key and table throttles as
exhausted capacity.

## Lambda concurrency

With `aws-default` or `strict`, each invocation takes one concurrent
execution for as long as it runs:

- A function with reserved concurrency (`PutFunctionConcurrency` or
  CloudFormation's `ReservedConcurrentExecutions`) runs at most that many
  at once. Reserved concurrency of 0 stops the function.
- Every other function shares the account's unreserved pool: 1,000
  executions per account and region, less what functions reserved.

`scale` applies to the account pool, never to reserved concurrency, which
you chose for the function. A scaled pool always admits one execution.

| Invocation | Throttled |
| --- | --- |
| `Invoke` (`RequestResponse`), API Gateway, Step Functions, EventBridge Scheduler | `429 TooManyRequestsException` with `Reason` `ReservedFunctionConcurrentInvocationLimitExceeded` or `ConcurrentInvocationLimitExceeded` |
| `Invoke` (`Event`), SNS, EventBridge | Accepted, then retried with backoff (100 ms doubling to 5 s) until the function's `MaximumEventAgeInSeconds`, default 6 hours |
| Event source mappings (SQS, Kinesis, DynamoDB Streams) | Retried with backoff, as Lambda's pollers do |

```json
{"profile": "aws-default", "scale": 0.01, "lambda": {"account_concurrency": 1000}}
```

| Field | Meaning |
| --- | --- |
| `disabled` | Turns concurrency off while the other limits apply. |
| `account_concurrency` | Concurrent executions per account and region, before scaling. Default 1,000. |

The `none` base enforces concurrency only when the profile has a `lambda`
section. Real executions are capped by the warm pool, four containers per
function by default (`--lambda-containers`); `stratus limits status` shows
under `emulator.lambda` how many invocations queued for a container, so you
can tell Stratus's own ceiling from the limits you are testing.

## The scale factor

One Stratus instance handles a few thousand requests per second, far below
most real quotas. `scale` brings the quotas down to the load you can
generate: with `scale: 0.01`, 3,500 writes per second per S3 prefix become
35, and the application meets the limit at a hundredth of production load.
A scaled rule always admits at least one request per burst.

Scaling keeps the order in which limits are hit, which is what tells you
which part of the design breaks first. It does not make Stratus's latency
or throughput say anything about AWS's.

## Seeing throttles

Every throttled response carries `X-Stratus-Limit: <rule id>`, and its
message names the rule. `stratus limits status` lists each rule's effective
rate and how many requests it admitted and throttled, plus the latest
breaches with their request IDs.

`stratus diagnose` groups these failures under `limit_exceeded`, separate
from chaos faults (`injected_fault`) and other throttling. The throttle
itself is expected; the question is whether the application retried and
whether the work still completed. `--exclude-injected` leaves out chaos
faults but never limit throttles.
