// Package operator is reserved for operator-job packs: agents that act on
// other systems through http_request + write-only secrets + host allowlists.
//
// There is no shipped operator pack today. The live proof is a workspace-
// specific Garden agent (the novel-request moderator): persona + skill +
// allow_hosts + {{cred:NAME}} + a schedule/webhook trigger. When a reusable
// pack exists, register it from this directory with workloads.Register.
//
// Do not add an operator backend, tool kind, or run engine. If a capability
// cannot be expressed as an audited product endpoint + http_request, stop
// and ask before writing AgentRay code.
package operator
