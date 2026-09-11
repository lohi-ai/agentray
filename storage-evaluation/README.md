# Storage evaluation harness

Reproducible local comparison of a pinned DuckDB release against the
production-pinned ClickHouse baseline on identical synthetic data. This is
a decision input for `docs/redesign/strategy.md` — not a migration, not a
production cutover, and no production data or infrastructure is touched.

## Layout

- `eval` — driver entrypoint; runs everything in a capped container
  (512 MiB / 1 CPU) that orchestrates sibling engine containers over the
  `storage-eval-net` bridge network via the mounted Docker socket.
- `harness/corpus.py` — deterministic seeded corpus (events, aliases,
  external_rows, ingest batch) + exact oracle manifest.
- `harness/queries.py` — the structured query registry; the only
  operations engines accept. The DuckDB worker resolves IDs internally —
  raw SQL never enters the writer process.
- `harness/engines.py` — pinned engine containers (2 CPU / 2 GiB each).
- `harness/runner.py` — one leg: load, oracle checks, cold/warm shapes,
  ingest-during-reads, RSS/CPU/disk sampling.
- `harness/report.py` — labeled report: MEASURED vs NOT RUN, baseline
  parity vs semantic gates.

## Pins

- DuckDB `1.5.5` (PyPI `duckdb==1.5.5`), Python 3.13 slim image by digest.
- ClickHouse `clickhouse/clickhouse-server:24.12-alpine` at the prod
  multi-arch digest (matches `infra/gce/infra/docker-compose.yml`).

## Commands

```bash
./eval doctor                                   # docker, images, disk
./eval corpus --scale 100000 --seed 20260912    # generate corpus + oracle
./eval smoke --seed 20260912                    # approved bounded run
./eval report --require-labeled-gates           # work/report.md
```

Smoke envelope (approved): 100k events, 1 reader, 7-day shapes, sequential
engines, 2 vCPU / 2 GiB per engine, 512 MiB driver, 8 GiB workdir, 15-min
wall cap per leg.

Full matrix (delivered, not run under the current disk preflight):

```bash
./eval matrix --scales 1000000,10000000 --readers 1,5,20 --days 7,30,90
./eval report --require-labeled-gates
```

Matrix requires ≥40 GiB free disk; preflight refuses otherwise.

## Semantics frozen from production

- 30-min sessionization keyed on (project, distinct_id), arrival order.
- Identity: anonymous → canonical via aliases (CH dictionary /
  DuckDB join).
- `insert_id` stored but never deduplicated on read — raw counts include
  duplicates (baseline parity, not a dedup guarantee).
- `external_rows` has no delete marker — upstream-deleted rows stay
  visible (baseline parity; the tombstone semantic gate is expected
  DIVERGENT on both engines).
- `cost_usd` is Float32 USD agent cost only — not billing revenue and no
  mixed-currency claim.

## NOT RUN gates

Crash/commit-before-ack replay, backup restore, cross-tenant/arbitrary-SQL
isolation, query cancellation, mixed-currency correctness, read-time dedup
guarantee, and the 1M/10M matrix under the failed disk preflight. These
stay labeled NOT RUN in `work/report.md`; nothing here is a production
go/no-go.
