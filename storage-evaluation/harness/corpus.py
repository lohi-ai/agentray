"""Deterministic synthetic corpus + exact oracle manifest.

Generates one seeded Parquet corpus (events, aliases, external_rows, an
ingest batch) and computes the expected answers for every oracle check in
the same pass, so expectations never depend on either engine under test.

Semantics mirrored from production code:
- sessionizer: 30-min inactivity gap, keyed on (project_id, distinct_id),
  arrival order by inserted_at (internal/dataplane/ingest/sessionizer.go).
- identity: anonymous distinct_id -> canonical via aliases
  (dictGetOrDefault in store.go); events without an alias keep distinct_id.
- dedup: InsertID is stored but never deduplicated on read
  (store.go:201-210) — the oracle freezes that: raw counts include dups.
- entities: external_rows ReplacingMergeTree(synced_at); latest synced_at
  wins; there is no delete marker (store.go:1248-1265) so upstream-deleted
  rows stay visible — reported as divergence, not tolerated.
- currency: cost_usd is Float32 (store.go:1121); the oracle sums the
  float32-rounded stored values in float64.
- funnel: windowFunnel over the analysis window, non-strict (equal
  timestamps may chain), canonical-id grouping, humans only
  (store.go:3486-3577).
- retention: weekly brackets from each user's first `user.signup`
  (store.go:3662-3704), humans only.
"""
from __future__ import annotations

import json
import random
import struct
import uuid
from collections import defaultdict
from datetime import datetime, timedelta
from pathlib import Path

import pyarrow as pa
import pyarrow.parquet as pq

from .util import (
    CORPUS_DAYS,
    CORPUS_END,
    FUNNEL_STEPS,
    FUNNEL_WINDOW_S,
    FIRST_EVENT,
    SESSION_WINDOW_S,
    corpus_dir,
    dump_json,
    load_json,
    ts,
    window_start,
)

EVENT_SCHEMA = pa.schema(
    [
        ("project_id", pa.string()),
        ("event_id", pa.string()),
        ("distinct_id", pa.string()),
        ("session_id", pa.string()),
        ("event_name", pa.string()),
        ("event_type", pa.string()),
        ("properties", pa.string()),
        ("agent_id", pa.string()),
        ("tool_name", pa.string()),
        ("tool_input", pa.string()),
        ("tool_output", pa.string()),
        ("tokens_input", pa.uint32()),
        ("tokens_output", pa.uint32()),
        ("cost_usd", pa.float32()),
        ("latency_ms", pa.uint32()),
        ("model_name", pa.string()),
        ("is_error", pa.uint8()),
        ("error_message", pa.string()),
        ("timestamp", pa.timestamp("ms", tz="UTC")),
        ("inserted_at", pa.timestamp("ms", tz="UTC")),
        ("visitor_class", pa.string()),
        ("bot_name", pa.string()),
        ("referrer_host", pa.string()),
        ("referrer_channel", pa.string()),
        ("user_agent", pa.string()),
        ("platform", pa.string()),
        ("insert_id", pa.string()),
        ("is_unplanned", pa.uint8()),
    ]
)

ALIAS_SCHEMA = pa.schema(
    [
        ("project_id", pa.string()),
        ("anonymous_id", pa.string()),
        ("canonical_id", pa.string()),
        ("version", pa.timestamp("ms", tz="UTC")),
    ]
)

ENTITY_SCHEMA = pa.schema(
    [
        ("project_id", pa.string()),
        ("connector_id", pa.string()),
        ("table_name", pa.string()),
        ("row_key", pa.string()),
        ("cursor", pa.string()),
        ("data", pa.string()),
        ("synced_at", pa.timestamp("ms", tz="UTC")),
    ]
)

# (name, weight, event_type). Weights sum to 1.0.
EVENT_NAMES = [
    ("user.pageview", 0.55, "track"),
    ("app.session_ping", 0.20, "track"),
    ("user.signup", 0.03, "track"),
    ("user.conversion", 0.02, "track"),
    ("agent.tool_call", 0.10, "agent"),
    ("order.paid", 0.05, "track"),
    ("user.identify", 0.05, "track"),
]

