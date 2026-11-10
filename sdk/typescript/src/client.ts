import { StratusError } from "./errors.js";
import { FetchTransport, type FetchLike, type Transport } from "./transport.js";
import type {
  BedrockFixture,
  BedrockFixtureInput,
  BedrockInvocation,
  BedrockInvocationFilter,
  BedrockMode,
  BedrockStatus,
  ChaosExperiment,
  ChaosReport,
  DiagnoseOptions,
  Diagnosis,
  Health,
  KafkaCluster,
  DnsResolution,
  ResetResult,
  SchedulerAdvance,
  SchedulerClock,
  SchedulerInvocation,
  SesMessage,
  SesMessageFilter,
  Snapshot,
} from "./types.js";

export const DEFAULT_ENDPOINT = "http://localhost:4566";

export interface StratusClientOptions {
  /** Defaults to $STRATUS_ENDPOINT, then http://localhost:4566. */
  endpoint?: string;
  /** Defaults to $AWS_REGION, then us-east-1. Only used by awsConfig(). */
  region?: string;
  fetch?: FetchLike;
  timeoutMs?: number;
  /** Replaces the HTTP layer entirely; endpoint is still used by awsConfig(). */
  transport?: Transport;
}

/** Settings that point an AWS SDK v3 client at Stratus. */
export interface AwsClientConfig {
  endpoint: string;
  region: string;
  credentials: { accessKeyId: string; secretAccessKey: string };
  /** S3 needs path-style addressing against a local endpoint. */
  forcePathStyle: true;
  /** Keeps host-prefixed operations (Cloud Map's DiscoverInstances) on the Stratus host. */
  disableHostPrefix: true;
}

export interface WaitOptions {
  timeoutMs?: number;
  intervalMs?: number;
}

/** Save, list and restore whole-emulator state. */
export class Snapshots {
  constructor(private readonly t: Transport) {}

  /** Captures all state. The server picks a name when none is given. */
  create(name?: string): Promise<Snapshot> {
    return this.t.request("POST", "/snapshots", name === undefined ? {} : { name });
  }

  async list(): Promise<Snapshot[]> {
    const res = await this.t.request<{ items: Snapshot[] | null }>("GET", "/snapshots");
    return res.items ?? [];
  }

  get(name: string): Promise<Snapshot> {
    return this.t.request("GET", `/snapshots/${encodeURIComponent(name)}`);
  }

  /** Replaces all live state with the snapshot. It can be restored again later. */
  async restore(name: string): Promise<Snapshot> {
    const res = await this.t.request<{ restored: Snapshot }>("POST", `/snapshots/${encodeURIComponent(name)}/restore`);
    return res.restored;
  }

  async delete(name: string): Promise<void> {
    await this.t.request("DELETE", `/snapshots/${encodeURIComponent(name)}`);
  }
}

/** Seeded, reproducible fault injection. */
export class Chaos {
  constructor(private readonly t: Transport) {}

  start(experiment: ChaosExperiment): Promise<ChaosReport> {
    return this.t.request("PUT", "/chaos", experiment);
  }

  /** The running experiment, or the last stopped one. Throws NoChaosExperiment if none was started. */
  status(): Promise<ChaosReport> {
    return this.t.request("GET", "/chaos");
  }

  /** Stops injecting and returns the final per-rule counts and hits. */
  stop(): Promise<ChaosReport> {
    return this.t.request("DELETE", "/chaos");
  }
}

/** The inbox that captures every email sent through SES v1 or v2. */
export class Ses {
  constructor(private readonly t: Transport) {}

  /** Emails sent so far, oldest first, optionally filtered. */
  async messages(filter: SesMessageFilter = {}): Promise<SesMessage[]> {
    const q = new URLSearchParams();
    if (filter.to) q.set("to", filter.to);
    if (filter.from) q.set("from", filter.from);
    if (filter.subject) q.set("subject", filter.subject);
    if (filter.since !== undefined) {
      q.set("since", filter.since instanceof Date ? filter.since.toISOString() : filter.since);
    }
    if (filter.limit !== undefined) q.set("limit", String(filter.limit));
    const qs = q.toString();
    const res = await this.t.request<{ items: SesMessage[] | null }>("GET", "/ses/messages" + (qs ? `?${qs}` : ""));
    return res.items ?? [];
  }

  message(id: string): Promise<SesMessage> {
    return this.t.request("GET", `/ses/messages/${encodeURIComponent(id)}`);
  }

  /** Empties the inbox and returns how many emails were removed. Identities and templates stay. */
  async clear(): Promise<number> {
    const res = await this.t.request<{ cleared: number }>("DELETE", "/ses/messages");
    return res.cleared;
  }
}

/** The EventBridge Scheduler clock: move it instead of sleeping. */
export class Scheduler {
  constructor(private readonly t: Transport) {}

