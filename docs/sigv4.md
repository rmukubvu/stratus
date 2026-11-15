# Strict signature mode

By default Stratus accepts any AWS signature, or none, as local emulators
do. That keeps test setup simple, but it also hides a whole class of bugs
until the code meets real AWS: a header changed after signing, a proxy that
re-encodes the path, the wrong region in the credential scope, a presigned
URL used after it expired, a cached signature retried twenty minutes later,
session credentials sent without their token.

Strict signature mode makes Stratus verify every AWS API request's
Signature Version 4 the way AWS does, and refuse what AWS would refuse,
with AWS's own error codes.

```bash
STRATUS_SIGV4=strict stratus        # or: stratus serve --sigv4 strict
stratus ci --sigv4 strict -- npm test
```

| `STRATUS_SIGV4` | Behaviour |
| --- | --- |
| `off` (default) | Signatures are not checked. |
| `audit` | Every request is verified; failures are recorded and logged (`sigv4 verification failed`) but let through. Use it to find signing problems without breaking a run. |
| `strict` | Requests that do not verify are refused as AWS would refuse them. |

It is independent of `STRATUS_IAM` ([iam.md](iam.md)): signature mode
checks *who signed* the request, IAM enforcement checks *what they may
do*. Turn on both to get AWS's full front door.

## What is verified

- **Authorization header** (`AWS4-HMAC-SHA256`): credential scope,
  `SignedHeaders`, signature, `X-Amz-Date` (or `Date`) matching the scope
  date, and `host` being signed.
- **Presigned URLs** (`X-Amz-Algorithm`, `X-Amz-Credential`, `X-Amz-Date`,
  `X-Amz-Expires`, `X-Amz-SignedHeaders`, `X-Amz-Signature`, and
  `X-Amz-Security-Token` for session credentials). `X-Amz-Expires` is
  enforced and may not exceed 604800 seconds (a week).
- **Clock skew.** A request signed more than 15 minutes before or after the
  server clock is refused, as in AWS. Presigned URLs are valid until
  `X-Amz-Date + X-Amz-Expires`.
- **The body.** For every service but S3 the canonical request uses the
  SHA-256 of the body Stratus received, so a body changed after signing
  fails. For S3 the body hash comes from `x-amz-content-sha256`, which signed
  requests must send:
  - a SHA-256 hex digest is checked against the body
    (`XAmzContentSHA256Mismatch`);
  - `UNSIGNED-PAYLOAD` and `STREAMING-UNSIGNED-PAYLOAD-TRAILER` sign no body;
  - `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` (and `-TRAILER`) aws-chunked uploads
    are verified chunk by chunk along the signature chain, including the
    trailer signature, and against `x-amz-decoded-content-length`.
- **S3's extra rules.** The path is signed exactly as sent (other services
  sign it URI-encoded once more), and every `x-amz-*` header must be signed.
- **Session tokens.** STS credentials need their own `X-Amz-Security-Token`;
  an IAM user's long-term key must not send one.

Correctness is pinned by the AWS SigV4 test suite and the S3 signing
examples from the AWS documentation (header, presigned and chunked), and
checked against the AWS CLI, boto3 and the AWS SDK for JavaScript v3.

## Which credentials are known

| Access key | Secret |
| --- | --- |
| `test`, or any twelve-digit account ID ([accounts.md](accounts.md)) | `test`, or `STRATUS_SIGV4_ROOT_SECRET` |
| IAM user keys from `CreateAccessKey` ([iam.md](iam.md)) | the secret IAM returned; inactive keys are refused |
| STS credentials from `AssumeRole` (including Lambda execution-role credentials) | the returned secret, with the session token, until they expire |
| Keys listed in `STRATUS_SIGV4_CREDENTIALS` | the configured secret |

`STRATUS_SIGV4_CREDENTIALS` is for code that signs with fixed fake keys: a
comma-separated list of `ACCESS_KEY:SECRET` or `ACCESS_KEY:SECRET:ACCOUNT`.
With an account, requests signed by that key act in that account.

```bash
STRATUS_SIGV4=strict \
STRATUS_SIGV4_CREDENTIALS="AKIAIOSFODNN7EXAMPLE:wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY:111122223333" \
stratus
```

