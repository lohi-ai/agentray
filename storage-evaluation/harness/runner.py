"""Run one engine leg: load corpus, oracle checks, cold/warm query shapes,
ingest-during-reads, resource sampling. Writes work/results/<run>.json.
"""
from __future__ import annotations

import json
import os
import subprocess
import statistics
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from . import engines, queries
from .util import WORK, code_digest, corpus_digest, dir_bytes, load_json


class Deadline(Exception):
    pass


class Sampler(threading.Thread):
    """Polls docker stats for the engine container; records idle/peak.

    memory_stats.usage is total container memory usage (cgroup), not
    process RSS — field names say so.
    """

    def __init__(self, container_name: str, interval=1.0):
        super().__init__(daemon=True)
        self.container_name = container_name
        self.interval = interval
        self.samples = []
        # Sampling failures are recorded, not swallowed: a leg with zero
        # samples must report the failure, not render as if memory were 0.
        self.errors = []
        self._stop_event = threading.Event()

    def run(self):
        try:
            cli = engines.client()
            c = cli.containers.get(self.container_name)
        except Exception as e:
            self.errors.append(f"{type(e).__name__}: {e}")
            return
        while not self._stop_event.is_set():
            try:
                s = c.stats(stream=False)
                mem = s["memory_stats"].get("usage", 0)
                cpu_delta = (
                    s["cpu_stats"]["cpu_usage"]["total_usage"]
                    - s["precpu_stats"]["cpu_usage"]["total_usage"]
                )
                sys_delta = (
                    s["cpu_stats"].get("system_cpu_usage", 0)
                    - s["precpu_stats"].get("system_cpu_usage", 0)
                )
                ncpu = len(s["cpu_stats"]["cpu_usage"].get("percpu_usage") or [1])
                cpu_pct = (cpu_delta / sys_delta * ncpu * 100.0) if sys_delta > 0 else 0.0
                self.samples.append({"t": time.time(), "mem": mem, "cpu_pct": cpu_pct})
            except Exception as e:
                self.errors.append(f"{type(e).__name__}: {e}")
            self._stop_event.wait(self.interval)

    def stop(self):
        self._stop_event.set()

    def summary(self) -> dict:
        out = {}
        if self.samples:
            mem = [s["mem"] for s in self.samples]
            cpu = [s["cpu_pct"] for s in self.samples]
            out.update({
                "mem_idle_bytes": mem[0],
                "mem_peak_bytes": max(mem),
                "cpu_pct_peak": round(max(cpu), 1),
                "cpu_pct_mean": round(statistics.mean(cpu), 1),
            })
        out["samples"] = len(self.samples)
        if self.errors:
            out["sample_errors"] = len(self.errors)
            out["sample_error_first"] = self.errors[0][:200]
        return out


def _pct(values, p):
    if not values:
        return None
    values = sorted(values)
    k = min(len(values) - 1, int(len(values) * p))
    return round(values[k] * 1000, 1)  # ms


def _overlaps(span_start: float, span_end: float,
              ingest_start: float, ingest_end: float) -> bool:
    """A read overlaps the ingest iff it began before the ack and ended
    after the ingest started — a read that finished before ingestion began
    does not count."""
    return span_start < ingest_end and span_end > ingest_start


def _run_shape(engine, shape_id, days):
    t0 = time.monotonic()
    engine.run_shape(shape_id, days)
    return time.monotonic() - t0


