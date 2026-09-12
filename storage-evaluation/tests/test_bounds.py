"""Bounded-execution tests: generation timeout, workdir cap during
generation, and bounded teardown — all with tiny budgets, no heavy runs."""
import time
import unittest

from harness.runner import Deadline, generate_bounded, stop_bounded


class TestGenerateBounded(unittest.TestCase):
    def test_tiny_budget_aborts(self):
        t0 = time.monotonic()
        with self.assertRaises(Deadline):
            # Unused seed: an existing corpus dir would fail fast with the
            # stale-corpus guard instead of exercising the timeout.
            generate_bounded(100000, 424242, 10, budget_s=0.05,
                             workdir_cap_gib=999)
        self.assertLess(time.monotonic() - t0, 10)

    def test_zero_workdir_cap_aborts(self):
        with self.assertRaises(Deadline):
            generate_bounded(100000, 424243, 10, budget_s=60,
                             workdir_cap_gib=0)


class _WedgedEngine:
    container_name = "storage-eval-nonexistent"

    def stop(self):
        time.sleep(60)  # never returns within the test budget


class TestStopBounded(unittest.TestCase):
    def test_wedged_stop_is_bounded(self):
        t0 = time.monotonic()
        stop_bounded(_WedgedEngine(), timeout_s=1)
        self.assertLess(time.monotonic() - t0, 10)


if __name__ == "__main__":
    unittest.main()