Root keys and configured keys ignore a stray `AWS_SESSION_TOKEN`, so an
environment left over from real AWS does not fail every call.

## Errors

Each service family gets the codes AWS returns:

| Problem | S3 | Query APIs (STS, SQS, SNS, IAM, ...) | JSON APIs (DynamoDB, KMS, Lambda, ...) |
| --- | --- | --- | --- |
| Signature does not match | `SignatureDoesNotMatch` 403 | `SignatureDoesNotMatch` 403 | `InvalidSignatureException` |
| Unknown or inactive access key | `InvalidAccessKeyId` 403 | `InvalidClientTokenId` 403 | `UnrecognizedClientException` |
| Missing, wrong or unexpected session token | `InvalidToken` 400 | `InvalidClientTokenId` 403 | `UnrecognizedClientException` |
| Expired session credentials | `ExpiredToken` 400 | `ExpiredToken` 403 | `ExpiredTokenException` |
| Malformed Authorization or presigned query | `AuthorizationHeaderMalformed` / `AuthorizationQueryParametersError` 400 | `IncompleteSignature` 400 | `IncompleteSignatureException` |
| Signed more than 15 minutes off | `RequestTimeTooSkewed` 403 | `SignatureDoesNotMatch` ("Signature expired") | `InvalidSignatureException` ("Signature expired") |
| Presigned URL expired | `AccessDenied` ("Request has expired") 403 | `RequestExpired` 400 | `RequestExpired` |
| Not signed at all | `AccessDenied` 403 | `MissingAuthenticationToken` 403 | `MissingAuthenticationTokenException` |

EC2 answers `AuthFailure` (401) for bad keys and signatures, as it does in
AWS. S3 also has `XAmzContentSHA256Mismatch`, `InvalidRequest` (missing
`x-amz-content-sha256`, or SigV2), `InvalidArgument`, `IncompleteBody` (a
malformed aws-chunked body) and `AccessDenied` for unsigned `x-amz-*`
headers.

As in AWS, a signature mismatch says what the server expected. Query and
JSON APIs append the canonical request and string to sign to the message;
S3 returns them as `<CanonicalRequest>` and `<StringToSign>` in the error
body. Diff that canonical request against the one your signer built: the
first line that differs is the bug.

## Finding the bug

`stratus sigv4 status` (or `GET /_stratus/control/v1/sigv4`) shows the
mode, how many requests verified, failed and were skipped, failures by
error code, and the latest 50 failures, each with the request ID, access
key, reason, and the canonical request and string to sign Stratus
expected.

`stratus diagnose` groups refused requests under the `signature` category
with `your_bug: true`: the signer is wrong, and AWS would refuse the same
request.

## Control at runtime

```
PUT    /_stratus/control/v1/sigv4   {"mode":"strict"|"audit"|"off"}; an empty body means strict
GET    /_stratus/control/v1/sigv4   mode, counters and recent failures
DELETE /_stratus/control/v1/sigv4   turn verification off and return the final status
```

`PUT` starts fresh counters. The same is available as `stratus sigv4 set
strict|audit|off`, `stratus sigv4 status` and `stratus sigv4 clear`, the
MCP tools `stratus_sigv4_set` and `stratus_sigv4_status`, and the SDKs'
`signatures` client (`stratus.signatures.set("strict")` in TypeScript and
Python).

## What is not checked

- Health, control and operator endpoints, and invocations of deployed API
  Gateway APIs under `/_aws/`: they are not AWS API calls.
- Operations AWS accepts unsigned when they arrive unsigned:
  `AssumeRoleWithWebIdentity`, `AssumeRoleWithSAML`, and Cognito user-pool
  calls authenticated by tokens (`InitiateAuth`, `SignUp`, `GetUser`, ...).
  If they are signed, the signature is verified.
- Browser uploads with an S3 POST policy (multipart/form-data POSTs): the
  policy signature is not verified yet.
- SigV4a (`AWS4-ECDSA-P256-SHA256`, used for multi-region access points) is
  let through unverified and counted as skipped.
- SigV2 is refused, as S3 refuses it for buckets created since 2020. The
  AWS CLI v1 presigns S3 URLs with SigV2 unless `s3 = signature_version =
  s3v4` is set in its config; v2 uses SigV4.
