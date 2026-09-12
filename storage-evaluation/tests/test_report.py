"""Report-rendering contract tests: coverage is keyed on the full
(engine, scale, readers, days) combination, missing resource samples never
render as zero, and teardown/overlap are surfaced — not dropped."""
import unittest

from harness import report
from harness.util import CAPS, WORK


def _leg(engine="clickhouse", scale=100000, readers=1, days=7,
         status="MEASURED", resources=None, ingest=None, teardown="stopped"):
    return {
        "engine": engine, "scale": scale, "seed": 1,
        "readers": readers, "days": days, "status": status,
        "checks": {}, "shapes": {}, "ingest": ingest or {},
        "resources": resources if resources is not None else {"samples": 3},
        "teardown": teardown,
        "provenance": {"code_digest": "abc123"},
    }


class TestReportRender(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        WORK.mkdir(parents=True, exist_ok=True)

    def test_matrix_ledger_is_per_combination(self):
        text, _ = report.render([_leg()], require_labeled=False)
        n = (len(CAPS["matrix"]["scales"]) * len(CAPS["matrix"]["readers"])
             * len(CAPS["matrix"]["days"]) * 2)
        rows = [l for l in text.splitlines() if l.startswith("| matrix ")]
        self.assertEqual(len(rows), n, rows[:5])
        self.assertTrue(all("NOT RUN" in r for r in rows))
        self.assertIn("| matrix 1000000 clickhouse r5 d30 | NOT RUN", text)

    def test_measured_matrix_combo_is_marked(self):
        leg = _leg(engine="duckdb", scale=1000000, readers=5, days=30)
        text, _ = report.render([leg], require_labeled=False)
        self.assertIn("| matrix 1000000 duckdb r5 d30 | MEASURED |", text)
        self.assertIn("| matrix 1000000 duckdb r5 d7 | NOT RUN", text)
        self.assertIn("| matrix 1000000 duckdb r1 d30 | NOT RUN", text)

    def test_failed_leg_keeps_real_status_in_ledger(self):
        leg = _leg(engine="clickhouse", scale=1000000, readers=1, days=7,
                   status="ABORTED")
        leg["abort_reason"] = "wall cap exceeded"
        text, _ = report.render([leg], require_labeled=False)
        self.assertIn("| matrix 1000000 clickhouse r1 d7 | ABORTED |", text)
        self.assertIn("wall cap exceeded", text)

    def test_missing_memory_samples_render_dash_not_zero(self):
        leg = _leg(resources={"samples": 0, "sample_errors": 2,
                              "sample_error_first": "boom"})
        leg["shapes"] = {"overview": {"first": {"p50_ms": 1, "p95_ms": 2},
                                      "repeat": {"p50_ms": 1, "p95_ms": 2}}}
        text, problems = report.render([leg], require_labeled=False)
        self.assertIn("| clickhouse | overview | 1 | 2 | 1 | 2 | — |", text)
        self.assertNotIn("| 0 |", text.split("Query latency")[1].split("##")[0])
        self.assertTrue(any("no resource samples" in p for p in problems))
        self.assertIn("sampler recorded 2 failed poll(s)", text)

    def test_teardown_and_overlap_are_rendered(self):
        leg = _leg(teardown="forced",
                   ingest={"rows": 10, "ack_s": 0.1, "visibility_lag_s": 0.2,
                           "expected_total": 110, "final_total": 110,
                           "readers_overlapping_ingest": 1})
        text, _ = report.render([leg], require_labeled=False)
        self.assertIn("| forced |", text)
        self.assertIn("readers overlapping ingest", text)
        self.assertIn("| 110 | 110 | 1 |", text)

    def test_ingest_loss_is_flagged(self):
        leg = _leg(ingest={"rows": 10, "ack_s": 0.1,
                           "expected_total": 110, "final_total": 105})
        text, _ = report.render([leg], require_labeled=False)
        self.assertIn("105 (MISMATCH)", text)

    def test_legacy_semantic_gate_does_not_use_current_corpus_note(self):
        leg = _leg()
        leg["checks"] = {
            "entity.tombstone_delete": {
                "kind": "semantic_gate", "status": "DIVERGENT",
                "expected": 0, "actual": 10,
            },
        }
        text, _ = report.render([leg], require_labeled=False)
        self.assertIn("semantic note unavailable: leg predates", text)

    def test_ack_is_not_durable_ack_claim(self):
        text, _ = report.render([_leg()], require_labeled=False)
        self.assertIn("NOT a", text)
        self.assertIn("commit-before-ack", text)

    def test_measured_leg_with_empty_sections_is_incoherent(self):
        leg = _leg()
        leg["checks"] = {}
        leg["shapes"] = {}
        leg["ingest"] = {}
        _, problems = report.render([leg], require_labeled=False)
        self.assertTrue(any("empty checks" in p for p in problems))
        self.assertTrue(any("empty shapes" in p for p in problems))
        self.assertTrue(any("empty ingest" in p for p in problems))

    def test_mixed_corpus_digests_are_warned(self):
        a = _leg(engine="clickhouse")
        b = _leg(engine="duckdb")
        a["provenance"]["corpus_digest"] = "aaa"
        b["provenance"]["corpus_digest"] = "bbb"
        text, _ = report.render([a, b], require_labeled=False)
        self.assertIn("MIXED CORPORA", text)

    def test_same_corpus_digest_no_warning(self):
        a = _leg(engine="clickhouse")
        b = _leg(engine="duckdb")
        a["provenance"]["corpus_digest"] = "aaa"
        b["provenance"]["corpus_digest"] = "aaa"
        text, _ = report.render([a, b], require_labeled=False)
        self.assertNotIn("MIXED CORPORA", text)


if __name__ == "__main__":
    unittest.main()
