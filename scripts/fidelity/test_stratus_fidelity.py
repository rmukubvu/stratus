"""Unit tests for stratus_fidelity (stdlib only: python -m unittest discover scripts/fidelity)."""

import json
import tempfile
import unittest
from collections import Counter
from pathlib import Path

import stratus_fidelity as sf


def row(check_id, service, labels=("plane:control",)):
    return json.dumps(
        {
            "check_id": check_id,
            "service": service,
            "path": f"behavioral/{service}/check_{check_id.split('.', 1)[1]}.py",
            "labels": list(labels),
        }
    )


def fake_tally(rows):
    """A stand-in for the suite's tally: passed/n over everything but tool_missing."""
    kinds = Counter(kind for _, kind, _ in rows)
    n = sum(v for k, v in kinds.items() if k != "tool_missing")
    return {
        "n": n,
        "passed": kinds["passed"],
        "failed_by_kind": {k: v for k, v in kinds.items() if k not in ("passed", "tool_missing")},
        "n_total": sum(kinds.values()),
    }


MANIFEST = sf.load_manifest(
    [
        row("sqs.send", "sqs", ("plane:data", "dp:api")),
        row("sqs.create", "sqs"),
        row("lambda__sqs.esm", "lambda__sqs", ("plane:control", "plane:data")),
        row("ec2.run", "ec2"),
        row("ec2__sqs.mixed", "ec2__sqs"),
        "",
    ]
)


class SelectTest(unittest.TestCase):
    def test_all_keeps_tests_whose_every_service_stratus_emulates(self):
        ids = [c.check_id for c in sf.select(MANIFEST)]
        self.assertEqual(ids, ["sqs.send", "sqs.create", "lambda__sqs.esm"])

    def test_single_and_cross_scopes_split_by_service_count(self):
        single = [c.check_id for c in sf.select(MANIFEST, scope=sf.SCOPE_SINGLE)]
        cross = [c.check_id for c in sf.select(MANIFEST, scope=sf.SCOPE_CROSS)]
        self.assertEqual(single, ["sqs.send", "sqs.create"])
        self.assertEqual(cross, ["lambda__sqs.esm"])

    def test_supported_set_narrows_selection(self):
        ids = [c.check_id for c in sf.select(MANIFEST, frozenset({"lambda"}))]
        self.assertEqual(ids, [])

    def test_unknown_scope_is_refused(self):
        with self.assertRaises(ValueError):
            sf.select(MANIFEST, scope="everything")


class CheckTest(unittest.TestCase):
    def test_planes_come_from_plane_labels(self):
        esm = next(c for c in MANIFEST if c.check_id == "lambda__sqs.esm")
        self.assertEqual(esm.planes, ("control", "data"))
        self.assertEqual(esm.services, ("lambda", "sqs"))
        self.assertTrue(esm.cross_service)


class ShardTest(unittest.TestCase):
    def test_shards_partition_the_checks(self):
        checks = sf.select(MANIFEST)
        parts = [sf.shard(checks, f"{i}/2") for i in (1, 2)]
        self.assertEqual([len(p) for p in parts], [2, 1])
        self.assertEqual(sorted(c.check_id for p in parts for c in p), sorted(c.check_id for c in checks))

    def test_bad_shard_is_refused(self):
        with self.assertRaises(ValueError):
            sf.shard(MANIFEST, "3/2")


class RunTest(unittest.TestCase):
    def test_run_command_starts_a_fresh_stratus_per_test(self):
        argv = sf.run_command("/bin/stratus", "/bin/runner", Path("t.py"), Path("r.json"))
        self.assertEqual(argv[:4], ["/bin/stratus", "ci", "--", "sh"])
        self.assertIn('--endpoint "$AWS_ENDPOINT_URL" --auth off', argv[5])
        self.assertEqual(argv[6:], ["/bin/runner", "t.py", "r.json"])

    def test_run_all_skips_tests_already_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            checks = sf.select(MANIFEST)
            done = sf.report_path(out, checks[0])
            done.parent.mkdir(parents=True)
            done.write_text("{}")
            ran = []
            count = sf.run_all(
                checks,
                Path("/suite"),
                out,
                lambda f, r: [str(f), str(r)],
                jobs=2,
                execute=lambda argv: ran.append(argv[0]) or 0,
                log=lambda _: None,
            )
            self.assertEqual(count, 2)
            self.assertEqual(
                sorted(ran),
                sorted(["/suite/" + checks[1].path, "/suite/" + checks[2].path]),
            )
            self.assertTrue(sf.report_path(out, checks[2]).parent.is_dir())


class ScoreTest(unittest.TestCase):
    def write_report(self, out, service, name, kind):
        path = out / service / f"{name}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        check = {"check_id": f"{service}.{name}", "service": service, "kind": kind}
        path.write_text(json.dumps({"check": check}))

    def test_scores_split_by_service_scope_and_plane(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            self.write_report(out, "sqs", "send", "passed")
            self.write_report(out, "sqs", "create", "not_implemented")
            self.write_report(out, "lambda__sqs", "esm", "tool_missing")
            (out / "sqs" / "junk.json").write_text("not json")
            scores = sf.score(sf.read_reports(out), MANIFEST, fake_tally)

        self.assertEqual(scores["overall"]["passed"], 1)
        self.assertEqual(scores["overall"]["n"], 2)
        self.assertEqual(scores["overall"]["n_total"], 3)
        self.assertEqual(list(scores["by_service"]), ["sqs"])
        self.assertEqual(scores["cross_service"]["n_total"], 1)
        self.assertEqual(scores["by_plane"]["data"]["n_total"], 2)
        self.assertEqual(scores["by_plane"]["control"]["passed"], 0)

    def test_markdown_shows_score_not_implemented_and_exclusions(self):
        s = {"n": 4, "passed": 3, "failed_by_kind": {"not_implemented": 1}, "n_total": 5}
        scores = {
            "overall": s,
            "single_service": s,
            "cross_service": s,
            "by_plane": {"control": s, "data": s},
            "by_service": {"sqs": s},
        }
        md = sf.render_markdown(scores, "abc123", "v0.2.0")
        self.assertIn("Suite `abc123`, Stratus `v0.2.0`.", md)
        self.assertIn("| `sqs` | 3 / 4 | 75.0% | 1 | 0 | 1 |", md)


if __name__ == "__main__":
    unittest.main()
