"""Tests for the batching client. Runnable with `python -m pytest` or directly
(`python sdk/python/tests/test_client.py`) — no server needed; the PoolManager is
replaced with a recorder."""

import json
import os
import sys
import threading
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from agentray import Client  # noqa: E402


class _FakeResp:
    def __init__(self, status):
        self.status = status


class _Recorder:
    """Stands in for urllib3.PoolManager, recording delivered batches."""

    def __init__(self, status=200):
        self.status = status
        self.batches = []
        self.lock = threading.Lock()

    def request(self, method, url, body=None, headers=None):
        with self.lock:
            payload = json.loads(body.decode("utf-8"))
            self.batches.append((method, url, payload))
        return _FakeResp(self.status)


def _new_client(recorder, **kw):
    c = Client(host="https://ar.example.com/", api_key="k", **kw)
    c._http = recorder  # inject the recorder in place of urllib3
    return c


def test_batches_flush_on_size():
    rec = _Recorder()
    c = _new_client(rec, flush_at=3, flush_interval=60)
    for i in range(3):
        c.capture("evt", distinct_id=f"u{i}", properties={"i": i})
    c.flush(timeout=2)
    assert len(rec.batches) == 1, rec.batches
    method, url, payload = rec.batches[0]
    assert method == "POST"
    assert url == "https://ar.example.com/batch"  # trailing slash trimmed
    assert payload["api_key"] == "k"
    assert len(payload["batch"]) == 3
    assert payload["batch"][0]["event"] == "evt"
    assert "timestamp" in payload["batch"][0]
    c.shutdown()


def test_identify_sets_person_traits():
    rec = _Recorder()
    c = _new_client(rec, flush_at=1, flush_interval=60)
    c.identify("user-1", traits={"plan": "pro"})
    c.flush(timeout=2)
    _, _, payload = rec.batches[0]
    ev = payload["batch"][0]
    assert ev["event"] == "$identify"
    assert ev["distinct_id"] == "user-1"
    assert ev["properties"]["$set"] == {"plan": "pro"}
    c.shutdown()


def test_flush_interval_delivers_partial_batch():
    rec = _Recorder()
    c = _new_client(rec, flush_at=100, flush_interval=0.1)
    c.capture("evt", distinct_id="u")
    time.sleep(0.4)  # under flush_at, so only the interval can trigger delivery
    assert len(rec.batches) == 1, "interval flush should deliver a partial batch"
    c.shutdown()


def test_shutdown_flushes_remaining():
    rec = _Recorder()
    c = _new_client(rec, flush_at=100, flush_interval=60)
    c.capture("evt", distinct_id="u")
    c.shutdown()  # must drain the buffer before the worker exits
    assert len(rec.batches) == 1


def test_4xx_not_retried():
    rec = _Recorder(status=401)
    c = _new_client(rec, flush_at=1, flush_interval=60, max_retries=3)
    c.capture("evt", distinct_id="u")
    c.flush(timeout=2)
    assert len(rec.batches) == 1, "a 4xx must not be retried"
    c.shutdown()


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"ok   {name}")
            except Exception as e:  # noqa: BLE001
                failures += 1
                print(f"FAIL {name}: {e}")
    sys.exit(1 if failures else 0)
