"""Keep the shared Go/Python fixtures tied to executable Python behavior."""
import importlib.util
import json
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("contract_vector_generator", ROOT / "scripts" / "generate_contract_vectors.py")
GENERATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GENERATOR)


class ContractVectorsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.vectors = json.loads(GENERATOR.DESTINATION.read_text(encoding="utf-8"))

    def test_committed_vectors_match_python_implementation(self):
        self.assertEqual(self.vectors, GENERATOR.generate_vectors(),
                         "Regenerate with uv run --frozen --extra hub python scripts/generate_contract_vectors.py")

    def test_http_deduplication_and_definite_errors(self):
        hub = {case["name"]: case for case in self.vectors["hub"]}
        self.assertEqual(hub["send_ask"]["response"], hub["retry_same_request"]["response"])
        self.assertEqual(hub["request_content_conflict"]["status"], 400)
        self.assertEqual(hub["unauthorized"]["status"], 401)
        for name in ("expired_request", "future_request"):
            self.assertFalse(hub[name]["response"]["ok"])

    def test_native_envelopes_cannot_forge_lines(self):
        for case in self.vectors["delivery"]:
            lines = case["text"].splitlines()
            self.assertEqual(len(lines), 3 if case["message"]["kind"] == "ask" else 2)
            self.assertEqual(json.loads(lines[1]), case["envelope"])
            self.assertEqual(case["envelope"]["body"], case["message"]["body"])

    def test_compact_mcp_results_preserve_receipts_and_omit_read_bodies(self):
        cases = {case["name"]: case["response"] for case in self.vectors["mcp"]}
        def result(name):
            return json.loads(cases[name]["result"]["content"][0]["text"])
        sent = result("send_compact")
        self.assertIn("request_id", result("send_queued"))
        self.assertEqual(result("send_queued")["status"], "queued_local")
        self.assertIs(sent["received_by_host"], False)
        self.assertEqual(sent["to"], result("send_verbose")["recipient"])
        self.assertNotIn("body", sent)
        self.assertIn("body", result("send_verbose"))
        self.assertTrue(result("inbox_compact")[1]["body_omitted"])
        self.assertNotIn("body", result("inbox_compact")[1])
        self.assertIn("body", result("inbox_include_read")[0])
        self.assertIn("body", result("inbox_compact")[0])
        self.assertEqual(cases["invalid_boolean"]["error"]["code"], -32602)
        self.assertEqual(cases["missing_caller_identity"]["error"]["code"], -32602)

    def test_bound_client_reads_and_spool_admission(self):
        cases = {case["name"]: case for case in self.vectors["mcp"]}
        def result(name):
            return json.loads(cases[name]["response"]["result"]["content"][0]["text"])
        self.assertTrue(result("peers_stale")["stale"])
        self.assertTrue(all(row["status"] == "unknown" and not row["online"] for row in result("peers_stale")["sessions"]))
        self.assertTrue(result("peers_missing")["stale"])
        self.assertEqual(result("unknown_local")["status"], "unknown_local")
        self.assertFalse(result("health_missing")["running"])
        inbox = cases["inbox_compact"]
        self.assertEqual(inbox["spooled"]["op"], "read")
        self.assertEqual(inbox["spooled"]["data"]["message_ids"], [result("inbox_compact")[0]["id"]])
        self.assertNotIn("spooled", cases["inbox_peek"])
        self.assertTrue(inbox["pending_reads"])
        queued = self.vectors["local"][0]
        self.assertTrue(queued["spool_removed"])
        self.assertEqual(queued["request"], queued["imported"])
        send = cases["send_queued"]["spooled"]["data"]
        self.assertEqual(send["expires"], GENERATOR.NOW + send["ttl"])

    def test_http_limits_hold_and_reset(self):
        cases = {case["name"]: case for case in self.vectors["hub"]}
        for name, status in (("not_found", 404), ("request_too_large", 413), ("quota_overloaded", 429), ("storage_unavailable", 503)):
            self.assertEqual(cases[name]["status"], status)
        self.assertEqual(cases["quota_overloaded"]["response"]["code"], "overloaded")
        for name in ("quota_overloaded", "storage_unavailable"):
            self.assertTrue(cases[name]["response"]["retryable"])
        self.assertEqual(cases["hop_limit"]["response"]["error"], "thread reached the 6-message limit; send a new message instead")
        self.assertEqual(cases["hop_5"]["response"]["result"]["hops"], 5)
        self.assertEqual(cases["result_held"]["response"]["result"]["status"], "pending")
        self.assertTrue(cases["sync_reset_required"]["response"]["result"]["reset_required"])
        empty = next(case for case in self.vectors["delivery"] if case["name"] == "empty_detail")
        self.assertEqual(empty["message"]["detail"], "")

    def test_mcp_framing_errors_recover(self):
        cases = {case["name"]: case for case in self.vectors["mcp_frames"]}
        for name in ("parse_error", "invalid_utf8", "oversize_then_ping"):
            self.assertEqual(cases[name]["responses"][0]["error"]["code"], -32700)
        self.assertEqual(cases["invalid_request"]["responses"][0]["error"]["code"], -32600)
        self.assertEqual(cases["oversize_then_ping"]["responses"][1]["result"], {})
        self.assertEqual(len(cases["notification_then_ping"]["responses"]), 1)


if __name__ == "__main__":
    unittest.main()