def generate_bounded(scale: int, seed: int, ingest_rows: int,
                     budget_s: float, workdir_cap_gib: float) -> Path:
    """Generate the corpus in a killable subprocess with a real wall budget
    and a workdir cap enforced *during* generation, not only after.

    Raises Deadline on timeout or workdir overflow; the child is killed
    either way.
    """
    WORK.mkdir(parents=True, exist_ok=True)
    err_path = WORK / "corpus-gen.err"
    with open(err_path, "w") as err_f:
        proc = subprocess.Popen(
            [sys.executable, "-m", "harness.corpus_gen",
             str(scale), str(seed), str(ingest_rows)],
            # stderr goes to a file, not a pipe: a child that dumps >64KiB
            # of traceback would deadlock a never-drained PIPE until the
            # budget kill. stdout stays a pipe — only the output path.
            stdout=subprocess.PIPE, stderr=err_f, text=True)
        t0 = time.monotonic()
        try:
            while proc.poll() is None:
                if time.monotonic() - t0 > budget_s:
                    proc.kill()
                    proc.wait()
                    raise Deadline(
                        f"corpus generation exceeded {budget_s}s budget")
                used = dir_bytes(WORK) / 1024**3
                if used > workdir_cap_gib:
                    proc.kill()
                    proc.wait()
                    raise Deadline(
                        f"workdir {used:.1f} GiB exceeded cap "
                        f"{workdir_cap_gib} GiB during generation")
                time.sleep(0.5)
            if proc.returncode != 0:
                try:
                    err = err_path.read_text()[-500:]
                except OSError:
                    err = ""
                raise RuntimeError(f"corpus generation failed: {err}")
            return Path(proc.stdout.read().strip().splitlines()[-1])
        finally:
            # Deterministic cleanup: kill if still running, always reap,
            # always close the pipe — no ResourceWarning, no zombie.
            if proc.poll() is None:
                proc.kill()
            proc.wait()
            if proc.stdout is not None:
                proc.stdout.close()


def stop_bounded(eng, stop_s: float = 20, fallback_s: float = 10):
    """Engine teardown under ONE total deadline (stop_s + fallback_s).

    The graceful eng.stop() gets stop_s; if it hangs or raises, the
    labeled force-remove fallback gets fallback_s — the whole cleanup path
    is bounded, not just the graceful half. Returns a status string:
    'stopped' (graceful ok), 'forced' (fallback removed the container),
    'failed: ...' (fallback raised), or 'unknown' (deadline expired with
    cleanup still in flight — a residual live container is possible).
    Never claims a removal it did not observe."""
    outcome = {}

    def _graceful():
        try:
            eng.stop()
            outcome["s"] = "stopped"
        except Exception as e:
            outcome["s"] = f"stop-failed: {type(e).__name__}: {e}"

    t = threading.Thread(target=_graceful, daemon=True)
    t.start()
    t.join(stop_s)
    if not t.is_alive() and outcome.get("s") == "stopped":
        return "stopped"
    # Hung or failed graceful stop -> bounded force-remove fallback. The
    # ownership-label guard lives inside engines.stop_container.
    def _force():
        try:
            engines.stop_container(eng.container_name)
            outcome["s"] = "forced"
        except Exception as e:
            outcome["s"] = f"failed: {type(e).__name__}: {e}"

    f = threading.Thread(target=_force, daemon=True)
    f.start()
    f.join(fallback_s)
    if f.is_alive():
        return "unknown"
    return outcome.get("s", "unknown")


def _safe_stop(eng):
    try:
        eng.stop()
    except Exception:
        pass


