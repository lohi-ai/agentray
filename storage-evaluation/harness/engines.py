"""Engine adapters: container lifecycle + query/ingest interfaces.

Both engines run as sibling containers on the storage-eval-net bridge
network, capped at 2 CPU / 2 GiB. The driver reaches them by container
name. Data dirs live under work/<engine>-data (bind-mounted, counted
toward the workdir cap).
"""
from __future__ import annotations

import atexit
import json
import os
import time
from pathlib import Path

import docker
import requests

from . import queries
from .util import (
    CH_HTTP_PORT,
    CH_IMAGE,
    CH_NAME,
    DUCK_NAME,
    DUCK_PORT,
    DRIVER_IMAGE,
    HOST_DIR,
    NETWORK,
    WORK,
)

_docker = None


def client() -> docker.DockerClient:
    global _docker
    if _docker is None:
        # docker-py leaks an unclosed unix socket when connect() fails
        # inside version negotiation (the socket is a connect() local,
        # orphaned before urllib3 tracks it). Fail fast on a missing
        # socket instead of paying that leak.
        host = os.environ.get("DOCKER_HOST", "unix:///var/run/docker.sock")
        if host.startswith("unix://") and not os.path.exists(host[7:]):
            raise RuntimeError(f"docker socket {host} not available")
        _docker = docker.from_env()
    return _docker


def close_client():
    """Close the lazily-created docker client — otherwise its socket
    leaks a ResourceWarning at interpreter exit."""
    global _docker
    if _docker is not None:
        try:
            _docker.close()
        except Exception:
            pass
        _docker = None


atexit.register(close_client)


def stop_container(name: str):
    try:
        c = client().containers.get(name)
        # Only remove containers this harness created — a name collision
        # with a foreign container must not be stopped.
        if c.labels.get("storage-eval") != "1":
            raise RuntimeError(f"refusing to remove unowned container {name}")
        c.remove(force=True)
    except docker.errors.NotFound:
        pass


def disk_bytes(container_name: str, path: str) -> int:
    """du -sb inside the engine container — measures what the engine wrote."""
    c = client().containers.get(container_name)
    rc, out = c.exec_run(["du", "-sb", path])
    if rc != 0:
        return -1
    return int(out.split()[0])


class Engine:
    name = "?"
    dialect = "?"
    container_name = ""

    def start(self, caps: dict): raise NotImplementedError
    def load(self, corpus: Path): raise NotImplementedError
    def ingest(self, corpus: Path) -> dict: raise NotImplementedError
    def count(self) -> int: raise NotImplementedError
    def disk(self) -> int: raise NotImplementedError
    def stop(self): stop_container(self.container_name)

    def run_check(self, check_id: str, days: int, deleted_keys: list[str]):
        spec = queries.CHECKS[check_id]
        sql = queries.render(spec[self.dialect], days=days, deleted_keys=deleted_keys)
        return self.query(sql)

    def run_shape(self, shape_id: str, days: int):
        sql = queries.render(queries.LOAD_SHAPES[shape_id][self.dialect], days=days)
        return self.query(sql)


