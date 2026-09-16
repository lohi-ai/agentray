# Design: declarative board content model

Status: implemented (redesign slice, ticket 007). Supersedes the flat
"create a dashboard, then create N charts" authoring path as the *composed*
board surface; the flat path is preserved, not removed.

## The problem it solves

Before this model, a board was assembled by issuing calls in order:
`create_dashboard`, then one `create_chart` per tile. Three things follow from
that shape, and all three are product problems rather than code-style problems.

1. **The board is half-built between two calls.** A caller that fails halfway
   leaves a board with one tile on it, visible to whoever is looking at the
   dashboard while the agent retries.
2. **Nothing validates a tile.** A chart's `metric` is a free string; a chart's
   `sql` is free SQL. A graph can therefore be added that names a number no
   implementation computes, or that aggregates one, and the mistake surfaces as
   a plausible-looking chart rather than as a refusal.
3. **A metric's meaning lives in the read that produced it.** Every tile would
   have to explain itself, and an agent adding a graph has no contract to write
   against.

The declarative model addresses all three: a board's content is one document
written in one revision-fenced call, every tile references a metric the server
declares it can compute, and each metric carries its definition, unit and
required instrumentation.

## The two objects

### Metric catalog (`internal/dataplane/store/metric_catalog.go`)

`metric_definitions` — one row per metric, served to every consumer: the web
renderer, an MCP client, a Garden preset, and a SQL query against
`metric_definitions` (Postgres; `run_sql` is the DuckDB event sandbox and does
not see this table).

| Column | Meaning |
|---|---|
| `key` | Stable metric id (`active_users`, `retention_d7`, …) — what a tile names |
| `metric_version` | The contract that fixes the semantics (`overview.v3`) |
| `label`, `unit`, `kind` | How it is titled, what it counts (`people`, `percent`, `pageviews`), and its shape (`value` / `series` / `breakdown`) |
| `metric_group` | `overview` / `acquisition` / `monetization` / `usage` |
| `definition` | The sentence that explains the number, served beside it |
| `prerequisite` | What must be instrumented before it can compute (empty when nothing is missing) |
| `displays`, `params` | Which tile displays are legal, and which params a tile may set |
| `sort_order`, `is_system` | Display order; the catalog is system-owned |

**The declaration in Go is the authority; the rows are its projection.** The
migration upserts every declared metric on boot and deletes rows the declaration
dropped, so the served catalog can never advertise a metric the code no longer
implements. The definition text is a constant shared with the overview read
(`metricDefActiveUsers` and friends), which is what stops the printed
explanation from surviving a change to the computation it describes.

**A metric is declared only when a deterministic implementation exists.** The
catalog is a promise that `read_metric` returns a real number, a real named
empty state, or a named prerequisite — never an invented figure. Purchases,
subscriptions and crashes are therefore *absent*: nothing computes them today,
and a catalog entry would be a tile that draws nothing while claiming to be a
metric. They are added when their implementation is.

### Board document (`internal/dataplane/store/boards.go`)

A board stays a `dashboards` row; its content is three columns added to it:

| Column | Meaning |
|---|---|
| `board_key` | The stable name a declaration addresses the board by, unique per project when set |
| `definition` | The document: `{version, sections:[{key, title, description, tiles:[…]}]}` |
| `definition_updated_at` | When the content was declared. `NULL` = never declared |

A **tile** is a placement, and there are exactly three kinds:

- `kind: "metric"` — declares a catalog metric (`metric`), how to draw it
  (`display`), how wide it is (`span`), and optionally the range it covers
  (`params.period`, `params.platform`). The server computes it.
- `kind: "chart"` — places a saved chart (`chart_id`) that already belongs to
  this board. The chart row keeps owning its query; a tile only positions it.
- `kind: "funnel"` — declares an ordered event sequence (`steps`, 2–8
  distinct names). The reader runs it through the funnel insight over the
  board's selected range, so the tile stores the steps and never a result.

