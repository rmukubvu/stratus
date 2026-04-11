# stratus

`stratus` is a fast local AWS emulator for real developer workflows.

Stratus emulates your AWS stack locally. Preflight verifies that it actually
works before you deploy.

Together, Stratus and Preflight give developers a credible local AWS delivery
loop.

What Stratus solves:

- run AWS-shaped infrastructure locally
- use real tooling like AWS CLI, SDKs, and CDK
- get fast feedback without deploying to AWS
- reproduce failures in a deterministic environment

What Preflight adds:

- prove the stack actually works, not just that resources were created
- validate structure, wiring, IAM, and behavior from the outside
- add diagnosis when something fails
- catch compatibility regressions before cloud deployment

The goal is not to pretend every AWS API exists. The goal is to make a
practical subset work well enough for real AWS CLI, SDK, CDK, Terraform, and
external black-box validation flows.

Current design priorities:

- compatibility over cleverness
- deterministic local behavior over broad flaky emulation
- one binary, one data directory
- permissive local auth handling
- black-box contract tests over implementation-driven confidence

Current Lambda execution runtimes:

- `python3.11`
- `nodejs20.x`

## What Works Today

`stratus` currently includes first-class support for 26 service families:

- STS
- S3
- Lambda
- SSM
- DynamoDB
- DynamoDB Streams
- CloudWatch Logs
- CloudWatch Metrics
- SQS
- SNS
- EventBridge
- IAM
- CloudFormation
- KMS
- API Gateway v2
- API Gateway REST
- Cognito IDP
- Step Functions
- Kinesis
- Secrets Manager
- ACM
- ECR
- ECS
- ELBv2
- RDS
- ElastiCache

Support is intentionally uneven by service. Some services have deeper
behavioral coverage than others. The compatibility bar is the contract suite,
not the presence of a package name.

## Validated Loop

The strongest current proof path for `stratus` is:

1. a real Java AWS SDK v2 smoke fixture hits STS, DynamoDB, SQS, and S3
2. a bootstrapless CDK fixture deploys a live local path through CloudFormation into API Gateway, Lambda, SQS, and DynamoDB
3. `preflight` lints and validates the deployed stack from the outside
4. the release gate chains the Java SDK smoke and the external `preflight` CDK smoke

This matters more than raw service count. The product claim is backed by real
tooling, real deployment, and black-box validation on the same local stack.

There is also a runnable example for:

- `API Gateway -> TypeScript Lambda -> DynamoDB`

See [examples/typescript-httpapi-dynamodb/README.md](/Users/robson/awsdev/stratus-git/examples/typescript-httpapi-dynamodb/README.md).

## Architecture

`stratus` is split into a few clear layers:

1. Front door
   - `net/http` listener
   - AWS request classification
   - permissive SigV4 parsing
   - shared error and response normalization
2. Service layer
   - one package per AWS service
   - business semantics live here, not in transport routing
3. Persistence
   - `bbolt` for metadata and control-plane state
   - filesystem blobs for larger payloads such as S3 objects and Lambda code
4. Execution
   - Docker-backed Lambda runtime
   - warm container reuse
   - local execute-api path for API Gateway-backed flows
5. Validation
   - unit tests for parsing and helpers
   - real AWS CLI and SDK contract tests
   - CDK fixture
   - external `preflight` harness

Important implementation points:

- The HTTP layer classifies requests before dispatch.
- Services own semantics; transport adapters stay thin.
- Auth is permissive in local mode. Missing auth is accepted by default.
- Unsupported behavior should fail explicitly, not silently.

## Project Layout

The main entry points are:

- `cmd/stratus/main.go`
- `internal/app`
- `internal/httpapi`
- `internal/services`
- `internal/store`
- `test/contract`
- `test/fixtures`
- `scripts`

Useful files when orienting yourself:

- `cmd/stratus/main.go`
- `internal/config/config.go`
- `internal/app/prettylog.go`
- `internal/httpapi/classify.go`
- `internal/services/cloudformation/service.go`
- `internal/services/lambda/service.go`
- `test/contract/contract_test.go`

## Requirements

- Go 1.25 or newer
- Docker if you want Lambda execution
- AWS CLI for contract and smoke flows
- Node.js for the CDK fixture
- Java 17+ and Maven for the Java SDK fixture

## Install

The release path for direct install is Homebrew tap plus GitHub Releases.

For macOS users, the recommended install is:

```bash
brew install rmukubvu/tap/stratus
```

Then run:

```bash
stratus
```

If you prefer building from source, or you are not using Homebrew, you can
still build locally with Go as described below.

Under the hood this expects:

- release archives attached to tagged GitHub releases in `rmukubvu/stratus`
- a tap repository at `rmukubvu/homebrew-tap`
- a generated formula at `Formula/stratus.rb`

