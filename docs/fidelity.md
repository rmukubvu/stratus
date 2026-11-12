# Fidelity against AWS's own tests

AWS publishes the tests it scores AWS Emulator with:
[aws/aws-emulator-evaluation-suite](https://github.com/aws/aws-emulator-evaluation-suite)
(CC0). Each test is a short boto3 program that drives a workflow and asserts
what real AWS did, and AWS verified every one against real accounts. This page
scores Stratus with the same tests and the same scoring rules, so the numbers
are directly comparable with the per-service scores on the
[AWS Emulator releases page](https://github.com/aws/aws-emulator/releases).

## How Stratus is scored

- **Which tests.** Every test whose services Stratus emulates: 10,061
  single-service tests across 34 suite services and 2,926 cross-service tests
  (for example `lambda__sqs`) whose services are all in Stratus. Tests for
  services Stratus does not have are left out, not counted as failures.
- **One fresh Stratus per test.** Each test runs under `stratus ci`, so it
  gets a private Stratus on a free port with an empty data directory, and
  everything is removed afterwards. That is the isolation the suite asks for.
- **The suite's own scoring.** `score = passed / scored`. Tests the runner
  could not run (a missing tool such as `terraform`, or a runner fault) are
  excluded, exactly as the suite's `summarize` command excludes them.

Run it yourself:

```sh
git clone https://github.com/aws/aws-emulator-evaluation-suite suite
(cd suite && uv sync --locked --extra checks)
go build -o stratus ./cmd/stratus
suite/.venv/bin/python scripts/fidelity/stratus_fidelity.py run \
  --suite suite --stratus ./stratus --out reports --jobs 8
suite/.venv/bin/python scripts/fidelity/stratus_fidelity.py report \
  --suite suite --out reports --markdown scores.md
```

`run` resumes: rerunning it only runs tests that have no report yet, and
`--service sqs` or `--scope single` narrow the selection. The
[Fidelity workflow](../.github/workflows/fidelity.yml) runs the whole
selection in 12 shards every Monday, with the Lambda, database, cache and
Kafka images pulled, and publishes `scores.md` in the run summary.

## Current scores

First run of the Fidelity workflow, 2026-11-12, with the Lambda, database,
cache and Kafka images pulled. The whole selection took 13 minutes across 12
shards.

Suite `4df2823b`, Stratus `86eb4cc`.

| | Passed / scored | Score | Not implemented | Other failures | Excluded |
|---|---:|---:|---:|---:|---:|
| All selected tests | 308 / 12855 | 2.4% | 7494 | 5053 | 132 |
| Single-service tests | 290 / 10040 | 2.9% | 5522 | 4228 | 21 |
| Cross-service tests | 18 / 2815 | 0.6% | 1972 | 825 | 111 |
| Control plane | 127 / 6996 | 1.8% | 3873 | 2996 | 96 |
| Data plane | 183 / 6186 | 3.0% | 3826 | 2177 | 69 |

Single-service tests, by service:

| | Passed / scored | Score | Not implemented | Other failures | Excluded |
|---|---:|---:|---:|---:|---:|
| `acm` | 0 / 252 | 0.0% | 212 | 40 | 0 |
| `apigateway` | 3 / 578 | 0.5% | 238 | 337 | 0 |
| `apigatewayv2` | 1 / 171 | 0.6% | 114 | 56 | 2 |
| `bedrock-runtime` | 0 / 85 | 0.0% | 10 | 75 | 0 |
| `cloudformation` | 2 / 388 | 0.5% | 294 | 92 | 19 |
| `cloudwatch` | 15 / 641 | 2.3% | 440 | 186 | 0 |
| `cognito-idp` | 2 / 582 | 0.3% | 443 | 137 | 0 |
| `dynamodb` | 30 / 886 | 3.4% | 413 | 443 | 0 |
| `dynamodbstreams` | 0 / 16 | 0.0% | 0 | 16 | 0 |
| `ecr` | 0 / 199 | 0.0% | 163 | 36 | 0 |
| `ecs` | 12 / 410 | 2.9% | 94 | 304 | 0 |
| `elasticache` | 0 / 150 | 0.0% | 39 | 111 | 0 |
| `elbv2` | 0 / 298 | 0.0% | 9 | 289 | 0 |
| `events` | 13 / 200 | 6.5% | 108 | 79 | 0 |
| `fis` | 0 / 51 | 0.0% | 28 | 23 | 0 |
| `iam` | 35 / 446 | 7.8% | 307 | 104 | 0 |
| `kafka` | 0 / 200 | 0.0% | 5 | 195 | 0 |
| `kinesis` | 15 / 214 | 7.0% | 112 | 87 | 0 |
| `kms` | 10 / 238 | 4.2% | 152 | 76 | 0 |
| `lambda` | 4 / 589 | 0.7% | 502 | 83 | 0 |
| `logs` | 3 / 446 | 0.7% | 327 | 116 | 0 |
| `rds` | 0 / 291 | 0.0% | 99 | 192 | 0 |
| `route53` | 11 / 114 | 9.6% | 23 | 80 | 0 |
| `s3` | 33 / 481 | 6.9% | 4 | 444 | 0 |
| `scheduler` | 3 / 120 | 2.5% | 95 | 22 | 0 |
| `secretsmanager` | 6 / 194 | 3.1% | 134 | 54 | 0 |
| `servicediscovery` | 11 / 74 | 14.9% | 10 | 53 | 0 |
| `ses` | 1 / 120 | 0.8% | 56 | 63 | 0 |
| `sesv2` | 0 / 120 | 0.0% | 1 | 119 | 0 |
| `sns` | 3 / 245 | 1.2% | 221 | 21 | 0 |
| `sqs` | 60 / 306 | 19.6% | 119 | 127 | 0 |
| `ssm` | 2 / 270 | 0.7% | 208 | 60 | 0 |
| `stepfunctions` | 10 / 520 | 1.9% | 445 | 65 | 0 |
| `sts` | 5 / 145 | 3.4% | 97 | 43 | 0 |

## What blocks the most tests

Most failures happen in a test's setup, before it reaches the behaviour it
is about. A few gaps account for most of them:

| Gap | Tests blocked | Where |
|---|---:|---|
| Tags refused on create (`CreateRole`, `CreateStack`, `CreateTopic`) | 3,223 | IAM, CloudFormation, SNS |
| No EC2 networking (`CreateVpc`, subnets, security groups) for tests that need a VPC | 1,161 | ELBv2, MSK, RDS, ElastiCache, ECS |
| `PutBucketTagging` and other bucket sub-resource PUTs answered as `CreateBucket` | 637 | S3, and every test that tags a bucket |
| Unknown routes answered with 404 or 400 instead of `NotImplemented` | 398 | SES v2, MSK |
| DynamoDB tables with a sort key (`RANGE`) | 227 | DynamoDB |
| `PutMetricAlarm` | 216 | CloudWatch |
| FIFO queues and topics | 97 | SQS, SNS |

Stratus refuses what it does not implement instead of pretending, which is
deliberate, but it means one missing parameter fails a whole workflow. The
suite tags almost everything it creates, so accepting and storing tags is
the single biggest lever.

Each run's reports name the failing operation and assertion, so
`reports/<service>/<test>.json` is a ready-made, prioritised fidelity
backlog.
