"""Structured query registry — the only operations engines accept.

The DuckDB worker resolves these IDs to SQL internally; the driver never
sends raw SQL to it (strategy.md: arbitrary SQL stays out of the writer
process). ClickHouse is a server, so the same IDs map to CH SQL sent over
HTTP by the driver.

{W7} / {W30} / {W90} / {WND} placeholders are SQL timestamp literals for the
window start; {END} is the corpus end. {DELETED_KEYS} is a quoted CSV list.
"""
from __future__ import annotations

from datetime import timedelta

from .util import CORPUS_END, FUNNEL_STEPS, FIRST_EVENT, ts, window_start

# --- shared view definitions -------------------------------------------------
# canonical_id resolution: CH uses the aliases_dict dictionary exactly like
# production (dictGetOrDefault); DuckDB uses a LEFT JOIN on the aliases table.

CH_VIEWS = [
    """CREATE OR REPLACE VIEW v_events AS
       SELECT e.*,
              dictGetOrDefault('eval.aliases_dict', 'canonical_id',
                               (project_id, distinct_id), distinct_id) AS canonical_id
       FROM events e""",
]

DUCK_VIEWS = [
    """CREATE OR REPLACE VIEW v_events AS
       SELECT e.*,
              coalesce(a.canonical_id, e.distinct_id) AS canonical_id
       FROM events e
       LEFT JOIN aliases a
         ON a.project_id = e.project_id AND a.anonymous_id = e.distinct_id""",
]

_FUNNEL_CH = f"""SELECT level, count() AS c FROM (
    SELECT windowFunnel(86400)(toDateTime(timestamp),
      event_name = '{FUNNEL_STEPS[0]}',
      event_name = '{FUNNEL_STEPS[1]}',
      event_name = '{FUNNEL_STEPS[2]}') AS level
    FROM v_events
    WHERE timestamp >= {{WIN}} AND visitor_class = 'human'
      AND event_name IN ('{FUNNEL_STEPS[0]}','{FUNNEL_STEPS[1]}','{FUNNEL_STEPS[2]}')
    GROUP BY canonical_id)
  GROUP BY level ORDER BY level"""

_FUNNEL_DUCK = f"""WITH ev AS (
    SELECT canonical_id, event_name, timestamp FROM v_events
    WHERE timestamp >= {{WIN}} AND visitor_class = 'human'
      AND event_name IN ('{FUNNEL_STEPS[0]}','{FUNNEL_STEPS[1]}','{FUNNEL_STEPS[2]}')),
  anchors AS (
    SELECT canonical_id, timestamp AS t1 FROM ev
    WHERE event_name = '{FUNNEL_STEPS[0]}'),
  chains AS (
    SELECT a.canonical_id, a.t1,
      (SELECT min(timestamp) FROM ev e2
        WHERE e2.canonical_id = a.canonical_id
          AND e2.event_name = '{FUNNEL_STEPS[1]}'
          AND e2.timestamp > a.t1
          AND e2.timestamp <= a.t1 + INTERVAL '86400 seconds') AS t2
    FROM anchors a),
  lvl AS (
    SELECT canonical_id,
      max(CASE WHEN t2 IS NULL THEN 1
          WHEN (SELECT min(e3.timestamp) FROM ev e3
                 WHERE e3.canonical_id = chains.canonical_id
                   AND e3.event_name = '{FUNNEL_STEPS[2]}'
                   AND e3.timestamp > chains.t2
                   AND e3.timestamp <= chains.t1 + INTERVAL '86400 seconds')
                   IS NOT NULL THEN 3 ELSE 2 END) AS level
    FROM chains GROUP BY canonical_id)
  SELECT level, count(*) AS c FROM lvl GROUP BY level ORDER BY level"""

_RETENTION_CH = f"""WITH firsts AS (
    SELECT canonical_id, min(timestamp) AS first_ts
    FROM v_events
    WHERE event_name = '{FIRST_EVENT}' AND visitor_class = 'human'
    GROUP BY canonical_id
    HAVING first_ts <= {{MATURE14}})
  SELECT (SELECT count() FROM firsts) AS base,
         uniqExact(e.canonical_id) AS w1
  FROM v_events e
  INNER JOIN firsts f ON e.canonical_id = f.canonical_id
  WHERE e.visitor_class = 'human'
    AND e.timestamp >= f.first_ts + INTERVAL 168 HOUR
    AND e.timestamp <  f.first_ts + INTERVAL 336 HOUR"""

