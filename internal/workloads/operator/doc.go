// Package operator is reserved for operator-job packs: agents that act on
// other systems through http_request + write-only secrets + host allowlists.
//
// There is still no shipped pack in this package. The live proof of the shape
// is a workspace-specific Garden agent (the novel-request moderator): persona +
// skill + allow_hosts + {{cred:NAME}} + a schedule/webhook trigger. When a
// reusable one exists, register it from this directory with workloads.Register.
//
// Not to be confused with the `operator` marketplace *category*, which does now
// ship a pack: `ops-watch` (in presets.go) is a read-and-report watcher over the
// project's own event store — activity_summary, run_sql, send_notification — and
// touches no other system, so it needs none of the machinery above. The
// distinction that matters is reach, not naming: a pack that only reads AgentRay
// belongs with the other presets; a pack that acts on someone else's system
// belongs here, behind allowlists and a vault.
//
// Do not add an operator backend, tool kind, or run engine. If a capability
// cannot be expressed as an audited product endpoint + http_request, stop
// and ask before writing AgentRay code.
package operator