class ClickHouseEngine(Engine):
    name = "clickhouse"
    dialect = "ch"
    container_name = CH_NAME
    db = "eval"

    def __init__(self):
        self.base = f"http://{CH_NAME}:{CH_HTTP_PORT}"
        self.sess = requests.Session()

    def start(self, caps: dict):
        stop_container(CH_NAME)
        data = WORK / "ch-data"
        # Fresh data dir per leg: a reused dir would double-load the corpus.
        if data.exists():
            import shutil
            shutil.rmtree(data)
        data.mkdir(parents=True, exist_ok=True)
        data.chmod(0o777)  # container runs as uid 101 (clickhouse)
        client().containers.run(
            CH_IMAGE,
            name=CH_NAME,
            detach=True,
            network=NETWORK,
            labels={"storage-eval": "1"},
            mem_limit=caps["engine_mem"],
            nano_cpus=caps["engine_cpus"] * 10**9,
            environment={
                "CLICKHOUSE_DB": self.db,
                "CLICKHOUSE_USER": "default",
                "CLICKHOUSE_PASSWORD": "",
                "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1",
            },
            volumes={str(data): {"bind": "/var/lib/clickhouse", "mode": "rw"}},
        )
        self._wait_ready()
        self._schema()

    def _wait_ready(self, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                r = self.sess.get(f"{self.base}/ping", timeout=3)
                if r.status_code == 200:
                    return
            except requests.RequestException:
                pass
            time.sleep(1)
        raise TimeoutError("clickhouse did not become ready")

    def _exec(self, sql: str):
        r = self.sess.post(f"{self.base}/", params={"database": "default"},
                           data=sql, timeout=120)
        if r.status_code != 200:
            raise RuntimeError(f"CH DDL failed: {r.text[:500]}")

    def _schema(self):
        self._exec(f"CREATE DATABASE IF NOT EXISTS {self.db}")
        # Mirrors store.go:1107-1164 (events) and 1173-1198 (aliases+dict).
        self._exec(f"""
CREATE TABLE IF NOT EXISTS {self.db}.events (
    project_id UUID, event_id UUID DEFAULT generateUUIDv4(),
    distinct_id String, session_id String,
    event_name LowCardinality(String), event_type LowCardinality(String),
    properties String,
    agent_id Nullable(String), tool_name Nullable(String),
    tool_input Nullable(String), tool_output Nullable(String),
    tokens_input Nullable(UInt32), tokens_output Nullable(UInt32),
    cost_usd Nullable(Float32), latency_ms Nullable(UInt32),
    model_name Nullable(String), is_error UInt8 DEFAULT 0,
    error_message Nullable(String),
    timestamp DateTime64(3, 'UTC'), inserted_at DateTime64(3, 'UTC') DEFAULT now64(),
    visitor_class LowCardinality(String) DEFAULT 'human',
    bot_name Nullable(String), referrer_host Nullable(String),
    referrer_channel LowCardinality(String) DEFAULT '',
    user_agent Nullable(String), platform LowCardinality(String) DEFAULT '',
    insert_id Nullable(String), is_unplanned UInt8 DEFAULT 0
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (project_id, event_name, timestamp, distinct_id)
TTL toDateTime(timestamp) + INTERVAL 1 YEAR""")
        self._exec(f"""
CREATE TABLE IF NOT EXISTS {self.db}.aliases (
    project_id UUID, anonymous_id String, canonical_id String,
    version DateTime64(3, 'UTC') DEFAULT now64()
) ENGINE = ReplacingMergeTree(version)
ORDER BY (project_id, anonymous_id)""")
        self._exec(f"""
CREATE TABLE IF NOT EXISTS {self.db}.external_rows (
    project_id UUID, connector_id UUID, table_name LowCardinality(String),
    row_key String, cursor String, data String,
    synced_at DateTime64(3, 'UTC') DEFAULT now64()
) ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (project_id, connector_id, table_name, row_key)""")
        self._exec(f"""
CREATE DICTIONARY IF NOT EXISTS {self.db}.aliases_dict (
    project_id UUID, anonymous_id String, canonical_id String
) PRIMARY KEY project_id, anonymous_id
SOURCE(CLICKHOUSE(TABLE 'aliases' DB '{self.db}' USER 'default'))
LAYOUT(COMPLEX_KEY_HASHED())
LIFETIME(MIN 1 MAX 5)""")
        for v in queries.CH_VIEWS:
            self._exec(v.replace("v_events", f"{self.db}.v_events")
                        .replace("FROM events", f"FROM {self.db}.events"))

    def load(self, corpus: Path):
        for table, fname in [
            ("events", "events.parquet"),
            ("aliases", "aliases.parquet"),
            ("external_rows", "external_rows.parquet"),
        ]:
            self._insert_parquet(table, corpus / fname)
        self._exec("SYSTEM RELOAD DICTIONARY eval.aliases_dict")

    def _insert_parquet(self, table: str, path: Path):
        cols = {
            "events": """project_id String, event_id String, distinct_id String,
                session_id String, event_name String, event_type String,
                properties String, agent_id Nullable(String), tool_name Nullable(String),
                tool_input Nullable(String), tool_output Nullable(String),
                tokens_input Nullable(UInt32), tokens_output Nullable(UInt32),
                cost_usd Nullable(Float32), latency_ms Nullable(UInt32),
                model_name Nullable(String), is_error UInt8,
                error_message Nullable(String),
                timestamp DateTime64(3), inserted_at DateTime64(3),
                visitor_class String, bot_name Nullable(String),
                referrer_host Nullable(String), referrer_channel String,
                user_agent Nullable(String), platform String,
                insert_id Nullable(String), is_unplanned UInt8""",
            "aliases": """project_id String, anonymous_id String,
                canonical_id String, version DateTime64(3)""",
            "external_rows": """project_id String, connector_id String,
                table_name String, row_key String, cursor String,
                data String, synced_at DateTime64(3)""",
        }[table]
        casts = {
            "events": "toUUID(project_id), toUUID(event_id), distinct_id, session_id, event_name, event_type, properties, agent_id, tool_name, tool_input, tool_output, tokens_input, tokens_output, cost_usd, latency_ms, model_name, is_error, error_message, timestamp, inserted_at, visitor_class, bot_name, referrer_host, referrer_channel, user_agent, platform, insert_id, is_unplanned",
            "aliases": "toUUID(project_id), anonymous_id, canonical_id, version",
            "external_rows": "toUUID(project_id), toUUID(connector_id), table_name, row_key, cursor, data, synced_at",
        }[table]
        sql = (f"INSERT INTO {self.db}.{table} SELECT {casts} "
               f"FROM input('{cols}') FORMAT Parquet")
        with open(path, "rb") as f:
            r = self.sess.post(f"{self.base}/", params={"query": sql},
                               data=f, timeout=600)
        if r.status_code != 200:
            raise RuntimeError(f"CH insert {table} failed: {r.text[:500]}")

    def ingest(self, corpus: Path) -> dict:
        t0 = time.monotonic()
        self._insert_parquet("events", corpus / "ingest_batch.parquet")
        ack = time.monotonic()
        return {"ack_s": ack - t0, "total": self.count()}

    def query(self, sql: str):
        r = self.sess.post(
            f"{self.base}/",
            params={"database": self.db, "default_format": "JSONEachRow"},
            data=sql,
            timeout=300,
        )
        if r.status_code != 200:
            raise RuntimeError(f"CH query failed: {r.text[:500]}")
        return [json.loads(line) for line in r.text.strip().splitlines()
                if line.strip()]

    def count(self) -> int:
        return int(self.query("SELECT count() AS c FROM events")[0]["c"])

    def disk(self) -> int:
        return disk_bytes(CH_NAME, "/var/lib/clickhouse")


class DuckDBEngine(Engine):
    name = "duckdb"
    dialect = "duck"
    container_name = DUCK_NAME

    def __init__(self):
        self.base = f"http://{DUCK_NAME}:{DUCK_PORT}"
        self.sess = requests.Session()

    def start(self, caps: dict):
        stop_container(DUCK_NAME)
        data = WORK / "duck-data"
        if data.exists():
            import shutil
            shutil.rmtree(data)
        data.mkdir(parents=True, exist_ok=True)
        client().containers.run(
            DRIVER_IMAGE,
            name=DUCK_NAME,
            detach=True,
            network=NETWORK,
            labels={"storage-eval": "1"},
            mem_limit=caps["engine_mem"],
            nano_cpus=caps["engine_cpus"] * 10**9,
            working_dir=str(HOST_DIR),
            volumes={
                str(HOST_DIR): {"bind": str(HOST_DIR), "mode": "ro"},
                str(data): {"bind": "/data", "mode": "rw"},
            },
            command=["python", "-m", "harness.duckdb_worker"],
        )
        self._wait_ready()
        self._post({"op": "init"})

    def _wait_ready(self, timeout=60):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                if self.sess.get(f"{self.base}/", timeout=2).status_code == 200:
                    return
            except requests.RequestException:
                pass
            time.sleep(0.5)
        raise TimeoutError("duckdb worker did not become ready")

    def _post(self, op: dict) -> dict:
        r = self.sess.post(f"{self.base}/", json=op, timeout=600)
        resp = r.json()
        if not resp.get("ok"):
            raise RuntimeError(
                f"duckdb op {op.get('op')} failed: {resp.get('error')}")
        return resp

    def load(self, corpus: Path):
        # The worker reads corpus files through its own mount of $HOST_DIR.
        self._post({"op": "load", "corpus": str(corpus)})

    def ingest(self, corpus: Path) -> dict:
        t0 = time.monotonic()
        resp = self._post({"op": "ingest", "corpus": str(corpus)})
        return {"ack_s": time.monotonic() - t0, "total": resp["total"]}

    def query(self, sql: str):
        # The worker only accepts structured IDs; raw SQL is rejected by
        # contract (strategy.md: arbitrary SQL stays out of the writer).
        raise NotImplementedError("use run_check/run_shape")

    def run_check(self, check_id: str, days: int, deleted_keys: list[str]):
        resp = self._post({"op": "query", "id": check_id,
                           "days": days, "deleted_keys": deleted_keys})
        return resp["rows"]

    def run_shape(self, shape_id: str, days: int):
        resp = self._post({"op": "query", "id": shape_id, "days": days})
        return resp["rows"]

    def count(self) -> int:
        return int(self._post({"op": "count"})["total"])

    def disk(self) -> int:
        return disk_bytes(DUCK_NAME, "/data")

    def stop(self):
        try:
            self._post({"op": "shutdown"})
        except Exception:
            pass
        stop_container(DUCK_NAME)
