// Package shared is the catch-all for code that more than one layer needs and
// that is not itself a product layer.
//
//	config      — process env
//	cronx       — 5-field cron matcher
//	credential  — write-only secret vault
//	opcore      — operation/tool/HTTP/CLI/MCP adapter wall
//	mcpclient   — Model Context Protocol client (rides sandbox.NewGuardedClient)
//
// Import rules: shared must not import channels, workloads, runtime,
// dataplane, or app. Every layer and the composition root may import shared.
package shared