def run_leg(engine_name: str, scale: int, seed: int, readers: int, days: int,
            caps: dict, deadline_s: float) -> dict:
    t_start = time.monotonic()
    eng = {"clickhouse": engines.ClickHouseEngine,
           "duckdb": engines.DuckDBEngine}[engine_name]()
    leg = {
        "engine": engine_name, "scale": scale, "seed": seed,
        "readers": readers, "days": days, "status": "MEASURED",
        "checks": {}, "shapes": {}, "ingest": {}, "resources": {},
        "provenance": {
            "code_commit": os.environ.get("EVAL_COMMIT") or None,
            "code_digest": code_digest(),
            # The limits this leg actually ran under — the run contract is
            # incomplete without them (same numbers, different envelope =
            # different evidence).
            "limits": {
                "engine_mem": caps["engine_mem"],
                "engine_cpus": caps["engine_cpus"],
                "workdir_gib": caps["workdir_gib"],
                "wall_s": deadline_s,
                "ingest_rows": caps["ingest_rows"],
            },
            # Driver envelope: the eval wrapper passes its own caps in;
            # absent means the leg ran outside the wrapper.
            "driver_image": os.environ.get("EVAL_DRIVER_IMAGE") or None,
            "driver_mem": os.environ.get("EVAL_DRIVER_MEM") or None,
            "driver_cpus": os.environ.get("EVAL_DRIVER_CPUS") or None,
        },
    }
    def remaining():
        left = deadline_s - (time.monotonic() - t_start)
        if left <= 0:
            raise Deadline(f"wall cap {deadline_s}s exceeded")
        return left

    def check_workdir():
        used = dir_bytes(WORK) / 1024**3
        if used > caps["workdir_gib"]:
            raise Deadline(
                f"workdir {used:.1f} GiB exceeded cap {caps['workdir_gib']} GiB")

    # Watchdog: the deadline is also enforced mid-call — a hung HTTP request
    # must not outlive the wall cap. On expiry the engine container is
    # force-stopped, which unblocks in-flight requests with an error; the
    # fired flag lets the leg report ABORTED rather than a spurious ERROR.
    # Armed before corpus generation so a slow generate() is capped too.
    fired = threading.Event()

    def _on_deadline():
        fired.set()
        engines.stop_container(eng.container_name)

    watchdog = threading.Timer(deadline_s, _on_deadline)
    watchdog.daemon = True

    sampler = Sampler(eng.container_name)
    try:
        watchdog.start()
        # Generation runs in a killable subprocess with its own share of
        # the wall budget and live workdir-cap enforcement — the engine
        # watchdog cannot stop driver-side work (no engine exists yet).
        corpus_dir = generate_bounded(
            scale, seed, caps["ingest_rows"],
            budget_s=remaining(), workdir_cap_gib=caps["workdir_gib"])
        oracle = load_json(corpus_dir / "oracle.json")
        deleted_keys = oracle["checks"]["entity.deleted_still_visible"]["deleted_keys"]
        leg["provenance"]["corpus_digest"] = corpus_digest(corpus_dir)
        # Executed provenance: the corpus this leg actually ran against,
        # recorded at run time — never grafted on later from whatever
        # oracle happens to be on disk at report time.
        leg["provenance"]["corpus_rows"] = oracle.get("total_rows")
        remaining()
        eng.start(caps)
        sampler.start()
        time.sleep(3)  # idle baseline sample
        leg["resources"]["disk_before_bytes"] = eng.disk()

        t0 = time.monotonic()
        eng.load(corpus_dir)
        leg["load_s"] = round(time.monotonic() - t0, 2)
        leg["resources"]["disk_after_load_bytes"] = eng.disk()
        remaining()
        check_workdir()

        # Oracle checks — exact comparison against the manifest.
        for cid, spec in oracle["checks"].items():
            if spec.get("status") == "NOT RUN":
                leg["checks"][cid] = {"kind": spec["kind"], "status": "NOT RUN",
                                      "note": spec.get("note", "")}
                continue
            try:
                rows = eng.run_check(cid, days, deleted_keys)
                extra = None
                if cid == "identity.canonical_events_7d":
                    extra = eng.run_check("identity.canonical_total_7d", days,
                                          deleted_keys)
                result = _grade(cid, spec, rows, extra)
                # Persist semantic context with the executed leg. A later
                # report must not depend on whatever scratch corpus happens
                # to remain on disk to explain this result.
                if spec.get("note"):
                    result["note"] = spec["note"]
                leg["checks"][cid] = result
            except Exception as e:
                leg["checks"][cid] = {"kind": spec["kind"], "status": "ERROR",
                                      "error": str(e)[:300]}
            remaining()

        # First timed pass then repeat passes. The first pass is not a true
        # cold read — oracle checks already touched the data — so the labels
        # say what was measured, not what was hoped.
        for phase in ("first", "repeat"):
            lat = {sid: [] for sid in queries.LOAD_SHAPES}
            passes = 1 if phase == "first" else 2
            for _ in range(passes):
                with ThreadPoolExecutor(max_workers=readers) as pool:
                    futs = []
                    for r in range(readers):
                        for sid in queries.LOAD_SHAPES:
                            futs.append((sid, pool.submit(_run_shape, eng, sid, days)))
                    for sid, f in futs:
                        lat[sid].append(f.result())
                remaining()
            for sid, vals in lat.items():
                d = leg["shapes"].setdefault(sid, {})
                d[phase] = {
                    "p50_ms": _pct(vals, 0.50), "p95_ms": _pct(vals, 0.95),
                    "p99_ms": _pct(vals, 0.99), "n": len(vals),
                }

        # Ingest leg: batch lands while readers run; overlap is recorded
        # with timestamps, not assumed. A span overlaps the ingest iff it
        # started before the ack AND ended after the ingest began.
        with ThreadPoolExecutor(max_workers=readers) as pool:
            def timed_read():
                s = time.monotonic()
                _run_shape(eng, "overview", days)
                return s, time.monotonic()
            futs = [pool.submit(timed_read) for _ in range(readers)]
            t0 = time.monotonic()
            ing = eng.ingest(corpus_dir)
            ingest_end = t0 + ing["ack_s"]
            expected_total = oracle["total_rows"] + oracle["ingest_rows"]
            # Independently observed visibility: poll count() after the ack
            # until the batch is reflected. Never assigned from ack_s.
            visible_at = None
            while time.monotonic() - t0 < 60:
                if eng.count() >= expected_total:
                    visible_at = time.monotonic() - t0
                    break
                time.sleep(0.5)
            spans = [f.result() for f in futs]
        overlap = sum(1 for s, e in spans if _overlaps(s, e, t0, ingest_end))
        leg["ingest"] = {
            "rows": oracle["ingest_rows"], "ack_s": round(ing["ack_s"], 2),
            # Independently observed via post-ack count() polling; this is
            # an upper bound at 0.5s poll granularity, not the ack time.
            "visibility_lag_s": (round(visible_at, 2) if visible_at is not None else ">60"),
            "expected_total": expected_total, "final_total": eng.count(),
            "readers_overlapping_ingest": overlap,
            "reader_spans_s": [[round(s - t0, 3), round(e - t0, 3)]
                              for s, e in spans],
        }
        leg["resources"]["disk_final_bytes"] = eng.disk()
        check_workdir()
        leg["status"] = "MEASURED"
    except Deadline as e:
        leg["status"] = "ABORTED"
        leg["abort_reason"] = str(e)
    except Exception as e:
        if fired.is_set():
            # The watchdog killed the engine mid-call; this is the wall cap
            # firing, not an engine fault.
            leg["status"] = "ABORTED"
            leg["abort_reason"] = f"wall cap {deadline_s}s exceeded"
        else:
            leg["status"] = "ERROR"
            leg["error"] = f"{type(e).__name__}: {e}"
    finally:
        watchdog.cancel()
        sampler.stop()
        if sampler.is_alive() or sampler.ident is not None:
            sampler.join(timeout=5)
        leg["resources"].update(sampler.summary())
        # Teardown is bounded end-to-end (graceful 20s + force-remove
        # fallback 10s); the outcome is recorded, never assumed.
        leg["teardown"] = stop_bounded(eng, stop_s=20, fallback_s=10)
    leg["wall_s"] = round(time.monotonic() - t_start, 1)
    return leg


