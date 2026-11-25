# Operation catalogue, coverage and `stratus check`

Stratus carries a catalogue of every operation AWS defines for the services
it emulates, generated from the AWS service models that botocore ships. Three
features read it:

- **Coverage**: which of those operations Stratus implements, per service, in
  [docs/coverage](coverage/README.md). Everything else answers
  `501 NotImplemented`, never an empty success.
- **Request validation**: requests whose input breaks the model's constraints
  get the error AWS sends (see below).
- **`stratus check`**: scans a codebase for the AWS calls it makes and lists
  the ones Stratus would answer with 501, before you run anything.

## `stratus check`

```bash
stratus check              # scan the current directory
stratus check ./services   # or a directory
stratus check --json .     # machine-readable
```

It reads source only and recognises:

| SDK | What it looks for |
| --- | --- |
| boto3 / botocore (Python) | `boto3.client("sqs")` or `.resource(...)`, then the snake_case methods that service defines (`send_message`) |
| AWS SDK for JavaScript v3 | an `@aws-sdk/client-*` import, then `new SendMessageCommand(...)`; `@aws-sdk/lib-dynamodb` commands count as the DynamoDB operations they send |
| AWS SDK for Go v2 | an `aws-sdk-go-v2/service/*` import, then `sqs.SendMessageInput{...}` |

`node_modules`, `vendor`, virtualenvs and build output are skipped. The
report lists operations Stratus does not implement and services it does not
emulate, each with `file:line`, then what works. It exits `1` when anything
would fail against Stratus and `0` when everything is implemented, so it can
gate a CI job.

Agents get the same scan as the MCP tool `stratus_check`, and
`stratus_coverage` answers "does Stratus implement `sqs AddPermission`?"
from the catalogue without calling a running Stratus.

## Request validation

Before a request reaches its service, Stratus checks its input against the
operation's model: required members, string and list lengths, number ranges,
patterns and enums. A request that breaks them gets AWS's own answer:

```
ValidationException: 1 validation error detected: Value 'AES_512' at 'keySpec'
failed to satisfy constraint: Member must satisfy enum value set: [AES_256, AES_128]
```

Validation covers the JSON-protocol services whose AWS request validation
answers with that message: ACM, DynamoDB, DynamoDB Streams, EventBridge,
Kinesis, KMS, SSM and Step Functions answer `ValidationException`; CloudWatch
Logs and Cognito answer `InvalidParameterException`. Other services are not
validated by the catalogue, because a wrong error code is worse than none.
Patterns that Go's regular expressions cannot express (lookahead,
backreferences) are skipped rather than enforced wrongly.

It is on by default. Turn it off with `--validation off` or
`STRATUS_VALIDATION=off`.

## Regenerating

The catalogue is pinned to the botocore version in
`scripts/models/requirements.txt`. To move it:

```bash
pip install -r scripts/models/requirements.txt
python scripts/models/generate.py          # rewrites internal/awsmodel/models/
go build -o /tmp/stratus ./cmd/stratus
STRATUS_VALIDATION=off DOCKER_HOST=unix:///nonexistent /tmp/stratus serve --port 4599 --data-dir /tmp/probe &
python scripts/models/coverage.py --endpoint http://localhost:4599
```

Validation is off for the probe because placeholders do not satisfy every
pattern. `coverage.py` resets the emulator, sends every operation once with its
required parameters filled with placeholders, and counts an operation as
implemented unless Stratus answers 501 or a not-implemented error. It
rewrites `internal/awsmodel/coverage.json` and `docs/coverage/`. Rerun it
whenever a change implements an operation.
