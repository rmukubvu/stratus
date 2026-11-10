# Route 53

Stratus emulates Route 53 hosted zones and record sets, and answers DNS
lookups against them through the control API. Changes are applied at once:
every change batch is `INSYNC` when the call returns, so there is nothing to
wait for.

```sh
aws route53 create-hosted-zone --name example.test --caller-reference ref-1

aws route53 change-resource-record-sets --hosted-zone-id <id> --change-batch '{
  "Changes": [
    {"Action": "CREATE", "ResourceRecordSet": {"Name": "db.example.test", "Type": "A", "TTL": 60,
      "ResourceRecords": [{"Value": "10.0.0.7"}]}},
    {"Action": "CREATE", "ResourceRecordSet": {"Name": "api.example.test", "Type": "CNAME", "TTL": 60,
      "ResourceRecords": [{"Value": "db.example.test"}]}}
  ]}'
```

## Resolving names

Stratus does not run a DNS server. Instead the control API resolves a name the
way a resolver inside the VPC would, which is what a test usually wants to
check:

```sh
curl 'localhost:4566/_stratus/control/v1/dns/resolve?name=api.example.test&type=A'
# {"name": "api.example.test.", "type": "A", "found": true, "answers": [
#   {"name": "api.example.test.", "type": "CNAME", "value": "db.example.test.", "ttl": 60, "zone_id": "Z..."},
#   {"name": "db.example.test.", "type": "A", "value": "10.0.0.7", "ttl": 60, "zone_id": "Z..."}]}
```

The same lookup is the `stratus_dns_resolve` MCP tool, `stratus.dns.resolve()`
in the TypeScript SDK and `stratus.dns.resolve()` in the Python SDK.

Resolution follows these rules:

- The most specific zone that contains the name answers. When a public and a
  private zone have the same name, the private zone wins.
- An exact name beats the closest wildcard (`*.example.test`).
- CNAMEs and alias records are followed, across zones, up to 8 steps.
- Multivalue, weighted, latency, failover and geolocation records all return
  every value: routing policies are stored and echoed but not evaluated.
- `found: false` means NXDOMAIN, or the name exists with no records of the
  requested type.

## Supported API

| Operation | Notes |
| --- | --- |
| `CreateHostedZone` | Public and private zones; new zones get NS and SOA records. Reusing a `CallerReference` fails with `HostedZoneAlreadyExists`, as on AWS. |
| `GetHostedZone`, `ListHostedZones`, `ListHostedZonesByName`, `GetHostedZoneCount` | |
| `UpdateHostedZoneComment`, `DeleteHostedZone` | A zone with records besides its apex NS and SOA cannot be deleted (`HostedZoneNotEmpty`). |
| `ChangeResourceRecordSets` | `CREATE`, `UPSERT` and `DELETE`. A batch is atomic: if any change is invalid, none apply and the error is `InvalidChangeBatch`. CNAME conflicts, records outside the zone and deleting a record whose values differ are rejected as on AWS. |
| `ListResourceRecordSets` | DNS order, with `StartRecordName`/`StartRecordType` paging. |
| `GetChange` | Every change is `INSYNC`. |
| `ListTagsForResource`, `ChangeTagsForResource` | |

Health checks, traffic policies, DNSSEC, query logging and VPC association
calls are not implemented.

## Cloud Map zones

A Cloud Map DNS namespace owns a hosted zone of the same name, and registered
instances write records into it. Those zones show up in `ListHostedZones` and
resolve like any other zone. See [cloudmap.md](cloudmap.md).

## Snapshots and reset

Zones and records are metadata, so snapshots capture and restore them like any
other service. Resetting `route53` alone also removes the zones Cloud Map
created; reset both services together to keep them consistent.
