"""Wire types of the Stratus control API. Responses are plain dicts with these shapes."""

from __future__ import annotations

from typing import Any, Dict, List, Literal, TypedDict

HealthStatus = Literal["ok", "degraded", "down"]
ChaosAction = Literal[
    "throttle", "internal_error", "unavailable", "error", "latency", "drop", "cold_start", "function_error", "duplicate"
]
#: "api" (default) for AWS API calls, "event" for internal deliveries, "all" for both.
ChaosScope = Literal["api", "event", "all"]
#: How Stratus treats request signatures: off accepts anything, audit records failures, strict refuses them.
SignatureMode = Literal["off", "audit", "strict"]
FailureCategory = Literal[
    "injected_fault",
    "signature",
    "limit_exceeded",
    "unsupported_operation",
    "missing_resource",
    "conflict",
    "invalid_request",
    "throttling",
    "auth",
    "connection",
    "emulator_error",
    "client_error",
]


class _HealthCheckBase(TypedDict):
    name: str
    status: HealthStatus
    critical: bool


class HealthCheck(_HealthCheckBase, total=False):
    detail: str


class Health(TypedDict):
    status: HealthStatus
    ready: bool
    started_at: str
    uptime_seconds: int
    services: List[str]
    checks: List[HealthCheck]


class _ResetBase(TypedDict):
    services: List[str]


class ResetResult(_ResetBase, total=False):
    buckets_cleared: List[str]
    runtime_recycled: bool
    engines_stopped: List[str]


class _SnapshotBase(TypedDict):
    id: str
    created_at: str
    services: List[str]
    size_bytes: int


class Snapshot(_SnapshotBase, total=False):
    # Engines whose data the snapshot holds, as "service/resource",
    # e.g. "rds/orders-db". Absent when no engine data was captured.
    engines: List[str]


class ChaosErrorSpec(TypedDict, total=False):
    status_code: int
    code: str
    message: str


class ChaosLatencySpec(TypedDict, total=False):
    delay_ms: int
    jitter_ms: int


class _ChaosRuleBase(TypedDict):
    service: str
    action: ChaosAction


class ChaosRule(_ChaosRuleBase, total=False):
    id: str
    scope: ChaosScope
    #: Service making an event delivery, such as "sns", "sqs", "events" or "stepfunctions".
    source: str
    start_after_seconds: int
    duration_seconds: int
    operation: str
    resource: str
    probability: float
    max_hits: int
    error: ChaosErrorSpec
    latency: ChaosLatencySpec


class _ChaosExperimentBase(TypedDict):
    rules: List[ChaosRule]


class ChaosExperiment(_ChaosExperimentBase, total=False):
    seed: int
    duration_seconds: int


class ChaosHit(TypedDict, total=False):
    time: str
    rule_id: str
    action: ChaosAction
    scope: ChaosScope
    request_id: str
    service: str
    operation: str
    path: str
    source: str
    source_arn: str
    match: int


class ChaosRuleReport(TypedDict):
    rule: ChaosRule
    matched: int
    injected: int


class _ChaosReportBase(TypedDict):
    id: str
    status: Literal["running", "expired", "stopped"]
    seed: int
    started_at: str
    rules: List[ChaosRuleReport]
    total_injected: int
    recent_hits: List[ChaosHit]


class ChaosReport(_ChaosReportBase, total=False):
    ends_at: str
    duration_seconds: int


LimitsBase = Literal["aws-default", "strict", "none"]


class RateRule(TypedDict, total=False):
    """A request-rate quota; a rule with a base rule's id replaces it."""

    id: str
    service: str
    operations: List[str]
    per_second: float
    region_per_second: Dict[str, float]
    burst_seconds: float
    per: Literal["account", "resource", "prefix"]
    disabled: bool


class DynamoDBCapacity(TypedDict, total=False):
    """DynamoDB table capacity: partitions, on-demand maximums and burst."""

    disabled: bool
    partition_read_units: float
    partition_write_units: float
    on_demand_read_units: float
    on_demand_write_units: float
    burst_seconds: float
    tables: Dict[str, Dict[str, int]]


