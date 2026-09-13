"""eval CLI: doctor | corpus | smoke | matrix | report.

Runs inside the driver container (512 MiB / 1 CPU). All engine containers
are siblings on storage-eval-net.
"""
from __future__ import annotations

import argparse
import hashlib
import json
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
    load_json,
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
    out = corpus_mod.generate(args.scale, args.seed, caps["ingest_rows"])
    print(f"corpus: {out}")
    return 0


def _run_legs(legs_spec, caps, tag):
    problems = _preflight(caps)
    if problems:
        for p in problems:
            print(f"PREFLIGHT FAIL: {p}", file=sys.stderr)
        return 1
    # Persist the requested run contract: the report's gate ledger and the
    # published run-metadata can then show what was asked for, not only
    # what completed.
    dump_json(WORK / "requested.json", {
        "tag": tag, "requested_at": time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        # Free disk observed at request time — the report's NOT RUN reason
        # must quote the preflight the run actually saw, not whatever the
        # disk happens to hold at render time.
        "preflight_free_gib": round(free_gib(WORK), 1),
        "legs": legs_spec,
        "caps": caps,
    })
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
    # Defaults come from the declared coverage dimensions in CAPS so the
    # report's gate ledger and this command can never disagree about what
    # "the matrix" is.
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
    dest = Path(__file__).resolve().parent.parent / "results"
    if not any(RESULTS.glob("*.json")) and any(dest.glob("*.json")):
        # No legs in work/results but committed evidence exists — this
        # publish would archive the real evidence and replace it with an
        # empty report. Refuse; the report itself was still written.
        print("publish skipped: no legs in work/results but results/ "
              "holds evidence; refusing to archive it for an empty run",
              file=sys.stderr)
        return 1
    _publish_durable()
    return 0


def _archive_prior(dest: Path, incoming: dict[str, bytes]) -> Path | None:
    """Move prior published outputs into a content-addressed archive dir.

    `incoming` maps name -> bytes of the files about to be published; only
    files that would actually change or disappear are archived — republishing
    identical results is a no-op, not a duplicate archive. The archive dir is
    named by a digest of the preserved content: if that exact evidence set is
    already archived, the prior files are simply removed instead of creating
    a second copy.
    Returns the archive dir used, or None when nothing needed preserving."""
    prior = [p for p in list(dest.glob("*.json")) + [dest / "report.md"]
             if p.exists()]
    prior = [p for p in prior
             if incoming.get(p.name) != p.read_bytes()]
    if not prior:
        return None
    h = hashlib.sha256()
    for p in sorted(prior, key=lambda x: x.name):
        h.update(p.name.encode())
        h.update(p.read_bytes())
    tag = h.hexdigest()[:12]
    # Content-addressed: the same evidence set is archived exactly once.
    existing = list((dest / "archive").glob(f"*-{tag}"))
    if existing:
        for old in prior:
            old.unlink()
        return existing[0]
    base = dest / "archive" / (
        f"{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}-{tag}")
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
    meta = {
        "generated_by": "storage-evaluation harness",
        "duckdb": DUCKDB_VERSION,
        "clickhouse_image": CH_IMAGE,
        # The driver contract: which image ran the harness and under what
        # envelope. Env vars are set by the eval wrapper; absent means the
        # publish ran outside it — record null, never a guessed tag.
        "driver_image": os.environ.get("EVAL_DRIVER_IMAGE"),
        "driver_limits": {
            "mem": os.environ.get("EVAL_DRIVER_MEM") or "unknown",
            "cpus": os.environ.get("EVAL_DRIVER_CPUS") or "unknown",
        },
        "code_digest": None,
        "legs": [],
    }
    requested = WORK / "requested.json"
    if requested.exists():
        meta["requested"] = load_json(requested)
    digests = set()
    for p in sorted(RESULTS.glob("*.json")):
        leg = load_json(p)
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
            "limits": prov.get("limits", "unknown"),
            "driver_image": prov.get("driver_image"),
            "teardown": leg.get("teardown"),
        })
        # corpus_rows is executed provenance only: recorded by the leg at
        # run time. Legs that predate the field report "unknown" — the
        # current on-disk oracle is never grafted onto old evidence.
        meta["legs"][-1]["corpus_rows"] = prov.get("corpus_rows", "unknown")
    mixed = len(digests) > 1
    meta["code_digest"] = "MIXED" if mixed else (digests.pop() if digests else None)
    # The slim driver image has no git; the eval wrapper passes the commit in.
    # Prefer the legs' own code_commit — the commit that produced the
    # evidence — over the publish-time HEAD, so republishing unchanged
    # results stays byte-identical.
    commits = {l.get("code_commit") for l in meta["legs"]
               if l.get("code_commit")}
    meta["commit"] = (commits.pop() if len(commits) == 1
                      else ("MIXED" if commits else
                            os.environ.get("EVAL_COMMIT") or None))
    if mixed:
        meta["provenance_note"] = (
            "legs were produced by different code states; per-leg "
            "code_digest/corpus_digest identify each")

    # Stage the full incoming set in memory so archiving can compare
    # content, not just names: unchanged files are left in place and only
    # genuinely superseded evidence is preserved under archive/.
    incoming = {}
    for p in sorted(RESULTS.glob("*.json")):
        incoming[p.name] = p.read_bytes()
    report_path = WORK / "report.md"
    if report_path.exists():
        incoming["report.md"] = report_path.read_bytes()
    # Serialized identically to dump_json so byte comparison is meaningful.
    incoming["run-metadata.json"] = json.dumps(
        meta, indent=2, sort_keys=True, default=str).encode()
    _archive_prior(dest, incoming)
    for name, content in incoming.items():
        cur = dest / name
        if not cur.exists() or cur.read_bytes() != content:
            cur.write_bytes(content)


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
