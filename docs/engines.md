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
  cluster in the restored state, from the data captured in the snapshot, on
  the port it had when the snapshot was taken. Endpoints are stable across
  restores.
- Shutdown removes every engine the process started, with its data volume,
  and the next start removes anything a crashed process left behind, the
  same way Lambda containers are reaped. A persistent `stratus serve`
  restarts the engines for its recorded instances on boot; they start empty
  unless the data directory was just seeded, so take a snapshot to keep
  data across restarts.

Containers carry the `io.stratus.kind` (`rds`, `elasticache`) and
`io.stratus.resource` labels, so `docker ps --filter label=io.stratus.kind=rds`
shows what is running.

## What a snapshot captures

A snapshot captures both the control plane (which instances and clusters
exist, their engines, credentials and endpoints) and the data inside the
engines. Seed your schema and rows once, snapshot, and every restore or
seeded job starts with them:

```sh
psql -h 127.0.0.1 -p 49312 -U app orders -f schema.sql -f fixtures.sql
redis-cli -p 49313 set feature:checkout on
stratus snapshot save seeded
# ... tests write, drop and flush whatever they like ...
stratus snapshot restore seeded    # rows and keys are back, same ports
```

How each engine is captured:

| Engine | What is copied | How it is made consistent |
| --- | --- | --- |
| PostgreSQL | `PGDATA` (`/var/lib/postgresql/data`) | container paused during the copy; crash recovery on start |
| MySQL, MariaDB | `/var/lib/mysql` | container paused during the copy; InnoDB crash recovery on start |
| Valkey | `/data` | `SAVE` writes `dump.rdb`, then the container is paused during the copy |
| Memcached | nothing | memory only, as on AWS; restores empty |

The copy is taken while the engine is paused, so it is the same image the
disk would hold after a sudden power loss. Committed transactions are
durable and the engine recovers on its next start, which is how restores
stay fast: no SQL is replayed. Clients are not disconnected; queries simply
wait for the moment the pause lasts.

Each engine is stored as `engines/<service>/<resource>.tar.gz` inside the
snapshot directory and counts toward `size_bytes`. The snapshot lists what it
captured under `engines`. An engine that was not running when the snapshot
was taken (for example because Docker was down) is left out and restores
empty. Snapshots taken before engine data was captured still restore; their
engines start empty.

### Seeded jobs

`stratus ci --seed <snapshot>` and `stratus serve --seed <snapshot>` start
every engine from the snapshot's data too. Each seeded Stratus publishes its
engines on new ports, so many jobs seeded from one snapshot run side by side;
read the endpoints from the API as usual. The seeded data is used on the
first boot and then dropped from the data directory.

## Not covered yet

Aurora clusters (`CreateDBCluster`), `ModifyDBInstance`, read replicas,
parameter groups, ElastiCache replication groups and serverless caches are
not implemented. Calls to them return `NotImplemented`.
