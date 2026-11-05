"""Runs against a real Stratus when STRATUS_ENDPOINT is set (CI starts one)."""

import os
import unittest
import urllib.error
import urllib.request

from stratus_sdk import StratusClient, StratusError

ENDPOINT = os.environ.get("STRATUS_ENDPOINT")


def s3(endpoint: str, path: str, method: str = "PUT") -> int:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(urllib.request.Request(endpoint + path, method=method), timeout=10) as res:
            return res.status
    except urllib.error.HTTPError as err:
        return err.code


@unittest.skipUnless(ENDPOINT, "set STRATUS_ENDPOINT to run against a real Stratus")
class IntegrationTest(unittest.TestCase):
    def test_round_trip(self) -> None:
        stratus = StratusClient(ENDPOINT)
        stratus.wait_until_ready(timeout=30)
        self.assertTrue(stratus.health()["ready"])
        stratus.reset("s3")

        # State made inside snapshot_scope is rolled back.
        with stratus.snapshot_scope():
            self.assertEqual(s3(stratus.endpoint, "/sdk-py-temp"), 200)
        self.assertEqual(s3(stratus.endpoint, "/sdk-py-temp", "HEAD"), 404)

        # Chaos fails exactly the first call, and diagnostics blames chaos for it.
        spec = {
            "seed": 7,
            "rules": [{"id": "first-put", "service": "s3", "operation": "CreateBucket", "action": "internal_error", "max_hits": 1}],
        }
        with stratus.chaos_experiment(spec) as run:
            self.assertEqual(s3(stratus.endpoint, "/sdk-py-chaos"), 500)
            self.assertEqual(s3(stratus.endpoint, "/sdk-py-chaos"), 200)
        assert run.report is not None
        self.assertEqual(run.report["total_injected"], 1)
        self.assertEqual(run.report["recent_hits"][0]["rule_id"], "first-put")

        diagnosis = stratus.diagnose(service="s3", since="1m")
        injected = [g for g in diagnosis["groups"] if g["category"] == "injected_fault"]
        self.assertTrue(injected, diagnosis["summary"])
        self.assertFalse(injected[0]["your_bug"])

        with self.assertRaises(StratusError) as cm:
            stratus.snapshots.restore("sdk-py-missing")
        self.assertEqual((cm.exception.code, cm.exception.status), ("SnapshotNotFound", 404))
        stratus.reset("s3")


if __name__ == "__main__":
    unittest.main()