PLATFORMS = ["web", "web", "web", "ios", "android", "server"]
CHANNELS = ["organic", "organic", "referral", "paid", "social", "email", ""]
MODELS = ["gpt-5-mini", "claude-sonnet-4.5", "gpt-5.2"]
TOOLS = ["run_sql", "explore_events", "create_chart", "search_docs"]

NS = uuid.UUID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")  # DNS namespace


def uid(*parts) -> str:
    return str(uuid.uuid5(NS, ":".join(str(p) for p in parts)))


def f32(x: float) -> float:
    return struct.unpack("f", struct.pack("f", x))[0]


def _prop_blob(rng: random.Random, target: int) -> str:
    return json.dumps({"p": "x" * max(0, target - 10)}, separators=(",", ":"))


def _funnel_level(items: list[tuple[str, int]]) -> int:
    """Greedy earliest-chain funnel, window anchored at each step-1 event.

    Mirrors windowFunnel(86400)(toDateTime(timestamp), s1, s2, s3) without
    strict_increase: non-decreasing timestamps (equal timestamps may chain),
    whole chain inside the window from the anchor. Max level over all
    anchors. `items` is [(name, ts_ms)] sorted by ts.
    """
    import bisect
    s1 = [t for n, t in items if n == FUNNEL_STEPS[0]]
    s2 = [t for n, t in items if n == FUNNEL_STEPS[1]]
    s3 = [t for n, t in items if n == FUNNEL_STEPS[2]]
    best = 0
    for a in s1:
        lvl = 1
        i2 = bisect.bisect_left(s2, a)  # first s2 with t >= a
        t2 = s2[i2] if i2 < len(s2) and s2[i2] <= a + FUNNEL_WINDOW_S * 1000 else None
        if t2 is not None:
            lvl = 2
            i3 = bisect.bisect_left(s3, t2)
            t3 = s3[i3] if i3 < len(s3) and s3[i3] <= a + FUNNEL_WINDOW_S * 1000 else None
            if t3 is not None:
                lvl = 3
        if lvl > best:
            best = lvl
        if best == 3:
            break
    return best


