# Cloud Map

Stratus emulates AWS Cloud Map (`servicediscovery`). HTTP namespaces answer
`DiscoverInstances`; DNS namespaces also own a Route 53 hosted zone, and every
registered instance gets its records there, so the names Cloud Map publishes
resolve the way they would on AWS. Every operation finishes before the call
returns: `GetOperation` always reports `SUCCESS`.

```sh
aws servicediscovery create-private-dns-namespace --name corp.local --vpc vpc-1
aws servicediscovery get-operation --operation-id <id>   # Targets.NAMESPACE is the namespace ID

aws servicediscovery create-service --name api --namespace-id <ns-id> \
  --dns-config '{"RoutingPolicy":"MULTIVALUE","DnsRecords":[{"Type":"A","TTL":60},{"Type":"SRV","TTL":60}]}' \
  --health-check-custom-config FailureThreshold=1

aws servicediscovery register-instance --service-id <srv-id> --instance-id api-1 \
  --attributes AWS_INSTANCE_IPV4=10.1.0.1,AWS_INSTANCE_PORT=8080,stage=prod
```

Then check what a client would see:

```sh
curl 'localhost:4566/_stratus/control/v1/dns/resolve?name=api.corp.local&type=SRV'
aws servicediscovery discover-instances --namespace-name corp.local --service-name api --query-parameters stage=prod
```

## Host prefix

`DiscoverInstances` is sent to `data-<host>` by the AWS SDKs, which breaks
when the endpoint is `127.0.0.1`. Set `AWS_DISABLE_HOST_PREFIX_INJECTION=true`
(or `disableHostPrefix: true` in the JavaScript SDK v3). `stratus ci`, the
GitHub Action, `stratus_connection_info` and the SDKs' `aws_env()` /
`awsConfig()` already do.

## Records

For each instance a DNS service publishes one record set per type in its
`DnsConfig`, named `<service>.<namespace>` and told apart by the instance ID,
as Cloud Map does:

| Record | Value comes from |
| --- | --- |
| `A` | `AWS_INSTANCE_IPV4` |
| `AAAA` | `AWS_INSTANCE_IPV6` |
| `CNAME` | `AWS_INSTANCE_CNAME` |
| `SRV` | `1 1 <AWS_INSTANCE_PORT> <instance-id>.<service>.<namespace>`, plus an A record for that host |

`MULTIVALUE` routing creates multivalue answer records; `WEIGHTED` creates
weighted records of equal weight. An instance missing the attribute a record
needs is rejected with `InvalidInput`.

## Health

Services with `HealthCheckCustomConfig` take their health from
`UpdateInstanceCustomHealthStatus`. An `UNHEALTHY` instance's records leave
the zone, and `DiscoverInstances` with `HealthStatus=HEALTHY` skips it; it
comes back when marked `HEALTHY`. Route 53 health checks
(`HealthCheckConfig`) are stored and echoed but not run: those instances stay
healthy.

`DiscoverInstances` returns every matching instance unless the request asks
for `HEALTHY`, `UNHEALTHY` or `HEALTHY_OR_ELSE_ALL`. `QueryParameters` must
all match; `OptionalParameters` narrow the result when any instance matches
them.

## Supported API

| Operation | Notes |
| --- | --- |
| `CreateHttpNamespace`, `CreatePrivateDnsNamespace`, `CreatePublicDnsNamespace` | DNS namespaces create a hosted zone; a private one needs `Vpc`. |
| `GetNamespace`, `ListNamespaces`, `DeleteNamespace` | A namespace with services cannot be deleted. Deleting it removes its zone. |
| `CreateService`, `GetService`, `ListServices`, `DeleteService` | `ListServices` takes the `NAMESPACE_ID` filter. A service with instances cannot be deleted. |
| `RegisterInstance`, `DeregisterInstance`, `GetInstance`, `ListInstances` | Registering an existing instance ID replaces it and its records. |
| `DiscoverInstances` | See above. |
| `GetInstancesHealthStatus`, `UpdateInstanceCustomHealthStatus` | |
| `GetOperation`, `ListOperations` | |
| `ListTagsForResource`, `TagResource`, `UntagResource` | |

`UpdateService`, `UpdateHttpNamespace` and the other update calls are not
implemented.
