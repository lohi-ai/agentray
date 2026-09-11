"""eval CLI: doctor | corpus | smoke | matrix | report.

Runs inside the driver container (512 MiB / 1 CPU). All engine containers
are siblings on storage-eval-net.
"""
from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path

from . import corpus as corpus_mod
from . import engines, report
from .util import (
    CAPS,
    CH_IMAGE,
    DRIVER_IMAGE,
    DUCKDB_VERSION,
    RESULTS,
    WORK,
    dir_bytes,
    dump_json,
    free_gib,
)


def _preflight(caps: dict) -> list[str]:
    problems = []
    free = free_gib(WORK)
    if free < caps["preflight_free_gib"]:
        problems.append(
            f"free disk {free:.1f} GiB < preflight {caps['preflight_free_gib']} GiB")
    used = dir_bytes(WORK) / 1024**3
    if used > caps["workdir_gib"]:
        problems.append(
            f"workdir {used:.1f} GiB exceeds cap {caps['workdir_gib']} GiB")
    return problems


def cmd_doctor(_args):
    ok = True
    try:
        cli = engines.client()
        ver = cli.version()["Version"]
        print(f"docker: {ver}")
    except Exception as e:
        print(f"docker: UNAVAILABLE ({e})")
        ok = False
    for img in (DRIVER_IMAGE, CH_IMAGE):
        try:
            engines.client().images.get(img)
            print(f"image: {img.split('@')[0]} present")
        except Exception:
            print(f"image: {img.split('@')[0]} MISSING")
            ok = False
    print(f"duckdb pin: {DUCKDB_VERSION}")
    print(f"free disk: {free_gib(WORK):.1f} GiB")
    print(f"workdir used: {dir_bytes(WORK) / 1024**3:.2f} GiB")
    return 0 if ok else 1


def cmd_corpus(args):
    for p in _preflight(CAPS["smoke"]):
        print(f"PREFLIGHT FAIL: {p}", file=sys.stderr)
        return 1
    out = corpus_mod.generate(args.scale, args.seed)
    print(f"corpus: {out}")
    return 0


def _run_legs(legs_spec, caps, tag):
    problems = _preflight(caps)
    if problems:
        for p in problems:
            print(f"PREFLIGHT FAIL: {p}", file=sys.stderr)
        return 1
    rc = 0
    for spec in legs_spec:
        name = (f"{tag}-{spec['engine']}-s{spec['scale']}"
                f"-r{spec['readers']}-d{spec['days']}")
        print(f"=== leg {name} ===", flush=True)
        try:
            leg = _timed_leg(spec, caps)
        except Exception as e:
            leg = {"engine": spec["engine"], "scale": spec["scale"],
                   "readers": spec["readers"], "days": spec["days"],
                   "status": "ERROR", "error": f"{type(e).__name__}: {e}",
                   "checks": {}, "shapes": {}, "ingest": {}, "resources": {}}
            rc = 1
        dump_json(RESULTS / f"{name}.json", leg)
        print(f"    -> {leg['status']} wall={leg.get('wall_s','?')}s", flush=True)
        if leg["status"] != "MEASURED":
            rc = 1
    return rc


def _timed_leg(spec, caps):
    from .runner import run_leg
    return run_leg(spec["engine"], spec["scale"], spec["seed"],
                   spec["readers"], spec["days"], caps, caps["wall_s"])


def cmd_smoke(args):
    caps = CAPS["smoke"]
    legs = [
        {"engine": "clickhouse", "scale": caps["scale"], "seed": args.seed,
         "readers": caps["readers"][0], "days": caps["days"][0]},
        {"engine": "duckdb", "scale": caps["scale"], "seed": args.seed,
         "readers": caps["readers"][0], "days": caps["days"][0]},
    ]
    return _run_legs(legs, caps, "smoke")


def cmd_matrix(args):
    caps = CAPS["matrix"]
    scales = [int(s) for s in args.scales.split(",")]
    readers = [int(r) for r in args.readers.split(",")]
    days = [int(d) for d in args.days.split(",")]
    legs = [
        {"engine": eng, "scale": s, "seed": args.seed, "readers": r, "days": d}
        for eng in ("clickhouse", "duckdb")
        for s in scales for r in readers for d in days
    ]
    return _run_legs(legs, caps, "matrix")


def cmd_report(args):
    out = report.write_report(args.require_labeled_gates)
    print(f"report: {out}")
    _publish_durable()
    return 0


def _publish_durable():
    """Copy report + leg results + run metadata into the committed
    storage-evaluation/results/ directory — the reviewable deliverable,
    not just the gitignored work/ scratch."""
    import shutil
    dest = Path(__file__).resolve().parent.parent / "results"
    dest.mkdir(exist_ok=True)
    meta = {
        "generated_by": "storage-evaluation harness",
        "duckdb": DUCKDB_VERSION,
        "clickhouse_image": CH_IMAGE,
        "legs": [],
    }
    for p in sorted(RESULTS.glob("*.json")):
        leg = report.load_json(p)
        shutil.copy2(p, dest / p.name)
        meta["legs"].append({
            "file": p.name, "engine": leg.get("engine"),
            "scale": leg.get("scale"), "seed": leg.get("seed"),
            "readers": leg.get("readers"), "days": leg.get("days"),
            "status": leg.get("status"),
            "corpus_rows": None,
        })
        cd = report.corpus_dir(leg.get("scale", 0), leg.get("seed", 0))
        oj = cd / "oracle.json"
        if oj.exists():
            meta["legs"][-1]["corpus_rows"] = report.load_json(oj).get("total_rows")
    # The slim driver image has no git; the eval wrapper passes the commit in.
    meta["commit"] = os.environ.get("EVAL_COMMIT") or None
    if (WORK / "report.md").exists():
        shutil.copy2(WORK / "report.md", dest / "report.md")
    dump_json(dest / "run-metadata.json", meta)


def main(argv=None):
    p = argparse.ArgumentParser(prog="eval")
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("doctor")
    c = sub.add_parser("corpus")
    c.add_argument("--scale", type=int, default=100_000)
    c.add_argument("--seed", type=int, default=20260912)
    s = sub.add_parser("smoke")
    s.add_argument("--seed", type=int, default=20260912)
    m = sub.add_parser("matrix")
    m.add_argument("--scales", default="1000000,10000000")
    m.add_argument("--readers", default="1,5,20")
    m.add_argument("--days", default="7,30,90")
    m.add_argument("--seed", type=int, default=20260912)
    r = sub.add_parser("report")
    r.add_argument("--require-labeled-gates", action="store_true")
    args = p.parse_args(argv)
    return {
        "doctor": cmd_doctor, "corpus": cmd_corpus, "smoke": cmd_smoke,
        "matrix": cmd_matrix, "report": cmd_report,
    }[args.cmd](args)


if __name__ == "__main__":
    sys.exit(main())
