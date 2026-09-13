// The events table is the one table SQL queries hit. We mirror its DuckDB
// DDL (internal/dataplane/store/duckdb.go) here so the editor can autocomplete
// columns and the schema-reference panel can list them with types — without a
// round-trip. Keep in sync with the `CREATE TABLE events` statement in
// duckdb.go.

export type EventColumn = {
  name: string;
  type: string;
  // One-line hint shown in the schema panel + autocomplete detail.
  note?: string;
};

export const EVENTS_TABLE = 'events';

export const EVENTS_COLUMNS: EventColumn[] = [
  { name: 'project_id', type: 'UUID', note: 'Owning project (auto-scoped per query)' },
  { name: 'event_id', type: 'UUID', note: 'Unique event id' },
  { name: 'distinct_id', type: 'VARCHAR', note: 'Stable per-user/visitor id' },
  { name: 'session_id', type: 'VARCHAR', note: 'Session id' },
  { name: 'event_name', type: 'VARCHAR', note: 'The event, e.g. page_view' },
  { name: 'event_type', type: 'VARCHAR', note: 'Category of event' },
  { name: 'properties', type: 'VARCHAR', note: 'JSON blob of custom props' },
  { name: 'agent_id', type: 'VARCHAR', note: 'Agent that emitted the event' },
  { name: 'tool_name', type: 'VARCHAR', note: 'Tool invoked' },
  { name: 'tool_input', type: 'VARCHAR' },
  { name: 'tool_output', type: 'VARCHAR' },
  { name: 'tokens_input', type: 'UINTEGER' },
  { name: 'tokens_output', type: 'UINTEGER' },
  { name: 'cost_usd', type: 'FLOAT' },
  { name: 'latency_ms', type: 'UINTEGER' },
  { name: 'model_name', type: 'VARCHAR' },
  { name: 'is_error', type: 'BOOLEAN', note: 'true when the event represents an error' },
  { name: 'error_message', type: 'VARCHAR' },
  { name: 'timestamp', type: 'TIMESTAMPTZ', note: 'When the event occurred' },
  { name: 'inserted_at', type: 'TIMESTAMPTZ', note: 'When it was ingested' },
  { name: 'visitor_class', type: 'VARCHAR', note: 'human / bot / …' },
  { name: 'bot_name', type: 'VARCHAR' },
  { name: 'referrer_host', type: 'VARCHAR' },
  { name: 'referrer_channel', type: 'VARCHAR' },
  { name: 'user_agent', type: 'VARCHAR' },
  { name: 'insert_id', type: 'VARCHAR', note: 'Idempotency key' },
  { name: 'is_unplanned', type: 'BOOLEAN' },
  { name: 'platform', type: 'VARCHAR', note: 'web / ios / android / server' },
];

export const EVENTS_COLUMN_NAMES = EVENTS_COLUMNS.map((c) => c.name);