class LambdaConcurrency(TypedDict, total=False):
    """Lambda concurrency: the account's concurrent executions."""

    disabled: bool
    account_concurrency: float


# "lambda" is a Python keyword, so this TypedDict uses the functional form.
LimitsProfile = TypedDict(
    "LimitsProfile",
    {
        "profile": LimitsBase,
        "scale": float,
        "rates": List[RateRule],
        "dynamodb": DynamoDBCapacity,
        "lambda": LambdaConcurrency,
    },
    total=False,
)


class TableCapacityStatus(TypedDict):
    table: str
    account: str
    region: str
    partitions: int
    read_units: float
    write_units: float
    read_throttled: int
    write_throttled: int


class LimitBreach(TypedDict, total=False):
    time: str
    rule_id: str
    kind: Literal["rate", "concurrency", "capacity"]
    key: str
    limit: float
    reason: str
    request_id: str
    service: str
    operation: str
    resource: str


class LimitsRuleStatus(TypedDict):
    rule: RateRule
    effective_per_second: float
    admitted: int
    throttled: int


class _LimitsStatusBase(TypedDict):
    profile: LimitsBase
    scale: float
    applied_at: str
    rules: List[LimitsRuleStatus]
    total_throttled: int
    recent_breaches: List[LimitBreach]


class LimitsStatus(_LimitsStatusBase, total=False):
    concurrency: List[Dict[str, Any]]
    dynamodb: DynamoDBCapacity
    capacity: List[TableCapacityStatus]
    emulator: Dict[str, Any]


class _SignatureFailureBase(TypedDict):
    time: str
    request_id: str
    service: str
    method: str
    path: str
    kind: str
    code: str
    reason: str
    refused: bool


class SignatureFailure(_SignatureFailureBase, total=False):
    """A request whose SigV4 signature did not verify, with what Stratus expected it to sign."""

    operation: str
    access_key_id: str
    presigned: bool
    canonical_request: str
    string_to_sign: str


class SignatureStatus(TypedDict):
    mode: SignatureMode
    verified: int
    failed: int
    skipped: int
    failures_by_code: Dict[str, int]
    recent_failures: List[SignatureFailure]


class DiagnosisGroup(TypedDict, total=False):
    service: str
    operation: str
    code: str
    status: int
    count: int
    first_seen: str
    last_seen: str
    message: str
    example_request_ids: List[str]
    example_paths: List[str]
    category: FailureCategory
    likely_cause: str
    next_step: str
    #: False when the failure is not the caller's fault: chaos, an unimplemented operation or an emulator error.
    your_bug: bool


class _DiagnosisBase(TypedDict):
    summary: str
    healthy: bool
    failures: int
    by_category: Dict[str, int]
    groups: List[DiagnosisGroup]


class Diagnosis(_DiagnosisBase, total=False):
    since: str


class SesDestination(TypedDict, total=False):
    to: List[str]
    cc: List[str]
    bcc: List[str]


class SesBody(TypedDict, total=False):
    text: str
    html: str


class SesTemplateRef(TypedDict, total=False):
    name: str
    data: str


class _SesMessageBase(TypedDict):
    id: str
    timestamp: str
    source: str
    destination: SesDestination
    subject: str
    body: SesBody
    #: Which API sent it: "ses" (v1) or "sesv2".
    api: str


class SesMessage(_SesMessageBase, total=False):
    """An email the application sent through SES. Nothing is delivered for real."""

    reply_to: List[str]
    template: SesTemplateRef
    raw: str
    tags: Dict[str, str]
    configuration_set: str


class _SchedulerInvocationBase(TypedDict):
    name: str
    group: str
    schedule_arn: str
    target: str
    scheduled_time: str
    execution_id: str
    input: str


class SchedulerInvocation(_SchedulerInvocationBase, total=False):
    """One delivery of an EventBridge schedule to its target."""

    #: Set when the target refused the delivery or a chaos rule failed it.
    error: str


