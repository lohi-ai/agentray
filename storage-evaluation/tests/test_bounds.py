"""Bounded-execution tests: generation timeout, workdir cap during
generation, and bounded teardown — all with tiny budgets, no heavy runs."""
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from harness import cli, engines
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


class _FailingEngine:
    container_name = "storage-eval-nonexistent"

    def stop(self):
        raise RuntimeError("shutdown POST refused")


class TestStopBounded(unittest.TestCase):
    def test_wedged_stop_is_bounded(self):
        t0 = time.monotonic()
        # No docker socket in the test container: the real fallback fails
        # fast, which must surface as 'failed:', not a claimed removal.
        status = stop_bounded(_WedgedEngine(), stop_s=0.5, fallback_s=5)
        self.assertLess(time.monotonic() - t0, 10)
        self.assertTrue(status.startswith("failed:"), status)

    def test_wedged_stop_forces_removal(self):
        calls = []
        with mock.patch.object(engines, "stop_container",
                               lambda name: calls.append(name)):
            status = stop_bounded(_WedgedEngine(), stop_s=0.5, fallback_s=5)
        self.assertEqual(status, "forced")
        self.assertEqual(calls, ["storage-eval-nonexistent"])

    def test_hung_fallback_reports_unknown(self):
        def hang(name):
            time.sleep(60)
        with mock.patch.object(engines, "stop_container", hang):
            t0 = time.monotonic()
            status = stop_bounded(_WedgedEngine(), stop_s=0.2, fallback_s=0.5)
        self.assertLess(time.monotonic() - t0, 10)
        self.assertEqual(status, "unknown")

    def test_failing_fallback_reports_failed(self):
        def boom(name):
            raise RuntimeError("docker socket gone")
        with mock.patch.object(engines, "stop_container", boom):
            status = stop_bounded(_WedgedEngine(), stop_s=0.2, fallback_s=5)
        self.assertTrue(status.startswith("failed:"), status)

    def test_failed_stop_still_attempts_fallback(self):
        calls = []
        with mock.patch.object(engines, "stop_container",
                               lambda name: calls.append(name)):
            status = stop_bounded(_FailingEngine(), stop_s=5, fallback_s=5)
        self.assertEqual(status, "forced")
        self.assertEqual(calls, ["storage-eval-nonexistent"])


class TestArchivePrior(unittest.TestCase):
    def test_successive_publications_preserve_all(self):
        with tempfile.TemporaryDirectory() as td:
            dest = Path(td)
            (dest / "run-a.json").write_text('{"run": "a"}')
            (dest / "report.md").write_text("report-a")
            cli._archive_prior(dest)
            (dest / "run-b.json").write_text('{"run": "b"}')
            (dest / "report.md").write_text("report-b")
            cli._archive_prior(dest)
            (dest / "run-c.json").write_text('{"run": "c"}')
            cli._archive_prior(dest)

            archive = dest / "archive"
            dirs = sorted(p.name for p in archive.iterdir() if p.is_dir())
            self.assertEqual(len(dirs), 3, dirs)
            contents = []
            for d in dirs:
                contents.append(sorted(
                    (p.name, p.read_text()) for p in archive.joinpath(d).iterdir()))
            flat = {name: text for d in contents for name, text in d}
            self.assertEqual(flat["run-a.json"], '{"run": "a"}')
            self.assertEqual(flat["run-b.json"], '{"run": "b"}')
            self.assertEqual(flat["run-c.json"], '{"run": "c"}')
            self.assertEqual(flat["report.md"], "report-b")

    def test_nothing_to_archive_returns_none(self):
        with tempfile.TemporaryDirectory() as td:
            self.assertIsNone(cli._archive_prior(Path(td)))


if __name__ == "__main__":
    unittest.main()
