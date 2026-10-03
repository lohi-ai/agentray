# Data backup and recovery

Data readiness is local to the serving colour. A successful connector run or a
broker acknowledgement is not proof that a particular DuckDB file can answer a
query. `source_status.readiness` becomes `ready` only after that file records a
complete, hole-free promotion and a governed query refreshes its live sandbox.

## Consistent backup

1. Stop source schedules and new ingest admission, then drain both colour
   workers. Do not reset or rename consumers.
2. Checkpoint and close each DuckDB. Copy the database, any required WAL, its
   `.ingest-loss` sidecar, and the receipt/hole tables as one volume snapshot.
3. Snapshot PostgreSQL metadata and the JetStream file store/config at the same
   recovery point. Record stream incarnation, subjects, durable names, consumer
   floors, schema/config versions, UTC timestamps, and checksums in a manifest.
4. Keep credentials in the existing protected secret backup. Never place DSNs,
   API keys, or decrypted credentials in the manifest or test evidence.

Separately copied live DuckDB/WAL, PostgreSQL, and broker files are not a
consistent backup. If admission cannot be quiesced, record that limitation and
use a storage-level atomic snapshot mechanism.

## Restore drill

Restore only into a new isolated environment. Restore the complete manifest
before starting workers; verify store/stream identities and retained coverage,
then replay the retained suffix and matching DLQ identities. Run a real governed
query to rebuild sandbox confirmation—memory from another process is never
restored evidence.

An older store with newer metadata, a missing WAL, a replaced volume, or a
stream whose retained prefix no longer covers the store remains `incomplete`.
Report the exact recovery point and unknown interval. Full history outside
retention requires an authorized source re-export or operator restore; do not
fabricate completeness or reset production consumers to make readiness green.

## Capacity and retention controls

- `DATA_DISK_RESERVE_BYTES` defaults to 4 GiB. New event/source data is refused
  retryably below the reserve; receipt and recovery writes retain headroom.
- `INGEST_STREAM_MAX_BYTES=0` preserves the broker's existing byte policy. A
  positive value opts the stream into a byte ceiling with discard-new behavior.
- `SOURCE_STAGING_TTL_DAYS` defaults to 7; `0` disables automatic cleanup.
  Cleanup requires C1's authoritative terminal failed/cancelled, non-resumable,
  non-active, no-pending-outbox eligibility and rechecks it transactionally.
- `SOURCE_FRESHNESS_MAX_AGE` is an explicit Go duration for manual/irregular
  syncs. Without it, completed irregular data is `stale`, never indefinitely
  fresh. Scheduled syncs use two observed schedule intervals plus one minute.

Cleanup never deletes an active or resumable generation and never claims that a
DuckDB `DELETE` shrinks the file. Checkpointing makes freed blocks reusable.

Receipt journals retain exact ordinary delivery identities for a bounded recent
global suffix (4,096). A repaired hole and its replay receipt
are removed atomically; unresolved and unverifiable holes remain indefinitely.
Source batch detail is compacted at an authoritative completion boundary while
the current complete run/generation stays available for reconciliation.

PostgreSQL publication observations follow the same rule: a completed run keeps
its own accepted batch set while resolved older run identities are deleted.
Legacy observations have no completion identity, so the newest 256 identities
per project/connector/table are retained. Terminal snapshot generation rows are
small shared tombstones and remain authoritative across colours; each DuckDB
records its own cleanup completion, and published outbox payloads are removed
after local cleanup without deleting another colour's discovery authority.

## AC-DATA-03 capacity envelope

`TestDataEnvelope` is the assertion-bearing capacity harness. Its normal test
behavior is a skip; setting only `AGENTRAY_DATA_ENVELOPE=1` fails preflight.
The run is authorized only on the foreman-leased GCE instance `lohi-app` in
project `lohi-dev-lohi`, zone `asia-southeast1-a`, for the
`lohi-app-frozen-surface` class. The approval variables are an operator
attestation, and the harness independently verifies that exact identity through
GCE metadata before creating fixtures. It also refuses outside 01:00–06:00 HCM
or unless its process has exactly one allowed CPU, nice value 19, a 2–2.5 GiB
cgroup `MemoryMax`, and an internal timeout of at most 60 minutes. The 2.5 GiB
parent ceiling reserves the fixed 512 MiB PostgreSQL allocation, so the
aggregate authorized ceiling can never exceed 3 GiB. The harness owns and
removes a `postgres:16-alpine` container, embedded file-backed JetStream,
DuckDB/WAL, sandbox children, spill files, and all generated corpus files.

