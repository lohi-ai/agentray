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
            archived = [pair for content in contents for pair in content]
            self.assertIn(("run-a.json", '{"run": "a"}'), archived)
            self.assertIn(("run-b.json", '{"run": "b"}'), archived)
            self.assertIn(("run-c.json", '{"run": "c"}'), archived)
            self.assertIn(("report.md", "report-a"), archived)
            self.assertIn(("report.md", "report-b"), archived)

    def test_nothing_to_archive_returns_none(self):
        with tempfile.TemporaryDirectory() as td:
            self.assertIsNone(cli._archive_prior(Path(td)))

    def test_identical_incoming_archives_nothing(self):
        # Republishing byte-identical results must not create an archive —
        # unchanged evidence stays in place, no duplicate history.
        with tempfile.TemporaryDirectory() as td:
            dest = Path(td)
            (dest / "run-a.json").write_bytes(b'{"run": "a"}')
            (dest / "report.md").write_bytes(b"report-a")
            incoming = {"run-a.json": b'{"run": "a"}',
                        "report.md": b"report-a"}
            self.assertIsNone(cli._archive_prior(dest, incoming))
            self.assertFalse((dest / "archive").exists())
            self.assertTrue((dest / "run-a.json").exists())

    def test_changed_incoming_archives_only_superseded(self):
        with tempfile.TemporaryDirectory() as td:
            dest = Path(td)
            (dest / "run-a.json").write_bytes(b'{"run": "a"}')
            (dest / "run-b.json").write_bytes(b'{"run": "b"}')
            incoming = {"run-a.json": b'{"run": "a"}',
                        "run-b.json": b'{"run": "b2"}'}
            used = cli._archive_prior(dest, incoming)
            self.assertIsNotNone(used)
            self.assertEqual(sorted(p.name for p in used.iterdir()),
                             ["run-b.json"])
            self.assertEqual((used / "run-b.json").read_bytes(),
                             b'{"run": "b"}')
            # The unchanged file was never moved.
            self.assertTrue((dest / "run-a.json").exists())

    def test_same_evidence_set_reuses_existing_archive(self):
        # Publishing A, then B, then A again must not archive A twice —
        # the content-addressed dir is reused and the file removed.
        with tempfile.TemporaryDirectory() as td:
            dest = Path(td)
            (dest / "run.json").write_bytes(b"A")
            first = cli._archive_prior(dest, {"run.json": b"B"})
            self.assertIsNotNone(first)
            (dest / "run.json").write_bytes(b"A")
            second = cli._archive_prior(dest, {"run.json": b"C"})
            self.assertEqual(first, second)
            self.assertFalse((dest / "run.json").exists())
            dirs = [p for p in (dest / "archive").iterdir() if p.is_dir()]
            self.assertEqual(len(dirs), 1)
            self.assertEqual((dirs[0] / "run.json").read_bytes(), b"A")

    def test_report_refuses_to_publish_over_evidence_with_no_legs(self):
        # work/results empty but results/ holds committed evidence —
        # publishing would archive it for an empty report.
        with tempfile.TemporaryDirectory() as td:
            empty_results = Path(td) / "empty"
            empty_results.mkdir()
            with mock.patch.object(cli, "RESULTS", empty_results), \
                    mock.patch.object(cli.report, "write_report",
                                      return_value=Path(td) / "r.md"), \
                    mock.patch.object(cli, "_publish_durable") as pub:
                rc = cli.cmd_report(
                    type("A", (), {"require_labeled_gates": False})())
            self.assertEqual(rc, 1)
            pub.assert_not_called()


if __name__ == "__main__":
    unittest.main()