class SchedulerClock(TypedDict):
    #: The scheduler's current time: real time plus the offset.
    now: str
    #: How far ahead of real time the scheduler runs, such as "1h0m0s".
    offset: str


class SchedulerAdvance(SchedulerClock):
    #: What fired because of this advance, in scheduled-time order.
    fired: List[SchedulerInvocation]


class _ClockStatusRequired(TypedDict):
    #: The emulator's current time.
    now: str
    #: The machine's real time.
    real_now: str
    #: How far the emulator clock is from real time, such as "1h30m0s".
    offset: str
    #: Whether the clock is held still.
    frozen: bool


class ClockStatus(_ClockStatusRequired, total=False):
    #: Where a frozen clock is held.
    frozen_at: str


class _ClockEffectRequired(TypedDict):
    source: str
    count: int


class ClockEffect(_ClockEffectRequired, total=False):
    detail: Any


class ClockAdvance(ClockStatus):
    #: What the move set off, such as schedules that fired; empty when nothing became due.
    effects: List[ClockEffect]


class BedrockStatus(TypedDict, total=False):
    #: "replay" or "record".
    mode: str
    #: The mode set by STRATUS_BEDROCK_MODE, restored by reset.
    default_mode: str
    #: Directory of fixture files (STRATUS_BEDROCK_FIXTURES), if set.
    fixture_dir: str
    #: Real Bedrock endpoint used in record mode.
    upstream: str


class BedrockFixture(TypedDict, total=False):
    """A canned model answer."""

    id: str
    #: "invoke" or "converse"; absent answers both.
    kind: str
    model: str
    request_hash: str
    match: Dict[str, str]
    text: str
    response: Any
    request: Any
    recorded_at: str
    #: "runtime" for fixtures added through the API, else the fixture file.
    origin: str


class _BedrockInvocationBase(TypedDict):
    id: str
    time: str
    operation: str
    model: str
    prompt: str
    request_hash: str
    #: replayed, recorded, synthesized, missed or failed.
    outcome: str


class BedrockInvocation(_BedrockInvocationBase, total=False):
    """One model call the application made."""

    response: str
    fixture_id: str
    input_tokens: int
    output_tokens: int
    error: str


class KafkaCluster(TypedDict):
    """An MSK cluster as the control API lists it."""

    name: str
    arn: str
    #: PROVISIONED or SERVERLESS.
    type: str
    state: str
    kafka_version: str
    #: host:port to pass to a Kafka client; empty while no broker runs.
    bootstrap_brokers: str


class DnsAnswer(TypedDict):
    """One record in a DNS answer."""

    name: str
    type: str
    value: str
    ttl: int
    zone_id: str


class DnsResolution(TypedDict):
    """What a lookup against the emulated hosted zones returns."""

    name: str
    type: str
    #: CNAME steps first, then the records of the requested type.
    answers: List[DnsAnswer]
    #: False for NXDOMAIN or when the name has no records of that type.
    found: bool


class LoadRunInput(TypedDict, total=False):
    """A load run to start: its limits and chaos last until the run stops."""

    name: str
    limits: LimitsProfile
    chaos: ChaosExperiment


class LoadCounts(TypedDict):
    """Requests by outcome; ``errors`` failed for reasons other than throttling or chaos."""

    requests: int
    ok: int
    throttled: int
    faults: int
    errors: int
    p50_ms: float
    p95_ms: float
    p99_ms: float
    max_ms: float


class LoadSecond(LoadCounts):
    t: int


class LoadOperation(LoadCounts):
    service: str
    operation: str


class LoadTotals(LoadCounts):
    per_second: float
    throttle_rate: float
    error_rate: float


class _LoadFindingBase(TypedDict):
    detector: str
    severity: str
    title: str
    detail: str
    suggestion: str


class LoadFinding(_LoadFindingBase, total=False):
    """A problem spotted in a load run's traffic, with what to do about it."""

    service: str
    operation: str
    resource: str
    evidence: Dict[str, Any]


