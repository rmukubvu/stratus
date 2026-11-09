# Bedrock: record once, replay forever

Stratus emulates the Amazon Bedrock Runtime API so code that calls an LLM
can run in CI without AWS credentials, without a bill and with the same
answer on every run. Answers come from fixtures:

- a **recorded** fixture pins one exact request to the answer real Bedrock
  gave it. Record once on your machine, commit the files, replay in CI.
- a **hand-written** fixture answers every request whose model and prompt
  match, such as "any Claude model, any prompt containing `refund`".

Embedding models need no fixtures at all: they get deterministic vectors.

## What is emulated

| Operation | Notes |
| --- | --- |
| `Converse` | Any model: the Converse format is the same for all of them. |
| `ConverseStream` | Replays the Converse answer as AWS event-stream events (`messageStart`, `contentBlockDelta`, ..., `metadata`). Text and tool input arrive in several deltas. |
| `InvokeModel` | Text fixtures are rendered in the provider's own body format: Anthropic Claude (Messages API), Amazon Nova, Amazon Titan Text, Meta Llama and Mistral (completion and chat). Other providers need a fixture with the exact `response` body. |
| `InvokeModelWithResponseStream` | Chunks in the provider's streaming format, e.g. Anthropic `message_start` ... `message_stop` with `amazon-bedrock-invocationMetrics`. Unknown providers get the whole body as one chunk. |
| `CountTokens` | Estimated as one token per four bytes of prompt text. |
| Embeddings via `InvokeModel` | `amazon.titan-embed-text-*` (v1: 1536 dimensions, v2: 1024 or `dimensions`) and `cohere.embed-*` (1024 or `output_dimension`, `embedding_types` supported). The same text always gives the same unit vector; the vectors carry no meaning, so similarity search returns stable but arbitrary neighbours. |

Model ids may be foundation model ids, cross-region inference profiles
(`us.anthropic...`, `eu.`, `apac.`, `global.`) or ARNs. Token counts in
responses and headers are estimates (one per four bytes), stable from run to
run.

Requests reach Bedrock when the SDK signs them for `bedrock`, which every AWS
SDK does, or when the host starts with `bedrock-runtime.`. Chaos rules work as
for any service, so a test can check retries against `ThrottlingException`:

```json
{"rules": [{"service": "bedrock", "operation": "Converse", "action": "throttle", "probability": 0.5}]}
```

## Configuration

| Variable | Meaning |
| --- | --- |
| `STRATUS_BEDROCK_FIXTURES` | Directory of fixture files, read on every request and written by record mode. Commit it next to your tests. |
| `STRATUS_BEDROCK_MODE` | `replay` (default) fails a request no fixture answers; `record` sends it to real Bedrock and saves the answer. |
| `STRATUS_BEDROCK_REGION` | Region of real Bedrock in record mode. Falls back to `AWS_REGION`, `AWS_DEFAULT_REGION`, then `us-east-1`. |
| `STRATUS_BEDROCK_ACCESS_KEY_ID`, `STRATUS_BEDROCK_SECRET_ACCESS_KEY`, `STRATUS_BEDROCK_SESSION_TOKEN` | Real credentials for record mode. Fall back to `AWS_ACCESS_KEY_ID` and friends. Separate names exist because the shell that runs Stratus often holds fake keys meant for the emulator. |
| `STRATUS_BEDROCK_ENDPOINT` | Overrides the real Bedrock endpoint, e.g. for a VPC endpoint. |

Credential profiles and SSO are not read. Export the keys of a profile with:

```sh
eval "$(aws configure export-credentials --profile dev --format env)"
```

## Recording

```sh
export STRATUS_BEDROCK_FIXTURES=testdata/bedrock
eval "$(aws configure export-credentials --format env)"
STRATUS_BEDROCK_MODE=record stratus &
go test ./...                       # every new model call is recorded once
git add testdata/bedrock
```

Each recording is one file named after kind, model and request hash, such as
`converse-anthropic.claude-3-5-haiku-20241022-v1_0-3f2a9c1b7d4e.json`. It
holds the request, for review, and the response. Requests already recorded
replay without calling AWS, so re-running in record mode only pays for new
calls. To re-record one call, delete its file.

Streaming calls are recorded through their non-streaming sibling: a
`ConverseStream` call records a `Converse` call and replays it as a stream.
One recording therefore serves both.

You can also switch modes on a running Stratus:

```
PUT /_stratus/control/v1/bedrock/mode   {"mode": "record"}
```

The switch lasts until a reset, which returns to `STRATUS_BEDROCK_MODE`.

## Replaying

In CI, point Stratus at the same directory and leave the mode at `replay`:

```yaml
- uses: rmukubvu/stratus@main
  env:
    STRATUS_BEDROCK_FIXTURES: testdata/bedrock
  with:
    run: go test ./...
```

A request no fixture answers fails with `ResourceNotFoundException`. The
message names the model and the request hash, so you can record it or pin a
hand-written fixture to it:

