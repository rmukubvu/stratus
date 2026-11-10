# Accounts and regions

Stratus keeps a separate set of resources for every AWS account and region,
so a test can model a landing zone, a hub-and-spoke network or a
multi-region deployment the way it runs in AWS.

## Choosing the account

The account is taken from the request's access key:

| Access key | Account |
| --- | --- |
| Twelve digits, e.g. `111111111111` | That account |
| Anything else (`test`, `AKIA…`) | `000000000000`, the default |

This is the convention local AWS emulators share, so tooling that already
sets `AWS_ACCESS_KEY_ID=111111111111` for LocalStack works unchanged. The
secret key is not checked.

```sh
AWS_ACCESS_KEY_ID=111111111111 aws sts get-caller-identity   # "Account": "111111111111"
AWS_ACCESS_KEY_ID=test         aws sts get-caller-identity   # "Account": "000000000000"
```

## Choosing the region

The region is the one the request is signed for (`--region`,
`AWS_REGION`). Unsigned requests, and requests signed for a name that is not
an AWS region, use `us-east-1`.

## What is scoped

- **Regional services** (SQS, SNS, Lambda, DynamoDB, EventBridge, Step
  Functions, KMS, RDS, …) keep one set of resources per account and region.
  ARNs, queue URLs and the like carry the real account and region:
  `arn:aws:sqs:eu-central-1:111111111111:orders`.
- **Global services** (IAM, S3, Route 53) keep one set per account, visible
  from every region. An S3 bucket remembers the region it was created in.
- **STS** answers `GetCallerIdentity` with the caller's account.
- **Engines** (RDS, ElastiCache, MSK) and **Lambda containers** are separate
  per account and region, so two accounts can each run a database called
  `orders-db`.

Services inside one account and region are wired to each other as before:
an SNS topic delivers to SQS queues and Lambda functions of the same account
and region.

## Snapshots, reset and seeds

Snapshots, `stratus ci --seed` forks and reset cover every account and region
at once. Resetting a service clears it in every account and region.

The default account and region keep the storage layout Stratus has always
used, so existing data directories and snapshots load unchanged. Other
scopes live beside them, prefixed with `@<account>/<region>/` in the metadata
database and the blob directory.

## Current limits

- Requests act in the caller's account. Reaching a resource in another
  account directly (for example sending to another account's queue URL) is
  not modelled; use STS `AssumeRole` into that account instead.
- The control API's service views (SES inbox, scheduler clock, MSK brokers,
  DNS resolver) show the default account and region.
- Policies are stored but not enforced unless IAM enforcement is turned on.