class Oracle:
    """Accumulates expected answers while the corpus is emitted."""

    def __init__(self):
        self.canonical_events_7d = defaultdict(int)
        self.sessions_7d = set()
        self.gap_violations = 0
        self.raw_7d = 0
        self.distinct_ids_7d = set()
        self.late_7d = 0
        self.funnel_levels = [0, 0, 0, 0]  # users at exact level 0..3
        self.retention_base = 0
        self.retention_w1 = 0
        self.retention_immature = 0
        self.entity_current = 0
        self.entity_deleted_keys: list[str] = []
        self.entity_join_cents_7d = 0
        self.currency_7d = 0.0
        self.total_rows = 0

    def note_row(self, ev: dict, canonical: str, order_amount_cents: int | None):
        """Per emitted row (duplicates included — raw counts must see them)."""
        in7 = ev["timestamp"] >= window_start(7)
        self.total_rows += 1
        if in7:
            self.raw_7d += 1
            self.distinct_ids_7d.add(ev["event_id"])
            self.canonical_events_7d[canonical] += 1
            self.sessions_7d.add(ev["session_id"])
            if ev["inserted_at"] - ev["timestamp"] > timedelta(hours=1):
                self.late_7d += 1
            if ev["cost_usd"] is not None:
                self.currency_7d += float(ev["cost_usd"])
            if order_amount_cents is not None:
                self.entity_join_cents_7d += order_amount_cents

    def note_user(self, funnel_level: int, first_signup_ms: int | None,
                  week1: bool, mature: bool):
        self.funnel_levels[funnel_level] += 1
        if first_signup_ms is not None:
            if mature:
                self.retention_base += 1
                if week1:
                    self.retention_w1 += 1
            else:
                self.retention_immature += 1

    def note_session_gaps(self, ts_list: list[int]):
        ts_list.sort()
        self.gap_violations += sum(
            1 for a, b in zip(ts_list, ts_list[1:]) if b - a >= SESSION_WINDOW_S * 1000
        )

    def manifest(self) -> dict:
        top = sorted(self.canonical_events_7d.items(), key=lambda kv: (-kv[1], kv[0]))[:10]
        return {
            "checks": {
                # kind=baseline_parity: both engines must reproduce frozen
                # current behavior (including its limitations). kind=
                # semantic_gate: the target semantics a migration must
                # deliver; parity on a limitation is never a pass.
                "identity.canonical_events_7d": {
                    "kind": "baseline_parity",
                    "total": sum(self.canonical_events_7d.values()),
                    "top10": top,
                },
                "sessionization.sessions_7d": {
                    "kind": "baseline_parity",
                    "distinct_sessions": len(self.sessions_7d),
                },
                "sessionization.gap_violations": {
                    "kind": "baseline_parity",
                    "count": self.gap_violations,
                },
                "dedup.raw_vs_distinct_7d": {
                    "kind": "baseline_parity",
                    "raw": self.raw_7d,
                    "distinct_event_ids": len(self.distinct_ids_7d),
                    "note": "InsertID is stored but never deduplicated on read (store.go:201-210); raw > distinct is frozen production behavior, not an engine bug.",
                },
                "dedup.read_time_guarantee": {
                    "kind": "semantic_gate",
                    "status": "NOT RUN",
                    "note": "Neither engine applies read-time dedup by default and the harness does not layer one on; the measured raw-vs-distinct gap quantifies exposure but is not a dedup guarantee.",
                },
                "late_events.bucket_7d": {
                    "kind": "baseline_parity",
                    "late_gt_1h": self.late_7d,
                },
                "funnel.signup_window_7d": {
                    "kind": "baseline_parity",
                    "steps": FUNNEL_STEPS,
                    "window_s": FUNNEL_WINDOW_S,
                    "level_histogram": self.funnel_levels,
                },
                "retention.week1_mature": {
                    "kind": "baseline_parity",
                    "first_event": FIRST_EVENT,
                    "base": self.retention_base,
                    "week1_users": self.retention_w1,
                    "immature_excluded": self.retention_immature,
                    "note": "Only mature cohorts count: first signup + 14d <= corpus end, so the week-1 bracket is fully observed.",
                },
                "entity.current_rows": {
                    "kind": "baseline_parity",
                    "count": self.entity_current,
                },
                "entity.deleted_still_visible": {
                    "kind": "baseline_parity",
                    "deleted_keys": self.entity_deleted_keys,
                    "expected_visible": len(self.entity_deleted_keys),
                    "note": "external_rows has no delete marker (store.go:1248-1265); upstream-deleted rows remain visible. Parity here documents the limitation.",
                },
                "entity.tombstone_delete": {
                    "kind": "semantic_gate",
                    "deleted_keys": self.entity_deleted_keys,
                    "expected_visible": 0,
                    "note": "Target semantics: a source delete must remove the row. Current schema has no tombstone, so both engines are expected to show the rows — that is a DIVERGENT gate result, not a pass.",
                },
                "entity_join.order_paid_7d": {
                    "kind": "baseline_parity",
                    "amount_cents": self.entity_join_cents_7d,
                },
                "currency.cost_7d": {
                    "kind": "baseline_parity",
                    "sum_usd": self.currency_7d,
                    "note": "cost_usd is Float32 USD agent cost only; parity here is NOT evidence of gross/net billing or mixed-currency correctness.",
                },
                "currency.mixed": {
                    "kind": "semantic_gate",
                    "status": "NOT RUN",
                    "note": "The Event schema carries only cost_usd Float32 — there is no multi-currency field to evaluate. Mixed-currency billing correctness is untestable in this harness.",
                },
            },
            "corpus_days": CORPUS_DAYS,
            "total_rows": self.total_rows,
        }


