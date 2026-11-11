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

By default policies are stored but not enforced: every caller can do
everything, which is what most test suites want. Set `STRATUS_IAM` to
check them:

| `STRATUS_IAM` | Behaviour |
| --- | --- |
| `off` (default) | Policies are not evaluated. |
| `audit` | Requests policies would refuse are logged (`iam refused request`) and let through. Use it to find missing permissions without breaking a run. |
| `enforce` | Requests policies do not allow fail with `AccessDenied` (`AccessDeniedException` for JSON APIs), as in AWS. |

What is checked:

- **Who.** IAM users (`AKIA…` keys) and assumed roles (`ASIA…` keys) are
  evaluated against their identity-based policies: inline policies and the
  default versions of attached managed policies. The account root (a
  twelve-digit or unknown key, such as `test`) is never refused, so
  existing suites keep working with enforcement on.
- **How.** AWS's evaluation logic: an explicit `Deny` wins, then any
  `Allow`, otherwise the request is implicitly denied. `Action`/`NotAction`,
  `Resource`/`NotResource`, wildcards, policy variables such as
  `${aws:username}`, and conditions (`String*`, `Arn*`, `Numeric*`, `Date*`,
  `Bool`, `IpAddress`, `Null`, `…IfExists`, `ForAnyValue:`/`ForAllValues:`)
  are supported. Global condition keys include `aws:SourceIp`,
  `aws:RequestedRegion`, `aws:SecureTransport`, `aws:CurrentTime`,
  `aws:PrincipalArn`, `aws:PrincipalAccount`, `aws:PrincipalType` and
  `aws:username`.
- **Credentials.** A deactivated access key fails with
  `InvalidClientTokenId` and an expired session with `ExpiredToken`.
  `sts:GetCallerIdentity` needs no permission, as in AWS.

- **Trust policies.** With `audit` or `enforce`, `sts:AssumeRole` needs
  the role to exist and its trust policy to allow the caller (this applies
  to an account root too), on top of the caller's own permission to call
  `sts:AssumeRole`. A trust policy can name a user or role ARN, an account
  ID or `arn:aws:iam::<account>:root`, and use conditions.
- **Resource policies.** S3 bucket policies (`put-bucket-policy`,
  `get-bucket-policy`, `delete-bucket-policy`), SQS and SNS `Policy`
  attributes, and Lambda permissions (`add-permission`) are evaluated with
  the caller's identity policies: within an account either one can allow a
  request, and an explicit `Deny` in either refuses it. `Principal` and
  `NotPrincipal` are matched against the caller.

Resources are named from the request for S3, SQS, SNS, DynamoDB, Lambda,
SSM, KMS (key IDs and ARNs), Secrets Manager (ARNs), Kinesis, CloudWatch
Logs, Step Functions, EventBridge rules, IAM and STS. For other requests
Stratus cannot name the resource, so statements are matched on their action
and conditions only. An operation Stratus cannot name is checked as
`<service>:*`, so only a caller allowed the whole service may call it.
