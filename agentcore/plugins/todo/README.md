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
[x] read the schema
[~] write the query
[ ] verify the numbers
```

An empty plan injects nothing, so the reminder appears only after the model has
actually written one.

#### Token effect

**Fixed** per turn, proportional to plan length — and **replaced**, not
accumulated: each turn's reminder supersedes the last rather than stacking.

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

- **The plan is not durable.** It lives in memory for the run; a crash-resumed
  run comes back with an empty plan, and the model has to rebuild it from the
  recovered transcript. Making it durable would mean a log entry kind, which is
  core's to own — see the goal plugin for how that split works.
- **One plan per run.** No nesting, no per-subagent plans; a spawned sub-agent
  gets its own `Store` or none.
- **Nothing verifies the plan.** An item marked `completed` is completed because
  the model said so. This is a focus mechanism, not an audit trail.
- **Status vocabulary is fixed** (`pending` / `in_progress` / `completed`). No
  `blocked`, which a long autonomous run arguably wants.
