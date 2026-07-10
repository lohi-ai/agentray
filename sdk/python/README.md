# agentray (Python)

Server-side event SDK for [AgentRay](https://agentray.lohi2.com). PostHog-compatible
payloads, background batching, non-blocking capture.

## Install

```bash
pip install agentray
```

## Usage

```python
from agentray import Client

ar = Client(host="https://agentray.example.com", api_key="phc_your_key")

ar.capture("order_paid", distinct_id="user-123", properties={"amount": 29})
ar.identify("user-123", traits={"plan": "pro"})

ar.flush()      # block until delivered
# ar.shutdown() is registered atexit automatically
```

`capture`/`identify` never block on the network — a daemon thread coalesces
events into `POST /batch` (flushing at 20 events or every 3s) and retries
transient 5xx/network errors with backoff. 4xx responses are not retried.

## Migrating from PostHog

Point an existing `posthog.capture(distinct_id, event, properties)` integration
at AgentRay by swapping the client; the wire payload (`distinct_id` + `event` +
`properties`, `$identify`/`$set` for person traits) is the same.