From the repository root, the complete non-interactive invocation is:

```sh
sudo systemd-run --scope --quiet --collect -p MemoryMax=2G \
  taskset -c 0 nice -n 19 \
  timeout --signal=TERM --kill-after=30s 3570s env \
  AGENTRAY_DATA_ENVELOPE=1 \
  AGENTRAY_DATA_ENVELOPE_APPROVED=1 \
  AGENTRAY_DATA_ENVELOPE_HOST_CLASS=lohi-app-frozen-surface \
  AGENTRAY_DATA_ENVELOPE_REPORT=/var/tmp/agentray-ac-data-03.json \
  go test ./internal/dataplane/ingest -run '^TestDataEnvelope$' -count=1 -timeout 59m -v
```

Run this command only during the authorized 01:00–06:00 HCM window; the harness
checks that window before creating any fixture. These are the authorized host
caps, not tuning suggestions. The main test, embedded broker, and sandbox
workers share the one-CPU, nice-19, 2 GiB scope; the harness launches disposable
PostgreSQL pinned to that same CPU, at Docker's lowest CPU share weight, with a
512 MiB hard memory/swap cap, for a 2.5 GiB aggregate ceiling. Preflight accepts
no parent `MemoryMax` above 2.5 GiB because PostgreSQL's allocation would then
exceed the 3 GiB aggregate authorization. The outer runner
sends TERM at 59m30s and KILL 30 seconds later, enforcing a 60-minute hard wall
clock; the Go test timeout is 59 minutes, and the harness cancels its work at
58 minutes to leave teardown headroom. PostgreSQL runs through an attached
Docker client owned by an independent watchdog. Normal return, startup failure,
panic, Go timeout, TERM, KILL, or parent disappearance therefore stops the
client, forcibly removes the named disposable container, reaps the watchdog,
and leaves no detached database behind. Any
missing or different detected cap fails preflight; do not weaken the check.

Do not replace the corpus constants or shorten the 15-minute steady phase to
claim acceptance. The deterministic corpus is 10M events and 1M external rows
per project for three projects. Four concurrent governed investigations cover
62-day activity, 62-day purchases, lifetime-first-payer, and queryability while
four publishers sustain a scheduled aggregate 100 accepted events/second.
The rate assertion permits zero accepted-event shortfall: at least 90,000
events must be accepted during the 15-minute steady phase (100/s inclusive).

The JSON report contains raw request samples and p95 inputs, per-phase and
per-tenant timings and totals, CPU/RAM/filesystem/disk facts, RSS, DuckDB/WAL,
spill and broker growth, the staging/promotion probe and configured retention
bounds, cgroup OOM counters, refusals, and the corpus seed hash. The test fails
when warm p95 exceeds 5s, cold p95 exceeds 30s, accepted-publication p95 exceeds
1s, queryable p95 exceeds 60s, the accepted rate is missed, any investigation
is refused/errors, cgroup OOM increases, RSS reaches 90% of available RAM or
grows by more than 10% of RAM across the steady-state quartiles, tenant totals
cross, or final event/external totals differ. Each concurrent steady-state
investigation is classified from the actual project sandbox process present
before and after the timed call: a surviving worker is warm, while a
spawned/replaced/evicted worker is cold. Warm p95 therefore excludes cold
refreshes, and queryability latency is recorded only after a
query proves the exact accepted event identities visible. RSS aggregates the
test process, all descendants (including sandbox workers), and the disposable
PostgreSQL process tree. A failed run is evidence; never edit the report or
substitute logged estimates for the measured samples.
