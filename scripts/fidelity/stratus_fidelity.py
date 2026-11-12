"""Score Stratus with AWS's own fidelity tests (aws/aws-emulator-evaluation-suite).

The suite's tests were verified against real AWS. This script picks the ones whose services
Stratus emulates, runs each against a fresh, throwaway Stratus (``stratus ci``), and scores the
reports exactly the way the suite's runner scores AWS Emulator.

    python stratus_fidelity.py run    --suite DIR --stratus BIN --out reports/ [--jobs N]
    python stratus_fidelity.py report --suite DIR --out reports/ --json scores.json --markdown scores.md

``run`` needs the suite's runner installed (``uv sync --locked --extra checks`` in the suite);
run this script with that environment's Python so ``report`` can reuse its scoring.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from collections.abc import Callable, Iterable, Sequence
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path
from typing import Any

#: The suite's service names for every service Stratus emulates. A suite service folder is one
#: service (``sqs``) or several joined by ``__`` (``lambda__sqs``) for a cross-service test.
STRATUS_SERVICES = frozenset(
    {
        "acm",
        "apigateway",
        "apigatewayv2",
        "bedrock-runtime",
        "cloudformation",
        "cloudwatch",
        "cognito-idp",
        "dynamodb",
        "dynamodbstreams",
        "ecr",
        "ecs",
        "elasticache",
        "elbv2",
        "events",
        "fis",
        "iam",
        "kafka",
        "kinesis",
        "kms",
        "lambda",
        "logs",
        "rds",
        "route53",
        "s3",
        "scheduler",
        "secretsmanager",
        "servicediscovery",
        "ses",
        "sesv2",
        "sns",
        "sqs",
        "ssm",
        "stepfunctions",
        "sts",
    }
)

SCOPE_SINGLE = "single"
SCOPE_CROSS = "cross"
SCOPE_ALL = "all"
SCOPES = (SCOPE_SINGLE, SCOPE_CROSS, SCOPE_ALL)

PLANE_CONTROL = "control"
PLANE_DATA = "data"


@dataclass(frozen=True)
class Check:
    """One row of the suite's ``manifest.jsonl``."""

    check_id: str
    service: str
    path: str
    labels: tuple[str, ...]

    @property
    def services(self) -> tuple[str, ...]:
        return tuple(self.service.split("__"))

    @property
    def cross_service(self) -> bool:
        return len(self.services) > 1

    @property
    def planes(self) -> tuple[str, ...]:
        """``control`` and/or ``data``, from the ``plane:`` labels (a test may carry both)."""
        return tuple(label.split(":", 1)[1] for label in self.labels if label.startswith("plane:"))

    @classmethod
    def from_row(cls, row: dict[str, Any]) -> Check:
        return cls(
            check_id=row["check_id"],
            service=row["service"],
            path=row["path"],
            labels=tuple(row.get("labels", ())),
        )


def load_manifest(lines: Iterable[str]) -> list[Check]:
    return [Check.from_row(json.loads(line)) for line in lines if line.strip()]


def select(
    checks: Iterable[Check], supported: frozenset[str] = STRATUS_SERVICES, scope: str = SCOPE_ALL
) -> list[Check]:
    """The checks every one of whose services Stratus emulates, narrowed to ``scope``."""
    if scope not in SCOPES:
        raise ValueError(f"unknown scope {scope!r}; want one of {SCOPES}")
    chosen = [c for c in checks if set(c.services) <= supported]
    if scope == SCOPE_SINGLE:
        return [c for c in chosen if not c.cross_service]
    if scope == SCOPE_CROSS:
        return [c for c in chosen if c.cross_service]
    return chosen


def shard(checks: Sequence[Check], spec: str) -> list[Check]:
    """The ``I/N`` share of ``checks`` (``I`` from 1): every N-th, so slow services spread out."""
    index, _, count = spec.partition("/")
    i, n = int(index), int(count)
    if not 1 <= i <= n:
        raise ValueError(f"shard {spec!r} is not I/N with 1 <= I <= N")
    return list(checks[i - 1 :: n])


