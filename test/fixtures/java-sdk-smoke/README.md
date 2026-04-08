# Java SDK Smoke

This fixture proves that the AWS SDK for Java v2 can talk to `stratus` over a
real network boundary.

The smoke path currently covers:

- `STS GetCallerIdentity`
- `DynamoDB CreateTable`
- `DynamoDB PutItem`
- `DynamoDB GetItem`
- `SQS CreateQueue`
- `SQS SendMessage`
- `SQS ReceiveMessage`
- `SQS DeleteMessage`
- `SQS DeleteQueue`
- `S3 CreateBucket`
- `S3 PutObject`
- `S3 GetObject`

## Run

Terminal 1:

```bash
stratus --log-format pretty --log-level debug
```

Terminal 2:

```bash
./scripts/smoke-java-sdk.sh
```

Or run the Maven fixture directly:

```bash
cd test/fixtures/java-sdk-smoke
mvn -q -Dstratus.endpoint=http://127.0.0.1:4566 test
```

The fixture code lives in:

- `src/test/java/com/stratus/fixtures/JavaSDKSmokeTest.java`

This is meant to be a black-box compatibility check, not an in-process unit
test.
