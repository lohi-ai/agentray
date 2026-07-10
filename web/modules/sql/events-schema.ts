// The events table is the one table SQL queries hit. We mirror its ClickHouse
// DDL (internal/storage/store.go) here so the editor can autocomplete columns and
// the schema-reference panel can list them with types — without a round-trip.
// Keep in sync with the `CREATE TABLE events` statement in store.go.

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
  { name: 'distinct_id', type: 'String', note: 'Stable per-user/visitor id' },
  { name: 'session_id', type: 'String', note: 'Session id' },
  { name: 'event_name', type: 'LowCardinality(String)', note: 'The event, e.g. page_view' },
  { name: 'event_type', type: 'LowCardinality(String)', note: 'Category of event' },
  { name: 'properties', type: 'String', note: 'JSON blob of custom props' },
  { name: 'agent_id', type: 'Nullable(String)', note: 'Agent that emitted the event' },
  { name: 'tool_name', type: 'Nullable(String)', note: 'Tool invoked' },
  { name: 'tool_input', type: 'Nullable(String)' },
  { name: 'tool_output', type: 'Nullable(String)' },
  { name: 'tokens_input', type: 'Nullable(UInt32)' },
  { name: 'tokens_output', type: 'Nullable(UInt32)' },
  { name: 'cost_usd', type: 'Nullable(Float32)' },
  { name: 'latency_ms', type: 'Nullable(UInt32)' },
  { name: 'model_name', type: 'Nullable(String)' },
  { name: 'is_error', type: 'UInt8', note: '1 when the event represents an error' },
  { name: 'error_message', type: 'Nullable(String)' },
  { name: 'timestamp', type: "DateTime64(3, 'UTC')", note: 'When the event occurred' },
  { name: 'inserted_at', type: "DateTime64(3, 'UTC')", note: 'When it was ingested' },
  { name: 'visitor_class', type: 'LowCardinality(String)', note: 'human / bot / …' },
  { name: 'bot_name', type: 'Nullable(String)' },
  { name: 'referrer_host', type: 'Nullable(String)' },
  { name: 'referrer_channel', type: 'LowCardinality(String)' },
  { name: 'user_agent', type: 'Nullable(String)' },
  { name: 'insert_id', type: 'Nullable(String)', note: 'Idempotency key' },
];

export const EVENTS_COLUMN_NAMES = EVENTS_COLUMNS.map((c) => c.name);
