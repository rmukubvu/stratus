# S3 versioning and Object Lock

Stratus enforces Object Lock the way S3 does, so a test can prove that an
audit record cannot be deleted or have its retention shortened.

## Versioning

`PutBucketVersioning` with `Status` `Enabled` turns on versioned storage:

- Every `PutObject`, `CopyObject` and `CompleteMultipartUpload` adds a
  version and returns `x-amz-version-id`. Earlier versions stay readable with
  `GetObject`/`HeadObject` `?versionId=`.
- `DeleteObject` without a version ID adds a delete marker. The key then
  reads as `NoSuchKey` with `x-amz-delete-marker: true`, but every version is
  still there.
- `DeleteObject` with `?versionId=` permanently deletes that version. If it
  was the current one, the next newest version becomes current again.
- `ListObjectVersions` (`GET ?versions`) lists versions and delete markers,
  newest first, in one page.
- An object written before versioning was enabled is the `null` version.
- `Suspended` writes and delete markers replace the `null` version.
- A bucket with versions or delete markers left is `BucketNotEmpty`.

## Object Lock

Create the bucket with Object Lock (`x-amz-bucket-object-lock-enabled: true`,
or `aws s3api create-bucket --object-lock-enabled-for-bucket`). That also
enables versioning, which then cannot be suspended. `PutObjectLockConfiguration`
on an existing bucket needs versioning `Enabled` first, as in S3.

A version can carry retention (`GOVERNANCE` or `COMPLIANCE` until a date) and
a legal hold. Set them with the `x-amz-object-lock-mode`,
`x-amz-object-lock-retain-until-date` and `x-amz-object-lock-legal-hold`
headers on `PutObject`, `CopyObject` or `CreateMultipartUpload`, with
`PutObjectRetention` / `PutObjectLegalHold` afterwards, or through the
bucket's default retention (`Rule.DefaultRetention` with `Days` or `Years`).

What is enforced:

| Request | Legal hold ON | COMPLIANCE, not expired | GOVERNANCE, not expired |
|---|---|---|---|
| `DeleteObject ?versionId=` | `AccessDenied` | `AccessDenied` | `AccessDenied` unless `x-amz-bypass-governance-retention: true` |
| `PutObjectRetention` extending the date, same mode | allowed | allowed | allowed |
| `PutObjectRetention` GOVERNANCE to COMPLIANCE, not shorter | allowed | n/a | allowed |
| `PutObjectRetention` shortening, removing or weakening | n/a | `AccessDenied` | `AccessDenied` unless bypass |
| `DeleteObject` without a version ID, or overwriting the key | allowed: adds a delete marker or a new version; the locked version stays |

Once retention expires the version can be deleted and its retention changed
freely. Lock headers or retention requests on a bucket without Object Lock
are `InvalidRequest`; a retain-until date in the past is `InvalidArgument`.

Retention is evaluated against the Stratus server clock.

With `STRATUS_IAM=enforce`, a request that sends the bypass header also needs
`s3:BypassGovernanceRetention`, and deleting or reading a specific version
needs `s3:DeleteObjectVersion` / `s3:GetObjectVersion`, as in AWS.

## Conditional writes

`PutObject` and `CompleteMultipartUpload` honour the S3 conditional-write
headers, which pair well with audit and checkpoint buckets:

- `If-None-Match: *` writes only when the key has no current object (a key
  whose latest version is a delete marker counts as absent); otherwise
  `412 PreconditionFailed`. Any other `If-None-Match` value is
  `501 NotImplemented`, as in S3.
- `If-Match: <etag>` writes only over the object with that ETag; a different
  object is `412 PreconditionFailed` and a missing key is `NoSuchKey`.

Conditional writes to the same key are serialised, so when several writers
race with `If-None-Match: *` exactly one wins.
