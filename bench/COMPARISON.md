# Benchmark: Claude Code harness vs agentcore — Base −2 Calculator

**Date:** 2026-07-29 · **Problem:** [PROBLEM.md](PROBLEM.md) — evaluate arithmetic
expressions (`+ - * /`, unary minus, parentheses, floor division) over
arbitrary-precision integers written in negabinary (base −2), literals up to
200 digits. 172 hidden tests (deterministic seed), judged by a brute-force
self-validated oracle ([judge/](judge/), `judge/oracle_test.go`).

Both solvers received the identical problem statement, the identical judge
feedback channel (`Verdict.Format`: pass count + first failing case), and the
same 10 s/case limit. Solutions are single-file Go, stdlib only.

## Results

| Metric | Claude Code harness | agentcore |
|---|---|---|
| Model | claude-fable-5 (interactive session) | gateway `pro` → claude-opus-4-6-thinking |
| Solved | ✅ 172/172 | ✅ 172/172 |
| Submissions to judge | 1 (passed first) | 1 (passed first) |
| Debug runs before submitting | 0 | 5 (`run_program` probes) |
| Agent turns | n/a (interactive) | 7 |
| Wall time (solve phase) | ~1 min authoring + 2.7 s judge | 138 s end to end |
| Tokens | not metered per-task | 55 594 in + 11 309 out (10 975 cache reads) |
| Stop | — | natural `stop`, final answer `SOLVED` |

Raw records: [results/claude-code.json](results/claude-code.json),
[results/agentcore.json](results/agentcore.json). The agent's passing source:
[results/agentcore-solution.go](results/agentcore-solution.go); mine:
[solutions/claude-code/main.go](solutions/claude-code/main.go).

## Reading the result

- **Outcome parity.** Both harnesses produced a first-submission full pass on a
  problem whose traps (floor vs Go's truncated division, big.Int requirement,
  negabinary conversion sign handling, unary-minus precedence) are exactly the
  kind that sink one-shot solutions. Both solutions independently converged on
  the same architecture: recursive-descent parser over `big.Int`, truncated
  `QuoRem` with a floor correction, digit-loop negabinary conversion.
- **Different working styles, same judge discipline.** The agentcore agent
  verified before submitting — 5 `run_program` probes covering conversion edge
  cases and precedence — then submitted once. I submitted cold. Its caution is
  the behavior the persona asked for and cost ~2 extra turns.
- **Caveats.** (1) The same Claude instance authored the problem, oracle, and
  generator before solving it, so the Claude Code row carries a problem-setter
  advantage; the agentcore agent saw the problem genuinely cold. (2) Models
  differ (fable-5 vs opus-4.6-thinking), so this compares *harness+model*
  stacks, not harnesses in isolation. (3) n=1 problem; this is a smoke-level
  parity check, not a benchmark suite.
- **What it exercised in agentcore:** multi-turn tool iteration to a natural
  stop, large tool arguments (full Go sources, ~4 KB per call) through
  `MaxTokens` headroom, the OpenAI-compat provider against a gateway that
  streams SSE even for non-stream requests, prompt-cache accounting
  (`cache_read_tokens` populated), and the allowlist policy gate.

## Problem 2 — Airplane Collision Simulator (compaction probe)

**Date:** 2026-07-29 · **Problem:** [PROBLEM2.md](PROBLEM2.md) — build a
self-contained two-airplane collision-simulator web page in four mandatory
milestones, one full-page `write_file` per milestone. Unlike Problem 1 this is
not a head-to-head: it is a live probe of agentcore's **in-loop compaction** at
the **production context budget (120k tokens, default keep-recent window)**.
The test seeds a ~452 KB prior design-discussion transcript through the durable
`ResumeSession` path, so the resumed run starts at the budget's edge and
compaction must summarize the old span mid-build.

Test: `TestBench_AgentcoreCollisionSimCompaction` ([collision_test.go](collision_test.go)),
same env gate as Problem 1.

| Metric | Result |
|---|---|
| Outcome | ✅ PASS — all compaction + page assertions |
| Turns / provider requests | 5 / 6 (one is the summarization call) |
| First compacted request | #2 (4 later requests carried the summary) |
| Milestone writes | 4 (all landed; work continued after compaction) |
| Final request size | 134 KB vs 452 KB seeded history (goal survived, pinned) |
| Final page | 21.2 KB, `SHIPPED`, every spec check green |
| Wall / tokens | 4 m 36 s · 326k in / 21.9k out (23.4k cache reads) |

Raw record: [results/agentcore-collision.json](results/agentcore-collision.json);
the page: [results/collision.html](results/collision.html).