Release maintainers should also set the `HOMEBREW_TAP_GITHUB_TOKEN` repository
secret in `rmukubvu/stratus` so the release workflow can update the tap repo.
Set `STRATUS_RELEASE_GITHUB_TOKEN` as well if you want the guarded
`Promote Release` workflow to create a tag that actually triggers the downstream
`Release` workflow. This token should have `Contents: Read and write` on the
`rmukubvu/stratus` repository.

The recommended release flow is:

1. push changes to `main`
2. wait for the `CI` workflow, including `release-gate`, to pass
3. open `Actions -> Promote Release`
4. enter a version such as `v0.1.1`

The promote workflow creates the tag only if the current `main` commit already
has a successful `CI` run. That tag then triggers
`.github/workflows/release.yml`, which publishes the release artifacts and
updates the Homebrew formula.

You can still create and push a tag manually if needed, but the promote
workflow is the safer default.

## Running stratus

Build:

```bash
go build ./cmd/stratus
```

The easiest way to remember the new flow is:

- `stratus` for humans
- `stratus dev` for explicit developer mode
- `stratus serve` for scripts and CI

### Quick start modes

Human-first mode:

```bash
./stratus
```

This is now the default local developer experience. It:

- starts the emulator
- serves the built-in operator portal at `/_stratus/`
- opens the portal in your browser by default

If you only remember one command, remember this one.

Explicit developer mode:

```bash
./stratus dev --port 4566 --data-dir ./data --log-format pretty --log-level debug
```

If you want the portal but do not want an automatic browser launch:

```bash
./stratus dev --no-open --port 4566 --data-dir ./data
```

Headless server mode for scripts, CI, or existing automation:

```bash
./stratus serve --port 4566 --data-dir ./data --log-format json
```

This is the stable machine-oriented mode. It does not try to open the browser.

Legacy compatibility:

```bash
./stratus --port 4566 --data-dir ./data
```

This still works and is treated as headless `serve` mode.

Health check:

```bash
curl http://127.0.0.1:4566/_stratus/health
```

Built-in operator portal:

```bash
open http://127.0.0.1:4566/_stratus/
```

If port `4566` is already in use on your machine, run `stratus` on a different
port such as `4567` and point the smoke scripts at that endpoint.

### Recommended local workflow

Start `stratus`:

```bash
./stratus
```

Then use the portal to copy or verify:

- the local endpoint for AWS CLI, SDKs, CDK, and Preflight
- the currently supported AWS services
- ready-to-run example commands
- recent activity and failures
- local CloudWatch-style logs

This is the intended onboarding path now. You should not need to remember the
older longer command lines just to get started.

## Logging and Terminal Output

`stratus` supports:

- `--log-format auto`
- `--log-format json`
- `--log-format pretty`

`pretty` is intended for local operator use. It uses a Lip Gloss-backed terminal
view with:

- a live status summary
- top service counters
- recent request and error activity
- Lambda runtime lifecycle visibility

`json` remains the better format for machine capture.

## Operator Portal

`stratus` now ships with a built-in read-only operator portal at:

```text
/_stratus/
```

The portal is designed to answer the first questions a developer has after
starting a local emulator:

- what endpoint should my tools use?
- which AWS services are available here?
- how do I connect the AWS CLI, Java SDK, CDK, and Preflight?
- what requests and failures happened last?
- what do the local CloudWatch-style logs look like?

The portal is part of the main product flow, not a separate extra tool. When
you run `stratus` or `stratus dev`, the emulator and the portal come up
together.

What the built-in portal currently gives you:

- endpoint and runtime status
- supported service inventory
- copyable quick-start examples for AWS CLI, Java SDK, CDK, and Preflight
- recent request activity
- recent failures
- CloudWatch-style log group, stream, and event browsing

Under the hood, the browser surface is backed by the operator API:

- `/_stratus/operator/bootstrap`
- `/_stratus/operator/overview`
- `/_stratus/operator/activity`
- `/_stratus/operator/errors`
- `/_stratus/operator/logs/groups`
- `/_stratus/operator/logs/streams`
- `/_stratus/operator/logs/events`

The original portal v1 design notes are still documented in:

- [`/Users/robson/awsdev/stratus-git/docs/portal-v1.md`](/Users/robson/awsdev/stratus-git/docs/portal-v1.md)

## AWS CLI Example

STS:

```bash
aws --endpoint-url http://127.0.0.1:4566 sts get-caller-identity
```

S3:

```bash
aws --endpoint-url http://127.0.0.1:4566 s3api create-bucket --bucket demo-bucket
aws --endpoint-url http://127.0.0.1:4566 s3api put-object --bucket demo-bucket --key hello.txt --body ./README.md
aws --endpoint-url http://127.0.0.1:4566 s3api get-object --bucket demo-bucket --key hello.txt /tmp/hello.txt
```

Lambda:

```bash
aws --endpoint-url http://127.0.0.1:4566 lambda list-functions
```

## Java AWS SDK Example

There is a real black-box Java SDK v2 fixture in:

- `test/fixtures/java-sdk-smoke`

The fixture currently proves that the Java SDK can talk to `stratus` over a
real network boundary for:

