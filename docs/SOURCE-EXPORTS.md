# Governed PostgreSQL source exports

AgentRay reads external PostgreSQL data only through an operator-approved export
contract. The application never accepts arbitrary source SQL, never creates
source roles or views, and never returns a DSN. New bindings must name an
ordinary `VIEW`; a base table is allowed only for the IDs of syncs that existed
before this policy and are explicitly grandfathered.

## 1. Operator preparation

Create a dedicated schema and least-privilege login in the source database.
The example is a recipe for the database operator to review and run—not SQL the
application executes:

```sql
CREATE SCHEMA agentray_exports;
CREATE VIEW agentray_exports.orders_v1 AS
SELECT id, updated_at, amount_minor, status FROM app.orders;

CREATE ROLE agentray_export_reader LOGIN PASSWORD '<managed secret>';
GRANT CONNECT ON DATABASE app TO agentray_export_reader;
GRANT USAGE ON SCHEMA agentray_exports TO agentray_export_reader;
GRANT SELECT ON agentray_exports.orders_v1 TO agentray_export_reader;
ALTER ROLE agentray_export_reader SET default_transaction_read_only = on;
```

Do not grant access to raw user/PII tables, routines, schema creation, role
administration, or database creation. AgentRay rejects administrative source
roles and forces bounded read-only sessions. A view definition remains an
operator trust boundary: read-only SQL cannot prove that a user-defined function
inside a view has no external side effects.

The key column must be permanently unique, non-null, non-empty and immutable.
AgentRay performs a full validation before and after a snapshot, but only the
operator can attest that a key will stay stable in the future. For incremental
exports, create an index that supports `(cursor_column, key_column)` keyset
reads. For snapshots, index the key. Retain an `EXPLAIN` from the source showing
an index-bound plan and review the whole-view validation cost against the source
budget.

## 2. Source policy file

Set `AGENTRAY_SOURCE_POLICY_FILE` to an absolute, operator-managed JSON file.
Malformed configured files fail startup; an absent file denies external dials.
The file has no secrets:

```json
{
  "version": 1,
  "destinations": [{
    "host": "analytics-db.internal.example",
    "port": 5432,
    "allowed_ip_cidrs": ["10.42.7.15/32"]
  }],
  "bindings": [{
    "project_id": "00000000-0000-4000-8000-000000000001",
    "connector_id": "00000000-0000-4000-8000-000000000002",
    "schema": "agentray_exports",
    "relation": "orders_v1",
    "relation_kind": "view",
    "columns": [
      {"name": "id", "pg_type": "bigint"},
      {"name": "updated_at", "pg_type": "timestamp with time zone"},
      {"name": "amount_minor", "pg_type": "bigint"},
      {"name": "status", "pg_type": "text"}
    ],
    "key_column": "id",
    "key_stability": "immutable_unique_non_null",
    "allowed_cursor_columns": ["updated_at"],
    "legacy_sync_ids": []
  }]
}
```

Every primary/fallback destination and every resolved address must match the
exact host, port and CIDR policy before a socket opens. Unix sockets,
link-local/metadata addresses, unapproved fallbacks, schema drift and extra
columns fail closed. DNS is resolved and checked again at dial time; the socket
is pinned to the checked address while TLS still verifies the configured host.

For an old table sync, use `relation_kind: "legacy_table"` and list only its
already-persisted sync ID in `legacy_sync_ids`. A new sync cannot claim that
grandfathering. Enumerate the old sync's approved columns too.

## 3. Sync modes and capture meaning

`sync_mode` is additive:

- omitted or empty preserves the old connector behavior, including legacy
  cursorless re-pulls and their existing row cap;
- `incremental` requires an approved cursor column and retains durable-before-
  cursor publication;
- `snapshot` uses key-only pagination, a resumable generation and deletion by
  absence only after complete promotion.

A live view snapshot describes the interval from `capture_started_at` through
`capture_finished_at`; it is not an as-of instant. Use an immutable,
operator-frozen export when historical reconciliation needs an exact instant.

Snapshot batches are first persisted in the PostgreSQL outbox, then published
to durable JetStream. Core NATS is refused for snapshots. Every serving DuckDB
stages batches privately and performs one binding-scoped transaction only after
the completion marker, exact contiguous batch manifest and expected row count
all match. The transaction deletes the old binding, inserts staged rows,
records the local promotion, and advances the applied stream mark. Missing,
dead-lettered, cancelled or conflicting batches leave the last complete live
rows unchanged. An empty complete generation legitimately removes all rows for
that binding.

Local promotion proves atomic application to that DuckDB only. It does not
claim that a query sandbox refreshed or that every serving store is queryable;
those readiness and cleanup decisions consume the published promotion and
generation descriptors separately.

## 4. Enable, observe, and roll back

1. Install additive metadata/staging schema with snapshot production disabled.
2. Upgrade every producer and consumer colour, including the inactive colour.
3. Configure exact destinations, view bindings and any explicit legacy IDs.
4. Drain pre-upgrade legacy messages for a binding on every serving store.
5. Opt in one sync with `sync_mode: "snapshot"`; preserve old active rows until
   its first complete generation promotes.
6. Observe generation identity, expected batch/row counts, capture interval,
   unpublished outbox and per-store promotion before calling it ready.

To roll back behavior, pause new runs and let the current generation finish or
cancel it. Keep snapshot-capable consumers while retained v1 messages can
replay. Disabling the producer alone does not make an old consumer safe: a late
snapshot batch could otherwise be mistaken for a legacy upsert. Never reset the
production stream or delete DuckDB volumes as a rollback shortcut.

Failed/cancelled generations are cleanup candidates only when terminal,
non-resumable, inactive on the store and free of unpublished outbox. Recheck
those predicates during deletion. The active last-complete generation and any
capturing/yielded generation are always protected.
