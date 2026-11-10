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
  /** Services whose engines (databases, caches) were stopped with their state. */
  engines_stopped?: string[];
}

export interface Snapshot {
  id: string;
  created_at: string;
  services: string[];
  /** Engines whose data the snapshot holds, as "service/resource", e.g. "rds/orders-db". */
  engines?: string[];
  size_bytes: number;
}

export type ChaosAction =
  | "throttle"
  | "internal_error"
  | "unavailable"
  | "error"
  | "latency"
  | "drop"
  | "cold_start"
  | "function_error"
  | "duplicate";

/** "api" (default) for AWS API calls, "event" for internal deliveries, "all" for both. */
export type ChaosScope = "api" | "event" | "all";

export interface ChaosRule {
  /** Defaults to rule-N. Shown in reports and the X-Stratus-Fault header. */
  id?: string;
  /** Defaults to "api", or "event" when source is set. */
  scope?: ChaosScope;
  /** AWS service name such as "s3" or "dynamodb", or "*" for all. For an event delivery, the receiving service. */
  service: string;
  /** Service making an event delivery, such as "sns", "sqs", "events" or "stepfunctions". */
  source?: string;
  operation?: string;
  /** Substring of the request path, S3 bucket or Lambda function name, or of a delivery's source or target ARN. */
  resource?: string;
  /** Seconds after the experiment starts before the rule becomes active. */
  start_after_seconds?: number;
  /** Seconds the rule stays active once started; 0 means until the experiment ends. */
  duration_seconds?: number;
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
  scope: ChaosScope;
  /** The API request ID, or the delivery's event ID. */
  request_id: string;
  service: string;
  operation?: string;
  /** The request path, or the delivery's target ARN. */
  path: string;
  source?: string;
  source_arn?: string;
  match: number;
}

export interface ChaosRuleReport {
  rule: Required<Pick<ChaosRule, "id" | "service" | "action">> & ChaosRule;
  matched: number;
  injected: number;
}

export interface ChaosReport {
  /** Identifies this run of the experiment. */
  id: string;
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

/** An email the application sent through SES. Nothing is delivered for real. */
export interface SesMessage {
  id: string;
  timestamp: string;
  source: string;
  destination: { to?: string[]; cc?: string[]; bcc?: string[] };
  reply_to?: string[];
  subject: string;
  body: { text?: string; html?: string };
  template?: { name: string; data?: string };
  raw?: string;
  tags?: Record<string, string>;
  configuration_set?: string;
  /** Which API sent it: "ses" (v1) or "sesv2". */
  api: "ses" | "sesv2";
}

export interface SesMessageFilter {
  /** A recipient address, matched against To, Cc and Bcc. */
  to?: string;
  from?: string;
  /** A case-insensitive substring of the subject. */
  subject?: string;
  /** A duration such as "5m", or a point in time. */
  since?: string | Date;
  /** Keep only the newest N. */
  limit?: number;
}

/** One delivery of an EventBridge schedule to its target. */
export interface SchedulerInvocation {
  name: string;
  group: string;
  schedule_arn: string;
  target: string;
  scheduled_time: string;
  execution_id: string;
  input: string;
  /** Set when the target refused the delivery or a chaos rule failed it. */
  error?: string;
}

export interface SchedulerClock {
  /** The scheduler's current time: real time plus the offset. */
  now: string;
  /** How far ahead of real time the scheduler runs, such as "1h0m0s". */
  offset: string;
}

export interface SchedulerAdvance extends SchedulerClock {
  /** What fired because of this advance, in scheduled-time order. */
  fired: SchedulerInvocation[];
}

/** What Bedrock does with a request no fixture answers. */
export type BedrockMode = "replay" | "record";

/** The request shape a fixture answers: InvokeModel bodies or Converse. */
export type BedrockKind = "invoke" | "converse";

export interface BedrockStatus {
  mode: BedrockMode;
  /** The mode set by STRATUS_BEDROCK_MODE, restored by reset. */
  default_mode: BedrockMode;
  /** Directory of fixture files (STRATUS_BEDROCK_FIXTURES), if set. */
  fixture_dir?: string;
  /** Real Bedrock endpoint used in record mode. */
  upstream?: string;
}

/** A canned model answer, as the control API returns it. */
export interface BedrockFixture {
  id: string;
  kind?: BedrockKind;
  model?: string;
  request_hash?: string;
  match?: { contains?: string };
  text?: string;
  response?: unknown;
  request?: unknown;
  recorded_at?: string;
  /** "runtime" for fixtures added through the API, else the fixture file. */
  origin?: string;
}

/** A fixture to add. Give text, or response together with kind. */
export interface BedrockFixtureInput {
  id?: string;
  /** Model id, inference profile or pattern with *, e.g. "anthropic.claude-*". */
  model?: string;
  /** Only answer prompts containing this text. */
  contains?: string;
  /** Pin the fixture to one exact request, as printed by a failed replay. */
  requestHash?: string;
  kind?: BedrockKind;
  text?: string;
  /** Exact response body: a Converse response, or the provider's InvokeModel body. */
  response?: unknown;
}

export type BedrockOutcome = "replayed" | "recorded" | "synthesized" | "missed" | "failed";

/** One model call the application made. */
export interface BedrockInvocation {
  id: string;
  time: string;
  operation: string;
  model: string;
  prompt: string;
  response?: string;
  request_hash: string;
  outcome: BedrockOutcome;
  fixture_id?: string;
  input_tokens?: number;
  output_tokens?: number;
  error?: string;
}

export interface BedrockInvocationFilter {
  model?: string;
  contains?: string;
  outcome?: BedrockOutcome;
  /** Only the newest N calls. */
  limit?: number;
}

/** An MSK cluster as the control API lists it. */
export interface KafkaCluster {
  name: string;
  arn: string;
  type: "PROVISIONED" | "SERVERLESS";
  state: string;
  kafka_version: string;
  /** host:port to pass to a Kafka client; empty while no broker runs. */
  bootstrap_brokers: string;
}

/** One record in a DNS answer. */
export interface DnsAnswer {
  name: string;
  type: string;
  value: string;
  ttl: number;
  zone_id: string;
}

/** What a lookup against the emulated hosted zones returns. */
export interface DnsResolution {
  name: string;
  type: string;
  /** CNAME steps first, then the records of the requested type. */
  answers: DnsAnswer[];
  /** False for NXDOMAIN or when the name has no records of that type. */
  found: boolean;
}
