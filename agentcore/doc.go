// Package agentcore is a reusable, product-agnostic agent runtime: the turn
// loop, tool calling, hooks, permissions, memory, and the Agent Definition. It
// knows nothing about analytics or any other product. A consumer injects a
// ToolSet, a Policy, a MemoryStore, and an AgentDefinition; agentcore drives
// them.
//
// # Boundaries
//
// Two rules define this package, and both are tests rather than promises (see
// boundary_test.go):
//
//   - The kernel depends on nothing else in this module. agentcore imports only
//     the standard library and golang.org/x/text. Everything product-specific
//     enters through an interface declared here — including the model itself:
//     agentcore declares [LLMProvider] and never speaks a wire protocol. The
//     OpenAI and Anthropic implementations live in agentray/ai, which imports
//     this package, so a vendor change cannot reach the loop.
//   - The kernel names no plugin. Delete every package under agentcore/plugins
//     and this package still compiles, still runs, and still passes its tests —
//     it just does less.
//
// # Composition
//
// An agent is not constructed, it is composed. A [Plugin] contributes to a
// [Registry]; [Build] turns a plugin set into an [Agent]. There is no privileged
// core to patch — agentcore's own behavior is just the default plugin set, and
// any of it can be replaced by registering something else in its place.
//
//	agent, err := agentcore.Build(
//		model.Plugin(...),      // seam: provider, ladder, retry
//		definition.Plugin(...), // seam: persona, skills, limits
//		policy.Plugin(...),     // seam: the permission gate
//		todo.Plugin(...),       // extension: adds a tool and a step interceptor
//	)
//
// [New] is the same composition reached through a flat [Config] instead of a
// plugin list; both write through the same exported Registry setters, and
// agentcore/plugins/preset carries a parity test proving they agree.
//
// A plugin contributes in exactly one of three ways, and which one it is can be
// read off its Register body:
//
//   - Seam — r.SetX(...). Keyed, exactly one provider, a second claim is a build
//     error naming both plugins. Bought property: replaceability.
//   - Extension — r.AddExtension(...). Ordered by explicit Priority, never by
//     registration order. This is how a capability reaches a running agent.
//   - Additive — r.AddTools / r.AddHooks / r.WrapProvider. Accumulates.
//
// # Reading this package
//
// The root is deliberately flat: these files are not independent concerns that
// happen to sit together, they are one machine reached through unexported
// fields of one [Agent]. The real boundary is agentcore/plugins, so the
// organizing question here is not "which folder?" but "why is this file in the
// kernel and not a plugin?". Every file answers with one of four words — loop,
// contract, log, or seam default — and README.md holds the map, which
// TestEveryKernelFileJustifiesItself gates.
//
// The layers that map runs through, outermost first:
//
//   - Composition — doc.go, plugin.go, compose.go, agent.go, describe.go.
//   - Contracts — provider.go, tool.go, permission.go, definition.go, hooks.go,
//     extension.go, memory.go, embed.go, env.go, sandbox.go. The types a plugin
//     outside this repo implements.
//   - The loop — driver.go, loop.go, turn.go, tooldispatch.go, result.go,
//     limits.go, prompt.go, cacheanchor.go, retry.go, schema.go, skill_tool.go.
//   - Durable state — session.go, session_tree.go, memsession.go, goal.go,
//     compaction.go, compactor.go, idempotency.go, delegation.go, fork.go,
//     lab.go. The loop is the only writer; everything else reduces the log.
//
// One file sits outside those layers on purpose: faux.go, a scripted
// [LLMProvider] that makes the loop, the hooks and the gate exercisable with no
// network and no key. It ships in the kernel rather than a test package because
// the plugins are tested against it too, and a plugin proving its behavior
// against the real loop is the point of the whole split.
package agentcore
