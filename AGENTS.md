# Working on Stratus

Stratus is a local AWS emulator in Go, built for CI runs and coding agents.
This file is the working guide for anyone, human or agent, changing it. The
user-facing story is in [README.md](README.md) and [docs/](docs/).

## Layout

```
cmd/stratus/            the binary: modes (dev, serve, ci, mcp, control commands) and usage text
internal/app/           composition root: builds every service per account and region and wires them
internal/httpapi/       request classification (which service and operation), middleware, dispatch
internal/services/<s>/  one package per AWS service: its operations, state and AWS-shaped errors
internal/control/       control API (/_stratus/control/v1): health, reset, snapshots, catalog of state
internal/clock/         the emulator clock every service reads time from
internal/chaos/         seeded fault injection;   internal/limits/  AWS quotas and throttling
internal/authn/         SigV4 checking;           internal/authz/   opt-in IAM enforcement
internal/iampolicy/     IAM policy evaluation;    internal/tags/    the shared resource-tag type
internal/container/     Docker: Lambda runtimes and real engines (RDS, ElastiCache, MSK)
internal/mcp/           MCP server for agents;    internal/diagnose/ failure explanations
sdk/typescript, sdk/python   control-API SDKs, published as stratus-emulator
docs/                   one page per feature
test/contract/          end-to-end tests against a built binary (needs Docker for some)
scripts/fidelity/       runs AWS's evaluation suite against Stratus
```

## Which files do I edit?

| Want to... | Edit |
| --- | --- |
| Implement or fix an AWS operation | `internal/services/<service>/` and its `_test.go` |
| Route a new operation or protocol shape | `internal/httpapi/classify.go` (+ `classify_test.go`) |
| Add a new service | its package, a `ServiceState` in `internal/control/catalog.go` (bucket prefixes, so reset and snapshots cover it), a field in `httpapi.Services`, wiring in `internal/app/stack.go`, the list in README.md |
| Connect services (SQS to Lambda, SNS to SQS) | the `Set<Thing>` calls in `internal/app/stack.go`; the consumer declares a small interface |
| Add a control-API feature | a `RegisterRoutes` in the owning package, `internal/control/client`, `internal/cli`, `internal/mcp/tools.go`, both SDKs, `docs/control-api.md` and a docs page, all in one change |
| Add a server flag | `internal/config/config.go`, the usage text in `cmd/stratus/main.go` |

## Rules

- **Never answer with an empty success.** An operation Stratus does not
  implement returns `501 NotImplemented` (`apierror.Error`), never a hollow
  `200`. A test or agent that only checks for success cannot tell the two
  apart.
- **Errors are AWS's.** Use the code, status and message wording AWS uses for
  that service and protocol (JSON `__type`, query XML, REST XML). Check them
  against the AWS docs or the botocore model, not memory.
- **State lives in the metadata store** under the service's bucket prefix,
  scoped per account and region (`store.Scoped`). Nothing a request changes
  may live only in memory, or reset, snapshots and restarts lose it.
- **Read time from `s.now`**, never `time.Now()`, in anything a test may want
  to move: deadlines, expiry, timestamps. The app points it at the emulator
  clock through `SetClock`. IDs must not be derived from the clock alone; it
  can be frozen.
- **One rule lives once.** Policy evaluation is `iampolicy`, quotas are
  `limits`, faults are `chaos`, tags are `tags`, scope and ARNs are `scope`.
  Reuse them rather than writing a second copy in a service.
- **Depend on ports, not packages.** A service takes what it calls as a
  small interface (see `control/ports.go`, `lambda.Invoker`); concrete
  wiring happens only in `internal/app`.
- **Agent-facing output is structured.** Control API and MCP errors carry a
  `code`, a `message` and a `hint` that says what to send instead.

## Tests

```bash
go build ./...
go test ./internal/... ./cmd/...          # fast, no Docker needed
go test ./internal/services/sqs/          # one service
go test ./test/contract/...               # end to end; Lambda and engine tests need Docker
(cd sdk/typescript && npm ci && npm test)
(cd sdk/python && PYTHONPATH=src python3 -m unittest discover -s tests)
```

- Name tests as the behaviour they pin down:
  `TestAdvancingTheClockCrossesAnSQSVisibilityTimeoutWithoutWaiting`, not
  `TestAdvance2`.
- Test the domain without HTTP where you can (pure functions, fake clocks
  and stores); add one HTTP-level test per feature to prove the wiring.
- Run `gofmt` and `go vet ./...` before committing.

## Releases

Maintainers push release tags (`v*` for the binary, `sdk-v*` for the SDKs);
the workflows in `.github/workflows/` do the rest. Do not create tags from a
change. When a release changes the version users install, the website's
install snippet is bumped separately.
