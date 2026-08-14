# definition

**Seam.** Not ejectable in practice — the definition is who the agent *is*.

Installs the `AgentDefinition` (soul, operating instructions, skills, scope),
the run's `Limits`, and its `Env` (sandbox, credential resolver).

## Model Experience

### Every request

#### What the model sees

The system prompt: the persona (`SOUL.md`), the operating instructions
(`AGENTS.md`), and the *names and descriptions* of enabled skills — not their
bodies.

#### Token effect

**Fixed** and **retained**: this text is in the prefix of every single provider
call in the run. It is the most expensive prose in the agent, paid for on every
turn, which is why skill bodies are deliberately left out of it.

#### KV cache effect

**Prefix-stable**, and it is the *reason* prefix caching pays: the definition
sits at the very front, unchanged for the whole run. Anything that mutates it
mid-run invalidates every downstream cache entry.

### The model calls `read_skill`

#### What the model sees

The full body of one skill, on demand.

#### Token effect

**Conditional.** A skill body costs nothing until the model decides it needs it.
`read_skill` bypasses the permission gate — it can only return skill bodies from
this agent's own definition, so it grants nothing new.

## Impact on the agent

- `Limits` bounds turns, tool calls, context tokens, and tool-result bytes. The
  tool-result bound is the fallback other plugins (spill) may take over.
- `Env.Credentials` is what makes `{{cred:NAME}}` resolve at the trust boundary —
  after tracing and gating, immediately before execution — so a secret never
  enters model context or a persisted trace.
- `Env.Sandbox` is what a shell/browser tool needs to exist at all.

## Known limitations and deferred work

- **Skills are selected by the model, not retrieved.** Selection is by
  name/description alone; there is no relevance ranking over skill content.
- **No per-skill token accounting.** A skill body that blows the context is only
  visible after the fact.
