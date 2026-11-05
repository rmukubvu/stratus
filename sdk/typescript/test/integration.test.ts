// Runs against a real Stratus when STRATUS_ENDPOINT is set (CI starts one).
import { test } from "node:test";
import assert from "node:assert/strict";
import { StratusClient, StratusError } from "../src/index.js";

const endpoint = process.env.STRATUS_ENDPOINT;
const skip = endpoint ? false : "set STRATUS_ENDPOINT to run against a real Stratus";

test("round trip against a real Stratus", { skip }, async () => {
  const stratus = new StratusClient({ endpoint });
  await stratus.waitUntilReady({ timeoutMs: 30_000 });
  assert.equal((await stratus.health()).ready, true);
  await stratus.reset("s3");

  const s3 = (path: string, method = "PUT") => fetch(stratus.endpoint + path, { method });

  // State made inside withSnapshot is rolled back.
  await stratus.withSnapshot(async () => {
    assert.equal((await s3("/sdk-ts-temp")).status, 200);
  });
  assert.equal((await s3("/sdk-ts-temp", "HEAD")).status, 404);

  // Chaos fails exactly the first call, and diagnostics blames chaos for it.
  const { report } = await stratus.withChaos(
    { seed: 7, rules: [{ id: "first-put", service: "s3", operation: "CreateBucket", action: "internal_error", max_hits: 1 }] },
    async () => {
      assert.equal((await s3("/sdk-ts-chaos")).status, 500);
      assert.equal((await s3("/sdk-ts-chaos")).status, 200);
    },
  );
  assert.equal(report.total_injected, 1);
  assert.equal(report.recent_hits[0]?.rule_id, "first-put");

  const diagnosis = await stratus.diagnose({ service: "s3", since: "1m" });
  const injected = diagnosis.groups.find((g) => g.category === "injected_fault");
  assert.ok(injected, diagnosis.summary);
  assert.equal(injected.your_bug, false);

  await assert.rejects(stratus.snapshots.restore("sdk-ts-missing"), (err: unknown) => {
    return err instanceof StratusError && err.code === "SnapshotNotFound" && err.status === 404;
  });
  await stratus.reset("s3");
});
