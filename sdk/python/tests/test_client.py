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
