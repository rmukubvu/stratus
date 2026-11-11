# IAM

Stratus models IAM per account (see [accounts.md](accounts.md)): roles,
users, access keys and managed policies live in one account and are visible
from every region.

## Identities

| Resource | Operations |
| --- | --- |
| Roles | `CreateRole`, `GetRole`, `ListRoles`, `DeleteRole`, inline `Put/Get/List/DeleteRolePolicy` |
| Users | `CreateUser`, `GetUser`, `ListUsers`, `DeleteUser`, inline `Put/Get/List/DeleteUserPolicy` |
| Access keys | `CreateAccessKey`, `ListAccessKeys`, `UpdateAccessKey`, `DeleteAccessKey` (two per user) |
| Managed policies | `CreatePolicy`, `GetPolicy`, `ListPolicies`, `DeletePolicy`, `Create/Get/List/DeletePolicyVersion`, `SetDefaultPolicyVersion` |
| Attachments | `Attach/DetachRolePolicy`, `Attach/DetachUserPolicy`, `ListAttachedRolePolicies`, `ListAttachedUserPolicies` |

Deleting a role, user or policy that still has policies, keys or
attachments fails with `DeleteConflict`, as in AWS.

## Credentials

A request's caller is decided by its access key:

| Access key | Caller |
| --- | --- |
| Twelve digits, e.g. `111111111111` | The root user of that account |
| `AKIA…` from `CreateAccessKey` | That IAM user, in the user's account |
| `ASIA…` from `sts assume-role` | The assumed role, in the role's account |
| Anything else (`test`) | The root user of `000000000000` |

`sts get-caller-identity` reports the caller, so code that checks who it
runs as sees the same ARNs it would in AWS. Keys are kept in the data
directory and survive restarts and snapshots.

## AWS managed policies

`arn:aws:iam::aws:policy/...` policies can be attached and read with
`GetPolicy` and `GetPolicyVersion`. Stratus ships the documents of the common
ones (`AdministratorAccess`, `PowerUserAccess`, `ReadOnlyAccess`,
`AmazonS3FullAccess`, `AmazonSQSFullAccess`, `AWSLambdaBasicExecutionRole`
and others). Any other AWS managed policy can still be attached, so templates
that reference it deploy, but it grants nothing once policies are enforced.

## Enforcement

Policies are stored but not enforced yet; every caller can do everything.
