import json
import threading
import unittest
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any, List, Optional, Tuple
from unittest import mock

from stratus_emulator import StratusClient, StratusError, UrllibTransport

SNAP = {"id": "tmp", "created_at": "t", "services": [], "size_bytes": 1}
REPORT = {"status": "stopped", "seed": 1, "started_at": "t", "rules": [], "total_injected": 0, "recent_hits": []}


class FakeTransport:
    """Records requests and answers from a queue of canned results or errors."""

    def __init__(self, *responses: Any) -> None:
        self.responses = list(responses)
        self.calls: List[Tuple[str, str, Any]] = []

    def request(self, method: str, path: str, body: Any = None) -> Any:
        self.calls.append((method, path, body))
        out = self.responses.pop(0)
        if isinstance(out, Exception):
            raise out
        return out


def client(*responses: Any) -> Tuple[StratusClient, FakeTransport]:
    t = FakeTransport(*responses)
    return StratusClient("http://stratus.test:4566", transport=t), t


class ClientTest(unittest.TestCase):
    def test_endpoint_defaults_from_env_and_trims_slash(self) -> None:
        with mock.patch.dict("os.environ", {"STRATUS_ENDPOINT": "http://from-env:1234/"}):
            self.assertEqual(StratusClient().endpoint, "http://from-env:1234")

    def test_reset_sends_named_services(self) -> None:
        c, t = client({"services": ["s3"]})
        self.assertEqual(c.reset("s3")["services"], ["s3"])
        self.assertEqual(t.calls, [("POST", "/reset", {"services": ["s3"]})])

    def test_snapshot_calls_map_to_control_api(self) -> None:
        c, t = client(SNAP, {"items": None}, {"restored": SNAP}, None)
        self.assertEqual(c.snapshots.create("a.b"), SNAP)
        self.assertEqual(c.snapshots.list(), [])
        self.assertEqual(c.snapshots.restore("a b"), SNAP)
        c.snapshots.delete("a.b")
        self.assertEqual(
            [(m, p) for m, p, _ in t.calls],
            [("POST", "/snapshots"), ("GET", "/snapshots"), ("POST", "/snapshots/a%20b/restore"), ("DELETE", "/snapshots/a.b")],
        )

    def test_diagnose_builds_query_and_normalises_empty_results(self) -> None:
        c, t = client({"summary": "No failed requests", "healthy": True, "failures": 0, "by_category": None, "groups": None})
        d = c.diagnose(since=datetime(2026, 1, 2, 3, 4, 5, tzinfo=timezone.utc), service="sqs", exclude_injected=True, max_groups=5)
        self.assertEqual(d["groups"], [])
        self.assertEqual(d["by_category"], {})
        self.assertEqual(
            t.calls[0][1],
            "/diagnostics?since=2026-01-02T03%3A04%3A05Z&service=sqs&exclude_injected=true&max_groups=5",
        )

    def test_is_ready_and_wait_until_ready(self) -> None:
        not_ready = StratusError("NotReady", "starting", status=503)
        c, t = client(StratusError(StratusError.UNREACHABLE, "refused"), not_ready, {"ready": True})
        c.wait_until_ready(interval=0.001)
        self.assertEqual(len(t.calls), 3)

        never, _ = client(*[StratusError(StratusError.UNREACHABLE, "refused")] * 1000)
        with self.assertRaises(StratusError) as cm:
            never.wait_until_ready(timeout=0.02, interval=0.005)
        self.assertEqual(cm.exception.code, StratusError.TIMEOUT)

    def test_snapshot_scope_restores_and_deletes_when_block_raises(self) -> None:
        c, t = client(SNAP, {"restored": SNAP}, None)
        with self.assertRaisesRegex(RuntimeError, "boom"):
            with c.snapshot_scope():
                raise RuntimeError("boom")
        self.assertEqual(
            [(m, p) for m, p, _ in t.calls],
            [("POST", "/snapshots"), ("POST", "/snapshots/tmp/restore"), ("DELETE", "/snapshots/tmp")],
        )
        self.assertRegex(t.calls[0][2]["name"], r"^sdk-tmp-[a-z0-9-]+$")

    def test_limits_set_status_and_clear(self) -> None:
        status = {"profile": "strict", "scale": 0.01, "applied_at": "t", "rules": [], "total_throttled": 0, "recent_breaches": []}
        c, t = client(status, status, status)
        self.assertEqual(c.limits.set({"profile": "strict"})["profile"], "strict")
        c.limits.status()
        c.limits.clear()
        self.assertEqual(t.calls, [("PUT", "/limits", {"profile": "strict"}), ("GET", "/limits", None), ("DELETE", "/limits", None)])

    def test_signatures_set_status_and_clear(self) -> None:
        status = {"mode": "strict", "verified": 0, "failed": 0, "skipped": 0, "failures_by_code": {}, "recent_failures": []}
        c, t = client(status, status, dict(status, mode="off"))
        self.assertEqual(c.signatures.set()["mode"], "strict")
        c.signatures.status()
        self.assertEqual(c.signatures.clear()["mode"], "off")
        self.assertEqual(t.calls, [("PUT", "/sigv4", {"mode": "strict"}), ("GET", "/sigv4", None), ("DELETE", "/sigv4", None)])

    def test_runs_map_to_control_api(self) -> None:
        summary = {"id": "run-1", "status": "running"}
        c, t = client(summary, summary, {"id": "run-1", "status": "finished"}, {"runs": [summary]})
        self.assertEqual(c.runs.start({"name": "load.js", "limits": {"profile": "strict"}})["id"], "run-1")
        c.runs.status()
        self.assertEqual(c.runs.stop("run-1")["status"], "finished")
        self.assertEqual(c.runs.list(), [summary])
        self.assertEqual(
            [(m, p) for m, p, _ in t.calls],
            [("POST", "/runs"), ("GET", "/runs/current"), ("POST", "/runs/run-1/stop"), ("GET", "/runs")],
        )
        self.assertEqual(t.calls[0][2], {"name": "load.js", "limits": {"profile": "strict"}})

    def test_advise_plans_proofs_and_records_their_runs(self) -> None:
        plan = {"option": "kinesis", "provable": True, "rate": 200, "script": "// k6"}
        c, t = client({"id": "adr-1", "proofs": [plan]}, {"id": "adr-1", "evidence": [{"option": "kinesis", "verdict": "held"}]})
        self.assertEqual(c.advise.plan_proofs("adr-1", scale=0.1)[0]["script"], "// k6")
        d = c.advise.record_evidence("adr-1", "kinesis", "run-3", 200, scale=0.1)
        self.assertEqual(d["evidence"][0]["verdict"], "held")
        self.assertEqual(
            t.calls,
            [
                ("POST", "/advise/decisions/adr-1/proofs", {"scale": 0.1}),
                ("POST", "/advise/decisions/adr-1/evidence", {"option": "kinesis", "run_id": "run-3", "target_rate": 200, "scale": 0.1}),
            ],
        )

    def test_advise_writes_terraform_and_records_its_test_deploy(self) -> None:
        module = {"option": "kinesis", "files": [{"name": "main.tf", "content": ""}], "deployable": True}
        c, t = client(
            {"id": "adr-1", "module": module, "test_deploy": {"override": {"name": "stratus_override.tf", "content": ""}}},
            {"id": "adr-1", "deployments": [{"option": "kinesis", "outcome": "passed"}]},
        )
        self.assertEqual(c.advise.terraform("adr-1", "kinesis")["test_deploy"]["override"]["name"], "stratus_override.tf")
        d = c.advise.record_deployment("adr-1", "kinesis", "passed", resources=1)
        self.assertEqual(d["deployments"][0]["outcome"], "passed")
        self.assertEqual(
            t.calls,
            [
                ("GET", "/advise/decisions/adr-1/terraform?option=kinesis", None),
                ("POST", "/advise/decisions/adr-1/deployments", {"option": "kinesis", "outcome": "passed", "resources": 1}),
            ],
        )

    def test_advise_maps_a_decision_from_start_to_adr(self) -> None:
        d = {"id": "adr-1", "status": "draft"}
        ruled_out = StratusError("OptionRuledOut", "sqs-standard was ruled out", "choose one of: kinesis", 409)
        c, t = client(
            {"families": [{"id": "messaging"}]},
            d,
            d,
            {**d, "status": "assessed", "recommended": "kinesis"},
            {**d, "status": "decided", "chosen": "kinesis"},
            {"id": "adr-1", "markdown": "# Order events\n"},
            {"decisions": [d]},
            ruled_out,
        )
        self.assertEqual(c.advise.families()[0]["id"], "messaging")
        c.advise.start("messaging", title="Order events", answers={"replay": True})
        c.advise.answer("adr-1", {"peak_rate": 2000})
        self.assertEqual(c.advise.assess("adr-1")["recommended"], "kinesis")
        self.assertEqual(c.advise.decide("adr-1", "kinesis", "we replay")["chosen"], "kinesis")
        self.assertEqual(c.advise.adr("adr-1"), "# Order events\n")
        self.assertEqual(len(c.advise.list()), 1)
        with self.assertRaises(StratusError) as caught:
            c.advise.decide("adr-1", "sqs-standard")
        self.assertEqual(caught.exception.code, "OptionRuledOut")
        self.assertEqual(
            [(m, p) for m, p, _ in t.calls],
            [
                ("GET", "/advise/families"),
                ("POST", "/advise/decisions"),
                ("POST", "/advise/decisions/adr-1/answers"),
                ("POST", "/advise/decisions/adr-1/assess"),
                ("POST", "/advise/decisions/adr-1/decide"),
                ("GET", "/advise/decisions/adr-1/adr?format=json"),
                ("GET", "/advise/decisions"),
                ("POST", "/advise/decisions/adr-1/decide"),
            ],
        )
        self.assertEqual(t.calls[1][2], {"family": "messaging", "title": "Order events", "answers": {"replay": True}})
        self.assertEqual(t.calls[4][2], {"option": "kinesis", "rationale": "we replay"})
        self.assertEqual(t.calls[7][2], {"option": "sqs-standard"})

    def test_runs_findings_reads_the_latest_run(self) -> None:
        finding = {"detector": "missing_queue", "severity": "high", "title": "t", "detail": "d", "suggestion": "s"}
        c, t = client({"id": "run-1", "findings": [finding]})
        self.assertEqual(c.runs.findings(), [finding])
        self.assertEqual(t.calls[0][:2], ("GET", "/runs/latest"))

    def test_runs_recording_always_stops(self) -> None:
        c, t = client({"id": "run-2", "status": "running"}, {"id": "run-2", "status": "finished"})
        with self.assertRaises(ValueError):
            with c.runs.recording({"name": "x"}) as rec:
                raise ValueError("boom")
        self.assertEqual(rec.summary, {"id": "run-2", "status": "finished"})
        self.assertEqual(t.calls[1][:2], ("POST", "/runs/run-2/stop"))

    def test_chaos_experiment_always_stops_and_exposes_report(self) -> None:
        c, t = client(REPORT, REPORT)
        spec = {"seed": 1, "rules": [{"service": "s3", "action": "throttle"}]}
        with c.chaos_experiment(spec) as run:
            pass
        self.assertEqual(run.report, REPORT)
        self.assertEqual(t.calls[0], ("PUT", "/chaos", spec))

        c2, t2 = client(REPORT, REPORT)
        with self.assertRaises(ValueError):
            with c2.chaos_experiment({"rules": [{"service": "*", "action": "drop"}]}):
                raise ValueError("x")
        self.assertEqual([m for m, _, _ in t2.calls], ["PUT", "DELETE"])

    def test_aws_settings(self) -> None:
        c = StratusClient("http://localhost:4566", region="eu-west-1")
        self.assertEqual(
            c.boto3_kwargs(),
            {"endpoint_url": "http://localhost:4566", "region_name": "eu-west-1", "aws_access_key_id": "test", "aws_secret_access_key": "test"},
        )
        self.assertEqual(c.aws_env()["AWS_ENDPOINT_URL"], "http://localhost:4566")
        self.assertEqual(c.aws_env()["AWS_DISABLE_HOST_PREFIX_INJECTION"], "true")


