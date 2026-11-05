# Stratus TypeScript SDK

Control a local [Stratus](../../README.md) from tests and agents: wait for it,
reset it, snapshot and roll back state, run seeded chaos, and ask why calls
failed. No runtime dependencies; uses the global `fetch` (Node 18+, Deno, Bun).

```ts
import { StratusClient, StratusError } from "@stratus/sdk";
import { S3Client } from "@aws-sdk/client-s3";

const stratus = new StratusClient(); // $STRATUS_ENDPOINT or http://localhost:4566
await stratus.waitUntilReady();

const s3 = new S3Client(stratus.awsConfig());

// Everything created inside is rolled back afterwards, even on failure.
await stratus.withSnapshot(async () => {
  await runMyTest(s3);
});

// Fail the first PutItem, then recover; the experiment always stops.
const { report } = await stratus.withChaos(
  { seed: 42, rules: [{ service: "dynamodb", operation: "PutItem", action: "throttle", max_hits: 1 }] },
  () => runMyTest(s3),
);
console.log(report.total_injected, report.recent_hits);

// Why did things fail? Groups, likely cause, next step, and whether chaos did it.
const diagnosis = await stratus.diagnose({ since: "5m", excludeInjected: true });
console.log(diagnosis.summary);
```

| Method | Control API |
| --- | --- |
| `health()`, `isReady()`, `waitUntilReady()` | `GET /health`, `GET /ready` |
| `reset(...services)` | `POST /reset` |
| `snapshots.create/list/get/restore/delete` | `/snapshots` |
| `chaos.start/status/stop` | `/chaos` |
| `diagnose(options)` | `GET /diagnostics` |
| `withSnapshot(fn)`, `withChaos(spec, fn)`, `awsConfig()` | helpers |

Every failure is a `StratusError` with a stable `code` (such as
`SnapshotNotFound`), a `hint` and the HTTP `status` (0 when Stratus could not
be reached).

## Jest / Vitest

```ts
const stratus = new StratusClient();
let baseline: string;

beforeAll(async () => {
  await stratus.waitUntilReady();
  baseline = (await stratus.snapshots.create("baseline")).id; // after seeding
});
afterEach(() => stratus.snapshots.restore(baseline));
```

## Development

```sh
npm ci && npm run build && npm test
STRATUS_ENDPOINT=http://localhost:4566 npm test   # also runs the integration test
```
