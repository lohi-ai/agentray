"""AgentRay Python SDK.

A minimal, dependency-light client for server-side event capture. Payloads are
PostHog-compatible (``distinct_id`` + ``event`` + ``properties``), so an existing
PostHog server integration can point at AgentRay by changing only the host.

    from agentray import Client

    ar = Client(host="https://agentray.example.com", api_key="phc_...")
    ar.capture("order_paid", distinct_id="user-123", properties={"amount": 29})
    ar.identify("user-123", traits={"plan": "pro"})
    ar.flush()   # or ar.shutdown() on process exit

Events are buffered and delivered by a background thread in batches to
``POST /batch``; ``flush()`` blocks until the buffer drains.
"""

from .client import Client

__all__ = ["Client"]
__version__ = "0.1.0"