  clock(): Promise<SchedulerClock> {
    return this.t.request("GET", "/scheduler/clock");
  }

  /**
   * Moves the clock forward by a duration such as "1h", or to a point in time,
   * and fires every schedule that became due (once each) before resolving.
   */
  async advance(by: string | Date): Promise<SchedulerAdvance> {
    const body = by instanceof Date ? { to: by.toISOString() } : { by };
    const res = await this.t.request<SchedulerAdvance>("POST", "/scheduler/advance", body);
    return { ...res, fired: res.fired ?? [] };
  }

  /** Fires one schedule now, whatever its expression or state says. */
  run(name: string, group?: string): Promise<SchedulerInvocation> {
    return this.t.request("POST", "/scheduler/run", group === undefined ? { name } : { name, group });
  }
}

/** MSK clusters, each a real Kafka-compatible broker. */
export class Kafka {
  constructor(private readonly t: Transport) {}

  /** Every cluster with the bootstrap address to hand to a Kafka client. */
  async clusters(): Promise<KafkaCluster[]> {
    const res = await this.t.request<{ items: KafkaCluster[] | null }>("GET", "/kafka/clusters");
    return res.items ?? [];
  }

  /** The bootstrap brokers (host:port) of the named cluster. */
  async bootstrapBrokers(name: string): Promise<string> {
    const cluster = (await this.clusters()).find((c) => c.name === name);
    if (!cluster) throw new StratusError("KafkaClusterNotFound", `MSK cluster "${name}" does not exist`, "list clusters with kafka.clusters()", 404);
    return cluster.bootstrap_brokers;
  }
}

/** DNS lookups against the emulated Route 53 zones, Cloud Map's included. */
export class Dns {
  constructor(private readonly t: Transport) {}

  /** Resolves a name the way a client inside the VPC would; type defaults to A. */
  async resolve(name: string, type = "A"): Promise<DnsResolution> {
    const q = new URLSearchParams({ name, type });
    const res = await this.t.request<DnsResolution>("GET", `/dns/resolve?${q.toString()}`);
    return { ...res, answers: res.answers ?? [] };
  }
}

/** Bedrock fixtures and the log of model calls. */
export class Bedrock {
  constructor(private readonly t: Transport) {}

  status(): Promise<BedrockStatus> {
    return this.t.request("GET", "/bedrock");
  }

  /** Switches between replay and record until the next reset. */
  setMode(mode: BedrockMode): Promise<BedrockStatus> {
    return this.t.request("PUT", "/bedrock/mode", { mode });
  }

  /** Every fixture in match order: runtime fixtures, then fixture files. */
  async fixtures(): Promise<BedrockFixture[]> {
    const res = await this.t.request<{ items: BedrockFixture[] | null }>("GET", "/bedrock/fixtures");
    return res.items ?? [];
  }

  /** Adds a fixture that lasts until reset and comes before fixture files. */
  addFixture(input: BedrockFixtureInput): Promise<BedrockFixture> {
    const { contains, requestHash, ...rest } = input;
    const body: Record<string, unknown> = { ...rest };
    if (contains !== undefined) body.match = { contains };
    if (requestHash !== undefined) body.request_hash = requestHash;
    return this.t.request("POST", "/bedrock/fixtures", body);
  }

  async deleteFixture(id: string): Promise<void> {
    await this.t.request("DELETE", `/bedrock/fixtures/${encodeURIComponent(id)}`);
  }

  /** Removes every runtime fixture and returns how many there were. Files stay. */
  async clearFixtures(): Promise<number> {
    const res = await this.t.request<{ cleared: number }>("DELETE", "/bedrock/fixtures");
    return res.cleared;
  }

  /** Model calls so far, oldest first, optionally filtered. */
  async invocations(filter: BedrockInvocationFilter = {}): Promise<BedrockInvocation[]> {
    const q = new URLSearchParams();
    if (filter.model) q.set("model", filter.model);
    if (filter.contains) q.set("contains", filter.contains);
    if (filter.outcome) q.set("outcome", filter.outcome);
    if (filter.limit !== undefined) q.set("limit", String(filter.limit));
    const qs = q.toString();
    const res = await this.t.request<{ items: BedrockInvocation[] | null }>("GET", "/bedrock/invocations" + (qs ? `?${qs}` : ""));
    return res.items ?? [];
  }

  async clearInvocations(): Promise<number> {
    const res = await this.t.request<{ cleared: number }>("DELETE", "/bedrock/invocations");
    return res.cleared;
  }
}

/**
 * Client for the Stratus control API.
 *
 * ```ts
 * const stratus = new StratusClient();
 * await stratus.waitUntilReady();
 * await stratus.withSnapshot(async () => {
 *   // anything created here is rolled back afterwards
 * });
 * ```
 */
