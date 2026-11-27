# SQS

Stratus emulates standard SQS queues over both the query (XML) and JSON
protocols, so the AWS CLI and every current AWS SDK work unchanged.

## Operations

| Operation | Notes |
| --- | --- |
| `CreateQueue`, `GetQueueUrl`, `ListQueues`, `DeleteQueue` | |
| `GetQueueAttributes`, `SetQueueAttributes` | `RedrivePolicy` moves messages to a dead-letter queue after `maxReceiveCount` receives. `SqsManagedSseEnabled`, `KmsMasterKeyId` and `KmsDataKeyReusePeriodSeconds` are kept and read back; messages are not encrypted at rest |
| `TagQueue`, `UntagQueue`, `ListQueueTags` | Also `tags` on `CreateQueue`; at most 50 per queue |
| `SendMessage`, `ReceiveMessage`, `DeleteMessage`, `ChangeMessageVisibility` | Long polling with `WaitTimeSeconds` |
| `SendMessageBatch`, `DeleteMessageBatch`, `ChangeMessageVisibilityBatch` | Each entry succeeds or fails on its own, as in AWS |

FIFO queues and message attributes are not implemented yet and return
`NotImplemented`.

## Lambda event source mappings

A Lambda event source mapping on a queue polls it the way Lambda's pollers
do, through the same receive path as `ReceiveMessage`:

- It receives up to `BatchSize` messages (default 10) and invokes the
  function once with them. With `MaximumBatchingWindowInSeconds`, it keeps
  gathering until the batch is full or the window ends.
- When the function returns, the batch is deleted. When it throws, times
  out or is throttled, nothing is deleted: the messages come back after the
  queue's `VisibilityTimeout` with a higher `ApproximateReceiveCount`, and
  once that passes the `RedrivePolicy`'s `maxReceiveCount` they move to the
  dead-letter queue.
- With `FunctionResponseTypes: ["ReportBatchItemFailures"]` the function can
  fail single messages by returning
  `{"batchItemFailures": [{"itemIdentifier": "<messageId>"}]}`; only those
  stay on the queue. An empty or missing list means every message succeeded,
  and an invalid response (not an object, an empty or unknown
  `itemIdentifier`) fails the whole batch, as in AWS.
- Messages already in the queue when the mapping is created are processed
  too, and a disabled mapping (`UpdateEventSourceMapping` with
  `Enabled: false`) leaves messages in the queue until it is enabled again.
- An idle mapping keeps one poller. With a backlog it scales up to five
  concurrent batches, or `ScalingConfig.MaximumConcurrency` (2 to 1,000).

Retries wait for the visibility timeout, so a queue with the default of 30
seconds retries a failed batch 30 seconds later, as AWS does. Tests that
expect quick retries should set a short `VisibilityTimeout` on the queue.

## Limits

Stratus enforces the limits that are part of the SQS API, so code that
works against Stratus does not hit them for the first time in AWS. They are
always on; they are not quotas you can raise.

| Limit | Value | Error |
| --- | --- | --- |
| Entries per batch | 1 to 10 | `AWS.SimpleQueueService.EmptyBatchRequest`, `AWS.SimpleQueueService.TooManyEntriesInBatchRequest` |
| Batch entry `Id` | 1 to 80 letters, digits, `-` or `_`; unique within the batch | `AWS.SimpleQueueService.InvalidBatchEntryId`, `AWS.SimpleQueueService.BatchEntryIdsNotDistinct` |
| Total body size of a `SendMessageBatch` | 1 MiB | `AWS.SimpleQueueService.BatchRequestTooLong` |
| Message body | the queue's `MaximumMessageSize` (default 1,048,576 bytes, as in AWS since August 2025) | `InvalidParameterValue`, for the request or for the batch entry |
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
