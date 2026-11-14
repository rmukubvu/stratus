# SQS

Stratus emulates standard SQS queues over both the query (XML) and JSON
protocols, so the AWS CLI and every current AWS SDK work unchanged.

## Operations

| Operation | Notes |
| --- | --- |
| `CreateQueue`, `GetQueueUrl`, `ListQueues`, `DeleteQueue` | |
| `GetQueueAttributes`, `SetQueueAttributes`, `ListQueueTags` | `RedrivePolicy` moves messages to a dead-letter queue after `maxReceiveCount` receives |
| `SendMessage`, `ReceiveMessage`, `DeleteMessage`, `ChangeMessageVisibility` | Long polling with `WaitTimeSeconds` |
| `SendMessageBatch`, `DeleteMessageBatch`, `ChangeMessageVisibilityBatch` | Each entry succeeds or fails on its own, as in AWS |

FIFO queues, message attributes and queue tags are not implemented yet and
return `NotImplemented`.

## Limits

Stratus enforces the limits that are part of the SQS API, so code that
works against Stratus does not hit them for the first time in AWS. They are
always on; they are not quotas you can raise.

| Limit | Value | Error |
| --- | --- | --- |
| Entries per batch | 1 to 10 | `AWS.SimpleQueueService.EmptyBatchRequest`, `AWS.SimpleQueueService.TooManyEntriesInBatchRequest` |
| Batch entry `Id` | 1 to 80 letters, digits, `-` or `_`; unique within the batch | `AWS.SimpleQueueService.InvalidBatchEntryId`, `AWS.SimpleQueueService.BatchEntryIdsNotDistinct` |
| Total body size of a `SendMessageBatch` | 1 MiB | `AWS.SimpleQueueService.BatchRequestTooLong` |
| Message body | the queue's `MaximumMessageSize` (default 262,144 bytes) | `InvalidParameterValue`, for the request or for the batch entry |
| `MaximumMessageSize` | 1,024 to 1,048,576 bytes | `InvalidAttributeValue` |
| `DelaySeconds` (queue or message) | 0 to 900 | `InvalidAttributeValue` / `InvalidParameterValue` |
| `VisibilityTimeout` (queue or change) | 0 to 43,200 | `InvalidAttributeValue` / `InvalidParameterValue` |
| `ReceiveMessageWaitTimeSeconds` | 0 to 20 | `InvalidAttributeValue` |
| `MessageRetentionPeriod` | 60 to 1,209,600 | `InvalidAttributeValue` |
| `MaxNumberOfMessages` on receive | 1 to 10 | `InvalidParameterValue` |

Errors that make a whole batch malformed (too many entries, a repeated `Id`,
a body total over 1 MiB) fail the request. Errors in one entry (a body that
is too long, an invalid receipt handle, a delay out of range) are reported in
that entry's `Failed` record with `SenderFault: true`, and the other entries
still succeed. Code that ignores `Failed` loses messages here the same way it
would in AWS.

Under IAM enforcement (`STRATUS_IAM=enforce`) a batch operation needs the
single-message action: `SendMessageBatch` is allowed by `sqs:SendMessage`.
