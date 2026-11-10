# Amazon MSK on a real Kafka broker

Every MSK cluster in Stratus is a real Kafka-compatible broker. Creating a
cluster starts a [Redpanda](https://redpanda.com) container, which speaks the
Kafka protocol, needs no ZooKeeper and is ready in a few seconds. The create
call returns once the broker has elected its controller, so the bootstrap
address is ready to use straight away: no waiters, no polling.

```sh
aws kafka create-cluster --cluster-name orders --kafka-version 3.6.0 \
  --number-of-broker-nodes 3 \
  --broker-node-group-info '{"InstanceType":"kafka.m5.large","ClientSubnets":["subnet-1","subnet-2","subnet-3"]}'

aws kafka get-bootstrap-brokers --cluster-arn <arn>
# { "BootstrapBrokerString": "127.0.0.1:41873" }
```

Any Kafka client works against that address: kafka-python, confluent-kafka,
KafkaJS, the Java client, `rpk` or `kcat`.

```python
from kafka import KafkaProducer
producer = KafkaProducer(bootstrap_servers="127.0.0.1:41873")
producer.send("payments", b"order-1 paid")
```

Agents and tests can skip the ARN lookup: the control API, the
`stratus_kafka_clusters` MCP tool and the SDKs list every cluster with its
bootstrap address.

```sh
curl localhost:4566/_stratus/control/v1/kafka/clusters
# {"items": [{"name": "orders", "bootstrap_brokers": "127.0.0.1:41873", ...}], "count": 1}
```

```ts
const brokers = await stratus.kafka.bootstrapBrokers("orders");
```

```python
brokers = stratus.kafka.bootstrap_brokers("orders")
```

## Supported API

| Operation | Notes |
| --- | --- |
| `CreateCluster`, `CreateClusterV2` | Provisioned and serverless clusters both run a real broker. |
| `DescribeCluster`, `DescribeClusterV2` | Echo the configuration you supplied (broker node group, encryption, authentication, monitoring, logging) so Terraform, CDK and CloudFormation see no drift. |
| `ListClusters`, `ListClustersV2` | `ListClusters` returns provisioned clusters; `ListClustersV2` takes `clusterTypeFilter`. Both take `clusterNameFilter` as a prefix. |
| `GetBootstrapBrokers` | Returns `BootstrapBrokerString` (plaintext). |
| `ListNodes` | One broker per cluster. |
| `DeleteCluster` | Stops and removes the broker. |
| `ListTagsForResource`, `TagResource`, `UntagResource` | |

Other operations (configurations, cluster updates, SCRAM secrets, replicators,
VPC connections) return `NotFoundException`.

## Brokers and images

| Setting | Default | Override |
| --- | --- | --- |
| Broker image | `redpandadata/redpanda:v24.2.7` | `STRATUS_KAFKA_IMAGE` |

Stratus never pulls images, so pre-pull the broker in CI:

```yaml
- run: docker pull redpandadata/redpanda:v24.2.7
```

If it is missing, `CreateCluster` fails with `ServiceUnavailable` naming the
image to pull.

Each cluster runs one single-node broker, whatever `NumberOfBrokerNodes`
says. The requested count is reported back unchanged. Topics default to one
replica, so a client that asks for a replication factor above 1 is refused by
the broker; use 1, or leave it unset.

The broker listens in plaintext on a free loopback port and advertises
`127.0.0.1:<port>`, which is what clients on the host connect to. TLS, SASL/SCRAM
and IAM authentication settings are stored and echoed but not enforced, so
point clients at the plaintext address. Lambda functions running in Docker
cannot reach that advertised address yet, so MSK event source mappings are not
supported.

## Lifecycle, reset and snapshots

Brokers follow the same rules as RDS and ElastiCache engines (see
[engines.md](engines.md)):

- `DeleteCluster` and `POST /_stratus/control/v1/reset` for `kafka` (or a
  full reset) stop the broker. The reset response lists `kafka` under
  `engines_stopped`.
- A snapshot captures each broker's topics, records and consumer group
  offsets. A restore brings the broker back on the same bootstrap address
  with that data, and `--seed` forks start from it on their own ports.
- Shutdown removes the brokers, and the next start reaps any left by a crashed
  process. Containers carry `io.stratus.kind=kafka`.
