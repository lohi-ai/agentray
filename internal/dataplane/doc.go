// Package dataplane is the AgentRay data layer: event capture, external
// source plugins, and the persistence those two write into.
//
//	capture (ingest)  →  NATS  →  ClickHouse
//	connector plugin  →  Engine  →  ClickHouse landing tables
//
// Subpackages: ingest, connector, store (Postgres+ClickHouse), usecase
// (analytics operations), alerting (threshold/anomaly → outbound notify).
//
// Import rules: this package (and its subpackages) must not import
// internal/channels, internal/workloads, internal/runtime, or internal/app.
// It may import internal/shared. Agents never import this package — they
// reach data through shared/opcore → usecase.
//
// Adding a source plugin
//
//  1. Create internal/dataplane/connector/<kind>.go.
//
//  2. Implement connector.Source (TestConnection, DiscoverSchema, PullRows, Close).
//
//  3. Register it from init:
//
//     func init() { connector.Register("stripe", openStripe) }
//
// The engine, HTTP admin, and UI pick the new kind up from connector.Kinds()
// with no other wiring. Do not give agents a DSN or a Source handle — they
// only see landed rows via run_sql.
package dataplane
