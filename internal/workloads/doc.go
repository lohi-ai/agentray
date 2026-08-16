// Package workloads is the AgentRay business layer: Garden packs. A pack is
// config only — persona, scopes, skills, and optionally the names of non-scope
// tools it needs (Pack.Tools, e.g. web_fetch) — installed through the same RBAC
// setters a human editor uses. There is no per-pack backend.
//
// Categories track the phases of a product's life, not an org chart:
//
//	validate  — before there is a product: demand + message evidence, with an
//	            EMPTY dataplane by design (web_fetch and the owner's testimony)
//	growth    — measure → diagnose → test → learn (the product we sell)
//	marketing — content loop, sits under the growth job
//	data      — SQL/dashboard helper (utility, not a vertical)
//	operator  — watch product/runtime health; escalate once, file the fix
//	support   — reserved; a future pack + channel, not a new engine
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
