"""Generate Stratus's operation catalogue from the AWS service models that botocore ships.

For every service Stratus emulates, this writes ``internal/awsmodel/models/<service>.json``: the
service's protocol and signing name, each operation's HTTP binding and declared errors, and the
input shapes reachable from those operations with the constraints AWS enforces on them
(``required``, ``min``, ``max``, ``pattern``, ``enum``). Documentation and output shapes are left
out; the catalogue exists to validate requests and to know what AWS defines.

    python scripts/models/generate.py                 # uses the installed botocore
    python scripts/models/generate.py --data DIR      # or a botocore data directory

Pin the botocore version in requirements.txt beside this script and regenerate when it moves.
"""

from __future__ import annotations

import argparse
import gzip
import json
import sys
from pathlib import Path
from typing import Any

#: Stratus service name (the catalog name in internal/control/catalog.go) -> botocore service
#: names. SES v1 and v2 are served by one Stratus service.
SERVICES: dict[str, list[str]] = {
    "acm": ["acm"],
    "apigateway": ["apigateway"],
    "apigatewayv2": ["apigatewayv2"],
    "bedrock": ["bedrock-runtime"],
    "cloudformation": ["cloudformation"],
    "cognitoidp": ["cognito-idp"],
    "dynamodb": ["dynamodb"],
    "dynamodbstreams": ["dynamodbstreams"],
    "ec2": ["ec2"],
    "ecr": ["ecr"],
    "ecs": ["ecs"],
    "elasticache": ["elasticache"],
    "elbv2": ["elbv2"],
    "events": ["events"],
    "fis": ["fis"],
    "iam": ["iam"],
    "kafka": ["kafka"],
    "kinesis": ["kinesis"],
    "kms": ["kms"],
    "lambda": ["lambda"],
    "logs": ["logs"],
    "monitoring": ["cloudwatch"],
    "rds": ["rds"],
    "route53": ["route53"],
    "s3": ["s3"],
    "scheduler": ["scheduler"],
    "secretsmanager": ["secretsmanager"],
    "servicediscovery": ["servicediscovery"],
    "ses": ["ses", "sesv2"],
    "sns": ["sns"],
    "sqs": ["sqs"],
    "ssm": ["ssm"],
    "stepfunctions": ["stepfunctions"],
    "sts": ["sts"],
}

SHAPE_KEYS = ("type", "required", "min", "max", "pattern", "enum")
MEMBER_KEYS = ("location", "locationName", "jsonName")


def latest_model(data: Path, name: str) -> Path:
    versions = sorted(p for p in (data / name).iterdir() if p.is_dir())
    if not versions:
        raise SystemExit(f"no model for {name} under {data}")
    model = versions[-1] / "service-2.json.gz"
    return model if model.exists() else versions[-1] / "service-2.json"


def load(path: Path) -> dict[str, Any]:
    opener = gzip.open if path.suffix == ".gz" else open
    with opener(path, "rt", encoding="utf-8") as f:
        return json.load(f)


def compact_shape(shape: dict[str, Any]) -> dict[str, Any]:
    out = {k: shape[k] for k in SHAPE_KEYS if k in shape}
    if "members" in shape:
        out["members"] = {
            name: {"shape": m["shape"], **{k: m[k] for k in MEMBER_KEYS if k in m}}
            for name, m in shape["members"].items()
        }
    for key in ("member", "key", "value"):
        if key in shape:
            out[key] = shape[key]["shape"]
    return out


def reachable(shapes: dict[str, Any], roots: list[str]) -> list[str]:
    seen: set[str] = set()
    stack = list(roots)
    while stack:
        name = stack.pop()
        if name in seen or name not in shapes:
            continue
        seen.add(name)
        shape = shapes[name]
        stack.extend(m["shape"] for m in shape.get("members", {}).values())
        stack.extend(shape[k]["shape"] for k in ("member", "key", "value") if k in shape)
    return sorted(seen)


def compact_model(model: dict[str, Any]) -> dict[str, Any]:
    meta = model["metadata"]
    shapes = model["shapes"]
    operations: dict[str, Any] = {}
    roots: list[str] = []
    for name, op in sorted(model["operations"].items()):
        entry: dict[str, Any] = {"http": {k: op["http"][k] for k in ("method", "requestUri") if k in op.get("http", {})}}
        if "input" in op:
            entry["input"] = op["input"]["shape"]
            roots.append(op["input"]["shape"])
        errors = sorted({e["shape"] for e in op.get("errors", [])})
        if errors:
            entry["errors"] = errors
        if op.get("deprecated"):
            entry["deprecated"] = True
        operations[name] = entry
    return {
        "name": meta.get("endpointPrefix", ""),
        "apiVersion": meta.get("apiVersion", ""),
        "protocol": meta.get("protocol", ""),
        "jsonVersion": meta.get("jsonVersion", ""),
        "targetPrefix": meta.get("targetPrefix", ""),
        "signingName": meta.get("signingName", meta.get("endpointPrefix", "")),
        "operations": operations,
        "shapes": {n: compact_shape(shapes[n]) for n in reachable(shapes, roots)},
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--data", type=Path, help="botocore data directory (default: the installed botocore's)")
    parser.add_argument("--out", type=Path, default=Path(__file__).resolve().parents[2] / "internal" / "awsmodel" / "models")
    args = parser.parse_args(argv)

    version = "unknown"
    data = args.data
    if data is None:
        import botocore  # type: ignore[import-untyped]

        data = Path(botocore.__file__).parent / "data"
        version = botocore.__version__

    args.out.mkdir(parents=True, exist_ok=True)
    for service, models in SERVICES.items():
        catalogue = {
            "service": service,
            "botocore": version,
            "models": [compact_model(load(latest_model(data, m))) | {"model": m} for m in models],
        }
        path = args.out / f"{service}.json"
        path.write_text(json.dumps(catalogue, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
        ops = sum(len(m["operations"]) for m in catalogue["models"])
        print(f"{service:18} {ops:5} operations  {path.stat().st_size / 1024:8.1f} KiB", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
