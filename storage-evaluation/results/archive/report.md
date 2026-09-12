# Storage evaluation report

Synthetic local corpus only. This is a decision input for docs/redesign/strategy.md, not a migration and not a production go/no-go. MEASURED rows were executed in this harness; NOT RUN rows are gates that remain open.

## Runs

| engine | scale | readers | days | status | wall_s | load_s | code |
|---|---|---|---|---|---|---|---|
| clickhouse | 100000 | 1 | 7 | MEASURED | 227.8 | 40.36 | 3d557ac2 |
| duckdb | 100000 | 1 | 7 | MEASURED | 44.0 | 15.13 | 3d557ac2 |

## Correctness

### Baseline parity (frozen current behavior, incl. limitations)

| check | engine | status | expected | actual |
|---|---|---|---|---|
| currency.cost_7d | clickhouse | PASS | 20.18883811908654 | 20.18883811908654 |
| dedup.raw_vs_distinct_7d | clickhouse | PASS | [7797, 7669] | [7797, 7669] |
| entity.current_rows | clickhouse | PASS | 10000 | 10000 |
| entity.deleted_still_visible | clickhouse | PASS | 100 | 100 |
| entity_join.order_paid_7d | clickhouse | PASS | 92127437 | 92127437 |
| funnel.signup_window_7d | clickhouse | PASS | [2223, 44, 0] | [2223, 44, 0] |
| identity.canonical_events_7d | clickhouse | PASS | {'top10': [['76bccbca-b862-57de-b8f3-c841d26318fa', 19], ['de369194-527e-5613-af… | {'top10': [['76bccbca-b862-57de-b8f3-c841d26318fa', 19], ['de369194-527e-5613-af… |
| late_events.bucket_7d | clickhouse | PASS | 399 | 399 |
| retention.week1_mature | clickhouse | PASS | [1632, 1281] | [1632, 1281] |
| sessionization.gap_violations | clickhouse | PASS | 0 | 0 |
| sessionization.sessions_7d | clickhouse | PASS | 7623 | 7623 |
| currency.cost_7d | duckdb | PASS | 20.18883811908654 | 20.18883811908654 |
| dedup.raw_vs_distinct_7d | duckdb | PASS | [7797, 7669] | [7797, 7669] |
| entity.current_rows | duckdb | PASS | 10000 | 10000 |
| entity.deleted_still_visible | duckdb | PASS | 100 | 100 |
| entity_join.order_paid_7d | duckdb | PASS | 92127437 | 92127437 |
| funnel.signup_window_7d | duckdb | PASS | [2223, 44, 0] | [2223, 44, 0] |
| identity.canonical_events_7d | duckdb | PASS | {'top10': [['76bccbca-b862-57de-b8f3-c841d26318fa', 19], ['de369194-527e-5613-af… | {'top10': [['76bccbca-b862-57de-b8f3-c841d26318fa', 19], ['de369194-527e-5613-af… |
| late_events.bucket_7d | duckdb | PASS | 399 | 399 |
| retention.week1_mature | duckdb | PASS | [1632, 1281] | [1632, 1281] |
| sessionization.gap_violations | duckdb | PASS | 0 | 0 |
| sessionization.sessions_7d | duckdb | PASS | 7623 | 7623 |

### Semantic gates (target semantics; parity on a limitation is not a pass)

| gate | engine | status | note |
|---|---|---|---|
| currency.mixed | clickhouse | NOT RUN | The Event schema carries only cost_usd Float32 — there is no multi-currency field to evaluate. Mixed-currency billing correctness is untestable in this harness. |
| dedup.read_time_guarantee | clickhouse | NOT RUN | Neither engine applies read-time dedup by default and the harness does not layer one on; the measured raw-vs-distinct gap quantifies exposure but is not a dedup guarantee. |
| entity.tombstone_delete | clickhouse | DIVERGENT | expected 0, got 100. Target semantics: a source delete must remove the row. Current schema has no tombstone, so both engines are expected to show the rows — that is a DIVERGENT gate result, not a pass. |
| currency.mixed | duckdb | NOT RUN | The Event schema carries only cost_usd Float32 — there is no multi-currency field to evaluate. Mixed-currency billing correctness is untestable in this harness. |
| dedup.read_time_guarantee | duckdb | NOT RUN | Neither engine applies read-time dedup by default and the harness does not layer one on; the measured raw-vs-distinct gap quantifies exposure but is not a dedup guarantee. |
| entity.tombstone_delete | duckdb | DIVERGENT | expected 0, got 100. Target semantics: a source delete must remove the row. Current schema has no tombstone, so both engines are expected to show the rows — that is a DIVERGENT gate result, not a pass. |

## Query latency (ms) and resources

| engine | shape | first p50 | first p95 | repeat p50 | repeat p95 | mem_peak MiB | cpu% peak | disk GiB |
|---|---|---|---|---|---|---|---|---|
| clickhouse | aggregate | 4002 | 4002 | 1105 | 1105 | 662 | 50.9 | 0.02 |
| clickhouse | entity_join | 2020 | 2020 | 1172 | 1172 | 662 | 50.9 | 0.02 |
| clickhouse | funnel | 1068 | 1068 | 432 | 432 | 662 | 50.9 | 0.02 |
| clickhouse | overview | 1014 | 1014 | 941 | 941 | 662 | 50.9 | 0.02 |
| clickhouse | retention | 9487 | 9487 | 1547 | 1547 | 662 | 50.9 | 0.02 |
| duckdb | aggregate | 42 | 42 | 35 | 35 | 425 | 39.2 | 0.11 |
| duckdb | entity_join | 344 | 344 | 149 | 149 | 425 | 39.2 | 0.11 |
| duckdb | funnel | 298 | 298 | 275 | 275 | 425 | 39.2 | 0.11 |
| duckdb | overview | 47 | 47 | 107 | 107 | 425 | 39.2 | 0.11 |
| duckdb | retention | 195 | 195 | 90 | 90 | 425 | 39.2 | 0.11 |

Latency columns: `first` is the first timed pass after oracle checks (not a true cold read); `repeat` is two further passes. n=1/2 samples are smoke-scale only. Memory is container cgroup usage, not process RSS.

## Ingest

| engine | rows | ack_s | visibility_lag_s | final_total |
|---|---|---|---|---|
| clickhouse | 10000 | 2.07 | 2.16 | 109538 |
| duckdb | 10000 | 0.24 | 0.26 | 109538 |

## Gate ledger

| gate | status | why |
|---|---|---|
| matrix 1000000 clickhouse | NOT RUN | free disk 16.1 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb | NOT RUN | free disk 16.1 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse | NOT RUN | free disk 16.1 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb | NOT RUN | free disk 16.1 GiB below the 40 GiB preflight |
| commit-before-ack crash/replay | NOT RUN | needs fault injection against a durable queue, not a benchmark |
| backup checkpoint + restore + replay | NOT RUN | needs a defined backup/restore procedure to exercise |
| cross-tenant / arbitrary-SQL isolation | NOT RUN | needs the project-isolated sandboxed execution env from strategy.md; this harness's docker-socket driver is trusted tooling, not that sandbox |
| query cancellation under load | NOT RUN | not exercised in this harness |

Vendor references: [DuckDB concurrency](https://duckdb.org/docs/current/connect/concurrency), [DuckDB tuning](https://duckdb.org/docs/current/guides/performance/how_to_tune_workloads), [DuckDB security](https://duckdb.org/docs/current/operations_manual/securing_duckdb/overview), [ClickHouse serving](https://clickhouse.com/resources/engineering/high-concurrency-sizing-user-analytics).
