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
      STRATUS_SIGV4: "off"      # or audit / strict
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

## Lambda, RDS, ElastiCache and MSK

Services that run in memory (S3, KMS, SQS, SNS, DynamoDB, SES, Secrets
Manager, IAM and the rest) need nothing else. Services that start their own
containers (Lambda, RDS, ElastiCache, MSK) need the host's Docker socket:

```yaml
services:
  stratus:
    image: ghcr.io/rmukubvu/stratus:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
```

Stratus notices it is running in a container that the daemon knows, and
starts its containers on a Docker network it shares with them, so it reaches
them by name instead of through published ports. Under Compose that is the
project's network, so other services can use the endpoints the API returns
(`stratus-<instance>-rds-orders:5432`) directly. Lambda code and layers are
copied into each function container, because the daemon cannot see paths
inside Stratus's container. See
[Runners in containers](ci.md#runners-in-containers-and-kubernetes) for the
details and the settings that override detection.
