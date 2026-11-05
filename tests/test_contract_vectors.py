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
        self.assertTrue(result("inbox_compact")[0]["body_omitted"])
        self.assertNotIn("body", result("inbox_compact")[0])
        self.assertIn("body", result("inbox_include_read")[0])
        self.assertIn("body", result("inbox_compact")[1])
        self.assertEqual(cases["invalid_boolean"]["error"]["code"], -32602)


if __name__ == "__main__":
    unittest.main()