The three kinds are not three ways of saying the same thing: a metric tile
declares *what number this is*, a chart tile places *an artifact that already
exists*, and a funnel tile declares *which events to measure passage through*.
That is why a chart tile may not carry a `display` (its chart kind decides) or
`params` (its query and the reader's range decide), and a funnel tile may not
carry either (it has exactly one drawing, and the board's range decides).

## The operations

Declared once in `internal/dataplane/usecase/boards.go` and projected by opcore
onto REST (`POST /api/op/<name>`), the in-process agent tool, the CLI, and MCP:

| Operation | Access | What it does |
|---|---|---|
| `list_metrics` | `analytics:read` | The catalog: valid keys, labels, units, definitions, prerequisites, legal displays |
| `read_metric` | `analytics:read` | One metric over a range, with its state, value/series/rows and the range's evidence |
| `get_board` | `analytics:read` | A board by id or key: the document, the catalog entries and chart rows its tiles reference, and a warning per dangling reference |
| `save_board` | `dashboards:write` | Declares the content in one write; a metric tile's `target` object appends a version to the metric's project-scoped target history |
| `set_metric_target` | `dashboards:write` | Declares or clears (`clear: true`) a metric's target directly — the only way to remove one |

`read_metric` runs the **same deterministic overview read** the overview surface
runs and projects one metric out of it. There is no second SQL path and no
second opinion: a board tile and the overview tile cannot disagree about the
same number.

`get_board` serves `has_definition`, the resolved references, and `warnings`
rather than logging dangling tiles: the person reading a board is the one who
needs to know that a chart was archived or that a metric left the catalog.

## Declaration semantics

- **Whole-document replacement.** The declaration is the board's content; the
  same document declared twice leaves the same board. An empty document is a
  declaration too (a deliberately empty board), which is why `has_definition` is
  a stored fact and not "has any sections".
- **Revision fence.** Updating requires the revision `get_board` returned; a
  stale one conflicts instead of overwriting. A declaration that carries no
  revision is a *create*: against an existing key it conflicts rather than
  silently taking over a board the caller has not read.
- **Idempotency.** An `idempotency_key` makes a retry return its first result
  (the resolved board), and reusing the key with a different document conflicts.
- **Archive is respected.** Declaring onto an archived board is refused
  (`ErrBoardArchived`) — the archive is the reversible removal, and writing
  content into a board the owner took off the menu would resurrect it silently.
- **Bounded document.** ≤ 12 sections, ≤ 24 tiles per section, tile `span` 1–3.
  A board is rendered, not paginated.
- **Validated, not repaired.** An unknown metric, a display the metric's kind
  cannot carry, a duplicate key, a chart that is not on this board, a bad
  version, an out-of-contract period or an unknown platform are all refusals
  that name the offending tile and what to do about it. A declaration is
  composed by a model as often as by a person; silently dropping half of it is
  worse than refusing.

## Compatibility

- Every board created before this model exists serves `has_definition: false`
  with an empty document, and keeps rendering from its `charts` rows through the
  existing chart operations and endpoints. The declarative model is additive.
- The flat `charts` list stays the pre-declaration layout: a board with no
  declaration and a board declared empty are different facts, and both are
  visible.
- `list_dashboards` gained `board_key` and `definition_updated_at` (additive
  JSON fields) so a menu can address declared boards by key.

## Default analysis boards (shipped)

Acquisition, Monetization and Usage are declared boards addressed by
`board_key` (`acquisition`, `monetization`, `usage`). Boot and project
creation seed them via `EnsureDefaultBoards`; an existing key is left
alone. Tile titles follow App Store Connect analytics labels (2026-09-14);
values are AgentRay catalog metrics via the same overview read `read_metric`
projects. Apple-shaped metrics with no implementation are not catalog
entries — the destinations render them as named empty states.

Adding a metric still means adding the definition *and* the implementation
the read dispatches to.

## Non-goals

- Project-defined metrics: a project cannot declare a metric into the catalog,
  because nothing would compute it. The catalog is system-owned until a
  declarative computation exists.
- Cross-board shared sections, tile-level filters beyond period/platform, and
  per-tile caching: not needed to declare a graph, so not in the document.
