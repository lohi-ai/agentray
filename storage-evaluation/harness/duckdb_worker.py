"""DuckDB engine worker — the one owning process for the native file.

Runs inside the engine container (2 CPU / 2 GiB). Serves a tiny HTTP API on
0.0.0.0:8710 reachable only on the storage-eval-net bridge network. It
accepts ONLY structured operation IDs resolved through harness.queries —
the driver cannot send raw SQL here, matching the plan's contract that
arbitrary SQL never enters the writer process.
"""
from __future__ import annotations

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import duckdb

from . import queries

DB_PATH = Path("/data/eval.duckdb")
LOCK = threading.Lock()  # one writer; readers serialize on the GIL anyway

CON = None


def _con():
    global CON
    if CON is None:
        CON = duckdb.connect(str(DB_PATH))
        CON.execute("SET memory_limit = '1.5GB'")
        CON.execute("SET threads = 2")
        CON.execute("SET TimeZone = 'UTC'")
        CON.execute("SET temp_directory = '/data/tmp'")
        CON.execute("SET preserve_insertion_order = false")
    return CON


def _init_schema():
    con = _con()
    con.execute(
        """CREATE TABLE IF NOT EXISTS events (
             project_id UUID, event_id UUID, distinct_id VARCHAR,
             session_id VARCHAR, event_name VARCHAR, event_type VARCHAR,
             properties VARCHAR, agent_id VARCHAR, tool_name VARCHAR,
             tool_input VARCHAR, tool_output VARCHAR,
             tokens_input UINTEGER, tokens_output UINTEGER,
             cost_usd FLOAT, latency_ms UINTEGER, model_name VARCHAR,
             is_error UTINYINT, error_message VARCHAR,
             timestamp TIMESTAMPTZ, inserted_at TIMESTAMPTZ,
             visitor_class VARCHAR, bot_name VARCHAR, referrer_host VARCHAR,
             referrer_channel VARCHAR, user_agent VARCHAR, platform VARCHAR,
             insert_id VARCHAR, is_unplanned UTINYINT)"""
    )
    con.execute(
        """CREATE TABLE IF NOT EXISTS aliases (
             project_id UUID, anonymous_id VARCHAR, canonical_id VARCHAR,
             version TIMESTAMPTZ)"""
    )
    con.execute(
        """CREATE TABLE IF NOT EXISTS external_rows (
             project_id UUID, connector_id UUID, table_name VARCHAR,
             row_key VARCHAR, cursor VARCHAR, data VARCHAR,
             synced_at TIMESTAMPTZ)"""
    )
    for v in queries.DUCK_VIEWS:
        con.execute(v)


def _load_parquet(table: str, path: Path):
    _con().execute(
        f"INSERT INTO {table} BY NAME SELECT * FROM read_parquet('{path}')"
    )


def _handle(op: dict) -> dict:
    kind = op.get("op")
    if kind == "init":
        _init_schema()
        return {"ok": True, "duckdb": duckdb.__version__}
    if kind == "load":
        corpus = Path(op["corpus"])
        for table, fname in [
            ("events", "events.parquet"),
            ("aliases", "aliases.parquet"),
            ("external_rows", "external_rows.parquet"),
        ]:
            _load_parquet(table, corpus / fname)
        return {"ok": True}
    if kind == "ingest":
        with LOCK:
            _load_parquet("events", Path(op["corpus"]) / "ingest_batch.parquet")
        n = _con().execute("SELECT count(*) FROM events").fetchone()[0]
        return {"ok": True, "total": n}
    if kind == "query":
        # Structured IDs only — the driver cannot send raw SQL to the writer.
        qid = op["id"]
        spec = queries.CHECKS.get(qid) or queries.LOAD_SHAPES.get(qid)
        if spec is None:
            return {"ok": False, "error": f"unknown query id {qid}"}
        sql = queries.render(spec["duck"], days=op.get("days", 7),
                             deleted_keys=op.get("deleted_keys"))
        cur = _con().execute(sql)
        cols = [d[0] for d in cur.description]
        rows = [dict(zip(cols, r)) for r in cur.fetchall()]
        return {"ok": True, "rows": rows}
    if kind == "count":
        n = _con().execute("SELECT count(*) FROM events").fetchone()[0]
        return {"ok": True, "total": n}
    if kind == "shutdown":
        threading.Thread(target=_shutdown, daemon=True).start()
        return {"ok": True}
    return {"ok": False, "error": f"unknown op {kind}"}


def _shutdown():
    import os
    import signal
    os.kill(os.getpid(), signal.SIGTERM)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            resp = _handle(json.loads(body or b"{}"))
        except Exception as e:  # engine errors are data, not crashes
            resp = {"ok": False, "error": f"{type(e).__name__}: {e}"}
        payload = json.dumps(resp, default=str).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *a):
        pass


def main():
    Path("/data/tmp").mkdir(parents=True, exist_ok=True)
    _init_schema()
    srv = ThreadingHTTPServer(("0.0.0.0", 8710), Handler)
    print(f"duckdb worker up, duckdb={duckdb.__version__}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
