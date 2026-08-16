# todo

**Contribution.** A tool plus a context hook. Ejectable — without it a long run
loses its checklist and drifts off the original task as compaction summarizes the
early turns away.

The plan is deliberately **not** part of the transcript. It lives in a `Store`
the tool writes and the hook reads, so compaction can never trim it. That is the
whole point of the capability, and it is why the plan is pinned to the *request
view* rather than appended to history.

## Model Experience

### Every request, once a plan exists

#### What the model sees

A trailing system reminder, rebuilt from the store on every turn:

##### Verbatim text

```
[run plan]
Current plan (your live todo list — keep it updated with update_plan):
[x] (187 earlier steps completed)
[x] read the schema
[~] write the query
[ ] verify the numbers
```

An empty plan injects nothing, so the reminder appears only after the model has
actually written one. The count line appears only once steps have been folded
(see below); it is not an item.

#### Token effect

**Fixed** per turn and **bounded** — capped at `maxRenderBytes` (2 KB, ~500
tokens) no matter how long the plan gets. It is **replaced**, not accumulated:
each turn's reminder supersedes the last rather than stacking.

#### KV cache effect

**Replacing.** The reminder is the final message, and its content changes
whenever the plan does, so the tail of the prefix is invalidated on any plan
change. The persona/skills prefix ahead of it is untouched.

### The model calls `update_plan`

#### What the model sees

`Plan updated.` followed by the re-rendered checklist, so the write is confirmed
against what was actually stored rather than what was sent.

#### Token effect

**Fixed**, small. The turn is **refunded against `MaxTurns`** — see below.

#### KV cache effect

**Append-only** for the call and its result.

## Impact on the agent

- **The plan survives compaction by construction.** Even after the original task
  and every early turn are summarized away, the freshly rendered checklist is in
  front of the model.
- **Plan turns are free.** `update_plan` declares itself
  `agentcore.BookkeepingTool`, so a turn spent only on plan updates is refunded
  against `MaxTurns`. Without this, a careful planner finishes fewer steps than a
  careless one on the same budget. `MaxToolCalls` still bounds a runaway planner.
- **The repeat guard ignores it.** A plan tool repeats by design; counting it
  would fire a loop warning at an agent doing exactly what it was told, and would
  let a real loop launder itself behind interleaved plan updates. The guard
  learns this from the run (`RunInfo.Bookkeeping`), not by importing this
  package.
- **Full replace, not delta.** The model always sends the whole list, so the
  store cannot drift out of sync with what the model believes the plan is.
- **At most one `in_progress` item**, enforced by the tool. A plan with three
  things in progress is not a plan.
- **The plan has a ceiling, because surviving compaction is not the same as
  being free.** The block is pinned into every request and nothing else in the
  system will ever bring it back down — the property that makes it useful is the
  property that makes it dangerous. An agent that decomposes as it discovers
  (what a long run does) appends steps and leaves finished ones behind as the
  record that the work happened; measured over 900 turns that reached **25 KB in
  every call, 39% of a 4000-token window**, growing forever. Three bounds now
  apply: completed steps beyond the most recent five are folded into a running
  count (`Store.Set`, `Store.Retired`), each step's text is clamped to 160 bytes,
  and the whole render is capped at 2 KB. Same run, after: **623 bytes, 1%.**
- **Folding drops with a priority, not a rule of thumb.** The step the agent is
  ON is reserved before anything else can crowd it out — it is the one line the
  model cannot choose its next action without — and the remainder is accounted
  for (`… (N further steps in the plan, not shown here)`) rather than silently
  vanished. Progress stays legible through the count, so a bounded plan does not
  send the agent to redo finished work.
- **The fold is applied in the store, not just in the render**, so what the model
  reads and what the store holds are the same list. A model shown a folded list
  sends the folded list back; a store that quietly held more would re-expand the
  render on the next write. `Retired` is a floor, never an overcount.
- **A crashed run gets its plan back.** `BeginRun` reconstructs the checklist
  from the `update_plan` calls already in the durable log — reading through
  `RunInfo.Session`, writing nothing, so the loop stays the only writer. Only a
  call that would be *accepted* counts: a rejected one left the original run's
  plan unchanged, so replaying it would install a plan that run never had. A
  finished run's plan is **not** inherited — `EntryLeaf` clears it, the same rule
  the goal gate uses, so a new task chained onto the session starts clean.

## Composition

```go
store := todo.NewStore()                       // one per run
agentcore.Build(/* … */, todo.With(store))
```

The tool and the hook are registered together because either alone is broken:
the tool without the hook writes a plan the model never sees again, and the hook
without the tool pins a plan nothing can write.

A composition with no `Store` is refused rather than silently downgraded — a
plan tool that forgets is worse for the model than no plan tool at all.

## Known limitations and deferred work

- **Recovery reads the whole log.** `BeginRun` scans for the last accepted
  `update_plan`, so on a windowed store whose window no longer reaches that call,
  the plan does not come back — no worse than before the recovery existed, but
  not a guarantee either. A dedicated entry kind would fix it, and that is core's
  to own; see the goal plugin for how that split works.
- **Folded steps are gone from the model's view for good.** The running count
  preserves that they happened, not what they were. A run that needs to recite
  its completed work should put that in its answer, not rely on the checklist.
  The full un-folded list is still in the durable log, which is what
  `internal/runtime`'s `/plan` command reads — so a human sees every step even
  though the model sees a summary of the old ones.
- **One plan per run.** No nesting, no per-subagent plans; a spawned sub-agent
  gets its own `Store` or none.
- **Nothing verifies the plan.** An item marked `completed` is completed because
  the model said so. This is a focus mechanism, not an audit trail.
- **Status vocabulary is fixed** (`pending` / `in_progress` / `completed`). No
  `blocked`, which a long autonomous run arguably wants.
