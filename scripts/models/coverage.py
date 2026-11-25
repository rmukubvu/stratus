"""Measure which AWS operations a running Stratus implements, and write the coverage pages.

For every operation in the catalogue (internal/awsmodel/models), this sends one request with
botocore, filling only the required parameters with placeholder values, and reads the answer:

* implemented: Stratus handled it, success or an AWS error such as ResourceNotFoundException;
* not implemented: Stratus answered 501, or a not-implemented / unknown-operation error.

It writes internal/awsmodel/coverage.json (embedded in the binary, read by ``stratus check``
and the MCP coverage tool) and docs/coverage/<service>.md.

    STRATUS_VALIDATION=off DOCKER_HOST=unix:///nonexistent stratus serve --port 4599 &
    python scripts/models/coverage.py --endpoint http://localhost:4599

Turn request validation off: placeholders do not satisfy every pattern, and a ValidationException
for an operation Stratus lacks would read as implemented. Docker off keeps placeholders from
starting engines; it does not change the result.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import sys
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
MODELS = ROOT / "internal" / "awsmodel" / "models"

#: Error codes Stratus answers with for an operation it does not implement.
NOT_IMPLEMENTED_CODES = frozenset(
    {
        "NotImplemented",
        "NotImplementedException",
        "UnknownOperationException",
        "UnknownOperation",
        "InvalidAction",
    }
)

IMPLEMENTED, NOT_IMPLEMENTED = "implemented", "not_implemented"

#: Resources created before probing, named like the placeholders, for services that look a
#: resource up before they look at the operation (S3 answers NoSuchBucket to anything otherwise).
SETUP: dict[str, list[tuple[str, dict[str, Any]]]] = {
    "s3": [("CreateBucket", {"Bucket": "stratusprobe"})],
}


@dataclass(frozen=True)
class Probe:
    service: str
    model: str
    operation: str
    status: str
    http_status: int
    code: str
    message: str

#: Parameters for operations Stratus implements for some inputs only, so the probe exercises the
#: supported path instead of reading "only X is supported" as not implemented.
PARAMS: dict[tuple[str, str], dict[str, Any]] = {
    ("apigatewayv2", "CreateApi"): {"Name": "stratusprobe", "ProtocolType": "HTTP"},
    ("elbv2", "CreateListener"): {
        "LoadBalancerArn": "stratusprobe",
        "DefaultActions": [{"Type": "forward", "TargetGroupArn": "stratusprobe"}],
    },
    ("secretsmanager", "DeleteSecret"): {"SecretId": "stratusprobe", "ForceDeleteWithoutRecovery": True},
    ("ssm", "ListTagsForResource"): {"ResourceType": "Parameter", "ResourceId": "stratusprobe"},
}


def placeholder(shape: Any, depth: int = 0) -> Any:
    """A value for a botocore shape that serializes: required members only, lengths at min."""
    kind = shape.type_name
    if kind == "structure":
        if depth > 6:
            return {}
        return {name: placeholder(shape.members[name], depth + 1) for name in shape.required_members}
    if kind == "list":
        count = max(int(shape.metadata.get("min", 0)), 0)
        return [placeholder(shape.member, depth + 1) for _ in range(min(count, 1))]
    if kind == "map":
        return {}
    if kind == "string":
        if shape.enum:
            return shape.enum[0]
        size = max(int(shape.metadata.get("min", 1)), 1)
        return ("stratusprobe" * (size // 12 + 1))[: max(size, 12)]
    if kind in ("integer", "long"):
        return max(int(shape.metadata.get("min", 1)), 1)
    if kind in ("float", "double"):
        return float(max(shape.metadata.get("min", 1), 1))
    if kind == "boolean":
        return False
    if kind == "blob":
        return b"stratus"
    if kind == "timestamp":
        return dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc)
    return None


def classify(http_status: int, code: str) -> str:
    if http_status == 501 or code in NOT_IMPLEMENTED_CODES:
        return NOT_IMPLEMENTED
    return IMPLEMENTED


def client_for(session: Any, endpoint: str, model: str) -> Any:
    import botocore.config

    config = botocore.config.Config(
        parameter_validation=False,
        retries={"max_attempts": 1, "mode": "standard"},
        connect_timeout=5,
        read_timeout=20,
        s3={"addressing_style": "path"},
        inject_host_prefix=False,
    )
    return session.create_client(
        model,
        region_name="us-east-1",
        endpoint_url=endpoint,
        aws_access_key_id="test",
        aws_secret_access_key="test",
        config=config,
    )


def probe_operation(session: Any, endpoint: str, service: str, model: str, operation: str) -> Probe:
    import botocore.exceptions

    client = client_for(session, endpoint, model)
    op_model = client.meta.service_model.operation_model(operation)
    params = PARAMS.get((service, operation))
    if params is None:
        params = placeholder(op_model.input_shape) if op_model.input_shape is not None else {}
    try:
        response = client._make_api_call(operation, params)
        status = response.get("ResponseMetadata", {}).get("HTTPStatusCode", 200)
        return Probe(service, model, operation, IMPLEMENTED, status, "", "")
    except botocore.exceptions.ClientError as err:
        meta = err.response.get("ResponseMetadata", {})
        error = err.response.get("Error", {})
        status = int(meta.get("HTTPStatusCode", 0))
        code = str(error.get("Code", ""))
        return Probe(service, model, operation, classify(status, code), status, code, str(error.get("Message", ""))[:200])
    except Exception as err:  # noqa: BLE001 - a probe failure is a result, not a crash
        return Probe(service, model, operation, IMPLEMENTED, 0, type(err).__name__, str(err)[:200])


def catalogue() -> dict[str, list[tuple[str, list[str]]]]:
    out: dict[str, list[tuple[str, list[str]]]] = {}
    for path in sorted(MODELS.glob("*.json")):
        data = json.loads(path.read_text(encoding="utf-8"))
        out[data["service"]] = [(m["model"], sorted(m["operations"])) for m in data["models"]]
    return out


def write_page(service: str, probes: list[Probe], out: Path) -> None:
    done = [p for p in probes if p.status == IMPLEMENTED]
    lines = [
        f"# {service} coverage",
        "",
        "Generated by `scripts/models/coverage.py`; do not edit by hand.",
        "",
        f"Stratus implements **{len(done)} of {len(probes)}** operations AWS defines for {service}.",
        "An operation that is not implemented answers `501 NotImplemented`, never an empty success.",
        "",
        "| Operation | Stratus |",
        "| --- | --- |",
    ]
    multi = len({p.model for p in probes}) > 1
    for p in sorted(probes, key=lambda p: (p.model, p.operation)):
        name = f"{p.operation} ({p.model})" if multi else p.operation
        lines.append(f"| {name} | {'yes' if p.status == IMPLEMENTED else 'no'} |")
    out.write_text("\n".join(lines) + "\n", encoding="utf-8")


def write_index(results: dict[str, list[Probe]], out: Path) -> None:
    total = sum(len(v) for v in results.values())
    done = sum(1 for v in results.values() for p in v if p.status == IMPLEMENTED)
    lines = [
        "# Operation coverage",
        "",
        "Generated by `scripts/models/coverage.py` from the AWS service models in",
        "`internal/awsmodel/models`; do not edit by hand. `stratus check` reads the same data.",
        "",
        f"Across the services Stratus emulates, it implements **{done} of {total}** operations AWS defines.",
        "Everything else answers `501 NotImplemented`.",
        "",
        "| Service | Implemented | AWS operations | |",
        "| --- | ---: | ---: | --- |",
    ]
    for service, probes in sorted(results.items()):
        n = sum(1 for p in probes if p.status == IMPLEMENTED)
        lines.append(f"| {service} | {n} | {len(probes)} | [details]({service}.md) |")
    out.write_text("\n".join(lines) + "\n", encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    import botocore.exceptions
    import botocore.session

    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--endpoint", default="http://localhost:4599")
    parser.add_argument("--service", action="append", help="probe only these Stratus services")
    parser.add_argument("--raw", type=Path, help="also write every probe's answer here, for debugging")
    parser.add_argument("--workers", type=int, default=8)
    args = parser.parse_args(argv)

    reset = urllib.request.Request(args.endpoint.rstrip("/") + "/_stratus/control/v1/reset", data=b"{}", method="POST")
    urllib.request.urlopen(reset, timeout=30).close()  # probe a fresh emulator: results must not depend on earlier runs

    session = botocore.session.get_session()
    for service, calls in SETUP.items():
        for operation, params in calls:
            try:
                client_for(session, args.endpoint, service)._make_api_call(operation, params)
            except botocore.exceptions.ClientError as err:
                raise SystemExit(f"setup {service} {operation}: {err}") from err
    def run_service(item: tuple[str, list[tuple[str, list[str]]]]) -> list[Probe]:
        # One service at a time, deletes last: a DeleteBucket that ran first would turn every
        # later S3 probe into NoSuchBucket.
        service, models = item
        ordered = sorted(((m, op) for m, ops in models for op in ops), key=lambda j: (j[1].startswith("Delete"), j))
        return [probe_operation(session, args.endpoint, service, m, op) for m, op in ordered]

    selected = [item for item in catalogue().items() if not args.service or item[0] in args.service]
    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        probes = [p for batch in pool.map(run_service, selected) for p in batch]

    results: dict[str, list[Probe]] = {}
    for p in probes:
        results.setdefault(p.service, []).append(p)

    if args.raw:
        args.raw.write_text(json.dumps([p.__dict__ for p in probes], indent=1), encoding="utf-8")

    coverage_path = ROOT / "internal" / "awsmodel" / "coverage.json"
    coverage = json.loads(coverage_path.read_text(encoding="utf-8")) if coverage_path.exists() else {}
    for service, items in results.items():
        coverage[service] = sorted(p.operation for p in items if p.status == IMPLEMENTED)
    coverage_path.write_text(json.dumps(coverage, indent=1, sort_keys=True) + "\n", encoding="utf-8")

    docs = ROOT / "docs" / "coverage"
    docs.mkdir(parents=True, exist_ok=True)
    for service, items in results.items():
        write_page(service, items, docs / f"{service}.md")
    if not args.service:
        write_index(results, docs / "README.md")

    for service, items in sorted(results.items()):
        n = sum(1 for p in items if p.status == IMPLEMENTED)
        print(f"{service:18} {n:4}/{len(items)}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
