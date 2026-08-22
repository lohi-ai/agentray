# agentray (Python)

Server-side event SDK for [AgentRay](https://agentray.lohi2.com). PostHog-compatible
payloads, background batching, non-blocking capture.

## Install

Releases are published to GitHub first — the wheel below is the exact artefact
CI built, tested and imported from a clean virtualenv before publishing:

```bash
pip install https://github.com/lohi-ai/agentray/releases/download/python-v0.1.0/agentray-0.1.0-py3-none-any.whl
```

Pick the version you want from [Releases](https://github.com/lohi-ai/agentray/releases?q=python); the tags are
`python-v<semver>`.

Once `agentray` is published on PyPI, the shorter form works and is the one to
prefer:

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

## Which app an event came from

Every event carries `platform: "server"`, so backend-sent events stay separable
from your website's and your app's in Traffic's platform split and the
per-platform funnel rather than collapsing into "unknown".

Relaying events for a client whose real platform you know? Say so once:

```python
Client(host=..., api_key=..., platform="ios")
```

A `platform` key passed to `capture()` does not override it — the value is
applied after your properties, so a surface cannot be mislabelled by accident.