**What it caught.** The first version of this bench used an artificially small
budget (6k) and exposed a real wedge in agentcore: with `MaxContextTokens`
below the default keep-recent window (20k), `shouldCompact` fired every turn
but `findCutPoint` saw the whole transcript as "recent" (cut 0), and the
deterministic elide only collapses bulky *tool results* — a transcript whose
bulk lives in assistant *tool-call arguments* (the write-a-full-file shape)
never shrank. Fixed by clamping the keep-recent window to half the budget
(`effectiveCompaction`, regression-tested in
`agentcore/compaction_clamp_test.go`); the final bench run above then passed at
the real 120k configuration with no special tuning.

**What it exercised:** durable-session resume rebuilding ~452 KB of history,
budget-triggered LLM summarization of a ~115k-token span, goal pinning across
compaction, continued multi-milestone tool work on the compacted transcript,
and spec-conformance of the finished artifact.

## Problem 3 — Lite CPU-realtime Vietnamese TTS (full-toolset, real prod data)

**Date:** 2026-07-29 · **Problem:** [PROBLEM3.md](PROBLEM3.md) — build a lite
Vietnamese TTS model + full pipeline for the production **thuy-trang** voice
that synthesizes **faster than real time on CPU**: pull real (text, audio)
pairs from the kiem-lai production DB + GCS bucket, train a smoke run that
demonstrably learns, run CPU inference, and measure the real-time factor.
Unlike Problems 1–2 the agent gets a **full local toolset** — `run_shell`
(bash in a scratch workspace; `DATABASE_URL` lives in the tool env only,
never the transcript), `write_file`, `read_file`, `list_dir` — and 80 turns /
200 tool calls to use it. Every reported number is verified against disk
(WAV RIFF headers, script files, report.json shape).

Test: `TestBench_AgentcoreLiteTTS` ([tts_test.go](tts_test.go)), same env gate.

| Metric | Run 1 (dev bucket, wrong spec) | Run 2 (prod bucket) |
|---|---|---|
| Outcome | ✅ PASS | ✅ PASS |
| Data | 364 pairs, 5 chapters — **wrong voice, disclosed** | **121 thuy-trang pairs**, 3 chapters, 111/10 split |
| Model | 3.6M-param FastSpeech-style + Griffin-Lim | 2.4M-param non-AR Conv-TTS + Griffin-Lim |
| Training | 100 steps, loss 42.63 → 12.12 | 200 steps, loss 16.43 → 8.81 |
| **RTF (CPU, M1 Pro)** | 0.048 (~21× real time) | **0.026 (~38× real time)**, 5 held-out sentences |
| Turns / requests / shell calls | 46 / 46 / 38 (4 non-zero exits) | 35 / 35 / 28 (4 non-zero exits) |
| Wall / tokens | 16 m 43 s · 453k in / 17.5k out | 10 m 1 s · 274k in / 14.5k out (262k cache reads) |

Raw records + the agent's own DESIGN.md/REPORT.md/scripts/WAVs:
[results/tts/](results/tts/) (run 2) and
[results/tts-run1-wrong-bucket/](results/tts-run1-wrong-bucket/) (run 1).
Full shell trace per run in `shell-log.json`.

**The notable result is run 1's failure handling.** The problem spec pointed
at the dev bucket (`dev01-sgp-novel-tts`, taken from api/.env) which turned
out to hold only 13 stale chapters of a *different* voice — the thuy-trang
objects live in `prd01-sgp-novel-tts`. The agent probed the bucket, discovered
the mismatch, used the real audio that did exist, and **explicitly disclosed
the voice substitution** in REPORT.md instead of fabricating or silently
failing — exactly the scope-honesty behavior the spec demands. The spec was
fixed and run 2 delivered the real thuy-trang pipeline.

**What it exercised in agentcore:** long free-form tool loops (35–46 turns) to
a natural stop, a shell tool with per-command timeouts (one 300 s `find /`
was killed and reported cleanly; the agent recovered), secret hygiene
(DATABASE_URL in tool env only — the persona ban on echoing it held), prompt
caching across a long run (262k cache-read tokens), and honest-reporting
verification with every claim checked against files on disk. Compaction never
fired — context stayed under the 200k production budget both runs.

## Reproduce

```bash
# Judge any candidate file:
go run ./bench/cmd/benchjudge bench/solutions/claude-code/main.go

# Live agentcore run (writes results/agentcore.json):
AGENTRAY_TEST_OPENAI_BASE_URL=… AGENTRAY_TEST_OPENAI_API_KEY=… AGENTRAY_TEST_OPENAI_MODEL=… \
  go test -timeout 30m -count=1 -v -run TestBench_AgentcoreSolvesProblem ./bench/
```
