"""Render report.md from result legs + oracle manifests + the gate ledger.

Every row is labeled MEASURED / NOT RUN / ABORTED / ERROR. Baseline-parity
checks (both engines must reproduce frozen current behavior, including its
limitations) are reported separately from semantic gates (the target
semantics a migration must deliver). Parity on a limitation is never a
gate pass. --require-labeled-gates fails the report if any row lacks a
label or any gate is missing.
"""
from __future__ import annotations

import sys
from pathlib import Path

from .util import (
    CAPS, RESULTS, UNRUN_GATES, WORK, corpus_dir, dump_json, free_gib,
    load_json,
)


def _fmt_ms(v):
    return f"{v:.0f}" if isinstance(v, (int, float)) else "—"


def _fmt_gib(b):
    return f"{b / 1024**3:.2f}" if isinstance(b, (int, float)) and b >= 0 else "—"


def collect_legs() -> list[dict]:
    legs = []
    if RESULTS.exists():
        for p in sorted(RESULTS.glob("*.json")):
            legs.append(load_json(p))
    return legs


def render(legs: list[dict], require_labeled: bool) -> tuple[str, list[str]]:
    problems = []
    manifests = {}
    for leg in legs:
        key = (leg.get("scale"), leg.get("seed"))
        if key not in manifests:
            mp = corpus_dir(leg.get("scale", 0), leg.get("seed", 0)) / "oracle.json"
            manifests[key] = load_json(mp) if mp.exists() else {}
    L = []
    L.append("# Storage evaluation report")
    L.append("")
    L.append("Synthetic local corpus only. This is a decision input for "
             "docs/redesign/strategy.md, not a migration and not a production "
             "go/no-go. MEASURED rows were executed in this harness; NOT RUN "
             "rows are gates that remain open.")
    L.append("")

    # --- legs ---------------------------------------------------------------
    L.append("## Runs")
    L.append("")
    digests = {l.get("provenance", {}).get("code_digest") for l in legs}
    if len(digests) > 1:
        L.append("**MIXED PROVENANCE**: legs were produced by different code "
                 "states; per-leg digest shown. Do not read this table as one "
                 "coherent run.")
        L.append("")
    L.append("| engine | scale | readers | days | status | wall_s | load_s | "
             "teardown | code |")
    L.append("|---|---|---|---|---|---|---|---|---|")
    for leg in legs:
        cd = (leg.get("provenance", {}).get("code_digest") or "")[:8] or "—"
        L.append(f"| {leg['engine']} | {leg['scale']} | {leg['readers']} | "
                 f"{leg['days']} | {leg['status']} | {leg.get('wall_s','—')} | "
                 f"{leg.get('load_s','—')} | {leg.get('teardown','—')} | {cd} |")
        if leg["status"] not in ("MEASURED", "NOT RUN", "ABORTED", "ERROR"):
            problems.append(f"leg {leg['engine']} has unlabeled status")
        # A MEASURED leg with no resource samples means sampling failed —
        # the latency table must not render that as zero memory.
        if leg["status"] == "MEASURED" and not leg.get(
                "resources", {}).get("samples"):
            problems.append(
                f"leg {leg['engine']} MEASURED with no resource samples")
    if not legs:
        L.append("| — | — | — | — | NOT RUN | — | — | — | — |")
    L.append("")
    for leg in legs:
        reason = leg.get("abort_reason") or leg.get("error")
        if reason:
            L.append(f"- {leg['engine']} {leg['status']}: {_short(reason, 160)}")
    if any(leg.get("abort_reason") or leg.get("error") for leg in legs):
        L.append("")

    # --- oracle checks ------------------------------------------------------
    L.append("## Correctness")
    L.append("")
    L.append("### Baseline parity (frozen current behavior, incl. limitations)")
    L.append("")
    L.append("| check | engine | status | expected | actual |")
    L.append("|---|---|---|---|---|")
    for leg in legs:
        for cid, res in leg.get("checks", {}).items():
            if res.get("kind") != "baseline_parity":
                continue
            exp = res.get("expected")
            act = res.get("actual")
            L.append(f"| {cid} | {leg['engine']} | {res['status']} | "
                     f"{_short(exp)} | {_short(act)} |")
            if res["status"] not in ("PASS", "DIVERGENT", "NOT RUN", "ERROR"):
                problems.append(f"{cid}/{leg['engine']} unlabeled")
    L.append("")
    L.append("### Semantic gates (target semantics; parity on a limitation is "
             "not a pass)")
    L.append("")
    L.append("| gate | engine | status | note |")
    L.append("|---|---|---|---|")
    for leg in legs:
        for cid, res in leg.get("checks", {}).items():
            if res.get("kind") != "semantic_gate":
                continue
            # New legs carry their oracle note. Do not recover a missing
            # legacy note from an arbitrary current scratch corpus: that
            # would silently rewrite historical evidence.
            note = res.get("note")
            if not note:
                note = "semantic note unavailable: leg predates per-check note capture"
            if res["status"] == "DIVERGENT":
                note = f"expected {res.get('expected')}, got {res.get('actual')}. {note}"
            L.append(f"| {cid} | {leg['engine']} | {res['status']} | {note} |")
            if res["status"] not in ("PASS", "DIVERGENT", "NOT RUN", "ERROR"):
                problems.append(f"semantic gate {cid}/{leg['engine']} unlabeled")
    # --require-labeled-gates: every manifest check must appear per leg.
    for leg in legs:
        spec_checks = manifests.get((leg.get("scale"), leg.get("seed")), {}).get(
            "checks", {})
        for cid in spec_checks:
            if cid not in leg.get("checks", {}):
                problems.append(f"missing check {cid} in {leg['engine']} leg")
    L.append("")

    # --- latency / resources -------------------------------------------------
    L.append("## Query latency (ms) and resources")
    L.append("")
    L.append("| engine | shape | first p50 | first p95 | repeat p50 | repeat p95 | "
             "mem_peak MiB | cpu% peak | disk GiB |")
    L.append("|---|---|---|---|---|---|---|---|---|")
    for leg in legs:
        res = leg.get("resources", {})
        disk = res.get("disk_final_bytes")
        mem = res.get("mem_peak_bytes")
        mem_s = f"{mem / 1024**2:.0f}" if isinstance(mem, (int, float)) else "—"
        for sid, sh in leg.get("shapes", {}).items():
            c, w = sh.get("first", {}), sh.get("repeat", {})
            L.append(f"| {leg['engine']} | {sid} | {_fmt_ms(c.get('p50_ms'))} | "
                     f"{_fmt_ms(c.get('p95_ms'))} | {_fmt_ms(w.get('p50_ms'))} | "
                     f"{_fmt_ms(w.get('p95_ms'))} | "
                     f"{mem_s} | "
                     f"{res.get('cpu_pct_peak', '—')} | {_fmt_gib(disk)} |")
    L.append("")
    L.append("Latency columns: `first` is the first timed pass after oracle "
             "checks (not a true cold read); `repeat` is two further passes. "
             "n=1/2 samples are smoke-scale only. Memory is container cgroup "
             "usage, not process RSS; `—` means sampling produced no data, "
             "not zero usage. DuckDB serves every op — reads included — on "
             "one locked connection, so its reader-concurrency numbers are "
             "serialized throughput, not parallel serving.")
    for leg in legs:
        res = leg.get("resources", {})
        if res.get("sample_errors"):
            L.append(f"- {leg['engine']}: resource sampler recorded "
                     f"{res['sample_errors']} failed poll(s) "
                     f"({_short(res.get('sample_error_first', ''), 120)}); "
                     f"{res.get('samples', 0)} sample(s) succeeded")
    L.append("")
    L.append("## Ingest")
    L.append("")
    L.append("| engine | rows | ack_s | visibility_lag_s | expected | "
             "final_total | readers overlapping ingest |")
    L.append("|---|---|---|---|---|---|---|")
    for leg in legs:
        ing = leg.get("ingest", {})
        exp, fin = ing.get("expected_total"), ing.get("final_total")
        # A final_total below expected_total is acknowledged-event loss —
        # flag it in the cell, never let it pass as a number.
        fin_s = ("—" if fin is None else
                 f"{fin} (MISMATCH)" if exp is not None and fin != exp else f"{fin}")
        L.append(f"| {leg['engine']} | {ing.get('rows','—')} | "
                 f"{ing.get('ack_s','—')} | {ing.get('visibility_lag_s','—')} | "
                 f"{exp if exp is not None else '—'} | {fin_s} | "
                 f"{ing.get('readers_overlapping_ingest','—')} |")
    L.append("")
    L.append("`ack_s` is the local insert call returning — it is NOT a "
             "durable-queue commit-before-ack; that crash/replay gate stays "
             "NOT RUN below. `visibility_lag_s` is observed by post-ack "
             "count() polling (0.5s granularity upper bound). Overlap counts "
             "reader spans that began before the ack and ended after the "
             "ingest started.")
    L.append("")

    # --- gate ledger ----------------------------------------------------------
    L.append("## Gate ledger")
    L.append("")
    L.append("| gate | status | why |")
    L.append("|---|---|---|")
    # Coverage is keyed on the full (engine, scale, readers, days)
    # combination — a leg at one reader/day count never marks the whole
    # scale MEASURED. A leg that ran but did not finish MEASURED keeps its
    # real status; a combination with no leg is NOT RUN.
    ran = {}
    for l in legs:
        key = (l["engine"], l["scale"], l["readers"], l["days"])
        prev = ran.get(key)
        if prev is None or (prev != "MEASURED" and l["status"] == "MEASURED"):
            ran[key] = l["status"]
    free = free_gib(WORK)
    need = CAPS["matrix"]["preflight_free_gib"]
    matrix_reason = (
        f"free disk {free:.1f} GiB below the {need} GiB preflight"
        if free < need else "not requested in this run")
    for eng in ("clickhouse", "duckdb"):
        for scale in CAPS["matrix"]["scales"]:
            for r in CAPS["matrix"]["readers"]:
                for d in CAPS["matrix"]["days"]:
                    st = ran.get((eng, scale, r, d))
                    if st == "MEASURED":
                        L.append(f"| matrix {scale} {eng} r{r} d{d} | "
                                 f"MEASURED | |")
                    elif st is not None:
                        L.append(f"| matrix {scale} {eng} r{r} d{d} | {st} | "
                                 f"leg ran but did not complete |")
                    else:
                        L.append(f"| matrix {scale} {eng} r{r} d{d} | "
                                 f"NOT RUN | {matrix_reason} |")
    for gid, label, why in UNRUN_GATES:
        L.append(f"| {label} | NOT RUN | {why} |")
    L.append("")
    L.append("Vendor references: [DuckDB concurrency](https://duckdb.org/docs/current/connect/concurrency), "
             "[DuckDB tuning](https://duckdb.org/docs/current/guides/performance/how_to_tune_workloads), "
             "[DuckDB security](https://duckdb.org/docs/current/operations_manual/securing_duckdb/overview), "
             "[ClickHouse serving](https://clickhouse.com/resources/engineering/high-concurrency-sizing-user-analytics).")
    L.append("")
    return "\n".join(L), problems


def _short(v, n=80):
    s = str(v)
    return s if len(s) <= n else s[:n] + "…"


def write_report(require_labeled: bool) -> Path:
    legs = collect_legs()
    text, problems = render(legs, require_labeled)
    out = WORK / "report.md"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(text)
    if require_labeled and problems:
        for p in problems:
            print(f"UNLABELED: {p}", file=sys.stderr)
        raise SystemExit(1)
    return out
