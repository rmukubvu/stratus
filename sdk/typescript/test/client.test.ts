import { test, describe } from "node:test";
import assert from "node:assert/strict";
import { StratusClient, StratusError, type FetchLike } from "../src/index.js";

interface Call {
  method: string;
  url: string;
  body: unknown;
}

/** A fetch that records calls and answers from a queue of canned responses. */
function fakeFetch(...responses: Array<{ status?: number; body?: unknown } | Error>) {
  const calls: Call[] = [];
  const fetch: FetchLike = async (url, init) => {
    calls.push({ method: init?.method ?? "GET", url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const next = responses.shift();
    if (!next) throw new Error(`unexpected request ${init?.method} ${url}`);
    if (next instanceof Error) throw next;
    const text = next.body === undefined ? "" : JSON.stringify(next.body);
    return new Response(text === "" ? null : text, { status: next.status ?? 200 });
  };
  return { fetch, calls };
}

const base = "http://stratus.test:4566/_stratus/control/v1";

describe("StratusClient", () => {
  test("defaults the endpoint from STRATUS_ENDPOINT and trims trailing slashes", () => {
    const prev = process.env.STRATUS_ENDPOINT;
    process.env.STRATUS_ENDPOINT = "http://from-env:1234/";
    try {
      assert.equal(new StratusClient().endpoint, "http://from-env:1234");
    } finally {
      if (prev === undefined) delete process.env.STRATUS_ENDPOINT;
      else process.env.STRATUS_ENDPOINT = prev;
    }
  });

  test("reset sends the named services", async () => {
    const { fetch, calls } = fakeFetch({ body: { services: ["s3"] } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    const res = await c.reset("s3");
    assert.deepEqual(res.services, ["s3"]);
    assert.deepEqual(calls, [{ method: "POST", url: `${base}/reset`, body: { services: ["s3"] } }]);
  });

  test("snapshot calls map to the control API and escape names", async () => {
    const snap = { id: "a.b", created_at: "t", services: [], size_bytes: 1 };
    const { fetch, calls } = fakeFetch({ status: 201, body: snap }, { body: { items: null } }, { body: { restored: snap } }, { status: 204 });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.deepEqual(await c.snapshots.create("a.b"), snap);
    assert.deepEqual(await c.snapshots.list(), []);
    assert.deepEqual(await c.snapshots.restore("a b"), snap);
    await c.snapshots.delete("a.b");

    assert.deepEqual(
      calls.map((x) => `${x.method} ${x.url.slice(base.length)}`),
      ["POST /snapshots", "GET /snapshots", "POST /snapshots/a%20b/restore", "DELETE /snapshots/a.b"],
    );
  });

  test("server errors become StratusError with code and hint", async () => {
    const { fetch } = fakeFetch({
      status: 404,
      body: { error: { code: "SnapshotNotFound", message: 'snapshot "x" does not exist', hint: "list snapshots" } },
    });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    await assert.rejects(c.snapshots.restore("x"), (err: unknown) => {
      assert.ok(err instanceof StratusError);
      assert.equal(err.code, "SnapshotNotFound");
      assert.equal(err.hint, "list snapshots");
      assert.equal(err.status, 404);
      return true;
    });
  });

  test("non-JSON errors keep the status", async () => {
    const { fetch } = fakeFetch({ status: 502, body: undefined });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    await assert.rejects(c.health(), { code: "HTTP502", status: 502 });
  });

  test("connection failures are reported as StratusUnreachable", async () => {
    const { fetch } = fakeFetch(new TypeError("fetch failed"));
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    await assert.rejects(c.health(), { code: StratusError.Unreachable, status: 0 });
  });

  test("waitUntilReady retries until ready and times out otherwise", async () => {
    const notReady = { status: 503, body: { error: { code: "NotReady", message: "starting" } } };
    const ok = fakeFetch(new TypeError("refused"), notReady, { body: { ready: true } });
    await new StratusClient({ endpoint: "http://stratus.test:4566", fetch: ok.fetch }).waitUntilReady({ intervalMs: 1 });
    assert.equal(ok.calls.length, 3);

    const never: FetchLike = async () => {
      throw new TypeError("refused");
    };
    await assert.rejects(
      new StratusClient({ endpoint: "http://stratus.test:4566", fetch: never }).waitUntilReady({ timeoutMs: 20, intervalMs: 5 }),
      { code: StratusError.Timeout },
    );
  });

  test("diagnose builds the query and normalises empty results", async () => {
    const { fetch, calls } = fakeFetch({ body: { summary: "No failed requests", healthy: true, failures: 0, by_category: null, groups: null } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    const d = await c.diagnose({ since: new Date("2026-01-02T03:04:05Z"), service: "sqs", excludeInjected: true, maxGroups: 5 });
    assert.deepEqual(d.groups, []);
    assert.deepEqual(d.by_category, {});
    assert.equal(
      calls[0]?.url,
      `${base}/diagnostics?since=2026-01-02T03%3A04%3A05.000Z&service=sqs&exclude_injected=true&max_groups=5`,
    );
  });

  test("withSnapshot restores and deletes even when the body throws", async () => {
    const snap = { id: "tmp", created_at: "t", services: [], size_bytes: 1 };
    const { fetch, calls } = fakeFetch({ status: 201, body: snap }, { body: { restored: snap } }, { status: 204 });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    await assert.rejects(
      c.withSnapshot(() => {
        throw new Error("boom");
      }),
      /boom/,
    );
    assert.deepEqual(
      calls.map((x) => `${x.method} ${x.url.slice(base.length)}`),
      ["POST /snapshots", "POST /snapshots/tmp/restore", "DELETE /snapshots/tmp"],
    );
    assert.match(String((calls[0]?.body as { name: string }).name), /^sdk-tmp-[a-z0-9-]+$/);
  });

  test("withChaos returns the result and the final report, and stops on failure", async () => {
    const report = { status: "stopped", seed: 1, started_at: "t", rules: [], total_injected: 0, recent_hits: [] };
    const ok = fakeFetch({ status: 201, body: report }, { body: report });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: ok.fetch });
    const out = await c.withChaos({ seed: 1, rules: [{ service: "s3", action: "throttle" }] }, () => 42);
    assert.equal(out.result, 42);
    assert.equal(out.report.status, "stopped");
    assert.deepEqual(ok.calls[0]?.body, { seed: 1, rules: [{ service: "s3", action: "throttle" }] });

    const failing = fakeFetch({ status: 201, body: report }, { body: report });
    const c2 = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: failing.fetch });
    await assert.rejects(c2.withChaos({ rules: [{ service: "*", action: "drop" }] }, () => Promise.reject(new Error("x"))), /x/);
    assert.deepEqual(failing.calls.map((x) => x.method), ["PUT", "DELETE"]);
  });

  test("awsConfig points an AWS SDK at Stratus", () => {
    const c = new StratusClient({ endpoint: "http://localhost:4566", region: "eu-west-1" });
    assert.deepEqual(c.awsConfig(), {
      endpoint: "http://localhost:4566",
      region: "eu-west-1",
      credentials: { accessKeyId: "test", secretAccessKey: "test" },
      forcePathStyle: true,
    });
  });
});

describe("ses and scheduler", () => {
  test("ses inbox calls map to the control API", async () => {
    const msg = { id: "m1", timestamp: "t", source: "a@b.example", destination: { to: ["c@d.example"] }, subject: "Hi", body: { text: "x" }, api: "ses" };
    const { fetch, calls } = fakeFetch({ body: { items: [msg], count: 1 } }, { body: msg }, { body: { items: null, count: 0 } }, { body: { cleared: 1 } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.deepEqual(await c.ses.messages({ to: "c@d.example", subject: "Hi", since: new Date("2026-11-09T12:00:00Z"), limit: 5 }), [msg]);
    assert.deepEqual(await c.ses.message("m 1"), msg);
    assert.deepEqual(await c.ses.messages(), []);
    assert.equal(await c.ses.clear(), 1);

    assert.deepEqual(
      calls.map((x) => `${x.method} ${x.url.slice(base.length)}`),
      [
        "GET /ses/messages?to=c%40d.example&subject=Hi&since=2026-11-09T12%3A00%3A00.000Z&limit=5",
        "GET /ses/messages/m%201",
        "GET /ses/messages",
        "DELETE /ses/messages",
      ],
    );
  });

  test("scheduler clock calls map to the control API", async () => {
    const fired = { name: "nightly", group: "default", schedule_arn: "arn", target: "arn:sqs", scheduled_time: "t", execution_id: "e", input: "{}" };
    const { fetch, calls } = fakeFetch(
      { body: { now: "t", offset: "1h0m0s", fired: [fired] } },
      { body: { now: "t2", offset: "2h0m0s", fired: null } },
      { body: fired },
      { body: { now: "t", offset: "0s" } },
    );
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.deepEqual((await c.scheduler.advance("1h")).fired, [fired]);
    assert.deepEqual((await c.scheduler.advance(new Date("2026-11-09T14:00:00Z"))).fired, []);
    assert.deepEqual(await c.scheduler.run("nightly", "billing"), fired);
    assert.equal((await c.scheduler.clock()).offset, "0s");

    assert.deepEqual(
      calls.map((x) => [x.method, x.url.slice(base.length), x.body]),
      [
        ["POST", "/scheduler/advance", { by: "1h" }],
        ["POST", "/scheduler/advance", { to: "2026-11-09T14:00:00.000Z" }],
        ["POST", "/scheduler/run", { name: "nightly", group: "billing" }],
        ["GET", "/scheduler/clock", undefined],
      ],
    );
  });
});
