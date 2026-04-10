# Stratus Portal V1

## Goal

Build a small local operator portal for `stratus` that helps developers inspect emulator state quickly without turning Stratus into a clone of the AWS Console.

The portal should answer four questions fast:

1. Is Stratus healthy?
2. What resources exist right now?
3. What is the emulator doing?
4. Where did the last failure happen?

## Product Position

This is an operator cockpit, not a cloud console.

Good:

- fast local inspection
- service inventory
- request activity
- logs and runtime visibility
- safe developer actions

Bad:

- pixel-copying AWS Console
- deep CRUD for every service in v1
- frontend abstractions that outrun the emulator
- any requirement that the portal must exist for Stratus to be useful

## Stack

- React
- Next.js App Router
- `shadcn/ui`
- Tailwind CSS
- `lucide-react`
- `@tanstack/react-query`

No Bubble Tea equivalent is needed here because this is the browser companion, not the terminal operator mode.

## Deployment Model

Two acceptable modes:

1. Separate app first

- repo or package: `stratus-portal`
- local dev server talks to Stratus over HTTP
- lowest risk

2. Embedded later

- build static assets
- serve from Stratus on a local route such as `/_stratus/ui`

V1 recommendation: separate app first.

That keeps the Go binary simple and lets the UI evolve independently while the operator API stabilizes.

## V1 Information Architecture

### Top Navigation

- `Overview`
- `Services`
- `Logs`
- `Activity`
- `Runtimes`
- `Errors`

### Overview Page

Purpose: one-screen health and system summary.

Widgets:

- Stratus status
  - endpoint
  - data dir
  - uptime
  - log level
- request counters
  - total
  - 2xx
  - 4xx
  - 5xx
- top services
  - sorted by recent request count
- recent failures
  - last 10 failed requests
- active Lambda runtimes
  - function name
  - warm/cold
  - last invoke

### Services Page

Purpose: browse service state without dropping to the CLI.

Layout:

- left rail: service list with counts
- main panel: resource table for selected service
- right panel: detail card for selected resource

Initial service coverage:

- S3
  - buckets
  - recent objects by bucket
- Lambda
  - functions
  - versions
  - aliases
  - event source mappings
- SQS
  - queues
  - key attributes
- DynamoDB
  - tables
- IAM
  - roles
- CloudFormation
  - stacks
- API Gateway v2
  - APIs
  - routes
- EventBridge
  - buses
  - rules

V1 rule: list and inspect only. No generic editing UI.

### Logs Page

Purpose: CloudWatch-inspired log browsing for local debugging.

Design direction:

- familiar AWS-like log stream layout
- darker log canvas within the light portal shell is fine
- keep search and filtering prominent

Core panels:

- log groups list
- log streams list
- live event viewer

Capabilities:

- filter by service
- filter by request ID
- filter by function name
- search message text
- pin the latest stream
- copy request ID / log line

V1 scope:

- CloudWatch Logs resources from the emulator
- synthesized request activity stream from the Stratus front door

### Activity Page

Purpose: browser version of the terminal request stream.

Table columns:

- time
- service
- operation
- method
- path
- status
- duration
- request ID

Filters:

- status class
- service
- operation
- path text

Selecting a row opens:

- request headers summary
- protocol classification
- SigV4 metadata if present
- normalized error if failed

### Runtimes Page

Purpose: make Lambda and event-driven execution inspectable.

Initial widgets:

- warm containers
- last cold start time
- last invoke result
- event source mappings
- async destination summaries

For each function:

- runtime
- timeout
- memory
- reserved concurrency
- handler
- last invoke status
- recent logs

### Errors Page

Purpose: fastest route to emulator debugging.

Sections:

- unsupported API calls
- recent 4xx
- recent 5xx
- CloudFormation failures
- Lambda runtime failures

Each error should show:

- timestamp
- service/operation
- request ID
- normalized AWS-shaped error
- short remediation note where possible

## Backend API Surface

V1 should add a small read-only operator API under:

- `/_stratus/operator/*`

Suggested endpoints:

- `GET /_stratus/operator/overview`
- `GET /_stratus/operator/activity`
- `GET /_stratus/operator/errors`
- `GET /_stratus/operator/services`
- `GET /_stratus/operator/services/{service}/resources`
- `GET /_stratus/operator/logs/groups`
- `GET /_stratus/operator/logs/streams?group=...`
- `GET /_stratus/operator/logs/events?group=...&stream=...`
- `GET /_stratus/operator/lambda/runtimes`

