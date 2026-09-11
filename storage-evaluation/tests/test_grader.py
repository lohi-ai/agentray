"""Grader contract tests: missing/null/truncated engine output is never a
pass; PASS always records compared values. Run inside the driver image:
  docker run --rm -v $PWD:$PWD -w $PWD storage-eval-driver:py3.13 \
    python -m unittest discover -s tests -v
"""
import unittest

from harness.runner import _grade


class TestGrader(unittest.TestCase):
    def test_empty_rows_is_error(self):
        r = _grade("sessionization.sessions_7d",
                   {"kind": "baseline_parity", "distinct_sessions": 5}, [])
        self.assertEqual(r["status"], "ERROR")

    def test_none_rows_is_error(self):
        r = _grade("sessionization.sessions_7d",
                   {"kind": "baseline_parity", "distinct_sessions": 5}, None)
        self.assertEqual(r["status"], "ERROR")

    def test_missing_column_is_error(self):
        r = _grade("sessionization.sessions_7d",
                   {"kind": "baseline_parity", "distinct_sessions": 5},
                   [{"wrong": 5}])
        self.assertEqual(r["status"], "ERROR")

    def test_null_value_is_error(self):
        r = _grade("currency.cost_7d",
                   {"kind": "baseline_parity", "sum_usd": 1.0},
                   [{"s": None}])
        self.assertEqual(r["status"], "ERROR")

    def test_truncated_top10_diverges_not_passes(self):
        spec = {"kind": "baseline_parity", "total": 100,
                "top10": [["u%d" % i, 10 - i] for i in range(10)]}
        rows = [{"canonical_id": "u%d" % i, "c": 10 - i} for i in range(5)]
        r = _grade("identity.canonical_events_7d", spec, rows,
                   extra_rows=[{"c": 100}])
        self.assertEqual(r["status"], "DIVERGENT")

    def test_pass_records_values(self):
        r = _grade("sessionization.sessions_7d",
                   {"kind": "baseline_parity", "distinct_sessions": 5},
                   [{"c": 5}])
        self.assertEqual(r["status"], "PASS")
        self.assertEqual(r["expected"], 5)
        self.assertEqual(r["actual"], 5)

    def test_entity_join_reads_amount_not_count(self):
        spec = {"kind": "baseline_parity", "amount_cents": 42}
        r = _grade("entity_join.order_paid_7d", spec,
                   [{"n": 7, "amount_cents": 42}])
        self.assertEqual(r["status"], "PASS")
        self.assertEqual(r["actual"], 42)

    def test_tombstone_gate_divergent_when_visible(self):
        spec = {"kind": "semantic_gate", "expected_visible": 0}
        r = _grade("entity.tombstone_delete", spec, [{"c": 100}])
        self.assertEqual(r["status"], "DIVERGENT")


if __name__ == "__main__":
    unittest.main()