class _LoadRunSummaryBase(TypedDict):
    id: str
    status: str
    started_at: str
    duration_seconds: float
    totals: LoadTotals
    timeline: List[LoadSecond]
    operations: List[LoadOperation]
    findings: List[LoadFinding]


class LoadRunSummary(_LoadRunSummaryBase, total=False):
    name: str
    ended_at: str
    limits: LimitsStatus
    chaos: ChaosReport


class _LoadRunInfoBase(TypedDict):
    id: str
    status: str
    started_at: str
    requests: int


class LoadRunInfo(_LoadRunInfoBase, total=False):
    name: str
    ended_at: str


AdviseVerdict = Literal["fits", "fits_with_caveats", "ruled_out"]
AdviseStatus = Literal["draft", "assessed", "decided"]
#: Answers keyed by question ID, e.g. {"ordering": "per_key", "peak_rate": 2000, "replay": True}.
AdviseAnswers = Dict[str, Any]


class AdviseReason(TypedDict, total=False):
    kind: Literal["ruled_out", "caveat", "strength", "unchecked"]
    text: str
    citation: Dict[str, str]


class AdviseCostLine(TypedDict):
    item: str
    price_id: str
    quantity: float
    unit: str
    usd: float


class _AdviseEstimateBase(TypedDict):
    option: str
    region: str
    monthly_usd: float
    lines: List[AdviseCostLine]


class AdviseEstimate(_AdviseEstimateBase, total=False):
    """An option's monthly cost at the stated average rate, from on-demand
    list prices published on ``prices_as_of``. With ``unpriced`` set, the
    total is a lower bound."""

    unpriced: List[str]
    assumptions: List[str]
    prices_as_of: str
    source: str


class _AdviseAssessmentBase(TypedDict):
    option: str
    verdict: AdviseVerdict
    score: int
    reasons: List[AdviseReason]


class AdviseAssessment(_AdviseAssessmentBase, total=False):
    cost: AdviseEstimate


class AdviseQuestion(TypedDict, total=False):
    id: str
    prompt: str
    why: str
    kind: Literal["choice", "quantity", "bool", "text"]
    choices: List[Dict[str, str]]
    unit: str
    min: float
    max: float
    default: Any


class AdviseFamily(TypedDict):
    id: str
    title: str
    summary: str
    options: List[Dict[str, Any]]
    questions: List[AdviseQuestion]


class _AdviseDecisionBase(TypedDict):
    id: str
    title: str
    family: str
    family_title: str
    status: AdviseStatus
    answers: AdviseAnswers
    pending_questions: List[AdviseQuestion]
    next: str
    created_at: str
    updated_at: str


class _AdviseEvidenceBase(TypedDict):
    option: str
    run_id: str
    verdict: Literal["held", "throttled", "failed", "short"]
    target_rate: float
    scale: float
    duration_seconds: float
    requests: int
    ok: int
    throttled: int
    errors: int
    faults: int
    per_second: float
    p99_ms: float
    recorded_at: str


class AdviseEvidence(_AdviseEvidenceBase, total=False):
    """What a load proof recorded for one option."""

    account: str
    throttled_by: List[str]
    findings: List[str]


class _AdviseProofPlanBase(TypedDict):
    option: str
    provable: bool
    account: str
    region: str
    rate: float
    duration_seconds: int
    scale: float
    limits: Dict[str, Any]


class AdviseProofPlan(_AdviseProofPlanBase, total=False):
    """How to prove one option under load (a k6 ``script`` and the ``limits``
    to run it under), or the ``reason`` it cannot be proved on Stratus."""

    reason: str
    operations: List[Dict[str, str]]
    script: str


class AdviseDecision(_AdviseDecisionBase, total=False):
    """An architecture decision with its ranking and what to do next."""

    evidence: List[AdviseEvidence]
    assessments: List[AdviseAssessment]
    chosen: str
    rationale: str
    recommended: str
    assessed_at: str
    decided_at: str