def _grade(cid: str, spec: dict, rows: list, extra_rows: list | None = None) -> dict:
    """Compare engine rows to the manifest expectation. Exact match only.

    Missing, empty, null, or truncated engine output is ERROR — never a
    pass. PASS always records the compared expected/actual values.
    """
    kind = spec["kind"]
    out = {"kind": kind, "status": "PASS", "expected": None, "actual": None}

    def err(msg):
        out["status"] = "ERROR"
        out["error"] = msg
        return out

    def fail(exp, act):
        out["status"] = "DIVERGENT"
        out["expected"], out["actual"] = exp, act
        return out

    def ok(exp, act):
        out["expected"], out["actual"] = exp, act
        return out

    if not isinstance(rows, list) or not rows:
        return err("engine returned no rows")

    def scalar(row, key):
        if not isinstance(row, dict) or key not in row or row[key] is None:
            raise KeyError(key)
        return row[key]

    try:
        if cid == "identity.canonical_events_7d":
            top = [[r["canonical_id"], int(r["c"])] for r in rows]
            if not extra_rows:
                return err("missing canonical total sub-query")
            total = int(scalar(extra_rows[0], "c"))
            exp = {"top10": [list(t) for t in spec["top10"]], "total": spec["total"]}
            act = {"top10": top, "total": total}
            return ok(exp, act) if act == exp else fail(exp, act)
        if cid == "sessionization.sessions_7d":
            act = int(scalar(rows[0], "c"))
            return ok(spec["distinct_sessions"], act) if act == spec["distinct_sessions"] \
                else fail(spec["distinct_sessions"], act)
        if cid == "sessionization.gap_violations":
            act = int(scalar(rows[0], "c"))
            return ok(spec["count"], act) if act == spec["count"] \
                else fail(spec["count"], act)
        if cid == "dedup.raw_vs_distinct_7d":
            raw = int(scalar(rows[0], "raw"))
            dist = int(scalar(rows[0], "distinct_ids"))
            exp = [spec["raw"], spec["distinct_event_ids"]]
            return ok(exp, [raw, dist]) if [raw, dist] == exp else fail(exp, [raw, dist])
        if cid == "late_events.bucket_7d":
            act = int(scalar(rows[0], "c"))
            return ok(spec["late_gt_1h"], act) if act == spec["late_gt_1h"] \
                else fail(spec["late_gt_1h"], act)
        if cid == "funnel.signup_window_7d":
            hist = [0, 0, 0, 0]
            for r in rows:
                lvl = int(scalar(r, "level"))
                cnt = int(scalar(r, "c"))
                if lvl < 4:
                    hist[lvl] = cnt
            # oracle histogram counts level-0 users too; engines only return >0
            exp = spec["level_histogram"]
            return ok(exp[1:], hist[1:]) if hist[1:] == exp[1:] else fail(exp[1:], hist[1:])
        if cid == "retention.week1_mature":
            base = int(scalar(rows[0], "base"))
            w1 = int(scalar(rows[0], "w1"))
            exp = [spec["base"], spec["week1_users"]]
            return ok(exp, [base, w1]) if [base, w1] == exp else fail(exp, [base, w1])
        if cid == "entity.current_rows":
            act = int(scalar(rows[0], "c"))
            return ok(spec["count"], act) if act == spec["count"] \
                else fail(spec["count"], act)
        if cid in ("entity.deleted_still_visible", "entity.tombstone_delete"):
            act = int(scalar(rows[0], "c"))
            return ok(spec["expected_visible"], act) if act == spec["expected_visible"] \
                else fail(spec["expected_visible"], act)
        if cid == "entity_join.order_paid_7d":
            act = int(scalar(rows[0], "amount_cents") or 0)
            return ok(spec["amount_cents"], act) if act == spec["amount_cents"] \
                else fail(spec["amount_cents"], act)
        if cid == "currency.cost_7d":
            act = float(scalar(rows[0], "s"))
            exp = float(spec["sum_usd"])
            # Float32 stored values summed in float64: exact match expected,
            # 1e-6 relative slack for engine summation-order differences.
            good = abs(act - exp) <= max(1e-6 * abs(exp), 1e-9)
            return ok(exp, act) if good else fail(exp, act)
        return err(f"unhandled check id {cid}")
    except (KeyError, TypeError, IndexError, ValueError) as e:
        return err(f"malformed engine output: {e}; rows={str(rows)[:200]}")
