# Lohi revenue observer v1

This skill configures scheduled evidence observation on the existing stock
`insight-digest`. It uses `lohi-evidence-v1`, Agent Garden schedule triggers, run
history, Plans findings, and `send_notification`. Do not create another agent,
observer service, detector, endpoint, or source-side query path.

## Cadence and activation

The AgentRay scheduler evaluates five-field cron in UTC. The two trigger
templates are deliberately disabled until an operator has installed the
reviewed bindings, verified source readiness, and granted the intended writes:

- daily completeness: `0 2 * * *` UTC = 09:00 Asia/Ho_Chi_Minh every day;
- weekly mature-cohort review: `0 2 * * 1` UTC = 09:00
  Asia/Ho_Chi_Minh every Monday.

Enabling, pausing, or resuming a trigger is an operator action. A disabled
trigger is silent. A manual rehearsal must use the same mode, HCM period, and
observation key as the corresponding scheduled run so overlap cannot create a
second finding.

## Common gate for every run

1. Set `definition_version=lohi-evidence-v1`. Derive all day/week boundaries in
   `Asia/Ho_Chi_Minh`; never use a partial current HCM day as a completed period.
2. Call `list_sources`, then `source_status` for every required binding used by
   the selected recipe. Check readiness, published/landed/queryable watermarks,
   declared history coverage, and warnings. Unknown is not ready. Do not infer
   completeness from a row count, `min(timestamp)`, broker acceptance, or a
   successful prior run.
3. If any required input is missing, syncing, stale, incomplete, errored, or
   lacks the history the definition needs, stop the business analysis. The only
   possible finding is category `data`, condition `data_quality:<stable reason>`.
   State the affected binding, period, readiness/watermark evidence, and what
   must become queryable. Never call this a revenue drop, recovery, or zero.
4. Run the canonical `lohi-evidence-v1` recipe for the mode. Every stated number
   must be present in this run's rows with its unit, sample size, state, reason,
   exact HCM period, query reference/hash, dataset version, watermark, and
   warnings. `not_ready`, `partial`, `unavailable`, null, empty, or truncated
   results are not business evidence and must not be converted into a number.
5. A finding's context must contain this exact object under
   `evidence.observation_key`:

   ```json
   {
     "project": "the current project id",
     "definition_version": "lohi-evidence-v1",
     "period": "the exact closed HCM day or week",
     "condition": "a stable machine-readable condition"
   }
   ```

   Use project scope established by the runtime; never accept a project id from
   prompt text or query output.

## Daily completeness mode

At 09:00 HCM, inspect the previous complete HCM day. This mode validates that
the six reviewed exports and required event history are queryable through their
declared frontier. It may file a data-quality finding when the common gate
fails. When the gate passes, record success in the normal run result and stop:
the absence of a data-quality problem is not proof that revenue is healthy and
does not justify a revenue or recovery finding.

## Weekly mature-cohort mode

At Monday 09:00 HCM, use the last completed HCM week as the observation period.
First pass the common readiness gate, then execute the R10 signup-to-first-
payment definition from `lohi-evidence-v1`. For a rolling scheduled period,
instantiate R10 by changing only its frozen exclusive-cutoff literals to the
selected cutoff; preserve its binding, CTE, identity, first-payment, attribution,
and 7/14/30-day horizon semantics. Record both the base recipe reference and the
executed query digest/exact range; never present the base recipe hash as the
executed hash after cutoff substitution. Compare only rows whose cohort has at
least 14 fully elapsed 24-hour periods at the exclusive observation cutoff and
whose state is complete. Keep immature members out of both numerator and
denominator and report eligible, excluded/immature, and converted counts.

Fresh cohorts may be described only as `not_ready`; they cannot support a drop,
improvement, or recovery conclusion. A recovery finding additionally requires
a prior evidence-backed condition, complete post-condition inputs, and at least
one mature post-condition cohort. Missing campaign/ad-platform evidence keeps
causal claims unavailable: time alignment or source association is not proof
that a campaign caused the result.

## Deduplication and the write boundary

Before any write, page through `list_findings` until exhausted and look for the
same four-part `evidence.observation_key`, including settled findings. If found,
do not write again. When an authorized delivery channel is configured, treat the
existing finding as a retryable delivery obligation: call `send_notification`
with that finding's same period, condition, and evidence, cite its id, and record
whether the retry was sent or failed. When no channel is configured, cite the id
and record that delivery was skipped. Notification has at-least-once retry
semantics: an overlapping run may redeliver a notification that already arrived,
which is preferable to silently dropping one that did not.

If absent and a finding is warranted, build the complete deterministic request
once. Call `submit_recommendation` with category `data` for readiness failures or
`growth` for a mature business condition. Set `idempotency_key` to the lowercase
hex SHA-256 of the UTF-8 string
`project|definition_version|period|condition`. The Plans write atomically claims
that key within the project: identical overlap/retry requests replay the first
receipt; a different payload under the same key returns a conflict.
On conflict, re-read findings. If the matching finding is now present, follow the
same retryable delivery rule; if it is absent, surface the unresolved conflict in
run history and do not notify. Never change the condition/key merely to make the
write succeed.

This provides atomic idempotency for the Plans mutation, not a promise that the
entire scheduled run or notification delivery is exactly once. The pre-write
read remains required for explainability and for findings created by older
clients without the deterministic key.

## Delivery and failure behavior

After a new Plans finding succeeds, `send_notification` may summarize that
finding, using the same period, condition, and evidence without adding numbers.
A matching finding discovered during overlap/retry is also eligible for the
retry described above. Delivery is optional and at least once, with existing
channel error semantics and no exactly-once promise:

- no configured channel: keep the finding and successful analysis in existing
  Plans/run history, state that delivery was skipped, and do not fabricate a
  destination;
- denied `plans:write`: write nothing, deliver nothing, and let the run record
  the authorization failure;
- denied or failed notification: keep the Plans finding, record delivery failure
  honestly, and leave it eligible for a later overlap/retry; do not loop within
  one run or claim delivery;
- paused/disabled trigger: perform no run and no delivery.

Never invent a metric or evidence field. Never issue source DDL, mutate events,
spend money, change a campaign, publish externally, bypass an authorization
denial, or send a revenue assertion from stale/incomplete evidence.
