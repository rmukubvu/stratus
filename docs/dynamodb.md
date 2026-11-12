# DynamoDB

Stratus runs a DynamoDB engine in process. It keeps items in the same state
store as every other service, so snapshots, resets and per-run isolation
cover DynamoDB data too.

## What works

| Area | Supported |
| --- | --- |
| Tables | `CreateTable`, `DescribeTable`, `UpdateTable`, `DeleteTable`, `ListTables` (paginated); partition and sort keys; `PAY_PER_REQUEST` and `PROVISIONED` billing; deletion protection; table classes |
| Indexes | Global and local secondary indexes with `KEYS_ONLY`, `INCLUDE` and `ALL` projections; adding and removing GSIs with `UpdateTable` |
| Items | `PutItem`, `GetItem`, `UpdateItem`, `DeleteItem` with `ReturnValues` (including `UPDATED_OLD` and `UPDATED_NEW`) and `ReturnValuesOnConditionCheckFailure` |
| Expressions | Condition, filter, key condition, projection and update expressions: comparisons, `BETWEEN`, `IN`, `AND`/`OR`/`NOT`, every function (`attribute_exists`, `attribute_type`, `begins_with`, `contains`, `size`), nested paths and list indexes, `SET` arithmetic, `if_not_exists`, `list_append`, `REMOVE`, `ADD`, `DELETE` |
| Reads | `Query` and `Scan` in key order with `Limit`, 1 MB pages, `ExclusiveStartKey`/`LastEvaluatedKey`, `ScanIndexForward`, `Select`, parallel scan segments, `ConsistentRead` |
| Batches | `BatchGetItem` (100 keys, 16 MB responses with `UnprocessedKeys`) and `BatchWriteItem` (25 requests) |
| Transactions | `TransactWriteItems` (all or nothing, `CancellationReasons`, `ClientRequestToken` idempotency for 10 minutes) and `TransactGetItems` |
| Other | TTL (expired items disappear on the next read), point-in-time recovery settings, tags, `ReturnConsumedCapacity`, streams, `x-amz-crc32` response checksums |

Numbers are exact decimals (`1E2` and `100` are the same key), reserved words
must be aliased with `#name` placeholders, and unused placeholders are
refused, as in DynamoDB.

## Not yet

PartiQL (`ExecuteStatement`, `BatchExecuteStatement`, `ExecuteTransaction`),
on-demand backups and restores, global tables, imports and exports, resource
policies, and the legacy `Expected`, `AttributeUpdates`, `KeyConditions`,
`QueryFilter` and `ScanFilter` parameters. Calls to these get an error that
names what is missing. Capacity is reported but never throttles writes; use
[chaos rules](chaos.md) to test throttling.
