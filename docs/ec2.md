# EC2 networking

Stratus emulates the part of EC2 that other services' resources sit in: a
VPC with subnets, an internet gateway, route tables and security groups. It
runs no instances. This is what tests and infrastructure code create before
they place a load balancer, database, cache, Kafka cluster or ECS service.

| Resource | Operations |
| --- | --- |
| VPCs | `CreateVpc`, `DescribeVpcs`, `DeleteVpc`, `ModifyVpcAttribute`, `DescribeVpcAttribute` |
| Subnets | `CreateSubnet`, `DescribeSubnets`, `DeleteSubnet`, `ModifySubnetAttribute` |
| Internet gateways | `CreateInternetGateway`, `DescribeInternetGateways`, `AttachInternetGateway`, `DetachInternetGateway`, `DeleteInternetGateway` |
| Route tables | `DescribeRouteTables`, `CreateRoute`, `DeleteRoute` |
| Security groups | `CreateSecurityGroup`, `DescribeSecurityGroups`, `DeleteSecurityGroup`, `AuthorizeSecurityGroupIngress`/`Egress`, `RevokeSecurityGroupIngress`/`Egress` |
| Other | `DescribeAvailabilityZones`, `DescribeAccountAttributes`, `CreateTags`, `DeleteTags`, `DescribeTags` |

Behaviour follows EC2 where tests depend on it:

- Creating a VPC also creates its main route table (with the `local` route)
  and its `default` security group, both removed with the VPC.
- CIDR blocks must be /16 to /28; a subnet must lie inside its VPC and not
  overlap another subnet (`InvalidSubnet.Range`, `InvalidSubnet.Conflict`).
- Deleting a VPC with subnets, an attached gateway or non-default security
  groups fails with `DependencyViolation`, as does deleting an attached
  gateway or a security group another group's rules reference.
- `Describe*` calls accept IDs and `Filter.N` filters, including `tag:<key>`,
  `tag-key`, `vpc-id`, `group-name` and `association.main`, with `*` and `?`
  wildcards. An unknown ID is an `Invalid…ID.NotFound` error.
- Create calls accept `TagSpecification` tags.
- Each region has three availability zones, `<region>a` to `<region>c`.

Errors use EC2's `<Response><Errors><Error>` envelope, so SDKs read the
error codes.