class _Handler(BaseHTTPRequestHandler):
    routes: dict = {}

    def _reply(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        self.server.last_body = self.rfile.read(length) if length else b""  # type: ignore[attr-defined]
        status, body = self.routes[(self.command, self.path)]
        raw = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else b""
        self.send_response(status)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    do_GET = do_POST = do_DELETE = do_PUT = _reply

    def log_message(self, *args: Any) -> None:
        pass


class UrllibTransportTest(unittest.TestCase):
    server: Optional[HTTPServer] = None

    @classmethod
    def setUpClass(cls) -> None:
        cls.server = HTTPServer(("127.0.0.1", 0), _Handler)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.endpoint = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls) -> None:
        assert cls.server
        cls.server.shutdown()
        cls.server.server_close()

    def transport(self) -> UrllibTransport:
        return UrllibTransport(self.endpoint, timeout=5)

    def test_success_with_json_body(self) -> None:
        _Handler.routes = {("POST", "/_stratus/control/v1/reset"): (200, {"services": ["s3"]})}
        self.assertEqual(self.transport().request("POST", "/reset", {"services": ["s3"]}), {"services": ["s3"]})
        self.assertEqual(json.loads(self.server.last_body), {"services": ["s3"]})  # type: ignore[union-attr]

    def test_empty_body_is_none(self) -> None:
        _Handler.routes = {("DELETE", "/_stratus/control/v1/snapshots/x"): (204, None)}
        self.assertIsNone(self.transport().request("DELETE", "/snapshots/x"))

    def test_server_error_carries_code_hint_and_status(self) -> None:
        body = {"error": {"code": "SnapshotNotFound", "message": "missing", "hint": "list snapshots"}}
        _Handler.routes = {("POST", "/_stratus/control/v1/snapshots/x/restore"): (404, body)}
        with self.assertRaises(StratusError) as cm:
            self.transport().request("POST", "/snapshots/x/restore")
        e = cm.exception
        self.assertEqual((e.code, e.message, e.hint, e.status), ("SnapshotNotFound", "missing", "list snapshots", 404))

    def test_non_json_error_keeps_status(self) -> None:
        _Handler.routes = {("GET", "/_stratus/control/v1/health"): (502, b"bad gateway")}
        with self.assertRaises(StratusError) as cm:
            self.transport().request("GET", "/health")
        self.assertEqual((cm.exception.code, cm.exception.status), ("HTTP502", 502))

    def test_unreachable(self) -> None:
        with self.assertRaises(StratusError) as cm:
            UrllibTransport("http://127.0.0.1:9", timeout=2).request("GET", "/health")
        self.assertEqual(cm.exception.code, StratusError.UNREACHABLE)
        self.assertEqual(cm.exception.status, 0)


