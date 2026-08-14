# sandbox

**Hybrid — seam + enforcement, the same shape as [`goal`](../goal/).** One
plugin contributes the isolation backend *and* the guard that reads what is sent
into it, because installing the substrate and forgetting the guard is a failure a
composition must not be able to express.

(The permission gate pairs the same way, but there the kernel does it:
`Registry.UsePolicy` claims the seam and adds the `PriorityGate` hook in one
call, so [`policy`](../policy/) is an adapter rather than a hybrid. Sandbox
cannot borrow that trick — the guard is the *host's*, not the kernel's, so the
pairing has to be enforced by this package's `Register`.)

Two plugins live here:

| plugin | seam | what it adds |
| --- | --- | --- |
| `sandbox.Plugin` (`In`, `Guarded`) | `sandbox` | the execution substrate + a `BeforeToolCall` guard at `PriorityGate` |
| `sandbox.Credentials` (`Vault`) | `credentials` | the secret resolver consulted at the trust boundary |

## Model Experience

### A blocked call

#### What the model sees

The guard's block reason, as the tool result. The tool never runs and never sees
the arguments — the model is told why, so it can correct course instead of
retrying blind.

#### Token effect

**Fixed** and small, but recurring: a model that keeps re-issuing a blocked call
pays for every refusal. `repeatguard` is what notices that chain.

### An allowed call

Nothing from this plugin. **Zero-direct** tokens; sandboxing is invisible to the
model, which is the point — it constrains what the *host* is exposed to, not
what the model may ask for.

## Impact on the agent

- The guard registers at **`PriorityGate`**, beside the permission gate and
  ahead of every consumer hook, so a call it blocks is never observed as allowed
  by anything downstream.
- **The guard's rules are not in this package.** What counts as injection or
  exfiltration is a product judgment that changes per deployment, so it arrives
  as configuration. This package owns the wiring and the ordering.
- `sandbox` and `credentials` are **separate seams inside one `Env`**, folded in
  at build time rather than merged at registration. That is what makes the
  composition order-independent: a plugin that installs a whole `Env`
  (`definition`) and one that installs a single capability cannot clobber each
  other, whichever was listed first.
- Registering the backend does **not** expose a tool. Tool exposure stays
  policy- and catalog-driven; a wired sandbox alone gives the model nothing.

## Known limitations and deferred work

- **The guard is one hook, not a chain.** A composition needing several
  independent guards installs the extra ones through `hooks.Plugin` at
  `PriorityGate` itself.
- **No per-tool scoping.** The guard sees every tool call, not only the ones
  that reach the sandbox. That is deliberate for now (injection text is
  dangerous wherever it lands), but it means a guard tuned for shell arguments
  also reads analytics queries.
