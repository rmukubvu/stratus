# Container image

Each release publishes a multi-arch image (linux/amd64 and linux/arm64) to
the GitHub Container Registry:

```sh
docker run --rm -p 4566:4566 ghcr.io/rmukubvu/stratus:latest
```

Tags are the release version without the `v` (for example `0.3.0`) and
`latest` for the newest non-prerelease. The image runs
`stratus serve --port 4566 --data-dir /var/lib/stratus --log-format json`,
declares `/var/lib/stratus` as a volume and has a health check on
`/_stratus/health`. Any `stratus serve` flags can be passed as the command.

## Docker Compose

```yaml
services:
  stratus:
    image: ghcr.io/rmukubvu/stratus:latest
    ports:
      - "4566:4566"
    environment:
      STRATUS_IAM: "off"        # or audit / enforce
    volumes:
      - stratus-data:/var/lib/stratus   # drop this for a fresh state each run

  app:
    build: .
    depends_on:
      stratus:
        condition: service_healthy
    environment:
      AWS_ENDPOINT_URL: http://stratus:4566
      AWS_ACCESS_KEY_ID: test
      AWS_SECRET_ACCESS_KEY: test
      AWS_REGION: us-east-1

volumes:
  stratus-data:
```

S3 clients inside the compose network should use path-style addressing
(`http://stratus:4566/<bucket>/<key>`), since `<bucket>.stratus` does not
resolve.

## Limits

Services that run in memory (S3, KMS, SQS, SNS, DynamoDB, SES, Secrets
Manager, IAM and the rest) work fully in the container. Services that start
their own containers (Lambda, RDS, ElastiCache, MSK) do not yet: even with
`/var/run/docker.sock` mounted, Stratus reaches those containers on
`127.0.0.1:<published port>`, which inside its own container is not the
host. Run the native binary (or `stratus ci`) when you need them.
