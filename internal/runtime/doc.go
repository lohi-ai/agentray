// Package runtime (import path internal/runtime, package name agentruntime) is
// the AgentRay runtime layer: AgentGarden authoring, the agentcore loop, policy,
// credentials, sandbox, memory, traces, Lab, and the scheduler that turns a
// channel envelope into a run.
//
// Import rules: this package may import agentcore, sandbox, opcore, usecase,
// storage, credential, httptool, and dataplane (via interfaces). It must not
// import internal/channels, internal/workloads, or internal/app.
//
// The portable libraries agentcore/ and sandbox/ stay at the module root so
// external importers (e.g. swatter) keep a stable path. They are the public
// face of this layer.
//
// Adding a new run trigger does not belong here — add a channel (see
// internal/channels). Adding a new agent persona does not belong here — add a
// workload pack (see internal/workloads). This package stays a generic
// governed runtime.
package agentruntime