MSG = {"id": "m1", "timestamp": "t", "source": "a@b.example", "destination": {"to": ["c@d.example"]}, "subject": "Hi", "body": {"text": "x"}, "api": "ses"}
FIRED = {"name": "nightly", "group": "default", "schedule_arn": "arn", "target": "arn:sqs", "scheduled_time": "t", "execution_id": "e", "input": "{}"}


class SesAndSchedulerTest(unittest.TestCase):
    def test_ses_inbox_calls_map_to_control_api(self) -> None:
        c, t = client({"items": [MSG], "count": 1}, MSG, {"items": None, "count": 0}, {"cleared": 1})
        since = datetime(2026, 11, 9, 12, 0, 0, tzinfo=timezone.utc)
        self.assertEqual(c.ses.messages(to="c@d.example", subject="Hi", since=since, limit=5), [MSG])
        self.assertEqual(c.ses.message("m 1"), MSG)
        self.assertEqual(c.ses.messages(), [])
        self.assertEqual(c.ses.clear(), 1)
        self.assertEqual(
            [(m, p) for m, p, _ in t.calls],
            [
                ("GET", "/ses/messages?to=c%40d.example&subject=Hi&since=2026-11-09T12%3A00%3A00Z&limit=5"),
                ("GET", "/ses/messages/m%201"),
                ("GET", "/ses/messages"),
                ("DELETE", "/ses/messages"),
            ],
        )

    def test_emulator_clock_calls_map_to_control_api(self) -> None:
        status = {"now": "t", "real_now": "r", "offset": "0s", "frozen": False}
        c, t = client(
            status,
            {**status, "offset": "30m0s", "effects": [{"source": "scheduler", "count": 1}]},
            {**status, "effects": None},
            {**status, "frozen": True, "frozen_at": "t"},
            status,
        )
        self.assertEqual(c.clock.status()["offset"], "0s")
        self.assertEqual(c.clock.advance("30m")["effects"], [{"source": "scheduler", "count": 1}])
        self.assertEqual(c.clock.advance(datetime(2026, 11, 23, 9, 0, 0, tzinfo=timezone.utc))["effects"], [])
        self.assertTrue(c.clock.freeze()["frozen"])
        self.assertFalse(c.clock.resume()["frozen"])
        self.assertEqual(
            t.calls,
            [
                ("GET", "/clock", None),
                ("POST", "/clock/advance", {"by": "30m"}),
                ("POST", "/clock/advance", {"to": "2026-11-23T09:00:00Z"}),
                ("POST", "/clock/freeze", None),
                ("POST", "/clock/resume", None),
            ],
        )

    def test_scheduler_clock_calls_map_to_control_api(self) -> None:
        c, t = client(
            {"now": "t", "offset": "1h0m0s", "fired": [FIRED]},
            {"now": "t2", "offset": "2h0m0s", "fired": None},
            FIRED,
            {"now": "t", "offset": "0s"},
        )
        self.assertEqual(c.scheduler.advance("1h")["fired"], [FIRED])
        self.assertEqual(c.scheduler.advance(datetime(2026, 11, 9, 14, 0, 0, tzinfo=timezone.utc))["fired"], [])
        self.assertEqual(c.scheduler.run("nightly", group="billing"), FIRED)
        self.assertEqual(c.scheduler.clock()["offset"], "0s")
        self.assertEqual(
            t.calls,
            [
                ("POST", "/scheduler/advance", {"by": "1h"}),
                ("POST", "/scheduler/advance", {"to": "2026-11-09T14:00:00Z"}),
                ("POST", "/scheduler/run", {"name": "nightly", "group": "billing"}),
                ("GET", "/scheduler/clock", None),
            ],
        )