```
Stratus has no Bedrock fixture for a Converse request to us.anthropic.claude-3-5-haiku-20241022-v1:0
(request_hash sha256:9b1c...). Record one by running with STRATUS_BEDROCK_MODE=record, or add one: ...
```

A recorded fixture matches only the exact request. Key order and whitespace
do not matter, but any change to the prompt, the system prompt, the
inference settings or the model id does. Prompts that embed the current
time or random ids will never match a recording; give those a hand-written
fixture instead.

## Fixture files

A fixture file holds one fixture or an array of them. Every `*.json` file in
the directory is read, subdirectories included, in path order.

```json
[
  {
    "id": "refunds",
    "model": "anthropic.claude-*",
    "match": {"contains": "refund"},
    "text": "Refunds take five business days to reach your card."
  },
  {
    "id": "lookup-order",
    "kind": "converse",
    "match": {"contains": "order 42"},
    "response": {
      "output": {"message": {"role": "assistant", "content": [
        {"toolUse": {"toolUseId": "tool-1", "name": "lookup_order", "input": {"order": 42}}}
      ]}},
      "stopReason": "tool_use",
      "usage": {"inputTokens": 12, "outputTokens": 8, "totalTokens": 20},
      "metrics": {"latencyMs": 0}
    }
  }
]
```

| Field | Meaning |
| --- | --- |
| `id` | Optional in files: defaults to the file name, with `-N` for arrays. |
| `model` | Model id, inference profile, ARN, or a pattern with `*`. A pattern matches the id as sent or the foundation model behind it, so `anthropic.claude-*` matches `us.anthropic.claude-...`. Omit to match every model. |
| `kind` | `converse` (Converse and ConverseStream) or `invoke` (InvokeModel and its stream). Omit to answer both. Required with `response`. |
| `match.contains` | Only prompts containing this text. The prompt is the system prompt, every message's text and every tool result, joined by newlines. |
| `request_hash` | Pins the fixture to one exact request. Recordings always carry it. |
| `text` | The answer, rendered in the right format for the call. |
| `response` | The exact body to return instead: a Converse response for `converse`, the provider's body for `invoke`. Use it for tool calls, stop reasons or providers Stratus does not know. |

**Match order.** Fixtures added through the control API come first, then
files in path order. A fixture with a matching `request_hash` always wins;
otherwise the first fixture whose model and prompt match answers. Put
specific fixtures before catch-alls.

An invalid fixture file fails every Bedrock request with a message naming
the file, rather than being skipped and causing confusing misses.

## Control API

```
GET    /_stratus/control/v1/bedrock                  mode, fixture directory and upstream
PUT    /_stratus/control/v1/bedrock/mode             {"mode": "record" | "replay"}
GET    /_stratus/control/v1/bedrock/fixtures         every fixture in match order, with its origin
POST   /_stratus/control/v1/bedrock/fixtures         add a fixture (same fields as a file)
DELETE /_stratus/control/v1/bedrock/fixtures         remove fixtures added here; files stay
DELETE /_stratus/control/v1/bedrock/fixtures/{id}    remove one fixture added here
GET    /_stratus/control/v1/bedrock/invocations      model calls, oldest first
DELETE /_stratus/control/v1/bedrock/invocations      empty the log
```

Fixtures added through the API live in the `bedrock` service's state, so a
reset clears them and a snapshot keeps them. Without
`STRATUS_BEDROCK_FIXTURES`, recordings are kept the same way.

The invocation log answers "what did my code actually ask the model?". Each
entry has the operation, model, flattened prompt, the answer's text, token
estimates, the fixture that answered and an outcome: `replayed`, `recorded`,
`synthesized` (embeddings), `missed` (no fixture) or `failed`. Filters:
`model` (pattern with `*`), `contains` (prompt substring), `outcome` and
`limit` (newest N).

```sh
curl -s localhost:4566/_stratus/control/v1/bedrock/invocations?outcome=missed
```

## From tests

```ts
import { StratusClient } from "stratus-emulator";
const stratus = new StratusClient();

await stratus.bedrock.addFixture({ model: "anthropic.claude-*", contains: "refund", text: "Five business days." });
await handleSupportTicket("Where is my refund?");
const [call] = await stratus.bedrock.invocations({ contains: "refund" });
expect(call.prompt).toContain("You are a support agent");
```

```python
from stratus_emulator import StratusClient
stratus = StratusClient()

stratus.bedrock.add_fixture("Five business days.", model="anthropic.claude-*", contains="refund")
handle_support_ticket("Where is my refund?")
assert "support agent" in stratus.bedrock.invocations(contains="refund")[0]["prompt"]
```

Coding agents get the same through MCP: `stratus_bedrock_fixture_add` and
`stratus_bedrock_invocations`.

## Not emulated

Guardrails (`ApplyGuardrail`, guardrail headers are ignored), async
invocations, bidirectional streaming, image and video generation models,
prompt management variables, and the Bedrock control plane
(`ListFoundationModels` and other `aws bedrock` commands). Requests carry no
real model behaviour: a fixture answers whatever matches, and Stratus does
not check `max_tokens`, stop sequences or tool schemas.