def report_path(out: Path, check: Check) -> Path:
    return out / check.service / f"{check.check_id.split('.', 1)[1]}.json"


def run_command(stratus: str, runner: str, check_file: Path, report: Path) -> list[str]:
    """``stratus ci`` starting a fresh Stratus and running one fidelity test against it.

    ``stratus ci`` picks a free port, a throwaway data dir and exports ``AWS_ENDPOINT_URL``; the
    runner signs with its fixed ``test``/``test`` keys (``--auth off``), which Stratus accepts.
    """
    script = 'exec "$0" run "$1" --endpoint "$AWS_ENDPOINT_URL" --auth off --report "$2"'
    return [stratus, "ci", "--", "sh", "-c", script, runner, str(check_file), str(report)]


def run_all(
    checks: Sequence[Check],
    suite: Path,
    out: Path,
    command: Callable[[Path, Path], list[str]],
    jobs: int,
    execute: Callable[[list[str]], int] | None = None,
    log: Callable[[str], None] = print,
) -> int:
    """Run every check that has no report yet; returns how many ran. Reruns resume."""
    execute = execute or _execute
    todo = [c for c in checks if not report_path(out, c).exists()]
    log(f"{len(checks) - len(todo)} of {len(checks)} already reported; running {len(todo)}")
    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=max(1, jobs)) as pool:
        futures = {}
        for check in todo:
            report = report_path(out, check)
            report.parent.mkdir(parents=True, exist_ok=True)
            futures[pool.submit(execute, command(suite / check.path, report))] = check
        for done, future in enumerate(as_completed(futures), 1):
            future.result()
            if done % 100 == 0 or done == len(todo):
                log(f"{done}/{len(todo)} ({time.monotonic() - started:.0f}s)")
    return len(todo)