Response design rules:

- JSON only
- stable resource identifiers
- compact payloads
- pagination hooks from day one

## Backend Data Sources

The portal should reuse data Stratus already has instead of inventing a second state model.

Primary sources:

- request/activity data from the HTTP front door
- persisted service metadata from Bolt buckets
- CloudWatch Logs state already stored by the logs service
- Lambda runtime manager state for warm containers and recent invokes

The key addition is an in-process operator store that keeps a rolling window of:

- recent requests
- recent failures
- recent unsupported operations

Suggested retention:

- 1,000 recent requests in memory
- 250 recent errors in memory
- no persistence required for request history in v1

## New Internal Subsystem

Add:

- `internal/operator/`

Suggested files:

- `internal/operator/store.go`
- `internal/operator/types.go`
- `internal/operator/activity.go`
- `internal/operator/errors.go`
- `internal/operator/services.go`
- `internal/operator/logs.go`
- `internal/operator/lambda.go`

Responsibilities:

- capture recent request summaries
- expose read models for the portal
- query existing service metadata safely

## Front Door Integration

The current request path in [`/Users/robson/awsdev/stratus-git/internal/httpapi/server.go`](/Users/robson/awsdev/stratus-git/internal/httpapi/server.go) already has the information the portal needs:

- service
- operation
- method
- path
- status
- duration
- request ID

V1 should record that into the operator store after each request completes.

That is a small, high-leverage change and does not require changing service semantics.

## UI Component List

Recommended `shadcn/ui` pieces:

- `Card`
- `Table`
- `Tabs`
- `Sheet`
- `Badge`
- `ScrollArea`
- `Input`
- `Select`
- `Separator`
- `Tooltip`
- `Skeleton`

Custom components:

- `ServicePill`
- `RequestStatusBadge`
- `MetricStat`
- `LogEventList`
- `ResourceTable`
- `RuntimeCard`
- `FailurePanel`

## Visual Direction

Use a premium operator aesthetic, not a generic admin dashboard.

Guidelines:

- restrained shell with stronger contrast in data panes
- service color accents, not rainbow overload
- monospaced treatment for logs, request IDs, ARNs, and paths
- subtle gradients and panel depth
- compact density where users are scanning operational data

CloudWatch inspiration is appropriate for the logs view only.

Do not copy AWS branding or console structure directly.

## Safe Actions For V1

V1 may include a few safe controls:

- clear request history
- copy resource ARN/name
- open related logs for a Lambda or API
- jump from CloudFormation stack to created resources

Avoid in v1:

- destructive resource deletion
- editing service state through the portal
- mutating queue contents

## V1 Milestones

### Mission P0: Operator API

- add in-memory operator store
- capture request summaries
- add overview, activity, and errors endpoints

Acceptance:

- browser or curl can read recent requests and failures

### Mission P1: Read-Only Portal Shell

- create Next.js app with shadcn
- build Overview, Activity, and Errors pages

Acceptance:

- local portal can connect to a running Stratus instance and render live state

### Mission P2: Services Explorer

- add resource listing endpoints
- build service/resource browsing for S3, Lambda, SQS, DynamoDB, CloudFormation

Acceptance:

- user can locate a resource without using AWS CLI

### Mission P3: CloudWatch-Style Logs

- expose log groups, streams, and events
- add request-ID and function filters

Acceptance:

- user can debug a failed Lambda or event flow entirely from the portal

### Mission P4: Runtime Visibility

- show Lambda warm container state and recent invokes

Acceptance:

- user can tell cold vs warm behavior and inspect recent runtime failures

## What Comes Later

Not for v1:

- write/mutate workflows
- Terraform/CDK import visualizations
- Preflight findings embedded in the portal
- traces or sequence diagrams
- replaying requests from the browser

Those are good v2 candidates once the operator API is stable.

## Recommendation

Build the portal.

But build it as a small read-only operator companion with a CloudWatch-style logs page, not as a local AWS Console clone.

The best first slice is:

1. Overview
2. Activity
3. Errors
4. Logs
5. Services explorer

That gives Stratus a genuinely useful human interface without compromising the compatibility-first runtime model.
