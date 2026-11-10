"""Wire types of the Stratus control API. Responses are plain dicts with these shapes."""

from __future__ import annotations

from typing import Any, Dict, List, Literal, TypedDict

HealthStatus = Literal["ok", "degraded", "down"]
ChaosAction = Literal[
    "throttle", "internal_error", "unavailable", "error", "latency", "drop", "cold_start", "function_error", "duplicate"
]
#: "api" (default) for AWS API calls, "event" for internal deliveries, "all" for both.
ChaosScope = Literal["api", "event", "all"]
FailureCategory = Literal[
    "injected_fault",
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