def _gen_entities(rng: random.Random, scale: int, projects: list[str]):
    """Emit entity rows; returns (rows, latest_by_key, order_pool, deleted_keys)."""
    n_entities = max(100, scale // 10)
    n_updates = int(n_entities * 0.10)
    n_deleted = max(1, int(n_entities * 0.01))
    rows = []
    latest = {}  # row_key -> data dict
    key_proj = {}
    order_pool = defaultdict(list)
    deleted = []
    base = CORPUS_END - timedelta(days=CORPUS_DAYS)
    for i in range(n_entities):
        proj = projects[i % len(projects)]
        key = f"order-{i:08d}"
        amount = rng.randint(500, 500_000)
        data = {"order_id": key, "amount_cents": amount, "status": "paid"}
        synced = base + timedelta(seconds=rng.randint(0, 3600))
        rows.append((proj, key, "v1", data, synced))
        latest[key] = data
        key_proj[key] = proj
        order_pool[proj].append(key)
    keys = list(latest)
    for upd_i in range(n_updates):
        key = keys[rng.randrange(len(keys))]
        data = dict(latest[key])
        data["amount_cents"] = rng.randint(500, 500_000)
        data["status"] = rng.choice(["paid", "refunded", "shipped"])
        # Strictly increasing synced_at per update: ReplacingMergeTree/
        # argMax must deterministically pick the same latest row the oracle
        # recorded (last write wins). Random seconds could collide or
        # reorder two updates to the same key.
        synced = base + timedelta(days=1, seconds=upd_i)
        rows.append((key_proj[key], key, "v2", data, synced))
        latest[key] = data
    for _ in range(n_deleted):
        key = keys[rng.randrange(len(keys))]
        if key not in deleted:
            deleted.append(key)
    entity_rows = [
        {
            "project_id": proj,
            "connector_id": uid("connector", proj),
            "table_name": "orders",
            "row_key": key,
            "cursor": ver,
            "data": json.dumps(data, separators=(",", ":")),
            "synced_at": synced,
        }
        for proj, key, ver, data, synced in rows
    ]
    return entity_rows, latest, order_pool, deleted


def _user_events(rng: random.Random, u_idx: int, canon: str, proj: str,
                 anon_ids: list[str], order_pool, scale: int, n_users: int,
                 start: datetime, span_s: int):
    """One user's events grouped by distinct_id (pre-sessionization)."""
    streams = defaultdict(list)
    n_events = max(1, int(rng.expovariate(n_users / scale)))
    for e_idx in range(n_events):
        name = rng.choices(
            [n for n, _w, _t in EVENT_NAMES],
            [w for _n, w, _t in EVENT_NAMES],
        )[0]
        etype = next(t for n, _w, t in EVENT_NAMES if n == name)
        ts_ev = start + timedelta(seconds=rng.random() * span_s)
        late = rng.random() < 0.05
        inserted = ts_ev + (
            timedelta(hours=1 + rng.random() * 71) if late else timedelta(seconds=rng.random() * 2)
        )
        distinct = canon
        if anon_ids and rng.random() < 0.25:
            distinct = rng.choice(anon_ids)
        is_agent = name == "agent.tool_call"
        is_bot = rng.random() < 0.02
        order_key = None
        if name == "order.paid" and order_pool[proj]:
            order_key = rng.choice(order_pool[proj])
        props = {"order_id": order_key} if order_key else {}
        props["pad"] = "x" * rng.randint(200, 900)
        if is_agent and rng.random() < 0.05:
            props["pad"] = "x" * 4096
        err = is_agent and rng.random() < 0.03
        ev = {
            "project_id": proj,
            "event_id": uid("ev", u_idx, e_idx),
            "distinct_id": distinct,
            "session_id": "",  # filled by sessionize
            "event_name": name,
            "event_type": etype,
            "properties": json.dumps(props, separators=(",", ":")),
            "agent_id": uid("agent", u_idx % 97) if is_agent else None,
            "tool_name": rng.choice(TOOLS) if is_agent else None,
            "tool_input": _prop_blob(rng, 600) if is_agent else None,
            "tool_output": _prop_blob(rng, 900) if is_agent else None,
            "tokens_input": rng.randint(50, 4000) if is_agent else None,
            "tokens_output": rng.randint(20, 2000) if is_agent else None,
            "cost_usd": f32(rng.random() * 0.05) if is_agent else None,
            "latency_ms": rng.randint(80, 9000) if is_agent else None,
            "model_name": rng.choice(MODELS) if is_agent else None,
            "is_error": 1 if err else 0,
            "error_message": "tool timeout" if err else None,
            "timestamp": ts_ev,
            "inserted_at": inserted,
            "visitor_class": "bot" if is_bot else "human",
            "bot_name": "evalbot" if is_bot else None,
            "referrer_host": "example.com" if rng.random() < 0.3 else None,
            "referrer_channel": rng.choice(CHANNELS),
            "user_agent": "eval-ua/1.0",
            "platform": rng.choice(PLATFORMS),
            "insert_id": uid("ins", u_idx, e_idx) if rng.random() < 0.02 else None,
            "is_unplanned": 0,
            "_canonical": canon,
            "_order_key": order_key,
        }
        streams[distinct].append(ev)
    return streams


def generate(scale: int, seed: int, ingest_rows: int = 10_000) -> Path:
    """Generate corpus + oracle manifest. Returns the corpus directory."""
    out = corpus_dir(scale, seed)
    manifest_path = out / "oracle.json"
    if manifest_path.exists():
        # Reuse only a corpus generated under identical parameters — a
        # stale ingest_rows would silently mis-size the ingest leg.
        m = load_json(manifest_path)
        if (m.get("scale"), m.get("seed"), m.get("ingest_rows")) == (
                scale, seed, ingest_rows):
            return out
        raise RuntimeError(
            f"corpus dir {out} exists with different parameters "
            f"(scale={m.get('scale')}, seed={m.get('seed')}, "
            f"ingest_rows={m.get('ingest_rows')}); remove it to regenerate")
    out.mkdir(parents=True, exist_ok=True)
    rng = random.Random(seed)
    projects = [uid("project", i) for i in range(3)]
    proj_weight = [0.6, 0.3, 0.1]
    n_users = max(50, scale // 20)

    entity_rows, latest_entities, order_pool, deleted_keys = _gen_entities(rng, scale, projects)
    oracle = Oracle()
    oracle.entity_current = len(latest_entities)
    oracle.entity_deleted_keys = sorted(deleted_keys)

    span_s = CORPUS_DAYS * 86400
    start = CORPUS_END - timedelta(days=CORPUS_DAYS)
    events_path = out / "events.parquet"
    writer = pq.ParquetWriter(events_path, EVENT_SCHEMA, compression="zstd")
    buf: list[dict] = []
    seq = 0
    aliases = []

    def flush():
        if buf:
            writer.write_table(pa.Table.from_pylist(buf, schema=EVENT_SCHEMA))
            buf.clear()

    for u_idx in range(n_users):
        canon = uid("user", u_idx)
        proj = rng.choices(projects, proj_weight)[0]
        anon_ids = []
        if rng.random() < 0.5:
            anon_ids.append(uid("anon", u_idx, 0))
        if rng.random() < 0.1:
            anon_ids.append(uid("anon", u_idx, 1))
        for a in anon_ids:
            aliases.append(
                {"project_id": proj, "anonymous_id": a, "canonical_id": canon, "version": start}
            )
        streams = _user_events(rng, u_idx, canon, proj, anon_ids, order_pool,
                               scale, n_users, start, span_s)

        # Per-user oracle contributions computed in event-time order.
        # Funnel mirrors the engine query: only events inside the 7-day
        # window participate (windowFunnel sees nothing outside it).
        funnel_items = []
        human_ts = []
        first_signup_ms = None
        w7_ms = int(window_start(7).timestamp() * 1000)
        for distinct, evs in streams.items():
            for ev in evs:
                if ev["visitor_class"] != "human":
                    continue
                t_ms = int(ev["timestamp"].timestamp() * 1000)
                human_ts.append(t_ms)
                if ev["event_name"] in FUNNEL_STEPS and t_ms >= w7_ms:
                    funnel_items.append((ev["event_name"], t_ms))
                if ev["event_name"] == FIRST_EVENT and (
                    first_signup_ms is None or t_ms < first_signup_ms
                ):
                    first_signup_ms = t_ms
        funnel_items.sort(key=lambda nt: nt[1])
        week1 = (
            first_signup_ms is not None
            and any(first_signup_ms + 7 * 86400_000 <= t < first_signup_ms + 14 * 86400_000
                    for t in human_ts)
        )
        # Mature cohort: the week-1 bracket [first+7d, first+14d) is fully
        # observed only when first+14d <= corpus end.
        mature = (first_signup_ms is not None
                  and first_signup_ms + 14 * 86400_000
                      <= int(CORPUS_END.timestamp() * 1000))
        oracle.note_user(_funnel_level(funnel_items), first_signup_ms, week1,
                         mature)

        # Sessionize each (project, distinct_id) stream in arrival order —
        # exactly the sessionizer's view — then emit rows + 2% duplicates.
        for distinct, evs in streams.items():
            evs.sort(key=lambda e: (e["inserted_at"], e["event_id"]))
            last_seen = None
            session_id = ""
            session_events = defaultdict(list)
            for ev in evs:
                t = ev["timestamp"]
                if last_seen is not None and abs((t - last_seen).total_seconds()) < SESSION_WINDOW_S:
                    if t > last_seen:
                        last_seen = t
                else:
                    session_id = uid("sess", proj, distinct, seq)
                    seq += 1
                    last_seen = t
                ev["session_id"] = session_id
                session_events[session_id].append(int(t.timestamp() * 1000))
                canon_ev = ev.pop("_canonical")
                order_key = ev.pop("_order_key")
                amount = None
                if order_key is not None and order_key in latest_entities:
                    amount = latest_entities[order_key]["amount_cents"]
                oracle.note_row(ev, canon_ev, amount)
                buf.append(ev)
                if rng.random() < 0.02:
                    dup = dict(ev)
                    dup["inserted_at"] = ev["inserted_at"] + timedelta(seconds=rng.randint(1, 300))
                    dup["insert_id"] = dup["insert_id"] or uid("insdup", ev["event_id"])
                    oracle.note_row(dup, canon_ev, amount)
                    buf.append(dup)
            for tss in session_events.values():
                oracle.note_session_gaps(tss)
            if len(buf) >= 50_000:
                flush()
    flush()
    writer.close()

    pq.write_table(pa.Table.from_pylist(aliases, schema=ALIAS_SCHEMA),
                   out / "aliases.parquet", compression="zstd")
    pq.write_table(pa.Table.from_pylist(entity_rows, schema=ENTITY_SCHEMA),
                   out / "external_rows.parquet", compression="zstd")

    # Ingest batch: fresh events near CORPUS_END, no duplicates, used for the
    # ingest-during-reads leg. Loaded after oracle checks.
    batch = []
    for i in range(ingest_rows):
        canon = uid("user", rng.randrange(n_users))
        proj = rng.choices(projects, proj_weight)[0]
        ts_ev = CORPUS_END - timedelta(seconds=rng.randint(1, 3600))
        batch.append(
            {
                "project_id": proj,
                "event_id": uid("ingest", i),
                "distinct_id": canon,
                "session_id": uid("isess", i),
                "event_name": "app.session_ping",
                "event_type": "track",
                "properties": json.dumps({"pad": "x" * 300}),
                "agent_id": None, "tool_name": None, "tool_input": None, "tool_output": None,
                "tokens_input": None, "tokens_output": None, "cost_usd": None,
                "latency_ms": None, "model_name": None,
                "is_error": 0, "error_message": None,
                "timestamp": ts_ev,
                "inserted_at": ts_ev + timedelta(seconds=1),
                "visitor_class": "human", "bot_name": None,
                "referrer_host": None, "referrer_channel": "",
                "user_agent": "eval-ua/1.0", "platform": "web",
                "insert_id": uid("ingest-ins", i), "is_unplanned": 0,
            }
        )
    pq.write_table(pa.Table.from_pylist(batch, schema=EVENT_SCHEMA),
                   out / "ingest_batch.parquet", compression="zstd")

    manifest = oracle.manifest()
    manifest["scale"] = scale
    manifest["seed"] = seed
    manifest["projects"] = projects
    manifest["ingest_rows"] = ingest_rows
    manifest["synthetic"] = True
    dump_json(out / "oracle.json", manifest)
    return out
