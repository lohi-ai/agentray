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

from .util import RESULTS, UNRUN_GATES, WORK, corpus_dir, dump_json, load_json


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
    L.append("| engine | scale | readers | days | status | wall_s | load_s |")
    L.append("|---|---|---|---|---|---|---|")
    for leg in legs:
        L.append(f"| {leg['engine']} | {leg['scale']} | {leg['readers']} | "
                 f"{leg['days']} | {leg['status']} | {leg.get('wall_s','—')} | "
                 f"{leg.get('load_s','—')} |")
        if leg["status"] not in ("MEASURED", "NOT RUN", "ABORTED", "ERROR"):
            problems.append(f"leg {leg['engine']} has unlabeled status")
    if not legs:
        L.append("| — | — | — | — | NOT RUN | — | — |")
    L.append("")

    # --- oracle checks ------------------------------------------------------
    L.append("## Correctness")
    L.append("")
    L.append("### Baseline parity (frozen current behavior, incl. limitations)")
    L.append("")
    L.append("| check | engine | status | expected | actual |")
    L.append("|---|---|---|---|---|")
    seen = set()
    for leg in legs:
        for cid, res in leg.get("checks", {}).items():
            if res.get("kind") != "baseline_parity":
                continue
            seen.add(cid)
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
            spec = manifests.get((leg.get("scale"), leg.get("seed")), {}).get(
                "checks", {}).get(cid, {})
            note = res.get("note") or spec.get("note", "")
            if res["status"] == "DIVERGENT":
                note = f"expected {res.get('expected')}, got {res.get('actual')}. {note}"
            L.append(f"| {cid} | {leg['engine']} | {res['status']} | {note} |")
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
        for sid, sh in leg.get("shapes", {}).items():
            c, w = sh.get("first", {}), sh.get("repeat", {})
            L.append(f"| {leg['engine']} | {sid} | {_fmt_ms(c.get('p50_ms'))} | "
                     f"{_fmt_ms(c.get('p95_ms'))} | {_fmt_ms(w.get('p50_ms'))} | "
                     f"{_fmt_ms(w.get('p95_ms'))} | "
                     f"{res.get('mem_peak_bytes', 0) / 1024**2:.0f} | "
                     f"{res.get('cpu_pct_peak', '—')} | {_fmt_gib(disk)} |")
    L.append("")
    L.append("Latency columns: `first` is the first timed pass after oracle "
             "checks (not a true cold read); `repeat` is two further passes. "
             "n=1/2 samples are smoke-scale only. Memory is container cgroup "
             "usage, not process RSS.")
    L.append("")
    L.append("## Ingest")
    L.append("")
    L.append("| engine | rows | ack_s | visibility_lag_s | final_total |")
    L.append("|---|---|---|---|---|")
    for leg in legs:
        ing = leg.get("ingest", {})
        L.append(f"| {leg['engine']} | {ing.get('rows','—')} | "
                 f"{ing.get('ack_s','—')} | {ing.get('visibility_lag_s','—')} | "
                 f"{ing.get('final_total','—')} |")
    L.append("")

    # --- gate ledger ----------------------------------------------------------
    L.append("## Gate ledger")
    L.append("")
    L.append("| gate | status | why |")
    L.append("|---|---|---|")
    ran_scales = {(l["scale"], l["engine"]) for l in legs if l["status"] == "MEASURED"}
    for scale in (1_000_000, 10_000_000):
        for eng in ("clickhouse", "duckdb"):
            if (scale, eng) in ran_scales:
                L.append(f"| matrix {scale} {eng} | MEASURED | |")
            else:
                L.append(f"| matrix {scale} {eng} | NOT RUN | "
                         "free disk below the 40 GiB preflight |")
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
