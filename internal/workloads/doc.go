// Package workloads is the AgentRay business layer: Garden packs. A pack is
// config only — persona, scopes, skills — installed through the same RBAC
// setters a human editor uses. There is no per-pack backend.
//
// Categories:
//
//	growth    — measure → diagnose → test → learn (the product we sell)
//	operator  — act on other systems via http_request + secrets
//	support   — reserved; a future pack + channel, not a new engine
//	data      — SQL/dashboard helper (utility, not a vertical)
//	marketing — content loop, sits under the growth job
//
// Import rules: this package is pure config. It must not import storage,
// dataplane, runtime, channels, or app.
//
// Adding a pack
//
//  1. Write a Pack literal in this package (or a subfolder).
//  2. Register it from init: Register(myPack).
//  3. That is the whole change. Marketplace UI, install, and seed read the
//     registry. Do not add internal/growth or internal/support packages.
package workloads
