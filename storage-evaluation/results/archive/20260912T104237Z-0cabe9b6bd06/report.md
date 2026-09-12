# Storage evaluation report

Synthetic local corpus only. This is a decision input for docs/redesign/strategy.md, not a migration and not a production go/no-go. MEASURED rows were executed in this harness; NOT RUN rows are gates that remain open.

## Runs

| engine | scale | readers | days | status | wall_s | load_s | teardown | seed | corpus | code |
|---|---|---|---|---|---|---|---|---|---|---|
| clickhouse | 100000 | 1 | 7 | MEASURED | 15.3 | 2.68 | stopped | 20260912 | 60404e7b | fd74dde1 |
| duckdb | 100000 | 1 | 7 | MEASURED | 6.7 | 1.32 | stopped | 20260912 | 60404e7b | fd74dde1 |

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
| clickhouse | aggregate | 15 | 15 | 11 | 11 | 553 | 27.9 | 0.01 |
| clickhouse | entity_join | 28 | 28 | 46 | 46 | 553 | 27.9 | 0.01 |
| clickhouse | funnel | 16 | 16 | 17 | 17 | 553 | 27.9 | 0.01 |
| clickhouse | overview | 20 | 20 | 22 | 22 | 553 | 27.9 | 0.01 |
| clickhouse | retention | 71 | 71 | 68 | 68 | 553 | 27.9 | 0.01 |
| duckdb | aggregate | 9 | 9 | 7 | 7 | 375 | 22.1 | 0.11 |
| duckdb | entity_join | 12 | 12 | 12 | 12 | 375 | 22.1 | 0.11 |
| duckdb | funnel | 22 | 22 | 19 | 19 | 375 | 22.1 | 0.11 |
| duckdb | overview | 17 | 17 | 11 | 11 | 375 | 22.1 | 0.11 |
| duckdb | retention | 11 | 11 | 14 | 14 | 375 | 22.1 | 0.11 |

Latency columns: `first` is the first timed pass after oracle checks (not a true cold read); `repeat` is two further passes. n=1/2 samples are smoke-scale only. Memory is container cgroup usage, not process RSS; `—` means sampling produced no data, not zero usage. DuckDB serves every op — reads included — on one locked connection, so its reader-concurrency numbers are serialized throughput, not parallel serving.

## Ingest

| engine | rows | ack_s | visibility_lag_s | expected | final_total | readers overlapping ingest |
|---|---|---|---|---|---|---|
| clickhouse | 10000 | 0.04 | 0.05 | 109538 | 109538 | 1 |
| duckdb | 10000 | 0.11 | 0.11 | 109538 | 109538 | 1 |

`ack_s` is the local insert call returning — it is NOT a durable-queue commit-before-ack; that crash/replay gate stays NOT RUN below. `visibility_lag_s` is observed by post-ack count() polling (0.5s granularity upper bound). Overlap counts reader spans that began before the ack and ended after the ingest started.

## Gate ledger

| gate | status | why |
|---|---|---|
| matrix 1000000 clickhouse r1 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r1 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r1 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r5 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r5 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r5 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r20 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r20 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 clickhouse r20 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r1 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r1 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r1 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r5 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r5 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r5 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r20 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r20 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 clickhouse r20 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r1 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r1 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r1 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r5 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r5 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r5 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r20 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r20 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 1000000 duckdb r20 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r1 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r1 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r1 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r5 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r5 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r5 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r20 d7 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r20 d30 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| matrix 10000000 duckdb r20 d90 | NOT RUN | free disk 26.2 GiB below the 40 GiB preflight |
| commit-before-ack crash/replay | NOT RUN | needs fault injection against a durable queue, not a benchmark |
| backup checkpoint + restore + replay | NOT RUN | needs a defined backup/restore procedure to exercise |
| cross-tenant / arbitrary-SQL isolation | NOT RUN | needs the project-isolated sandboxed execution env from strategy.md; this harness's docker-socket driver is trusted tooling, not that sandbox |
| query cancellation under load | NOT RUN | not exercised in this harness |

Vendor references: [DuckDB concurrency](https://duckdb.org/docs/current/connect/concurrency), [DuckDB tuning](https://duckdb.org/docs/current/guides/performance/how_to_tune_workloads), [DuckDB security](https://duckdb.org/docs/current/operations_manual/securing_duckdb/overview), [ClickHouse serving](https://clickhouse.com/resources/engineering/high-concurrency-sizing-user-analytics).
