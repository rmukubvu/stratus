"""Client for the Stratus control API."""

from __future__ import annotations

import os
import secrets
import time
import urllib.parse
from contextlib import contextmanager
from datetime import datetime, timezone
from typing import Any, Dict, Iterator, List, Optional, Union, cast

from .errors import StratusError
from .transport import Transport, UrllibTransport
from .types import (
    BedrockFixture,
    BedrockInvocation,
    BedrockStatus,
    ChaosExperiment,
    ChaosReport,
    Diagnosis,
    Health,
    ResetResult,
    SchedulerAdvance,
    SchedulerClock,
    SchedulerInvocation,
    SesMessage,
    Snapshot,
)

DEFAULT_ENDPOINT = "http://localhost:4566"


class Snapshots:
    """Save, list and restore whole-emulator state."""

    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def create(self, name: Optional[str] = None) -> Snapshot:
        """Capture all state. The server picks a name when none is given."""
        return cast(Snapshot, self._t.request("POST", "/snapshots", {} if name is None else {"name": name}))

    def list(self) -> List[Snapshot]:
        res = self._t.request("GET", "/snapshots") or {}
        return cast(List[Snapshot], res.get("items") or [])

    def get(self, name: str) -> Snapshot:
        return cast(Snapshot, self._t.request("GET", f"/snapshots/{_quote(name)}"))

    def restore(self, name: str) -> Snapshot:
        """Replace all live state with the snapshot. It can be restored again later."""
        return cast(Snapshot, self._t.request("POST", f"/snapshots/{_quote(name)}/restore")["restored"])

    def delete(self, name: str) -> None:
        self._t.request("DELETE", f"/snapshots/{_quote(name)}")


class Chaos:
    """Seeded, reproducible fault injection."""

    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def start(self, experiment: ChaosExperiment) -> ChaosReport:
        return cast(ChaosReport, self._t.request("PUT", "/chaos", experiment))

    def status(self) -> ChaosReport:
        """The running experiment, or the last stopped one. Raises NoChaosExperiment if none was started."""
        return cast(ChaosReport, self._t.request("GET", "/chaos"))

    def stop(self) -> ChaosReport:
        """Stop injecting and return the final per-rule counts and hits."""
        return cast(ChaosReport, self._t.request("DELETE", "/chaos"))


class Ses:
    """The inbox that captures every email sent through SES v1 or v2."""

    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def messages(
        self,
        to: Optional[str] = None,
        sender: Optional[str] = None,
        subject: Optional[str] = None,
        since: Union[str, datetime, None] = None,
        limit: Optional[int] = None,
    ) -> List[SesMessage]:
        """Emails sent so far, oldest first.

        ``to`` matches To, Cc and Bcc; ``subject`` is a case-insensitive
        substring; ``since`` is a duration such as ``"5m"`` or a datetime;
        ``limit`` keeps only the newest N.
        """
        query: Dict[str, str] = {}
        if to:
            query["to"] = to
        if sender:
            query["from"] = sender
        if subject:
            query["subject"] = subject
        if since is not None:
            query["since"] = _rfc3339(since) if isinstance(since, datetime) else since
        if limit is not None:
            query["limit"] = str(limit)
        path = "/ses/messages" + ("?" + urllib.parse.urlencode(query) if query else "")
        res = self._t.request("GET", path) or {}
        return cast(List[SesMessage], res.get("items") or [])

    def message(self, message_id: str) -> SesMessage:
        return cast(SesMessage, self._t.request("GET", f"/ses/messages/{_quote(message_id)}"))

    def clear(self) -> int:
        """Empty the inbox and return how many emails were removed. Identities and templates stay."""
        return int(self._t.request("DELETE", "/ses/messages")["cleared"])


class Scheduler:
    """The EventBridge Scheduler clock: move it instead of sleeping."""

    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def clock(self) -> SchedulerClock:
        return cast(SchedulerClock, self._t.request("GET", "/scheduler/clock"))

    def advance(self, by: Union[str, datetime]) -> SchedulerAdvance:
        """Move the clock forward by a duration such as ``"1h"``, or to a datetime.

        Every schedule that became due fires once before this returns; the
        result lists what fired.
        """
        body = {"to": _rfc3339(by)} if isinstance(by, datetime) else {"by": by}
        res = self._t.request("POST", "/scheduler/advance", body)
        res["fired"] = res.get("fired") or []
        return cast(SchedulerAdvance, res)

    def run(self, name: str, group: Optional[str] = None) -> SchedulerInvocation:
        """Fire one schedule now, whatever its expression or state says."""
        body: Dict[str, str] = {"name": name}
        if group is not None:
            body["group"] = group
        return cast(SchedulerInvocation, self._t.request("POST", "/scheduler/run", body))