if __name__ == "__main__":
    unittest.main()


class BedrockTest(unittest.TestCase):
    def test_fixture_mode_and_invocation_calls_map_to_control_api(self) -> None:
        fixture = {"id": "refunds", "model": "anthropic.*", "match": {"contains": "refund"}, "text": "five days", "origin": "runtime"}
        inv = {"id": "1", "time": "t", "operation": "Converse", "model": "m", "prompt": "p", "request_hash": "sha256:x", "outcome": "missed"}
        status = {"mode": "record", "default_mode": "replay"}
        c, t = client(
            fixture,
            {"items": [fixture], "count": 1},
            {"deleted": "refunds"},
            {"cleared": 2},
            {"items": [inv], "count": 1},
            {"items": None, "count": 0},
            {"cleared": 1},
            status,
            status,
        )
        self.assertEqual(c.bedrock.add_fixture("five days", model="anthropic.*", contains="refund", fixture_id="refunds"), fixture)
        self.assertEqual(c.bedrock.fixtures(), [fixture])
        c.bedrock.delete_fixture("refunds")
        self.assertEqual(c.bedrock.clear_fixtures(), 2)
        self.assertEqual(c.bedrock.invocations(outcome="missed", contains="refund", limit=3), [inv])
        self.assertEqual(c.bedrock.invocations(), [])
        self.assertEqual(c.bedrock.clear_invocations(), 1)
        self.assertEqual(c.bedrock.set_mode("record")["mode"], "record")
        self.assertEqual(c.bedrock.status()["default_mode"], "replay")
        self.assertEqual(
            t.calls,
            [
                ("POST", "/bedrock/fixtures", {"id": "refunds", "model": "anthropic.*", "text": "five days", "match": {"contains": "refund"}}),
                ("GET", "/bedrock/fixtures", None),
                ("DELETE", "/bedrock/fixtures/refunds", None),
                ("DELETE", "/bedrock/fixtures", None),
                ("GET", "/bedrock/invocations?contains=refund&outcome=missed&limit=3", None),
                ("GET", "/bedrock/invocations", None),
                ("DELETE", "/bedrock/invocations", None),
                ("PUT", "/bedrock/mode", {"mode": "record"}),
                ("GET", "/bedrock", None),
            ],
        )

    def test_add_fixture_with_exact_response(self) -> None:
        c, t = client({"id": "fx-1"})
        response = {"output": {"message": {"role": "assistant", "content": [{"text": "hi"}]}}}
        c.bedrock.add_fixture(kind="converse", response=response, request_hash="sha256:abc")
        self.assertEqual(t.calls, [("POST", "/bedrock/fixtures", {"kind": "converse", "request_hash": "sha256:abc", "response": response})])


