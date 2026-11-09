# Real database and cache engines

RDS instances and ElastiCache clusters are real servers. `CreateDBInstance`
starts a PostgreSQL, MySQL or MariaDB container and `CreateCacheCluster`
starts a Valkey (Redis-compatible) or Memcached container. The call returns
once the engine accepts connections, so the endpoint in the response is ready
to use straight away: no waiters, no polling.

```sh
aws rds create-db-instance --db-instance-identifier orders-db \
  --engine postgres --master-username app --master-user-password secret123 \
  --db-name orders

aws rds describe-db-instances --db-instance-identifier orders-db \
  --query 'DBInstances[0].Endpoint'
# { "Address": "127.0.0.1", "Port": 49312, ... }

PGPASSWORD=secret123 psql -h 127.0.0.1 -p 49312 -U app orders -c 'select 1'
```

```sh
aws elasticache create-cache-cluster --cache-cluster-id sessions --engine redis
aws elasticache describe-cache-clusters --cache-cluster-id sessions \
  --show-cache-node-info --query 'CacheClusters[0].CacheNodes[0].Endpoint'
redis-cli -p 49313 ping
```

## Engines and images

| Service | Engine names | Default image | Override |
| --- | --- | --- | --- |
| RDS | `postgres`, `aurora-postgresql` | `postgres:16-alpine` | `STRATUS_RDS_POSTGRES_IMAGE` |
| RDS | `mysql`, `aurora-mysql` | `mysql:8.0` | `STRATUS_RDS_MYSQL_IMAGE` |
| RDS | `mariadb` | `mariadb:11` | `STRATUS_RDS_MARIADB_IMAGE` |
| ElastiCache | `redis`, `valkey` | `valkey/valkey:8` | `STRATUS_ELASTICACHE_REDIS_IMAGE` |
| ElastiCache | `memcached` | `memcached:1.6-alpine` | `STRATUS_ELASTICACHE_MEMCACHED_IMAGE` |

Stratus never pulls images. Like Lambda runtimes, an engine image must be
present locally, so test runs stay deterministic and work offline. If it is
missing, the create call fails with a `ServiceUnavailable` error that names
the image to `docker pull`. Pre-pull the engines you use in CI:

```yaml
- run: docker pull postgres:16-alpine && docker pull valkey/valkey:8
```

## Endpoints

Every engine is published on a free loopback port, so parallel Stratus
instances (for example several `stratus ci` jobs) never collide. Read the
port from the API; the `Port` parameter on create is accepted but not
honoured. The address is `127.0.0.1`. Code running inside a Lambda container
reaches the same engine at `host.docker.internal:<port>`.

`MasterUsername` and `MasterUserPassword` are required and become the
engine's credentials. `DBName` is created on first start; PostgreSQL defaults
to `postgres`. The password is never returned by the API.

Stratus runs one node per cluster. `NumCacheNodes` is accepted but the
cluster always reports a single node, whose endpoint is the one to use.
Memcached clusters also expose it as the `ConfigurationEndpoint`.

## Lifecycle and cleanup

An engine lives exactly as long as its record:

- `DeleteDBInstance` and `DeleteCacheCluster` stop and remove the container.
- `POST /_stratus/control/v1/reset` for `rds` or `elasticache` (or a full
  reset) stops the service's engines along with its state. The response lists
  them under `engines_stopped`.
- A snapshot restore stops every engine and starts one for each instance and
  cluster in the restored state, on the port it had when the snapshot was
  taken. Endpoints are stable across restores.
- Shutdown removes every engine the process started, and the next start
  removes anything a crashed process left behind, the same way Lambda
  containers are reaped. A persistent `stratus serve` restarts the engines
  for its recorded instances on boot.

Containers carry the `io.stratus.kind` (`rds`, `elasticache`) and
`io.stratus.resource` labels, so `docker ps --filter label=io.stratus.kind=rds`
shows what is running.

## What a snapshot captures

A snapshot captures the control plane: which instances and clusters exist,
their engines, credentials and endpoints. It does not capture the data inside
the engines. A restored instance comes back empty, with the same endpoint and
credentials, so seed scripts that create schema and rows must run after a
restore, not before the snapshot. Capturing engine data in snapshots is a
planned follow-up.

## Not covered yet

Aurora clusters (`CreateDBCluster`), `ModifyDBInstance`, read replicas,
parameter groups, ElastiCache replication groups and serverless caches are
not implemented. Calls to them return `NotImplemented`.