def _execute(argv: list[str]) -> int:
    return subprocess.run(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode


@dataclass(frozen=True)
class Result:
    check_id: str
    service: str
    kind: str
    cleanup_failed: bool


def read_reports(out: Path) -> list[Result]:
    """The results in the run reports under ``out``; unreadable files are skipped."""
    results = []
    for path in sorted(out.rglob("*.json")):
        try:
            check = json.loads(path.read_text(encoding="utf-8"))["check"]
            results.append(
                Result(
                    check["check_id"],
                    check["service"],
                    check["kind"],
                    bool(check.get("cleanup_failed")),
                )
            )
        except (OSError, ValueError, KeyError, TypeError):
            continue
    return results


Tally = Callable[[Iterable[tuple[str, str, bool]]], dict[str, Any]]


def score(results: Sequence[Result], manifest: Sequence[Check], tally: Tally) -> dict[str, Any]:
    """Overall, per-service and per-plane scores, each computed by the suite's own ``tally``.

    Per-service scores are for single-service tests; cross-service tests are scored together.
    """
    by_id = {c.check_id: c for c in manifest}

    def rows(rs: Iterable[Result]) -> list[tuple[str, str, bool]]:
        return [(r.check_id, r.kind, r.cleanup_failed) for r in rs]

    single = [r for r in results if "__" not in r.service]
    cross = [r for r in results if "__" in r.service]
    services: dict[str, list[Result]] = {}
    for r in single:
        services.setdefault(r.service, []).append(r)
    planes = {
        plane: [r for r in results if plane in by_id.get(r.check_id, _NO_CHECK).planes]
        for plane in (PLANE_CONTROL, PLANE_DATA)
    }
    return {
        "overall": tally(rows(results)),
        "single_service": tally(rows(single)),
        "cross_service": tally(rows(cross)),
        "by_plane": {plane: tally(rows(rs)) for plane, rs in planes.items()},
        "by_service": {svc: tally(rows(rs)) for svc, rs in sorted(services.items())},
    }


_NO_CHECK = Check("", "", "", ())


def _pct(part: int, whole: int) -> str:
    return f"{100 * part / whole:.1f}%" if whole else "n/a"


def render_markdown(scores: dict[str, Any], suite_version: str, stratus_version: str) -> str:
    """The scores as the Markdown tables ``docs/fidelity.md`` publishes."""

    def line(name: str, s: dict[str, Any]) -> str:
        failed = s["failed_by_kind"]
        not_impl = failed.get("not_implemented", 0)
        other = s["n"] - s["passed"] - not_impl
        excluded = s["n_total"] - s["n"]
        return (
            f"| {name} | {s['passed']} / {s['n']} | {_pct(s['passed'], s['n'])} "
            f"| {not_impl} | {other} | {excluded} |"
        )

    header = (
        "| | Passed / scored | Score | Not implemented | Other failures | Excluded |\n"
        "|---|---:|---:|---:|---:|---:|"
    )
    summary = [
        line("All selected tests", scores["overall"]),
        line("Single-service tests", scores["single_service"]),
        line("Cross-service tests", scores["cross_service"]),
        line("Control plane", scores["by_plane"][PLANE_CONTROL]),
        line("Data plane", scores["by_plane"][PLANE_DATA]),
    ]
    services = [line(f"`{svc}`", s) for svc, s in scores["by_service"].items()]
    return "\n".join(
        [
            f"Suite `{suite_version}`, Stratus `{stratus_version}`.",
            "",
            header,
            *summary,
            "",
            "Single-service tests, by service:",
            "",
            header,
            *services,
            "",
        ]
    )


def _suite_version(suite: Path) -> str:
    try:
        return subprocess.run(
            ["git", "-C", str(suite), "rev-parse", "--short", "HEAD"],
            capture_output=True,
            text=True,
            check=True,
        ).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"


def main(argv: Sequence[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    sub = ap.add_subparsers(dest="command", required=True)
    for name in ("run", "report", "list"):
        p = sub.add_parser(name)
        p.add_argument("--suite", type=Path, required=True, help="the evaluation suite checkout")
        p.add_argument("--scope", choices=SCOPES, default=SCOPE_ALL)
        p.add_argument(
            "--service",
            action="append",
            help="only tests of this suite service (repeatable; default: all Stratus services)",
        )
        if name == "run":
            p.add_argument("--stratus", required=True, help="the stratus binary")
            p.add_argument(
                "--runner",
                default=str(Path(sys.executable).with_name("fidelity-tests-runner")),
                help="the suite's fidelity-tests-runner (default: beside this Python)",
            )
            p.add_argument("--jobs", type=int, default=os.cpu_count() or 2)
            p.add_argument("--shard", metavar="I/N", help="run only the I-th of N shares")
        if name in ("run", "report"):
            p.add_argument("--out", type=Path, required=True, help="the reports directory")
        if name == "report":
            p.add_argument("--json", type=Path, help="write the scores as JSON")
            p.add_argument("--markdown", type=Path, help="write the scores as Markdown tables")
            p.add_argument("--stratus-version", default="dev")
    args = ap.parse_args(argv)

    manifest = load_manifest((args.suite / "manifest.jsonl").read_text("utf-8").splitlines())
    supported = frozenset(args.service) if args.service else STRATUS_SERVICES
    chosen = select(manifest, supported, args.scope)

    if args.command == "list":
        for check in chosen:
            print(check.path)
        return 0

    if args.command == "run":
        stratus, runner = os.path.abspath(args.stratus), os.path.abspath(args.runner)
        run_all(
            shard(chosen, args.shard) if args.shard else chosen,
            args.suite.resolve(),
            args.out.resolve(),
            lambda check_file, report: run_command(stratus, runner, check_file, report),
            args.jobs,
        )
        return 0

    from fidelity_tests_runner.verdicts import tally  # the suite's own scoring

    wanted = {c.check_id for c in chosen}
    results = [r for r in read_reports(args.out) if r.check_id in wanted]
    scores = score(results, manifest, tally)
    scores["suite_version"] = _suite_version(args.suite)
    scores["stratus_version"] = args.stratus_version
    if args.json:
        args.json.write_text(json.dumps(scores, indent=2, sort_keys=True) + "\n", "utf-8")
    markdown = render_markdown(scores, scores["suite_version"], args.stratus_version)
    if args.markdown:
        args.markdown.write_text(markdown, "utf-8")
    print(markdown)
    return 0


if __name__ == "__main__":
    sys.exit(main())
