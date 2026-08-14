# preset

**Composition, not a capability.** This package has no plugin of its own — it
assembles the others.

`preset.Plugins(cfg)` returns the plugin list that reproduces
`agentcore.New(cfg)`, and `preset_test.go` proves the two agree field by field
via `Describe()`. If the plugin surface ever drifts from `Config`, that test
fails.

`preset.Full(cfg, opts)` is that list **plus** the capabilities `Config` has no
field for: `spill`, `jobs`, `repeat_guard`, `session_query`, and the two
`observe` plugins. The split is load-bearing — `Plugins` is pinned to
`agentcore.New` parity and must stay a pure mirror of `Config`, so the
capabilities that answer to no `Config` field live one layer up. **`Full` is
where a deployment starts; `Plugins` is where the parity proof lives.**

Every `Options` field degrades to *off*, never to *wrong*. A nil `Spill` leaves
the loop's own head+tail truncation in place rather than minting locators into a
store that dies with the process — the locator is written to the durable session
log, so an in-memory store would make a resumed run read its own spill back as
not-found.

## Model Experience

None. Composition happens before any model call.

## Impact on the agent

It is the starting point for a custom agent. Take the default list, drop or
replace what you want to change, append your own, and build:

```go
ps := preset.Plugins(cfg)
ps = preset.Replace(ps, myDriverPlugin{})        // swap the control flow (r.SetDriver)
ps = append(ps, myCapability{})                  // add a capability
agent, err := agentcore.Build(ps...)
```

The driver seam has no preset entry: `Build` installs the default reason→act
driver when nothing claims it, so swapping control flow is a plugin whose
`Register` calls `r.SetDriver(myDriver)`.

Two tests here carry the architectural claims:

- **`TestPluginCompositionMatchesNew`** — the two entry points cannot drift.
- **`TestEjectRemovesEveryTrace`** — building the same agent with and without a
  capability leaves *no sign* of it in the one without: no tool, no name in the
  extension list. This is the test that fails if someone re-couples the loop to
  a plugin by name, because the loop would keep doing the work with the plugin
  gone.

## Known limitations and deferred work

- **Parity is checked through `Describe()`**, so a Config field that `Describe`
  does not report could drift undetected. The test guards against a vacuous pass
  by asserting the dump is not thin, but that is a proxy, not a proof.
- **`Replace` appends.** A replacement lands at the end of the list rather than
  in the original position. Harmless today — seams are keyed and hooks are
  prioritized — but it means list order is not a stable diagnostic.
