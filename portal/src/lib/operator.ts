export type RequestRecord = {
  time: string;
  request_id: string;
  service: string;
  operation: string;
  method: string;
  path: string;
  status: number;
  duration_ms: number;
  error_code?: string;
  error_message?: string;
};

export type ServiceCount = {
  service: string;
  count: number;
};

export type Overview = {
  endpoint: string;
  data_dir: string;
  log_level: string;
  log_format: string;
  started_at: string;
  uptime_seconds: number;
  total_requests: number;
  status_2xx: number;
  status_4xx: number;
  status_5xx: number;
  top_services: ServiceCount[];
  recent_errors: RequestRecord[];
};

export type GroupSummary = {
  name: string;
  created_at: number;
};

export type StreamSummary = {
  group_name: string;
  stream_name: string;
  created_at: number;
  last_ingestion_time: number;
  last_event_time: number;
  stored_bytes: number;
};

export type EventSummary = {
  timestamp: number;
  message: string;
};

export const baseUrl =
  process.env.STRATUS_BASE_URL?.replace(/\/$/, "") ?? "http://127.0.0.1:4566";

export class OperatorFetchError extends Error {
  constructor(
    message: string,
    readonly endpoint: string,
    readonly detail?: string,
  ) {
    super(message);
    this.name = "OperatorFetchError";
  }
}

async function operatorFetch<T>(path: string): Promise<T> {
  let response: Response;
  try {
    response = await fetch(`${baseUrl}${path}`, {
      cache: "no-store",
    });
  } catch (error) {
    const message =
      error instanceof Error ? error.message : "failed to reach Stratus";
    throw new OperatorFetchError(
      "The portal could not reach the configured Stratus endpoint.",
      baseUrl,
      message,
    );
  }
  if (!response.ok) {
    const body = await response.text();
    const detail = body.trim();
    if (
      detail.includes("NoSuchBucket") ||
      detail.includes("service %q is not implemented") ||
      detail.includes("operator endpoint not found")
    ) {
      throw new OperatorFetchError(
        "The portal is not talking to a Stratus build that exposes the operator API.",
        baseUrl,
        detail,
      );
    }
    throw new OperatorFetchError(
      `operator request failed: ${response.status}`,
      baseUrl,
      detail,
    );
  }
  return response.json();
}

export async function getOverview() {
  return operatorFetch<Overview>("/_stratus/operator/overview");
}

export async function getActivity(searchParams?: {
  service?: string;
  status?: string;
  q?: string;
  limit?: string;
}) {
  const query = new URLSearchParams();
  if (searchParams?.service) query.set("service", searchParams.service);
  if (searchParams?.status) query.set("status", searchParams.status);
  if (searchParams?.q) query.set("q", searchParams.q);
  if (searchParams?.limit) query.set("limit", searchParams.limit);
  const suffix = query.toString() ? `?${query.toString()}` : "";
  return operatorFetch<{ items: RequestRecord[] }>(
    `/_stratus/operator/activity${suffix}`,
  );
}

export async function getErrors(limit = "50") {
  return operatorFetch<{ items: RequestRecord[] }>(
    `/_stratus/operator/errors?limit=${limit}`,
  );
}

export async function getLogGroups() {
  return operatorFetch<{ items: GroupSummary[] }>("/_stratus/operator/logs/groups");
}

export async function getLogStreams(group?: string) {
  const suffix = group ? `?group=${encodeURIComponent(group)}` : "";
  return operatorFetch<{ items: StreamSummary[] }>(
    `/_stratus/operator/logs/streams${suffix}`,
  );
}

export async function getLogEvents(group?: string, stream?: string, limit = 200) {
  if (!group || !stream) {
    return { items: [] as EventSummary[] };
  }
  return operatorFetch<{ items: EventSummary[] }>(
    `/_stratus/operator/logs/events?group=${encodeURIComponent(group)}&stream=${encodeURIComponent(stream)}&limit=${limit}`,
  );
}

export function formatDateTime(value: number | string) {
  const date = new Date(value);
  return new Intl.DateTimeFormat("en-CA", {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).format(date);
}

export function formatDuration(ms: number) {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

export function statusTone(status: number) {
  if (status >= 500) return "destructive";
  if (status >= 400) return "warning";
  return "success";
}