class Bedrock:
    """Bedrock fixtures and the log of model calls."""

    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def status(self) -> BedrockStatus:
        return cast(BedrockStatus, self._t.request("GET", "/bedrock"))

    def set_mode(self, mode: str) -> BedrockStatus:
        """Switch between ``"replay"`` and ``"record"`` until the next reset."""
        return cast(BedrockStatus, self._t.request("PUT", "/bedrock/mode", {"mode": mode}))

    def fixtures(self) -> List[BedrockFixture]:
        """Every fixture in match order: runtime fixtures, then fixture files."""
        res = self._t.request("GET", "/bedrock/fixtures") or {}
        return cast(List[BedrockFixture], res.get("items") or [])

    def add_fixture(
        self,
        text: Optional[str] = None,
        *,
        model: Optional[str] = None,
        contains: Optional[str] = None,
        kind: Optional[str] = None,
        response: Any = None,
        request_hash: Optional[str] = None,
        fixture_id: Optional[str] = None,
    ) -> BedrockFixture:
        """Answer matching model calls with ``text``, or with an exact ``response`` body and ``kind``.

        ``model`` is a model id, inference profile or pattern with ``*``;
        ``contains`` limits the fixture to prompts containing that text.
        The fixture lasts until reset and comes before fixture files.
        """
        body: Dict[str, Any] = {}
        for key, value in (("id", fixture_id), ("model", model), ("kind", kind), ("text", text), ("request_hash", request_hash)):
            if value is not None:
                body[key] = value
        if contains is not None:
            body["match"] = {"contains": contains}
        if response is not None:
            body["response"] = response
        return cast(BedrockFixture, self._t.request("POST", "/bedrock/fixtures", body))

    def delete_fixture(self, fixture_id: str) -> None:
        self._t.request("DELETE", f"/bedrock/fixtures/{_quote(fixture_id)}")

    def clear_fixtures(self) -> int:
        """Remove every runtime fixture and return how many there were. Fixture files stay."""
        return int(self._t.request("DELETE", "/bedrock/fixtures")["cleared"])

    def invocations(
        self,
        model: Optional[str] = None,
        contains: Optional[str] = None,
        outcome: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> List[BedrockInvocation]:
        """Model calls so far, oldest first."""
        query: Dict[str, str] = {}
        for key, value in (("model", model), ("contains", contains), ("outcome", outcome)):
            if value:
                query[key] = value
        if limit is not None:
            query["limit"] = str(limit)
        path = "/bedrock/invocations" + ("?" + urllib.parse.urlencode(query) if query else "")
        res = self._t.request("GET", path) or {}
        return cast(List[BedrockInvocation], res.get("items") or [])

    def clear_invocations(self) -> int:
        return int(self._t.request("DELETE", "/bedrock/invocations")["cleared"])


class ChaosRun:
    """Handle yielded by :meth:`StratusClient.chaos_experiment`; ``report`` is set on exit."""

    def __init__(self, started: ChaosReport) -> None:
        self.started = started
        self.report: Optional[ChaosReport] = None


class StratusClient:
    """Client for the Stratus control API.

    ::

        stratus = StratusClient()
        stratus.wait_until_ready()
        with stratus.snapshot_scope():
            ...  # anything created here is rolled back afterwards
    """

    def __init__(
        self,
        endpoint: Optional[str] = None,
        *,
        region: Optional[str] = None,
        timeout: float = 60.0,
        transport: Optional[Transport] = None,
    ) -> None:
        self.endpoint = (endpoint or _env("STRATUS_ENDPOINT") or DEFAULT_ENDPOINT).rstrip("/")
        self.region = region or _env("AWS_REGION") or _env("AWS_DEFAULT_REGION") or "us-east-1"
        self._t: Transport = transport or UrllibTransport(self.endpoint, timeout)
        self.snapshots = Snapshots(self._t)
        self.chaos = Chaos(self._t)
        self.ses = Ses(self._t)
        self.scheduler = Scheduler(self._t)
        self.bedrock = Bedrock(self._t)

    def health(self) -> Health:
        return cast(Health, self._t.request("GET", "/health"))

    def is_ready(self) -> bool:
        """True when Stratus accepts traffic; False when it is starting, down or unreachable."""
        try:
            self._t.request("GET", "/ready")
            return True
        except StratusError:
            return False

    def wait_until_ready(self, timeout: float = 30.0, interval: float = 0.1) -> None:
        """Poll until Stratus is ready. Raises StratusTimeout after ``timeout`` seconds."""
        deadline = time.monotonic() + timeout
        while True:
            if self.is_ready():
                return
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise StratusError(
                    StratusError.TIMEOUT,
                    f"Stratus at {self.endpoint} was not ready after {timeout}s",
                    "check that it is running: stratus wait --timeout 30s",
                )
            time.sleep(min(interval, remaining))

    def reset(self, *services: str) -> ResetResult:
        """Reset the named services, or everything when none are named."""
        return cast(ResetResult, self._t.request("POST", "/reset", {"services": list(services)}))

    def diagnose(
        self,
        since: Union[str, datetime, None] = None,
        service: Optional[str] = None,
        exclude_injected: bool = False,
        max_groups: Optional[int] = None,
    ) -> Diagnosis:
        """Group recent failed requests and explain each group.

        ``since`` is a duration such as ``"5m"`` or a datetime.
        """
        query: Dict[str, str] = {}
        if since is not None:
            query["since"] = _rfc3339(since) if isinstance(since, datetime) else since
        if service:
            query["service"] = service
        if exclude_injected:
            query["exclude_injected"] = "true"
        if max_groups is not None:
            query["max_groups"] = str(max_groups)
        path = "/diagnostics" + ("?" + urllib.parse.urlencode(query) if query else "")
        d: Dict[str, Any] = self._t.request("GET", path)
        d["by_category"] = d.get("by_category") or {}
        d["groups"] = d.get("groups") or []
        return cast(Diagnosis, d)

    @contextmanager
    def snapshot_scope(self) -> Iterator[Snapshot]:
        """Roll all state back to how it was on entry, even if the block raises.

        The temporary snapshot is deleted on exit.
        """
        snap = self.snapshots.create(_temp_snapshot_name())
        try:
            yield snap
        finally:
            try:
                self.snapshots.restore(snap["id"])
            finally:
                self.snapshots.delete(snap["id"])

    @contextmanager
    def chaos_experiment(self, experiment: ChaosExperiment) -> Iterator[ChaosRun]:
        """Run the block under a chaos experiment and always stop it on exit.

        The final report is available as ``run.report`` after the block.
        """
        run = ChaosRun(self.chaos.start(experiment))
        try:
            yield run
        finally:
            run.report = self.chaos.stop()

    def boto3_kwargs(self) -> Dict[str, Any]:
        """Keyword arguments for ``boto3.client(...)`` that point it at Stratus."""
        return {
            "endpoint_url": self.endpoint,
            "region_name": self.region,
            "aws_access_key_id": "test",
            "aws_secret_access_key": "test",
        }

    def aws_env(self) -> Dict[str, str]:
        """Environment variables that point the AWS CLI or any AWS SDK at Stratus."""
        return {
            "AWS_ENDPOINT_URL": self.endpoint,
            "STRATUS_ENDPOINT": self.endpoint,
            "AWS_REGION": self.region,
            "AWS_DEFAULT_REGION": self.region,
            "AWS_ACCESS_KEY_ID": "test",
            "AWS_SECRET_ACCESS_KEY": "test",
        }


def _env(name: str) -> Optional[str]:
    return os.environ.get(name) or None


def _quote(name: str) -> str:
    return urllib.parse.quote(name, safe="")


def _rfc3339(t: datetime) -> str:
    if t.tzinfo is None:
        t = t.replace(tzinfo=timezone.utc)
    return t.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _temp_snapshot_name() -> str:
    return f"sdk-tmp-{int(time.time() * 1000):x}-{secrets.token_hex(4)}"