- STS `GetCallerIdentity`
- DynamoDB `CreateTable`, `PutItem`, `GetItem`
- SQS `CreateQueue`, `SendMessage`, `ReceiveMessage`, `DeleteMessage`, `DeleteQueue`
- S3 `CreateBucket`, `PutObject`, `GetObject`

Quick start:

Terminal 1:

```bash
./stratus --log-format pretty --log-level debug
```

Terminal 2:

```bash
./scripts/smoke-java-sdk.sh
```

If `stratus` is running on another port:

```bash
STRATUS_ENDPOINT_URL=http://127.0.0.1:4567 ./scripts/smoke-java-sdk.sh
```

Full local release gate:

```bash
PREFLIGHT_DIR=/path/to/preflight ./scripts/release-gate.sh
```

That script runs the Java SDK smoke first, then the `preflight` CDK smoke
against a fresh `stratus` binary.

You can also run the Maven fixture directly:

```bash
cd test/fixtures/java-sdk-smoke
mvn -q -Dstratus.endpoint=http://127.0.0.1:4566 test
```

See also:

- `test/fixtures/java-sdk-smoke/README.md`
- `test/fixtures/java-sdk-smoke/src/test/java/com/stratus/fixtures/JavaSDKSmokeTest.java`

## CDK Example

There is a minimal inline Lambda + HTTP API CDK fixture in:

- `test/fixtures/cdk-inline-httpapi`

It deploys a small stack through CloudFormation and then invokes the local
execute-api path.

Run it with a live `stratus` instance:

```bash
./scripts/smoke-cdk-template.sh
```

If `stratus` is not on `4566`:

```bash
STRATUS_ENDPOINT_URL=http://127.0.0.1:4567 ./scripts/smoke-cdk-template.sh
```

The script performs a real `cdk deploy`, resolves the API ID from
CloudFormation, and calls:

```text
/_aws/execute-api/<api-id>/hello
```

## Using preflight

`preflight` should remain an external consumer-side validator, not a package
inside `stratus`. That keeps the compatibility pressure black-box.

The `stratus` repo includes a convenience wrapper:

- `scripts/smoke-preflight.sh`

By default this script expects a sibling checkout named `preflight-stratus`:

```text
../preflight-stratus
```

The wrapper will:

1. build a local `stratus` binary
2. choose a local port
3. point `preflight` at that binary and endpoint
4. run the selected `preflight` smoke fixture

Run it:

```bash
./scripts/smoke-preflight.sh
```

Override the companion repo location if needed:

```bash
PREFLIGHT_DIR=/path/to/preflight-stratus ./scripts/smoke-preflight.sh
```

Pick a specific fixture target:

```bash
PREFLIGHT_TARGET=cdk ./scripts/smoke-preflight.sh
```

Override the endpoint explicitly:

```bash
STRATUS_PREFLIGHT_ENDPOINT=http://127.0.0.1:4567 ./scripts/smoke-preflight.sh
```

The companion `preflight` repo has been refactored to support emulator backends
instead of a Floci-only path, so `stratus` can now be treated as the primary
target for those validation runs.

## Persistence Model

`stratus` separates metadata from payloads:

- metadata and control-plane state live in `bbolt`
- large blobs live on the filesystem under the data directory

This keeps the metadata store simple while avoiding full object persistence
inside a key-value store.

## Lambda Execution Model

Lambda is the only Docker-dependent part of the current runtime. The first-cut
policy is intentionally simple:

- sync invoke support
- warm container reuse
- host-enforced timeouts
- stdout and stderr capture
- cleanup on function delete and shutdown

Core control-plane services still boot without Docker.

## Testing and Validation

The test strategy is contract-first.

Main layers:

- unit tests for parsing, rendering, and storage helpers
- real AWS CLI contract tests
- SDK smoke tests
- CDK fixture smoke
- external `preflight` validation

Run the Go tests:

```bash
go test ./...
```

Run the Java SDK smoke:

```bash
./scripts/smoke-java-sdk.sh
```

Run the CDK smoke:

```bash
./scripts/smoke-cdk-template.sh
```

Run the `preflight` smoke:

```bash
./scripts/smoke-preflight.sh
```

## Compatibility Philosophy

The standard for unsupported behavior is simple:

- if supported, it should behave predictably and be covered by tests
- if unsupported, it should fail explicitly with an AWS-shaped error
- silent partial behavior is not acceptable

This means the surface area should grow only when the contract coverage grows
with it.

## Current Caveats

- service depth still varies; some services are control-plane-heavy
- CloudFormation support is practical but not complete AWS parity
- Docker is optional for startup but required for Lambda execution
- the release claim should follow the contract and smoke suite, not raw service count

## Summary

`stratus` is intended to be a compatibility-focused local AWS control plane with
real developer workflows as the bar:

- AWS CLI should work
- SDKs should work
- CDK should deploy
- `preflight` should validate the behavior from the outside

That is the standard the codebase is working toward.
