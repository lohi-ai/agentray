// Package channels is the AgentRay channel layer: how work arrives at a
// runtime. A channel is an adapter that turns a native payload (HTTP body,
// cron tick, MCP request, future Slack/email/voice message) into a runtime
// DispatchRequest. It must not contain business logic.
//
// Import rules: this package must not import dataplane, workloads, storage,
// or app. It may document runtime.DispatchRequest but must not import
// runtime internals (the composition root maps Envelope → DispatchRequest).
//
// Adding a channel
//
//  1. Add a Kind constant below (or in kind.go).
//  2. Register it from init via Register(Info{...}).
//  3. In internal/app, mount the HTTP (or other) ingress and convert the
//     native payload with NewEnvelope, then call runtime.Dispatcher.
//
// Do not add a new run engine. Async channels (schedule, webhook, future
// Slack) share the scheduler's NATS path. Sync channels (chat, lab) call
// Runner directly because they stream. MCP is a tool projection of opcore,
// not an agent run — it is still a channel because it is an ingress.
//
// Voice / omnichannel support is a future adapter in this package, not a
// new backend. It must land transcripts as events on the same person graph.
package channels
