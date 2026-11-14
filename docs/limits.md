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

`stratus ci --limits strict -- go test ./...` applies a profile to a CI run.
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
| `burst_seconds` | How many seconds of `per_second` may arrive at once. Defaults to 1. |
| `per` | What shares one bucket: `account` (default; one per account and region), `resource` (one per S3 bucket or Lambda function) or `prefix` (one per S3 key prefix). |

Each rule is a token bucket that starts full. A request that would exceed
any rule covering it is throttled and consumes nothing, as in AWS. Accounts
and regions always have separate buckets.

### Base profiles

`aws-default` holds the default quotas AWS documents for the APIs most
often called on a hot path:

| Rule | Service | Operations | Rate | Per |
| --- | --- | --- | --- | --- |
| `s3-reads-per-prefix` | s3 | GetObject, HeadObject | 5,500/s | prefix |
| `s3-writes-per-prefix` | s3 | PutObject, CopyObject, DeleteObject(s), multipart create and complete | 3,500/s | prefix |
| `ssm-get-parameters` | ssm | GetParameter, GetParameters, GetParametersByPath | 40/s | account |
| `kms-cryptographic-operations` | kms | Encrypt, Decrypt, GenerateDataKey and the other cryptographic operations | 5,500/s | account |
| `secretsmanager-get-secret-value` | secretsmanager | GetSecretValue, DescribeSecret, BatchGetSecretValue | 10,000/s | account |

Some of these differ by region or can be raised in AWS; override them to
match your account. `strict` is the same set at 1% (`scale` 0.01), so
missing retries and backoff show up with very little load.

DynamoDB capacity and Lambda concurrency are not request rates; they are
modelled separately and arrive in later releases.

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
