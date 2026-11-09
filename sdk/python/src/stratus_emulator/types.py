"""Wire types of the Stratus control API. Responses are plain dicts with these shapes."""

from __future__ import annotations

from typing import Dict, List, Literal, TypedDict

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


class Snapshot(TypedDict):
    id: str
    created_at: str
    services: List[str]
    size_bytes: int


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
