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

  test("limits set, status and clear call the limits endpoint", async () => {
    const status = { profile: "strict", scale: 0.01, applied_at: "t", rules: [], total_throttled: 0, recent_breaches: [] };
    const f = fakeFetch({ body: status }, { body: status }, { body: status });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: f.fetch });
    assert.equal((await c.limits.set({ profile: "strict" })).profile, "strict");
    await c.limits.status();
    await c.limits.clear();
    assert.deepEqual(
      f.calls.map((x) => x.method),
      ["PUT", "GET", "DELETE"],
    );
    assert.ok(f.calls.every((x) => x.url.endsWith("/_stratus/control/v1/limits")));
    assert.deepEqual(f.calls[0]?.body, { profile: "strict" });
  });

  test("signatures set, status and clear call the sigv4 endpoint", async () => {
    const status = { mode: "strict", verified: 0, failed: 0, skipped: 0, failures_by_code: {}, recent_failures: [] };
    const f = fakeFetch({ body: status }, { body: status }, { body: { ...status, mode: "off" } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: f.fetch });
    assert.equal((await c.signatures.set()).mode, "strict");
    await c.signatures.status();
    assert.equal((await c.signatures.clear()).mode, "off");
    assert.deepEqual(
      f.calls.map((x) => x.method),
      ["PUT", "GET", "DELETE"],
    );
    assert.ok(f.calls.every((x) => x.url.endsWith("/_stratus/control/v1/sigv4")));
    assert.deepEqual(f.calls[0]?.body, { mode: "strict" });
  });

  test("runs start, status, stop and list call the runs endpoints", async () => {
    const summary = { id: "run-1", status: "running", started_at: "t", duration_seconds: 0, totals: {}, timeline: [], operations: [] };
    const f = fakeFetch({ status: 201, body: summary }, { body: summary }, { body: { ...summary, status: "finished" } }, { body: { runs: [] } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: f.fetch });
    assert.equal((await c.runs.start({ name: "load.js", limits: { profile: "strict" } })).id, "run-1");
    await c.runs.status();
    assert.equal((await c.runs.stop("run-1")).status, "finished");
    assert.deepEqual(await c.runs.list(), []);
    assert.deepEqual(
      f.calls.map((x) => `${x.method} ${x.url.replace("http://stratus.test:4566/_stratus/control/v1", "")}`),
      ["POST /runs", "GET /runs/current", "POST /runs/run-1/stop", "GET /runs"],
    );
    assert.deepEqual(f.calls[0]?.body, { name: "load.js", limits: { profile: "strict" } });
  });

  test("runs.findings reads the latest run's findings", async () => {
    const finding = { detector: "missing_queue", severity: "high", title: "t", detail: "d", suggestion: "s" };
    const f = fakeFetch({ body: { id: "run-1", findings: [finding] } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: f.fetch });
    assert.deepEqual(await c.runs.findings(), [finding]);
    assert.ok(f.calls[0]?.url.endsWith("/runs/latest"));
  });

  test("runs.during stops the run even when the test throws", async () => {
    const summary = { id: "run-2", status: "running", started_at: "t", duration_seconds: 0, totals: {}, timeline: [], operations: [] };
    const f = fakeFetch({ status: 201, body: summary }, { body: summary });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch: f.fetch });
    await assert.rejects(c.runs.during({}, () => Promise.reject(new Error("boom"))), /boom/);
    assert.deepEqual(f.calls.map((x) => x.method), ["POST", "POST"]);
    assert.ok(f.calls[1]?.url.endsWith("/runs/run-2/stop"));
  });

  test("awsConfig points an AWS SDK at Stratus", () => {
    const c = new StratusClient({ endpoint: "http://localhost:4566", region: "eu-west-1" });
    assert.deepEqual(c.awsConfig(), {
      endpoint: "http://localhost:4566",
      region: "eu-west-1",
      credentials: { accessKeyId: "test", secretAccessKey: "test" },
      forcePathStyle: true,
      disableHostPrefix: true,
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

  test("emulator clock calls map to the control API", async () => {
    const status = { now: "t", real_now: "r", offset: "0s", frozen: false };
    const { fetch, calls } = fakeFetch(
      { body: status },
      { body: { ...status, offset: "30m0s", effects: [{ source: "scheduler", count: 1 }] } },
      { body: { ...status, effects: null } },
      { body: { ...status, frozen: true, frozen_at: "t" } },
      { body: status },
    );
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.equal((await c.clock.status()).offset, "0s");
    assert.deepEqual((await c.clock.advance("30m")).effects, [{ source: "scheduler", count: 1 }]);
    assert.deepEqual((await c.clock.advance(new Date("2026-11-23T09:00:00Z"))).effects, []);
    assert.equal((await c.clock.freeze()).frozen, true);
    assert.equal((await c.clock.resume()).frozen, false);

    assert.deepEqual(
      calls.map((x) => [x.method, x.url.slice(base.length), x.body]),
      [
        ["GET", "/clock", undefined],
        ["POST", "/clock/advance", { by: "30m" }],
        ["POST", "/clock/advance", { to: "2026-11-23T09:00:00.000Z" }],
        ["POST", "/clock/freeze", undefined],
        ["POST", "/clock/resume", undefined],
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

describe("bedrock", () => {
  test("fixture, mode and invocation calls map to the control API", async () => {
    const fixture = { id: "refunds", model: "anthropic.*", match: { contains: "refund" }, text: "five days", origin: "runtime" };
    const inv = { id: "1", time: "t", operation: "Converse", model: "m", prompt: "p", request_hash: "sha256:x", outcome: "missed" };
    const status = { mode: "record", default_mode: "replay" };
    const { fetch, calls } = fakeFetch(
      { status: 201, body: fixture },
      { body: { items: [fixture], count: 1 } },
      { body: { deleted: "refunds" } },
      { body: { cleared: 2 } },
      { body: { items: [inv], count: 1 } },
      { body: { items: null, count: 0 } },
      { body: { cleared: 1 } },
      { body: status },
      { body: status },
    );
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.deepEqual(await c.bedrock.addFixture({ id: "refunds", model: "anthropic.*", contains: "refund", text: "five days" }), fixture);
    assert.deepEqual(await c.bedrock.fixtures(), [fixture]);
    await c.bedrock.deleteFixture("refunds");
    assert.equal(await c.bedrock.clearFixtures(), 2);
    assert.deepEqual(await c.bedrock.invocations({ outcome: "missed", contains: "refund", limit: 3 }), [inv]);
    assert.deepEqual(await c.bedrock.invocations(), []);
    assert.equal(await c.bedrock.clearInvocations(), 1);
    assert.equal((await c.bedrock.setMode("record")).mode, "record");
    assert.equal((await c.bedrock.status()).default_mode, "replay");

    assert.deepEqual(
      calls.map((x) => [x.method, x.url.slice(base.length), x.body]),
      [
        ["POST", "/bedrock/fixtures", { id: "refunds", model: "anthropic.*", text: "five days", match: { contains: "refund" } }],
        ["GET", "/bedrock/fixtures", undefined],
        ["DELETE", "/bedrock/fixtures/refunds", undefined],
        ["DELETE", "/bedrock/fixtures", undefined],
        ["GET", "/bedrock/invocations?contains=refund&outcome=missed&limit=3", undefined],
        ["GET", "/bedrock/invocations", undefined],
        ["DELETE", "/bedrock/invocations", undefined],
        ["PUT", "/bedrock/mode", { mode: "record" }],
        ["GET", "/bedrock", undefined],
      ],
    );
  });

  test("addFixture sends a request hash and an exact response", async () => {
    const { fetch, calls } = fakeFetch({ status: 201, body: { id: "fx-1" } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });
    const response = { output: { message: { role: "assistant", content: [{ text: "hi" }] } } };
    await c.bedrock.addFixture({ kind: "converse", requestHash: "sha256:abc", response });
    assert.deepEqual(calls[0]!.body, { kind: "converse", response, request_hash: "sha256:abc" });
  });
});

describe("kafka", () => {
  test("clusters and bootstrap brokers come from the control API", async () => {
    const cluster = { name: "orders", arn: "arn:aws:kafka:us-east-1:000000000000:cluster/orders/x", type: "PROVISIONED", state: "ACTIVE", kafka_version: "3.6.0", bootstrap_brokers: "127.0.0.1:41001" };
    const { fetch, calls } = fakeFetch({ body: { items: [cluster], count: 1 } }, { body: { items: [cluster], count: 1 } }, { body: { items: null, count: 0 } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.deepEqual(await c.kafka.clusters(), [cluster]);
    assert.equal(await c.kafka.bootstrapBrokers("orders"), "127.0.0.1:41001");
    await assert.rejects(c.kafka.bootstrapBrokers("missing"), (err: unknown) => err instanceof StratusError && err.code === "KafkaClusterNotFound");

    assert.deepEqual(calls.map((x) => `${x.method} ${x.url.slice(base.length)}`), ["GET /kafka/clusters", "GET /kafka/clusters", "GET /kafka/clusters"]);
  });
});

describe("dns", () => {
  test("resolve asks the control API and defaults to A records", async () => {
    const answer = { name: "api.corp.local.", type: "A", value: "10.0.0.1", ttl: 60, zone_id: "Z1" };
    const { fetch, calls } = fakeFetch({ body: { name: "api.corp.local.", type: "A", answers: [answer], found: true } }, { body: { name: "x.corp.local.", type: "SRV", answers: null, found: false } });
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.deepEqual((await c.dns.resolve("api.corp.local")).answers, [answer]);
    const missing = await c.dns.resolve("x.corp.local", "SRV");
    assert.equal(missing.found, false);
    assert.deepEqual(missing.answers, []);

    assert.deepEqual(calls.map((x) => `${x.method} ${x.url.slice(base.length)}`), ["GET /dns/resolve?name=api.corp.local&type=A", "GET /dns/resolve?name=x.corp.local&type=SRV"]);
  });
});

describe("advise", () => {
  test("a decision goes from start to an ADR through the control API", async () => {
    const decision = { id: "adr-1", title: "Order events", family: "messaging", status: "draft", answers: {}, pending_questions: [], next: "" };
    const { fetch, calls } = fakeFetch(
      { body: { families: [{ id: "messaging" }] } },
      { status: 201, body: decision },
      { body: decision },
      { body: { ...decision, status: "assessed", recommended: "kinesis" } },
      { body: { ...decision, status: "decided", chosen: "kinesis" } },
      { body: { id: "adr-1", markdown: "# Order events\n" } },
      { body: { decisions: [decision] } },
      { status: 409, body: { error: { code: "OptionRuledOut", message: "sqs-standard was ruled out", hint: "choose one of: kinesis" } } },
    );
    const c = new StratusClient({ endpoint: "http://stratus.test:4566", fetch });

    assert.equal((await c.advise.families())[0]?.id, "messaging");
    await c.advise.start({ family: "messaging", title: "Order events", answers: { replay: true } });
    await c.advise.answer("adr-1", { peak_rate: 2000 });
    assert.equal((await c.advise.assess("adr-1")).recommended, "kinesis");
    assert.equal((await c.advise.decide("adr-1", "kinesis", "we replay")).chosen, "kinesis");
    assert.equal(await c.advise.adr("adr-1"), "# Order events\n");
    assert.equal((await c.advise.list()).length, 1);
    await assert.rejects(c.advise.decide("adr-1", "sqs-standard"), (err: unknown) => err instanceof StratusError && err.code === "OptionRuledOut" && err.hint === "choose one of: kinesis");

    assert.deepEqual(
      calls.map((x) => `${x.method} ${x.url.slice(base.length)}`),
      [
        "GET /advise/families",
        "POST /advise/decisions",
        "POST /advise/decisions/adr-1/answers",
        "POST /advise/decisions/adr-1/assess",
        "POST /advise/decisions/adr-1/decide",
        "GET /advise/decisions/adr-1/adr?format=json",
        "GET /advise/decisions",
        "POST /advise/decisions/adr-1/decide",
      ],
    );
    assert.deepEqual(calls[1]?.body, { family: "messaging", title: "Order events", answers: { replay: true } });
    assert.deepEqual(calls[2]?.body, { answers: { peak_rate: 2000 } });
    assert.deepEqual(calls[4]?.body, { option: "kinesis", rationale: "we replay" });
    assert.deepEqual(calls[7]?.body, { option: "sqs-standard" });
  });
});
