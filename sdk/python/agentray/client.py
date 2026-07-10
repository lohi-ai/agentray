"""Background-batching AgentRay client.

Design goals mirror the browser SDK: never block the caller's hot path, coalesce
events into ``POST /batch``, and lose nothing on a clean shutdown. A single daemon
worker thread owns delivery; ``capture``/``identify`` only enqueue.
"""

from __future__ import annotations

import atexit
import json
import queue
import threading
import time
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

import urllib3


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class Client:
    """Thread-safe AgentRay event client.

    Args:
        host: Base URL of the AgentRay server (trailing slash tolerated).
        api_key: Project API key.
        flush_at: Flush when this many events are buffered.
        flush_interval: Max seconds a buffered event waits before delivery.
        max_retries: Delivery attempts per batch before the batch is dropped.
    """

    def __init__(
        self,
        host: str,
        api_key: str,
        flush_at: int = 20,
        flush_interval: float = 3.0,
        max_retries: int = 3,
    ) -> None:
        self._host = host.rstrip("/")
        self._api_key = api_key
        self._flush_at = max(1, flush_at)
        self._flush_interval = flush_interval
        self._max_retries = max(1, max_retries)

        self._queue: "queue.Queue[Optional[Dict[str, Any]]]" = queue.Queue()
        self._http = urllib3.PoolManager(retries=False)
        self._stopped = threading.Event()
        self._flushed = threading.Condition()
        self._worker = threading.Thread(target=self._run, name="agentray-flush", daemon=True)
        self._worker.start()
        atexit.register(self.shutdown)

    # --- public API -------------------------------------------------------

    def capture(
        self,
        event: str,
        distinct_id: str,
        properties: Optional[Dict[str, Any]] = None,
    ) -> None:
        """Queue an event for delivery. Never blocks on the network."""
        self._queue.put(
            {
                "event": event,
                "distinct_id": distinct_id,
                "properties": properties or {},
                "timestamp": _now_iso(),
            }
        )

    def identify(
        self,
        distinct_id: str,
        traits: Optional[Dict[str, Any]] = None,
    ) -> None:
        """Set person properties (PostHog-compatible ``$identify`` + ``$set``)."""
        self.capture("$identify", distinct_id, {"$set": traits or {}})

    def flush(self, timeout: Optional[float] = None) -> None:
        """Block until the current buffer has been delivered (or timeout)."""
        self._queue.join() if timeout is None else self._join_with_timeout(timeout)

    def shutdown(self) -> None:
        """Flush and stop the worker. Idempotent; called automatically at exit."""
        if self._stopped.is_set():
            return
        self._stopped.set()
        self._queue.put(None)  # wake + sentinel
        self._worker.join(timeout=5.0)

    # --- worker -----------------------------------------------------------

    def _run(self) -> None:
        batch: List[Dict[str, Any]] = []
        deadline: Optional[float] = None
        while True:
            timeout = None if deadline is None else max(0.0, deadline - time.monotonic())
            try:
                item = self._queue.get(timeout=timeout)
            except queue.Empty:
                self._deliver(batch)
                batch, deadline = [], None
                continue

            if item is None:  # shutdown sentinel
                self._queue.task_done()
                self._deliver(batch)
                return

            batch.append(item)
            self._queue.task_done()
            if deadline is None:
                deadline = time.monotonic() + self._flush_interval
            if len(batch) >= self._flush_at:
                self._deliver(batch)
                batch, deadline = [], None

    def _deliver(self, batch: List[Dict[str, Any]]) -> None:
        if not batch:
            return
        body = json.dumps({"api_key": self._api_key, "batch": batch}).encode("utf-8")
        backoff = 0.5
        for attempt in range(self._max_retries):
            try:
                resp = self._http.request(
                    "POST",
                    f"{self._host}/batch",
                    body=body,
                    headers={"Content-Type": "application/json"},
                )
                # 4xx is a client error (bad key/payload) — retrying won't help.
                if resp.status < 500:
                    return
            except Exception:
                pass
            if attempt + 1 < self._max_retries:
                time.sleep(backoff)
                backoff = min(backoff * 2, 8.0)
        # Give up after max_retries — drop rather than block the process forever.

    def _join_with_timeout(self, timeout: float) -> None:
        end = time.monotonic() + timeout
        while not self._queue.empty() and time.monotonic() < end:
            time.sleep(0.02)
