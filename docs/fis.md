# AWS Fault Injection Service (FIS) API

Stratus speaks the AWS Fault Injection Service API, so experiment templates
you already run against AWS can run against Stratus unchanged, from the
AWS CLI, any AWS SDK, CloudFormation-generated JSON or your own tooling.
Each experiment runs on Stratus's [deterministic chaos engine](chaos.md),
so a failing run can be replayed with the same seed.

```sh
export AWS_ENDPOINT_URL=http://localhost:4566
aws fis create-experiment-template --cli-input-json file://template.json
aws fis start-experiment --experiment-template-id EXT... --tags stratus:seed=42
aws fis get-experiment --id EXP...
aws fis stop-experiment --id EXP...
```

Requests are recognised by their SigV4 signing name (`fis`) or a `fis.*`
host, so the FIS paths never collide with S3 buckets.

## Example

A minute of slow checkout invocations, then a minute where a quarter of
them fail:

```json
{
  "description": "checkout degrades, then fails",
  "targets": {
    "checkout": {
      "resourceType": "aws:lambda:function",
      "resourceArns": ["arn:aws:lambda:us-east-1:000000000000:function:checkout"],
      "selectionMode": "ALL"
    }
  },
  "actions": {
    "slow": {
      "actionId": "aws:lambda:invocation-add-delay",
      "targets": {"Functions": "checkout"},
      "parameters": {"duration": "PT1M", "startupDelayMilliseconds": "800"}
    },
    "fail": {
      "actionId": "aws:lambda:invocation-error",
      "targets": {"Functions": "checkout"},
      "parameters": {"duration": "PT1M", "invocationPercentage": "25"},
      "startAfter": ["slow"]
    }
  },
  "stopConditions": [{"source": "none"}],
  "roleArn": "arn:aws:iam::000000000000:role/fis"
}
```

## Supported operations

| Operation | Notes |
| --- | --- |
| `CreateExperimentTemplate` | The template is validated when it is created, so mistakes surface before a test run. |
| `GetExperimentTemplate`, `ListExperimentTemplates`, `DeleteExperimentTemplate` | |
| `UpdateExperimentTemplate` | `targets` and `actions` replace the template's whole maps. |
| `StartExperiment` | Replaces any running chaos experiment, FIS or native. |
| `GetExperiment`, `ListExperiments` | Experiment and per-action state, with start and end times. |
| `StopExperiment` | |
| `ListActions`, `GetAction` | The actions below, with their parameters. |
| `ListTargetResourceTypes` | |

Templates and experiments live in Stratus's state, so the control API's
reset, snapshot and restore cover them (service name `fis`).

## Supported actions

| Action | Effect in Stratus |
| --- | --- |
| `aws:fis:wait` | Waits. Use it with `startAfter` to sequence actions. |
| `aws:lambda:invocation-error` | Target functions are not run and fail with a function error (`X-Amz-Function-Error: Unhandled`). Applies to `Invoke` calls and to event-driven invocations (SQS, SNS, EventBridge, Step Functions). `preventExecution` must be `true`. |
| `aws:lambda:invocation-add-delay` | Delays invocations of the target functions by `startupDelayMilliseconds` (default 1000), however they are invoked. |
| `aws:fis:inject-api-internal-error` | Returns `InternalFailure` from the `service`'s `operations`. |
| `aws:fis:inject-api-throttle-error` | Returns the service's throttling error. |
| `aws:fis:inject-api-unavailable-error` | Returns `ServiceUnavailable`. |

AWS limits the `inject-api` actions to a few services; Stratus accepts any
service it emulates. Their `Roles` target is accepted but not matched,
because Stratus does not track caller identity: the faults apply to every
caller.

### Stratus extensions

These actions fault the event deliveries Stratus makes internally, which
FIS cannot reach in AWS. They take no targets; parameters narrow them:

| Action | Effect |
| --- | --- |
| `stratus:delivery:drop` | Loses deliveries. |
| `stratus:delivery:duplicate` | Delivers events twice. |
| `stratus:delivery:delay` | Delays deliveries by `delayMilliseconds` plus up to `jitterMilliseconds`. Async events can arrive out of order. |
| `stratus:delivery:error` | Fails deliveries with `statusCode` (default 500) and `errorCode` (default `InternalFailure`). |

| Parameter | Meaning |
| --- | --- |
| `source` | Service making the delivery: `sqs`, `kinesis`, `dynamodb`, `sns`, `events`, `stepfunctions` or `lambda`. |
| `service` | Service receiving it: `lambda`, `sqs`, `sns` or `stepfunctions`. |
| `operation` | `Invoke`, `SendMessage`, `Publish` or `StartExecution`. |
| `resource` | Substring of the source or target ARN. |
| `percentage` | Share of matching deliveries to fault, (0, 100]. |
| `duration` | ISO 8601 duration. |

See [Event deliveries](chaos.md#event-deliveries) for which paths exist and
what each fault does to them.

## Targets

`aws:lambda:function` targets select functions by `resourceArns`; leave
them out to select every function. `selectionMode` can be `ALL`,
`COUNT(n)` or `PERCENT(n)`; selection is deterministic (the first functions
by name). `resourceTags` and `filters` are rejected rather than ignored.

## Timing

Every action runs for its `duration` (ISO 8601, such as `PT5M`), starting
once all of its `startAfter` actions have finished. The experiment
completes when the last action does. An action without a duration runs
until the experiment is stopped, and nothing can start after it.

## Reproducing a run

FIS has no seed, so Stratus reads one from the `stratus:seed` tag, on the
template or in `StartExperiment` (which wins). Without one it picks a seed
and reports it in the experiment's `stratus:seed` tag. Start the experiment
again with that tag to inject the same faults into the same sequence of
calls.

The experiment also shows up in `stratus chaos status`, with one rule per
action (`<action>` or `<action>/<n>`), so hits and the `X-Stratus-Fault`
header name the FIS action that caused them.

## Limits

- One chaos experiment runs at a time. Starting an FIS experiment stops
  the running one, which then reports `stopped`.
- Stop conditions are stored but not evaluated: Stratus has no CloudWatch
  alarms to watch.
- Experiment state is derived when you ask for it. After a Stratus
  restart, experiments that were running report `stopped`.
- `clientToken` is accepted and not used for idempotency. Tagging
  operations, experiment options, log configuration, target account
  configurations and safety levers are not implemented.
