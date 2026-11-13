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

Full local run, 2026-11-13, with the three blocker PRs listed under
Progress, with the Lambda, database, cache and Kafka images pulled.

Suite `4df2823b`, Stratus `780069c` (#27's first commit; the merged head differs only in request logging).

| | Passed / scored | Score | Not implemented | Other failures | Excluded |
|---|---:|---:|---:|---:|---:|
| All selected tests | 879 / 12691 | 6.9% | 7301 | 4511 | 296 |
| Single-service tests | 841 / 10023 | 8.4% | 5420 | 3762 | 38 |
| Cross-service tests | 38 / 2668 | 1.4% | 1881 | 749 | 258 |
| Control plane | 367 / 6955 | 5.3% | 4112 | 2476 | 137 |
| Data plane | 521 / 6054 | 8.6% | 3372 | 2161 | 201 |

Single-service tests, by service:

| | Passed / scored | Score | Not implemented | Other failures | Excluded |
|---|---:|---:|---:|---:|---:|
| `acm` | 0 / 252 | 0.0% | 213 | 39 | 0 |
| `apigateway` | 3 / 578 | 0.5% | 349 | 226 | 0 |
| `apigatewayv2` | 1 / 171 | 0.6% | 145 | 25 | 2 |
| `bedrock-runtime` | 0 / 85 | 0.0% | 31 | 54 | 0 |
| `cloudformation` | 2 / 386 | 0.5% | 276 | 108 | 21 |
| `cloudwatch` | 153 / 641 | 23.9% | 272 | 216 | 0 |
| `cognito-idp` | 2 / 582 | 0.3% | 439 | 141 | 0 |
| `dynamodb` | 339 / 882 | 38.4% | 156 | 387 | 4 |
| `dynamodbstreams` | 2 / 16 | 12.5% | 0 | 14 | 0 |
| `ecr` | 0 / 199 | 0.0% | 166 | 33 | 0 |
| `ecs` | 12 / 410 | 2.9% | 206 | 192 | 0 |
| `elasticache` | 0 / 150 | 0.0% | 128 | 22 | 0 |
| `elbv2` | 6 / 298 | 2.0% | 92 | 200 | 0 |
| `events` | 13 / 200 | 6.5% | 102 | 85 | 0 |
| `fis` | 0 / 51 | 0.0% | 5 | 46 | 0 |
| `iam` | 54 / 444 | 12.2% | 260 | 130 | 2 |
| `kafka` | 0 / 200 | 0.0% | 128 | 72 | 0 |
| `kinesis` | 15 / 214 | 7.0% | 118 | 81 | 0 |
| `kms` | 9 / 238 | 3.8% | 146 | 83 | 0 |
| `lambda` | 5 / 587 | 0.9% | 509 | 73 | 2 |
| `logs` | 3 / 446 | 0.7% | 344 | 99 | 0 |
| `rds` | 0 / 291 | 0.0% | 180 | 111 | 0 |
| `route53` | 11 / 114 | 9.6% | 23 | 80 | 0 |
| `s3` | 62 / 478 | 13.0% | 78 | 338 | 3 |
| `scheduler` | 25 / 120 | 20.8% | 7 | 88 | 0 |
| `secretsmanager` | 6 / 194 | 3.1% | 126 | 62 | 0 |
| `servicediscovery` | 15 / 74 | 20.3% | 15 | 44 | 0 |
| `ses` | 1 / 120 | 0.8% | 60 | 59 | 0 |
| `sesv2` | 0 / 120 | 0.0% | 87 | 33 | 0 |
| `sns` | 16 / 245 | 6.5% | 88 | 141 | 0 |
| `sqs` | 55 / 302 | 18.2% | 107 | 140 | 4 |
| `ssm` | 2 / 270 | 0.7% | 210 | 58 | 0 |
| `stepfunctions` | 18 / 520 | 3.5% | 295 | 207 | 0 |
| `sts` | 11 / 145 | 7.6% | 59 | 75 | 0 |

## Progress

| Run | Change | Passed / scored | Score |
|---|---|---:|---:|
| 2026-11-12 | First Fidelity workflow run (CI, 12 shards) | 308 / 12,855 | 2.4% |
| 2026-11-12 | Same code, local run (the baseline for the rows below) | 290 / 12,717 | 2.3% |
| 2026-11-12 | #25: tags on create, S3 bucket sub-resources, NotImplemented for unknown routes | 401 / 12,705 | 3.2% |
| 2026-11-13 | #26: EC2 networking, CloudWatch alarms; #27: rebuilt DynamoDB engine | 879 / 12,691 | 6.9% |

DynamoDB went from 26 to 339 passing (38%), CloudWatch from 15 to 153 (24%),
S3 from 29 to 62, Scheduler from 3 to 25 and IAM from 35 to 54.

## What blocks the most tests now

The setup blockers this page used to list (tags, EC2 networking, S3
sub-resources, DynamoDB sort keys, `PutMetricAlarm`) are fixed. Counting the
operations that most often answer `NotImplemented`:

| Gap | Tests blocked | Where |
|---|---:|---|
| CloudFormation resource types and properties (`WaitConditionHandle`, `S3::Bucket`, `RDS::DBInstance`, `CloudWatch::Alarm`, `SQS::QueuePolicy`, `Number` parameters, ...) | 752 | CloudFormation and every cross-service test that deploys a stack |
| ACM (`ImportCertificate`, ACME endpoints) | 291 | ACM, API Gateway custom domains |
| IAM instance profiles, groups and service-linked roles | 186 | IAM, SSM, ECS |
| Step Functions activities, versions and execution history | 186 | Step Functions |
| Cognito auth flows, MFA and domains | 174 | Cognito |
| SQS FIFO queues, KMS encryption and redrive validation | 84 | SQS |
| RDS clusters and engine versions; ElastiCache subnet groups | 191 | RDS, ElastiCache |
| DynamoDB PartiQL (`ExecuteStatement` and friends) | 65+ | DynamoDB |

CloudFormation is the biggest lever left: one unsupported resource type
fails the whole stack, and stacks are how most cross-service tests build
their fixtures.

Each run's reports name the failing operation and assertion, so
`reports/<service>/<test>.json` is a ready-made, prioritised fidelity
backlog.
