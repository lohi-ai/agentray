# Query access contract

AgentRay serves one guarded DuckDB query path to legacy REST (`POST
/api/sql/run`), operation REST (`POST /api/op/run_sql`), MCP, in-process runtime
tools, saved queries, and saved SQL charts. Each adapter reaches the same
`run_sql` operation and storage guard; adapters may wrap the result differently,
but must return the same authorized rows and error kind.

## SQL and tenant boundary

- Input remains `{ "sql": "…" }` and accepts one DuckDB `SELECT` (including
  `WITH [RECURSIVE]`). DDL/DML, multiple statements, external table functions,
  file/URL table reads, `ATTACH`, `INSTALL`, `COPY`, and dynamic SQL are denied.
- `events` may appear exactly once directly after `FROM`. `external_rows` may
  appear directly after `FROM` or `JOIN`. Comma joins, quoted tenant-table names,
  and `JOIN events` are rejected. Source-looking text inside strings or comments
  is not rewritten.
- The server replaces those names with project-scoped CTEs and supplies the
  project argument. A SQL-provided `project_id` predicate can only narrow the
  already-scoped rows; it cannot cross projects.
- `events.canonical_id` is the identity-stitched person key. Use raw
  `distinct_id` only for an exact-id lookup.

`external_rows` stores current connector state, not history. Its identity is
`(project_id, connector_id, table_name, row_key)`. Every reference CTE must
filter both `connector_id` and `table_name`; two connectors may expose the same
table and row key. Extract fields from `data` with
`json_extract_string(data, '$.field')` (or `json_extract` for JSON values), and
aggregate one-to-many facts by the intended user/date grain before joining them
to people. `synced_at` is landing time, not necessarily source event time.

Reusable evidence queries return:

```text
date, series, value, unit, sample_size, state, reason
```

Cohort rows additionally use `cohort_date, age_days, eligible, converted`.
These columns describe computation, not source readiness; additive readiness
metadata belongs to the data-readiness contract and adapters must preserve it.

## Numbers and saved-chart ranges

DuckDB HUGEINT and DECIMAL values cross the API as exact strings. Valid JSON
numbers remain numbers and NULL remains NULL. The sandbox rejects NaN or
infinity recursively before it reports a successful result. Consumers must not
coerce an unsafe integer, null, missing field, non-finite value, or unparseable
string to zero; table/SQL inspection remains available for exact strings that a
chart cannot safely plot.

A range-driven saved SQL chart opts in with both quoted tokens:

```sql
WHERE source_time >= '{{from}}' AND source_time < '{{to}}'
```

The client resolves the applied range once to UTC (`from` inclusive, `to`
exclusive) without rounding sub-day precision. `{{hours}}` may also be used but
does not replace the two absolute tokens. Tokens in comments do not count.
Unknown, unquoted, missing, partial, reversed, or invalid bindings fail closed
with “This query cannot apply the selected date range”; the query is not sent.
On a valid range the card displays the exact applied bounds, clears old data
while loading, ignores stale responses, and distinguishes empty, SQL error,
capacity refusal, and non-plottable numeric results.

## Two independent permission systems

Network adapters authorize management credentials by Access class:

| Purpose | Required credential scopes |
|---|---|
| Investigation over events and connected-source status | `analytics:read` + `sources:read` |
| Board/chart authoring | add `dashboards:write` |
| Finding/experiment authoring | add `plans:write` |
| Memory/notification writes | narrowly add `growth:write` |
| Source lifecycle operations | separate `sources:manage` credential (implies `sources:read`) |

`sources:read` alone cannot run analytics; `analytics:read` alone cannot manage
sources or author boards. Capture, missing, invalid, revoked, or foreign
credentials fail closed. `tools/list` advertises only authorized operations and
`tools/call` re-authorizes the requested name. A read-only saved-query caller
does not update `result_cache`, and a missing/foreign saved-query id returns a
non-disclosing error.

Runtime Scopes (`monitor`, `data_quality`, `analyze_build`, `growth_suggest`)
select tools inside an already-authorized agent run; they are not network
credentials. `data_quality` and `analyze_build` currently grant `run_sql`.
`analyze_build` also includes source and board mutations, and `growth_suggest`
includes finding, memory, and notification writes. `ReadOnly` keeps classified
project reads while removing board/source/finding/memory/notification writes,
HTTP/shell tools, plugins, and delegation.

## Errors and capacity evidence

SQL/validation/result-limit failures are author-correctable 400-class errors.
A sandbox child outage, admission saturation, timeout, or engine resource
refusal is `retryable`; REST uses HTTP 503 and the legacy route adds
`Retry-After: 5`, while MCP carries the same semantic kind in
`_meta.error_code`. Error text is sanitized and must not echo a DSN, credential,
foreign saved SQL, or filesystem path.

The default sandbox admits two concurrent requests and retains two live project
children; each child has a 96 MB DuckDB memory limit, 256 MB spill limit, 30 s
execution timeout, and the whole request is bounded to 60 s. A three-project,
four-investigator workload can therefore produce explicit bounded refusals.
Refusal is an accepted isolation outcome, not a latency pass.

Capacity acceptance must run on a named disposable Linux host with 10 million
events plus 1 million external rows per project across three projects, four
concurrent investigators, 30 cold samples, 100 warm samples, and at least ten
minutes of 100 accepted events/second. Declare the resource and an existing
artifact directory; enabling the suite with either missing fails:

```sh
export AGENTRAY_QUERY_CAPACITY=1
export AGENTRAY_QUERY_CAPACITY_HOST='cpu=… ram=… disk=… cgroup=…'
export AGENTRAY_QUERY_CAPACITY_ARTIFACT_DIR=/absolute/disposable/evidence
export AGENTRAY_TEST_DATABASE_URL='postgres://…/disposable?sslmode=disable'

go test ./internal/dataplane/store -run '^TestQueryCapacity' -count=1 -timeout 90m -v
go test ./internal/app -run '^TestQueryCapacity' -count=1 -timeout 90m -v
go test ./internal/dataplane/store -run 'SandboxIsolationHostileQuery|SandboxRequestDeadlineCoversAllPhases|SandboxBudgetFitsContainer|SandboxChildInheritsNoSecrets|SandboxWaiterTimeoutDoesNotKillTheChild' -count=1 -v
```

The suites retain corpus/host dimensions, query hash and plan, seed/cold/warm
durations, concurrent requests, accepted writes, refusals, and HTTP refusal
envelopes as JSON. A successful supported run targets warm p95 at or below 5 s
and cold whole-request time at or below 30 s. Local macOS tests validate the
contract but cannot establish Linux RLIMIT/OOM or approved-corpus capacity.