class KafkaTest(unittest.TestCase):
    def test_clusters_and_bootstrap_brokers_come_from_the_control_api(self) -> None:
        cluster = {
            "name": "orders",
            "arn": "arn:aws:kafka:us-east-1:000000000000:cluster/orders/x",
            "type": "PROVISIONED",
            "state": "ACTIVE",
            "kafka_version": "3.6.0",
            "bootstrap_brokers": "127.0.0.1:41001",
        }
        listing = {"items": [cluster], "count": 1}
        c, t = client(listing, listing, {"items": None, "count": 0})

        self.assertEqual(c.kafka.clusters(), [cluster])
        self.assertEqual(c.kafka.bootstrap_brokers("orders"), "127.0.0.1:41001")
        with self.assertRaises(StratusError) as caught:
            c.kafka.bootstrap_brokers("missing")
        self.assertEqual(caught.exception.code, "KafkaClusterNotFound")
        self.assertEqual(t.calls, [("GET", "/kafka/clusters", None)] * 3)


class DnsTest(unittest.TestCase):
    def test_resolve_asks_the_control_api_and_defaults_to_a_records(self) -> None:
        answer = {"name": "api.corp.local.", "type": "A", "value": "10.0.0.1", "ttl": 60, "zone_id": "Z1"}
        c, t = client(
            {"name": "api.corp.local.", "type": "A", "answers": [answer], "found": True},
            {"name": "x.corp.local.", "type": "SRV", "answers": None, "found": False},
        )

        self.assertEqual(c.dns.resolve("api.corp.local")["answers"], [answer])
        missing = c.dns.resolve("x.corp.local", "SRV")
        self.assertFalse(missing["found"])
        self.assertEqual(missing["answers"], [])
        self.assertEqual(
            t.calls,
            [("GET", "/dns/resolve?name=api.corp.local&type=A", None), ("GET", "/dns/resolve?name=x.corp.local&type=SRV", None)],
        )
