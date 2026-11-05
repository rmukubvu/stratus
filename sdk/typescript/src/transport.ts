import { StratusError } from "./errors.js";

/** Sends one control API request and returns the decoded JSON body. */
export interface Transport {
  request<T>(method: string, path: string, body?: unknown): Promise<T>;
}

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface FetchTransportOptions {
  fetch?: FetchLike;
  /** Per-request timeout. Defaults to 60s, above the server's 30s drain limit. */
  timeoutMs?: number;
}

export const CONTROL_PREFIX = "/_stratus/control/v1";

/** Transport over the global fetch (Node 18+, Deno, Bun, browsers). */
export class FetchTransport implements Transport {
  private readonly fetch: FetchLike;
  private readonly timeoutMs: number;

  constructor(
    readonly endpoint: string,
    options: FetchTransportOptions = {},
  ) {
    const f = options.fetch ?? globalThis.fetch;
    if (typeof f !== "function") {
      throw new Error("no fetch implementation available; pass one in options.fetch");
    }
    this.fetch = f.bind(globalThis);
    this.timeoutMs = options.timeoutMs ?? 60_000;
  }

  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const url = this.endpoint + CONTROL_PREFIX + path;
    const init: RequestInit = { method, signal: AbortSignal.timeout(this.timeoutMs) };
    if (body !== undefined) {
      init.body = JSON.stringify(body);
      init.headers = { "content-type": "application/json" };
    }

    let res: Response;
    try {
      res = await this.fetch(url, init);
    } catch (err) {
      if (err instanceof Error && (err.name === "TimeoutError" || err.name === "AbortError")) {
        throw new StratusError(StratusError.Timeout, `${method} ${url} timed out after ${this.timeoutMs}ms`, undefined, 0, { cause: err });
      }
      throw new StratusError(
        StratusError.Unreachable,
        `cannot reach Stratus at ${this.endpoint}`,
        "start it with `stratus serve`, or set STRATUS_ENDPOINT",
        0,
        { cause: err },
      );
    }

    const text = await res.text();
    const payload = text === "" ? undefined : parse(text);
    if (!res.ok) {
      throw toError(res.status, payload, text);
    }
    if (text !== "" && payload === undefined) {
      throw new StratusError(StratusError.BadResponse, `${method} ${path} returned invalid JSON`, undefined, res.status);
    }
    return payload as T;
  }
}

function parse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

function toError(status: number, payload: unknown, text: string): StratusError {
  const e = (payload as { error?: { code?: unknown; message?: unknown; hint?: unknown } } | undefined)?.error;
  if (e && typeof e.code === "string") {
    return new StratusError(e.code, String(e.message ?? ""), typeof e.hint === "string" ? e.hint : undefined, status);
  }
  return new StratusError(`HTTP${status}`, text.trim() || `request failed with status ${status}`, undefined, status);
}
