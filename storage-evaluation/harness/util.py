"""Shared constants and helpers for the storage evaluation harness.

Everything here is local-only: synthetic data, pinned containers, no
production access. Paths resolve to the host-absolute storage-evaluation/
directory (EVAL_HOST_DIR) so bind-mount sources the driver hands to the
Docker daemon resolve on the host, not inside the driver container.
"""
from __future__ import annotations

import json
import os
import shutil
from datetime import datetime, timedelta, timezone
from pathlib import Path

UTC = timezone.utc

# Fixed corpus end: every run compares identical windows regardless of the
# wall clock, so oracle expectations are reproducible across machines.
CORPUS_END = datetime(2026, 9, 1, tzinfo=UTC)
CORPUS_DAYS = 90
SESSION_WINDOW_S = 30 * 60  # mirrors internal/dataplane/ingest/sessionizer.go
FUNNEL_STEPS = ["user.pageview", "user.signup", "user.conversion"]  # store.go defaults
FUNNEL_WINDOW_S = 86400  # production default when no range is given
FIRST_EVENT = "user.signup"  # retention cohort anchor, mirrors store.go retention()

HOST_DIR = Path(os.environ.get("EVAL_HOST_DIR") or Path(__file__).resolve().parent.parent)
WORK = HOST_DIR / "work"
RESULTS = WORK / "results"

# The eval wrapper builds/tags the driver image with a requirements+
# Dockerfile fingerprint and passes it in; the fallback is the plain tag.
DRIVER_IMAGE = os.environ.get("EVAL_DRIVER_IMAGE", "storage-eval-driver:py3.13")
# Prod pin from infra/gce/infra/docker-compose.yml (24.12-alpine), resolved to
# the multi-arch digest recorded in plan.md.
CH_IMAGE = (
    "clickhouse/clickhouse-server:24.12-alpine"
    "@sha256:cd450891db46cc6ffe313ca2b0fb7dbfb897a6873ca74a724cbe050a2cf62621"
)
DUCKDB_VERSION = "1.5.5"
NETWORK = "storage-eval-net"
CH_NAME = "storage-eval-ch"
DUCK_NAME = "storage-eval-duck"
CH_HTTP_PORT = 8123
DUCK_PORT = 8710

# Approved resource envelope. Smoke caps are the foreman-approved bound;
# matrix caps are plan.md's, gated on the 40 GiB free-disk preflight.
CAPS = {
    "smoke": {
        "scale": 100_000,
        "readers": [1],
        "days": [7],
        "engine_mem": "2g",
        "engine_cpus": 2,
        "workdir_gib": 8,
        "wall_s": 15 * 60,
        "preflight_free_gib": 10,
        "ingest_rows": 10_000,
    },
    "matrix": {
        "scales": [1_000_000, 10_000_000],
        "engine_mem": "2g",
        "engine_cpus": 2,
        "workdir_gib": 24,
        "wall_s": 6 * 3600,
        "preflight_free_gib": 40,
        "ingest_rows": 10_000,
    },
}

# Gates the harness cannot exercise locally. They stay NOT RUN in the report
# until separately exercised; report --require-labeled-gates enforces labels.
UNRUN_GATES = [
    ("crash_replay", "commit-before-ack crash/replay", "needs fault injection against a durable queue, not a benchmark"),
    ("backup_restore", "backup checkpoint + restore + replay", "needs a defined backup/restore procedure to exercise"),
    ("tenant_sql_isolation", "cross-tenant / arbitrary-SQL isolation", "needs the project-isolated sandboxed execution env from strategy.md; this harness's docker-socket driver is trusted tooling, not that sandbox"),
    ("query_cancellation", "query cancellation under load", "not exercised in this harness"),
]


def ts(dt: datetime) -> str:
    """SQL literal for a tz-aware datetime at millisecond precision."""
    return dt.astimezone(UTC).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]


def window_start(days: int) -> datetime:
    return CORPUS_END - timedelta(days=days)


def dir_bytes(path: Path) -> int:
    total = 0
    for root, _dirs, files in os.walk(path):
        for f in files:
            try:
                total += (Path(root) / f).stat().st_size
            except OSError:
                pass
    return total


def free_gib(path: Path) -> float:
    return shutil.disk_usage(path).free / (1024 ** 3)


def dump_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj, indent=2, sort_keys=True, default=str))


def load_json(path: Path):
    return json.loads(path.read_text())


def corpus_dir(scale: int, seed: int) -> Path:
    return WORK / f"corpus-{scale}-s{seed}"


def code_digest() -> str:
    """SHA-256 over the harness source + build inputs — identifies the exact
    code that ran a leg, including uncommitted edits."""
    import hashlib
    h = hashlib.sha256()
    for p in sorted(HOST_DIR.glob("harness/*.py")) + [
            HOST_DIR / "eval", HOST_DIR / "Dockerfile",
            HOST_DIR / "requirements.txt"]:
        if p.exists():
            h.update(p.name.encode())
            h.update(p.read_bytes())
    return h.hexdigest()


def corpus_digest(cdir: Path) -> str:
    """SHA-256 over the corpus files + oracle manifest for a leg."""
    import hashlib
    h = hashlib.sha256()
    for name in ("events.parquet", "aliases.parquet", "external_rows.parquet",
                 "ingest_batch.parquet", "oracle.json"):
        p = cdir / name
        if p.exists():
            h.update(name.encode())
            h.update(p.read_bytes())
    return h.hexdigest()
