"""eval CLI: doctor | corpus | smoke | matrix | report.

Runs inside the driver container (512 MiB / 1 CPU). All engine containers
are siblings on storage-eval-net.
"""
from __future__ import annotations

import argparse
import os
import shutil
import sys
import time
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
    # Preflight against the envelope the requested scale will actually run
    # under — a matrix-size corpus must not pass on smoke's 10 GiB floor.
    caps = CAPS["matrix"] if args.scale > CAPS["smoke"]["scale"] else CAPS["smoke"]
    for p in _preflight(caps):
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


def _archive_prior(dest: Path) -> Path | None:
    """Move prior published outputs into a unique per-publish archive dir.
    Successive publications never overwrite each other's evidence; returns
    the archive dir used, or None when there was nothing to preserve."""
    prior = [p for p in list(dest.glob("*.json")) + [dest / "report.md"]
             if p.exists()]
    if not prior:
        return None
    base = dest / "archive" / time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    target = base
    n = 1
    while target.exists():
        n += 1
        target = base.with_name(f"{base.name}-{n}")
    target.mkdir(parents=True)
    for old in prior:
        shutil.move(str(old), target / old.name)
    return target


def _publish_durable():
    """Copy report + leg results + run metadata into the committed
    storage-evaluation/results/ directory — the reviewable deliverable,
    not just the gitignored work/ scratch."""
    dest = Path(__file__).resolve().parent.parent / "results"
    dest.mkdir(exist_ok=True)
    # Preserve prior outputs under a unique per-publish archive dir —
    # successive publications never overwrite each other's evidence.
    _archive_prior(dest)
    meta = {
        "generated_by": "storage-evaluation harness",
        "duckdb": DUCKDB_VERSION,
        "clickhouse_image": CH_IMAGE,
        "code_digest": None,
        "legs": [],
    }
    digests = set()
    for p in sorted(RESULTS.glob("*.json")):
        leg = report.load_json(p)
        shutil.copy2(p, dest / p.name)
        prov = leg.get("provenance", {})
        digests.add(prov.get("code_digest"))
        meta["legs"].append({
            "file": p.name, "engine": leg.get("engine"),
            "scale": leg.get("scale"), "seed": leg.get("seed"),
            "readers": leg.get("readers"), "days": leg.get("days"),
            "status": leg.get("status"),
            "corpus_rows": None,
            "code_commit": prov.get("code_commit"),
            "code_digest": prov.get("code_digest"),
            "corpus_digest": prov.get("corpus_digest"),
            "teardown": leg.get("teardown"),
        })
        # corpus_rows is executed provenance only: recorded by the leg at
        # run time. Legs that predate the field report "unknown" — the
        # current on-disk oracle is never grafted onto old evidence.
        meta["legs"][-1]["corpus_rows"] = prov.get("corpus_rows", "unknown")
    mixed = len(digests) > 1
    meta["code_digest"] = "MIXED" if mixed else (digests.pop() if digests else None)
    if mixed:
        meta["provenance_note"] = (
            "legs were produced by different code states; per-leg "
            "code_digest/corpus_digest identify each")
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