export class StratusClient {
  readonly endpoint: string;
  readonly region: string;
  readonly snapshots: Snapshots;
  readonly chaos: Chaos;
  readonly ses: Ses;
  readonly scheduler: Scheduler;
  readonly bedrock: Bedrock;
  readonly kafka: Kafka;
  readonly dns: Dns;
  private readonly t: Transport;

  constructor(options: StratusClientOptions = {}) {
    this.endpoint = trimSlash(options.endpoint ?? env("STRATUS_ENDPOINT") ?? DEFAULT_ENDPOINT);
    this.region = options.region ?? env("AWS_REGION") ?? env("AWS_DEFAULT_REGION") ?? "us-east-1";
    this.t =
      options.transport ??
      new FetchTransport(this.endpoint, {
        ...(options.fetch && { fetch: options.fetch }),
        ...(options.timeoutMs !== undefined && { timeoutMs: options.timeoutMs }),
      });
    this.snapshots = new Snapshots(this.t);
    this.chaos = new Chaos(this.t);
    this.ses = new Ses(this.t);
    this.scheduler = new Scheduler(this.t);
    this.bedrock = new Bedrock(this.t);
    this.kafka = new Kafka(this.t);
    this.dns = new Dns(this.t);
  }

  health(): Promise<Health> {
    return this.t.request("GET", "/health");
  }

  /** True when Stratus accepts traffic; false when it is starting, down or unreachable. */
  async isReady(): Promise<boolean> {
    try {
      await this.t.request("GET", "/ready");
      return true;
    } catch (err) {
      if (err instanceof StratusError) return false;
      throw err;
    }
  }

  /** Polls until Stratus is ready. Throws StratusTimeout after timeoutMs (default 30s). */
  async waitUntilReady({ timeoutMs = 30_000, intervalMs = 100 }: WaitOptions = {}): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      if (await this.isReady()) return;
      if (Date.now() >= deadline) {
        throw new StratusError(
          StratusError.Timeout,
          `Stratus at ${this.endpoint} was not ready after ${timeoutMs}ms`,
          "check that it is running: stratus wait --timeout 30s",
          0,
        );
      }
      await sleep(Math.min(intervalMs, Math.max(0, deadline - Date.now())));
    }
  }

  /** Resets the named services, or everything when none are named. */
  reset(...services: string[]): Promise<ResetResult> {
    return this.t.request("POST", "/reset", { services });
  }

  /** Groups recent failed requests and explains each group. */
  async diagnose(options: DiagnoseOptions = {}): Promise<Diagnosis> {
    const q = new URLSearchParams();
    if (options.since !== undefined) {
      q.set("since", options.since instanceof Date ? options.since.toISOString() : options.since);
    }
    if (options.service) q.set("service", options.service);
    if (options.excludeInjected) q.set("exclude_injected", "true");
    if (options.maxGroups !== undefined) q.set("max_groups", String(options.maxGroups));
    const qs = q.toString();
    const d = await this.t.request<Diagnosis>("GET", "/diagnostics" + (qs ? `?${qs}` : ""));
    return { ...d, by_category: d.by_category ?? {}, groups: d.groups ?? [] };
  }

  /**
   * Runs fn, then rolls all state back to how it was before, even if fn throws.
   * The temporary snapshot is deleted afterwards.
   */
  async withSnapshot<T>(fn: () => Promise<T> | T): Promise<T> {
    const snap = await this.snapshots.create(tempSnapshotName());
    try {
      return await fn();
    } finally {
      try {
        await this.snapshots.restore(snap.id);
      } finally {
        await this.snapshots.delete(snap.id);
      }
    }
  }

  /**
   * Runs fn under a chaos experiment and always stops it afterwards.
   * Returns fn's result and the final report; if fn throws, the experiment is still stopped.
   */
  async withChaos<T>(experiment: ChaosExperiment, fn: () => Promise<T> | T): Promise<{ result: T; report: ChaosReport }> {
    await this.chaos.start(experiment);
    let result: T;
    try {
      result = await fn();
    } catch (err) {
      await this.chaos.stop();
      throw err;
    }
    const report = await this.chaos.stop();
    return { result, report };
  }

  /** Spread into any AWS SDK v3 client: `new S3Client(stratus.awsConfig())`. */
  awsConfig(): AwsClientConfig {
    return {
      endpoint: this.endpoint,
      region: this.region,
      credentials: { accessKeyId: "test", secretAccessKey: "test" },
      forcePathStyle: true,
      disableHostPrefix: true,
    };
  }
}

function env(name: string): string | undefined {
  const v = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env?.[name];
  return v === "" ? undefined : v;
}

function trimSlash(s: string): string {
  return s.replace(/\/+$/, "");
}

function tempSnapshotName(): string {
  return `sdk-tmp-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