_RETENTION_DUCK = f"""WITH firsts AS (
    SELECT canonical_id, min(timestamp) AS first_ts
    FROM v_events
    WHERE event_name = '{FIRST_EVENT}' AND visitor_class = 'human'
    GROUP BY canonical_id
    HAVING first_ts <= {{MATURE14}})
  SELECT (SELECT count(*) FROM firsts) AS base,
         count(DISTINCT e.canonical_id) AS w1
  FROM v_events e
  INNER JOIN firsts f ON e.canonical_id = f.canonical_id
  WHERE e.visitor_class = 'human'
    AND e.timestamp >= f.first_ts + INTERVAL '168 hours'
    AND e.timestamp <  f.first_ts + INTERVAL '336 hours'"""

_DELETED_CH = """SELECT count() AS c FROM (
    SELECT row_key FROM external_rows FINAL GROUP BY row_key)
  WHERE row_key IN ({DELETED_KEYS})"""

_DELETED_DUCK = """SELECT count(*) AS c FROM (
    SELECT DISTINCT row_key FROM external_rows)
  WHERE row_key IN ({DELETED_KEYS})"""

_ENTITY_JOIN_CH = """SELECT count() AS n,
    sum(toInt64OrZero(JSONExtractString(latest.data, 'amount_cents'))) AS amount_cents
  FROM events e
  INNER JOIN (
    SELECT project_id, row_key, argMax(data, synced_at) AS data
    FROM external_rows GROUP BY project_id, row_key) latest
    ON latest.project_id = e.project_id
   AND latest.row_key = JSONExtractString(e.properties, 'order_id')
  WHERE e.event_name = 'order.paid' AND e.timestamp >= {WIN}"""

_ENTITY_JOIN_DUCK = """SELECT count(*) AS n,
    sum(cast(json_extract_string(latest.data, '$.amount_cents') AS BIGINT)) AS amount_cents
  FROM events e
  INNER JOIN (
    SELECT project_id, row_key, arg_max(data, synced_at) AS data
    FROM external_rows GROUP BY project_id, row_key) latest
    ON latest.project_id = e.project_id
   AND latest.row_key = json_extract_string(e.properties, '$.order_id')
  WHERE e.event_name = 'order.paid' AND e.timestamp >= {WIN}"""

# --- oracle check queries ----------------------------------------------------
# id -> {"ch": sql, "duck": sql}. {WIN} renders to the 7-day window start for
# checks (the approved smoke window).

