// Wire types of the Stratus control API (/_stratus/control/v1).
// Field names match the JSON exactly so responses need no mapping.

export type HealthStatus = "ok" | "degraded" | "down";

export interface HealthCheck {
  name: string;
  status: HealthStatus;
  critical: boolean;
  detail?: string;
}

export interface Health {
  status: HealthStatus;
  ready: boolean;
  started_at: string;
  uptime_seconds: number;
  services: string[];
  checks: HealthCheck[];
}

export interface ResetResult {
  services: string[];
  buckets_cleared?: string[];
  runtime_recycled?: boolean;
}

export interface Snapshot {
  id: string;
  created_at: string;
  services: string[];
  size_bytes: number;
}

export type ChaosAction =
  | "throttle"
  | "internal_error"
  | "unavailable"
  | "error"
  | "latency"
  | "drop"
  | "cold_start";

export interface ChaosRule {
  /** Defaults to rule-N. Shown in reports and the X-Stratus-Fault header. */
  id?: string;
  /** AWS service name such as "s3" or "dynamodb", or "*" for all. */
  service: string;
  operation?: string;
  /** Substring of the request path, S3 bucket or Lambda function name. */
  resource?: string;
  /** In (0, 1]. Defaults to 1. */
  probability?: number;
  /** Stop firing after this many injections. */
  max_hits?: number;
  action: ChaosAction;
  /** Required for action "error". */
  error?: ChaosErrorSpec;
  /** Required for action "latency". */
  latency?: ChaosLatencySpec;
}

export interface ChaosErrorSpec {
  status_code: number;
  code: string;
  message?: string;
}

export interface ChaosLatencySpec {
  delay_ms: number;
  jitter_ms?: number;
}

export interface ChaosExperiment {
  /** Fixes every fault decision, so a run can be replayed exactly. */
  seed?: number;
  duration_seconds?: number;
  rules: ChaosRule[];
}

export interface ChaosHit {
  time: string;
  rule_id: string;
  action: ChaosAction;
  request_id: string;
  service: string;
  operation?: string;
  path: string;
  match: number;
}

export interface ChaosRuleReport {
  rule: Required<Pick<ChaosRule, "id" | "service" | "action">> & ChaosRule;
  matched: number;
  injected: number;
}

export interface ChaosReport {
  status: "running" | "expired" | "stopped";
  seed: number;
  started_at: string;
  ends_at?: string;
  duration_seconds?: number;
  rules: ChaosRuleReport[];
  total_injected: number;
  recent_hits: ChaosHit[];
}

export type FailureCategory =
  | "injected_fault"
  | "unsupported_operation"
  | "missing_resource"
  | "conflict"
  | "invalid_request"
  | "throttling"
  | "auth"
  | "connection"
  | "emulator_error"
  | "client_error";

export interface DiagnosisGroup {
  service: string;
  operation?: string;
  code: string;
  status: number;
  count: number;
  first_seen: string;
  last_seen: string;
  message: string;
  example_request_ids: string[];
  example_paths: string[];
  category: FailureCategory;
  likely_cause: string;
  next_step: string;
  /** False when the failure is not the caller's fault: chaos, an unimplemented operation or an emulator error. */
  your_bug: boolean;
}

export interface Diagnosis {
  summary: string;
  healthy: boolean;
  since?: string;
  failures: number;
  by_category: Partial<Record<FailureCategory, number>>;
  groups: DiagnosisGroup[];
}

export interface DiagnoseOptions {
  /** A duration such as "5m", or a point in time. */
  since?: string | Date;
  service?: string;
  excludeInjected?: boolean;
  /** 1 to 100. Defaults to 20 on the server. */
  maxGroups?: number;
}
