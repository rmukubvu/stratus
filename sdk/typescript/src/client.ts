import { StratusError } from "./errors.js";
import { FetchTransport, type FetchLike, type Transport } from "./transport.js";
import type {
  ChaosExperiment,
  ChaosReport,
  DiagnoseOptions,
  Diagnosis,
  Health,
  ResetResult,
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