CHECKS = {
    "identity.canonical_events_7d": {
        "ch": """SELECT canonical_id, count() AS c FROM v_events
                 WHERE timestamp >= {WIN} GROUP BY canonical_id
                 ORDER BY c DESC, canonical_id LIMIT 10""",
        "duck": """SELECT canonical_id, count(*) AS c FROM v_events
                   WHERE timestamp >= {WIN} GROUP BY canonical_id
                   ORDER BY c DESC, canonical_id LIMIT 10""",
    },
    "identity.canonical_total_7d": {
        "ch": "SELECT count() AS c FROM v_events WHERE timestamp >= {WIN}",
        "duck": "SELECT count(*) AS c FROM v_events WHERE timestamp >= {WIN}",
    },
    "sessionization.sessions_7d": {
        "ch": "SELECT uniqExact(session_id) AS c FROM events WHERE timestamp >= {WIN}",
        "duck": "SELECT count(DISTINCT session_id) AS c FROM events WHERE timestamp >= {WIN}",
    },
    "sessionization.gap_violations": {
        "ch": """SELECT count() AS c FROM (
                   SELECT session_id,
                          lagInFrame(toUnixTimestamp64Milli(timestamp))
                            OVER (PARTITION BY session_id ORDER BY timestamp) AS prev_ms,
                          toUnixTimestamp64Milli(timestamp) AS cur_ms
                   FROM events)
                 WHERE prev_ms > 0 AND cur_ms - prev_ms >= 1800000""",
        "duck": """SELECT count(*) AS c FROM (
                     SELECT session_id,
                            epoch_ms(timestamp) -
                              lag(epoch_ms(timestamp))
                                OVER (PARTITION BY session_id ORDER BY timestamp) AS gap_ms
                     FROM events)
                   WHERE gap_ms >= 1800000""",
    },
    "dedup.raw_vs_distinct_7d": {
        "ch": "SELECT count() AS raw, uniqExact(event_id) AS distinct_ids FROM events WHERE timestamp >= {WIN}",
        "duck": "SELECT count(*) AS raw, count(DISTINCT event_id) AS distinct_ids FROM events WHERE timestamp >= {WIN}",
    },
    "late_events.bucket_7d": {
        "ch": """SELECT count() AS c FROM events
                 WHERE timestamp >= {WIN}
                   AND inserted_at > timestamp + INTERVAL 1 HOUR""",
        "duck": """SELECT count(*) AS c FROM events
                   WHERE timestamp >= {WIN}
                     AND inserted_at > timestamp + INTERVAL '1 hour'""",
    },
    "funnel.signup_window_7d": {
        "ch": _FUNNEL_CH.replace("{WIN}", "{W7}"),
        "duck": _FUNNEL_DUCK.replace("{WIN}", "{W7}"),
    },
    "retention.week1_mature": {
        "ch": _RETENTION_CH,
        "duck": _RETENTION_DUCK,
    },
    "entity.current_rows": {
        "ch": "SELECT count() AS c FROM (SELECT row_key FROM external_rows FINAL GROUP BY row_key)",
        "duck": """SELECT count(*) AS c FROM (
                     SELECT row_key, arg_max(data, synced_at) AS d
                     FROM external_rows GROUP BY row_key)""",
    },
    "entity.deleted_still_visible": {"ch": _DELETED_CH, "duck": _DELETED_DUCK},
    "entity.tombstone_delete": {"ch": _DELETED_CH, "duck": _DELETED_DUCK},
    "entity_join.order_paid_7d": {
        "ch": _ENTITY_JOIN_CH.replace("{WIN}", "{W7}"),
        "duck": _ENTITY_JOIN_DUCK.replace("{WIN}", "{W7}"),
    },
    "currency.cost_7d": {
        "ch": "SELECT sum(toFloat64(cost_usd)) AS s FROM events WHERE timestamp >= {W7} AND cost_usd IS NOT NULL",
        "duck": "SELECT sum(cast(cost_usd AS DOUBLE)) AS s FROM events WHERE timestamp >= {W7} AND cost_usd IS NOT NULL",
    },
}

# --- load-matrix query shapes ------------------------------------------------
# id -> {"ch": sql, "duck": sql}; {WIN} is the window start for the leg's days.

LOAD_SHAPES = {
    "overview": {
        "ch": """SELECT toDate(timestamp) AS d, count() AS events,
                        uniqExact(canonical_id) AS users, uniqExact(session_id) AS sessions
                 FROM v_events WHERE timestamp >= {WIN}
                 GROUP BY d ORDER BY d""",
        "duck": """SELECT cast(timestamp AS DATE) AS d, count(*) AS events,
                          count(DISTINCT canonical_id) AS users,
                          count(DISTINCT session_id) AS sessions
                   FROM v_events WHERE timestamp >= {WIN}
                   GROUP BY d ORDER BY d""",
    },
    "aggregate": {
        "ch": """SELECT event_name, count() AS c, uniqExact(canonical_id) AS users
                 FROM v_events WHERE timestamp >= {WIN}
                 GROUP BY event_name ORDER BY c DESC LIMIT 20""",
        "duck": """SELECT event_name, count(*) AS c, count(DISTINCT canonical_id) AS users
                   FROM v_events WHERE timestamp >= {WIN}
                   GROUP BY event_name ORDER BY c DESC LIMIT 20""",
    },
    "entity_join": {"ch": _ENTITY_JOIN_CH, "duck": _ENTITY_JOIN_DUCK},
    "funnel": {"ch": _FUNNEL_CH, "duck": _FUNNEL_DUCK},
    "retention": {"ch": _RETENTION_CH, "duck": _RETENTION_DUCK},
}


def render(sql: str, days: int = 7, deleted_keys: list[str] | None = None) -> str:
    out = sql.replace("{END}", f"'{ts(CORPUS_END)}'")
    out = out.replace("{MATURE14}", f"'{ts(CORPUS_END - timedelta(days=14))}'")
    out = out.replace("{W7}", f"'{ts(window_start(7))}'")
    out = out.replace("{W30}", f"'{ts(window_start(30))}'")
    out = out.replace("{W90}", f"'{ts(window_start(90))}'")
    out = out.replace("{WIN}", f"'{ts(window_start(days))}'")
    keys = ", ".join("'" + k + "'" for k in (deleted_keys or [])) or "''"
    out = out.replace("{DELETED_KEYS}", keys)
    return out
