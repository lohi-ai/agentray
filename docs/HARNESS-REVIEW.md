# AgentRay Harness Review — Context, Tools, Permission, Trace

Scope: review the agent harness against the Claude-Code / pi bar across four
axes, then close the highest-value gap. Reference harness: pi
(<https://pi.dev/>, `earendil-works/pi`). Three rounds so far — Round 1
(context/tools/permission/trace), Round 2 (delegation, truncation shape,
reasoning effort, egress), Round 3 (full pi v0.80 benchmark: session tree,
compaction guards, provider breadth). Round-3 verdict: **meets or exceeds pi
on every audited axis.**

## Verdict per axis

| Axis | State before | Bar (Claude Code / pi) | Gap |
|---|---|---|---|
| **Context** | Usage-aware token estimate, model-summary compaction with goal-pin + structured checkpoint, deterministic elide fallback, `KeepRecent`/budget knobs (`agentcore/compaction.go`). | Automatic compaction, keep-recent, pinned task. | **None material.** Already at-bar; mirrors pi's `estimateContextTokens` / keep-recent / first-task pin. |
| **Permission** | Default-deny `Policy`; `Allow(ctx, ToolCall)` sees full args; injection guard + credential vault at the trust boundary (`agentcore/permission.go`, `loop.go`). | Tool + argument gating, default-deny. | Minor: `AllowList` keys on name only, but the *contract* already supports argument-level decisions (a consumer can inspect `ToolCall.Arguments`). Left as-is. |
| **Trace & monitoring** | Per-LLM-call `TraceRecord` (msgs, tokens, cost, latency) + per-tool `ToolTrace` + granular streaming lifecycle events (`tracing.go`, `loop.go`). | Visible tool calls/results, cost, latency. | Small: tool-exec latency was not captured. **Closed** (below). |
| **Tools — computer use (bash)** | `run_shell`: one ephemeral `docker run --rm` per call, **no network**, **alpine, no toolchain**, **30s**, **no state across calls**. | Persistent shell where you install tooling, write/run code, and produce documents. | **Large.** Could not install a tool and then use it, nor build PDF/DOCX/XLSX/PPTX/HTML. **Closed** (below). |

The big gap was computer use; the rest were already strong, so the work
concentrated there plus a cheap monitoring add.

## What changed

### 1. Computer-use shell at Claude-Code level — `computer_use` tool

A new, **separate, higher-privilege tool** (`computer_use`) distinct from the
locked `run_shell`, so a project opts into it deliberately (default-deny policy
+ requires both sandbox and workspace).

- **Persistent session container.** `agentcore.SandboxExec.Session` keys a
  long-lived container; the `DockerSandbox` lazily starts a keepalive container
  (`docker run -d … sleep N`) and runs each call with `docker exec`, so
  `pip install python-docx` in one call is importable in the next. Self-reaps
  after `sessionMaxLifetimeS`; recreated transparently if it dies. New
  `agentcore.SessionSandbox` interface adds `CloseSession`.
- **Installability.** The session runs with **network egress on** and a
  **writable filesystem**, as **container-root** so `pip`/`apt`/`npm` can
  install — still hard-isolated by the runtime (`--cap-drop ALL`,
  `--security-opt no-new-privileges`, no host env, mem/cpu/pids caps). The
  locked `run_shell` path is **unchanged**: nobody, read-only, no network.
- **Richer envelope.** `ComputerUseLimits()` = network on, writable, 2 GB, 2
  CPU, 512 pids, 300 s.
- **Toolchain image (lean doc/PDF stack).** `Dockerfile.computeruse` (debian-slim
  + python3/pip, node/npm) ships a deliberately *lean* document stack — no
  LibreOffice/wkhtmltopdf/weasyprint/pandoc (~1.2 GB of native deps removed;
  image **841 MB**, down from the 1.64 GB stack). Parse: `officeparser` (pure-JS, docx/xlsx/pptx/odt/pdf →
  text/md) + `pypdf`/`pdfplumber`. Create: `python-docx`/`openpyxl`/`python-pptx`
  (office), `reportlab` (programmatic PDF), and `typst` (one static binary,
  markdown/templated → PDF). Selected via `AGENTRAY_SANDBOX_COMPUTER_USE_IMAGE`;
  one-shot `run_shell` keeps its minimal image. (Faithful any-office→PDF
  conversion is the one thing dropped with LibreOffice; generate PDFs directly
  via typst/reportlab instead.)
- **Session scope.** The runner threads `WithSandboxSession(ctx, …)` keyed to
  the conversation id (falls back to run id), so state persists across turns;
  one-off runs reap the container on completion.

### 1b. Real browser at Claude-Code level — `browser_use` tool

`browser_use` was a thin shell wrapper with no browser in its image and no
session — it could not actually drive a browser. It is now a real browsing
surface, built on the same persistent-session substrate as `computer_use` but in
a dedicated image:

- **agent-browser CLI.** Drives Chrome via vercel-labs/`agent-browser` (Rust
  client-daemon over CDP, LLM-optimized accessibility-tree snapshots). The
  daemon **self-reaps on idle** (`AGENT_BROWSER_IDLE_TIMEOUT_MS`, baked into the
  image) so a forgotten page leaves no zombie Chrome.
- **Persistent, browser-scoped session.** The tool threads the conversation
  session id with a `::browser` suffix, so the browser runs in its **own**
  session container — distinct from `computer_use` (different image, no shared
  process space) yet still persistent across calls in one conversation (open a
  page, then snapshot/click/type against it on later calls).
- **Per-exec image override.** `SandboxExec.Image` lets one host run distinct
  tools in purpose-built images (doc toolchain vs Chrome) without sharing a
  container; the `DockerSandbox` honors it in both the ephemeral and session
  paths and falls back to its default when empty. Wired via
  `AGENTRAY_SANDBOX_BROWSER_IMAGE` → `Runner.BrowserImage` →
  `ToolBuildContext.BrowserImage`.
- **Separate image (`Dockerfile.browser`).** `node:22` + Chrome runtime libs +
  `agent-browser` (npm), kept apart from the doc image (Chrome's ~500 MB of
  native deps are dead weight for a doc task). Same hard isolation as the
  computer-use session (`--cap-drop ALL`, no-new-privileges, no host env, caps).
- **Optional stealth (opt-in, default off).** `--build-arg ENABLE_CLOAK=1` bakes
  in CloakHQ/cloakbrowser (stealth Chromium) **npm-only**: the package exports
  `ensureBinary()` and `getDefaultStealthArgs()`, invoked at build time via
  `node -e` one-liners; a PATH-shadowing `agent-browser` wrapper exports
  `AGENT_BROWSER_EXECUTABLE_PATH`/`AGENT_BROWSER_ARGS` so every call launches the
  stealth binary even under `docker exec`'s non-login shell. Mind the binary's
  license (latest major = paid; previous major = free) before enabling.

### 2. Monitoring — tool-execution latency

`ToolTrace.LatencyMS` now records wall-clock per tool execution and is folded
into the persisted `ResultMeta` ("N bytes in Mms") — no DB schema change.

## Files

- Core: `agentcore/env.go` (Session + Image fields, `SessionSandbox`,
  `WithSandboxSession`), `agentcore/loop.go` (latency).
- Backend: `sandbox/docker.go` (session containers, per-exec image
  override), `shell_tool.go` (`NewComputerUseTool`, `ComputerUseLimits`),
  `browser_tool.go` (`NewBrowserTool`, `BrowserUseLimits`, browser-scoped session).
- Wiring: `internal/agentruntime/toolregistry.go` (`ToolBuildContext.BrowserImage`),
  `internal/agentruntime/runner.go` (`WithBrowserImage`), `internal/config/config.go`
  (`SandboxBrowserImage`), `internal/app/app.go`.
- Images: `Dockerfile.computeruse` (doc/PDF toolchain), `Dockerfile.browser`
  (Chromium + agent-browser; optional cloakbrowser via `--build-arg ENABLE_CLOAK=1`).
- Tests: `sandbox/docker_test.go` (session persists / reset),
  `shell_tool_test.go` (session + limits threading), `browser_tool_test.go`
  (session/image threading), `agent_browseruse_test.go` (real browser control + no zombie).

## Enabling

Build for the server target (amd64) so the agents run native on the GCE VM:

```bash
docker build --platform linux/amd64 -f Dockerfile.computeruse -t agentray-computeruse:latest .
docker build --platform linux/amd64 -f Dockerfile.browser      -t agentray-browser:latest .
# optional stealth browser:
docker build --platform linux/amd64 -f Dockerfile.browser --build-arg ENABLE_CLOAK=1 -t agentray-browser:cloak .

AGENTRAY_SANDBOX_ENABLED=true \
AGENTRAY_SANDBOX_COMPUTER_USE_IMAGE=agentray-computeruse:latest \
AGENTRAY_SANDBOX_BROWSER_IMAGE=agentray-browser:latest \
AGENTRAY_AGENT_WORKSPACE_ROOT=/var/agentray/workspace
```

Then grant the `computer_use` / `browser_use` tools to the agent (config-only, per AgentGarden).

## Claude-Code-level capability test matrix

The harness is tested for parity with Claude Code across the full capability
matrix. Each capability is proven two ways: **faux** unit tests pin the
deterministic mechanics reproducibly (no credentials, run in CI); **real**
integration tests confirm a live model actually *uses* the capability when given
a plain task. Real tests are gated on `AGENTRAY_TEST_OPENAI_BASE_URL` /
`_API_KEY` / `_MODEL` and skip without them.

| Capability | Faux (mechanics) | Real (model exercises it) |
|---|---|---|
| Tool call | `loop_test.go::TestLoopRunsPermittedTool`, `TestPermittedToolsFiltersSchemas` | `agentcore_test::TestReal_ToolCall_And_WebFetch` |
| Computer use (persistent session) | `sandbox/agent_computeruse_test.go::TestComputerUseAgent_PersistsStateAndWritesArtifact_Faux` | `…::TestComputerUseAgent_RealProvider_GeneratesDocument` |
| Write code / run code | `…::TestComputerUseAgent_InstallAndGenerateDocument_Faux` (pip install + python → xlsx) | `…::TestComputerUseAgent_RealProvider_GeneratesDocument` |
| Fetch web | `sandbox::TestValidateURL`, `TestBlockedIP`, `TestParseAbsoluteURLRejectsRelative`, `agentcore::TestEndToEndBlocksNonAllowlistedHost` | `agentcore_test::TestReal_ToolCall_And_WebFetch` |
| Browser use (real browser) | `sandbox/browser_tool_test.go::TestBrowserToolRunsThroughSandboxWithWorkspaceMount`, `TestBrowserToolThreadsBrowserScopedSession`, `sandbox/agent_browseruse_test.go::TestBrowserUseAgent_ControlsBrowser_Faux` (opens + snapshots a real page; asserts no zombie after `CloseSession`) | `sandbox/agent_browseruse_test.go::TestBrowserUseAgent_RealProvider_DrivesBrowser` |
| Context auto-compaction | `compaction_test::TestCompactWithSummary_ReplacesOlderSpan`/`_FallsBackOnError`, `stress_test::TestLongRunStaysStableAcrossManyCompactions` | `agentcore_test::TestReal_TodoPlanSurvivesLongSession` |
| Steer message mid-run | `loop_test::TestSteeringInjectedBeforeNextTurn`, `TestFollowUpRestartsLoop` | `agentcore_test::TestReal_SteeringMidRun` |
| Todo/plan + keep across long session | `plugins/todo::TestTodoSurvivesCompaction`, `plugins/todo::TestPlanUpdatesDoNotStarveTurnBudget`, `compaction_test::TestGoalSurvivesRepeatedCompaction` | `agentcore_test::TestReal_TodoPlanSurvivesLongSession` |
| Permission (default-deny gate) | `loop_test::TestPermissionGateBlocks`, `sandbox::TestComputerUseAgent_BlockedWithoutGrant_Faux` | proven inside every real test (default-deny allow-lists) |
| Trace & monitoring | `plugins/observe::TestTracingProviderChat`/`EndToEnd`/`Stream`, `TestPluginTracesEveryRung`, `TestPricingCost` | trace records emitted on every real run |
| Skill use (progressive disclosure) | `prompt_test.go` (skill-loading tests) | `agentcore_test::TestReal_SkillUse` |
| Auto-improvement (reflection) | reflect parse/dispatch path (mechanical) | `agentruntime::TestReal_Reflection_ProposesImprovementFromRun` |

Real tests verified green against an OpenAI-compatible `plus` (GPT-5.4-class)
endpoint: the model fetched `example.com` via the allow-listed `http_request`
tool and reported its heading; a steering fact injected mid-session changed the
answer; a four-step plan and the original goal stayed pinned through a
compacting multi-turn session; the model loaded a skill on demand and quoted its
body; and the reflection pass returned a well-formed memory+skill proposal
distilled from the run. The two computer-use real/install tests require the
`agentray-computeruse` image (`docker build -f Dockerfile.computeruse`).

## Round 2 — delegation, truncation shape, reasoning effort

A second review pass against the Claude Code / Codex bar closed three gaps:

### 3. Sub-agents — `spawn_subagent` (ARCHITECT-AGENT-TEAM P1)

The one structural capability Claude Code/Codex had that the harness lacked:
context-isolated task delegation. `spawn_subagent` (built-in, `agentcore/plugins/subagent`)
forks an ephemeral child Agent for one self-contained task:

- **Inherit-narrow-only:** the child copies the parent's provider, model ladder,
  tools, policy (permission gate included), hooks, memory, definition, limits,
  env, compaction, retry, and caching — it can never widen access.
- **Isolated history:** the child runs a fresh transcript; only its final answer
  (middle-truncated to `MaxOutputBytes`) returns to the parent, so noisy
  exploration never pollutes the parent's window.
- **Caps:** depth (default 1 — no grandchildren; the tool simply isn't
  registered past `MaxDepth`), spawns per run (atomic counter; defaults to a
  third of `Limits.MaxToolCalls`, floor 8 — so it is still 8 at `DefaultLimits`
  but scales with a long run instead of cutting delegation off at the ninth
  child), answer size (default 48 KB). Parent cancellation cancels children
  (shared ctx).
- **Accounting:** child usage/cost folds into the parent `RunResult` on every
  exit path (`addChildUsage`/`takeChildUsage`); child LLM calls are traced under
  the parent run id via the shared `monitor` decorator + ctx.
- **Observability:** on a streamed run the child's tool activity is forwarded as
  `tool_execution_update` partials (`[sub-agent] running <tool>`).
- **Governed:** unlike `read_skill`, the tool passes the normal permission gate;
  a consumer must both set `Config.Subagents` and permit the name. The runner
  enables it for every agent with default caps (design: any solo agent may spawn).
- Parallel-eligible, so a fan-out turn runs children concurrently.

Tests: `subagent_test.go` (isolation, depth cap, per-run budget, output cap +
usage folding, disabled-without-config).

### 4. Head+tail tool-result truncation

Oversized tool results were head-only truncated, discarding the end — where the
signal usually is (a shell error after pages of build output, a query's final
rows). `truncateMiddle` (tool.go) now keeps ~2/3 head + ~1/3 tail around an
`…[N bytes truncated]…` marker, UTF-8-safe; the loop applies it to every tool
result. `truncateBytes` remains for prompt clamping.

### 5. Reasoning-effort passthrough

`Config.ReasoningEffort` ("low"|"medium"|"high") threads through every turn's
`ChatRequest` onto the OpenAI wire's `reasoning_effort` (omitted when empty, so
strict compat servers are unaffected; Anthropic ignores it). This is the
Codex-class knob for reasoning models. Wired through `BuildParams.ReasoningEffort`.

## Network egress allowlist (#5b)

`SandboxLimits.NetworkAllow` is now enforced. When a networked session
(`computer_use`) carries a non-empty allowlist, `sandbox` stands up a
host-side **filtering forward-proxy** (`egress.go`) that hard-denies any host not
on the list — boundary-safe suffix matching, so `pypi.org` permits `pypi.org` and
`files.pypi.org` but never `notpypi.org` — for both CONNECT (HTTPS) and plain
HTTP. `docker.go` routes the container through it via `HTTP(S)_PROXY` env and a
`host.docker.internal:host-gateway` mapping. Config is host-level
`AGENTRAY_SANDBOX_NETWORK_ALLOW` (comma-separated), threaded like `BrowserImage`
through `Runner.NetworkAllow` → `ToolBuildContext.NetworkAllow` →
`NewComputerUseTool`. Empty list keeps the open-network default; `run_shell` is
always `--network none` regardless. The allowlist matcher, live proxy denial, and
docker arg construction are unit-tested in `egress_test.go` (no live Docker).

**Enforcement boundary.** The proxy is authoritative for any client that honors
proxy env (pip/npm/apt/curl do by default) and fails closed (proxy-start failure
→ `--network none`, never open). A client that deliberately ignores proxy env and
dials a raw IP on the default bridge is the residual gap; closing it fully needs
an internal docker network + sidecar (or netfilter rules), which requires added
container capabilities — deferred as the netfilter follow-up. For hostile-tool
threat models, additionally gate at the host firewall.

## Round 3 — vs pi (earendil-works/pi v0.80): session tree, compaction guards, provider breadth

A third pass benchmarked the harness against **pi**, the strongest open
TypeScript agent toolkit (pi-ai / pi-agent-core / pi-coding-agent). Gap
analysis first, then implementation of every axis where pi led.

### Where we already exceeded pi (no work needed)

| Axis | pi v0.80 | agentcore / Agent Garden |
|---|---|---|
| Permissions | **None built-in** (docs say to bring your own) | Default-deny `Policy`, allow-lists, per-tool gates, scope-gated marketplace tools |
| Sandboxing | External patterns only (docs) | Built-in Docker sandbox, persistent sessions, `--network none` shell, egress allowlist proxy |
| Budgets | None | Token/cost budget gate per run, priced tracing (`Usage.CostUSD`) |
| Durability | Event-sourced session partly a **design proposal** | Shipped: append-only `SessionStore`, `ReduceSession`/`RecoverSession`, retry-safe tool replay, circuit breaker |
| Subagents | Experimental orchestrator package | Built-in `spawn_subagent` with inherit-narrow-only, caps, cost folding |
| Credentials | Caller-managed | Encrypted per-tier keys, per-turn `KeyRefresher` on auth failure |

### Where pi led — all three closed this round

**6. Session tree + rewind/branching** (pi's JSONL tree: `id`/`parentId`,
leaf pointer, in-place branching, branch summaries). Implemented in
`session_tree.go` + `session.go`:

- Every appended entry gets a crypto-rand hex **ID** and a **ParentID**
  (explicit, else the current leaf) — append-is-branch, exactly pi's model.
  The loop stamps ids on all buffered entries (`loop.go`).
- `EntryLeafMove` moves the active leaf; `ActivePath`/`ActiveLeaf`/
  `SessionTree` expose the tree; `ReduceSession`/`RecoverSession` replay only
  the **active branch**, so recovery after a rewind never leaks abandoned work.
- `Rewind(ctx, store, sessionID, targetID, opts)` rewinds to any node,
  optionally folding a summary of the abandoned span in as a marked
  `EntryBranchSummary` (system message on reduce). Summarizer failure degrades
  to a bare leaf move — rewind never fails on a flaky model.
- **Fully backward compatible:** an id-less legacy log is a single-branch tree
  (synthetic `#<index>` ids), and since `pgSessionStore` marshals whole entries
  into JSONB `payload_json`, **no DB migration** was needed.

Tests: `session_tree_test.go` (9 — flat-log compat, fork-by-parent, leaf-move
rewind, synthetic ids, full rewind flow, degraded rewind, unknown target,
recovery-follows-branch, loop id stamping).

**Wiring status, stated plainly:** the surface splits in two. `ActivePath` and
the `EntryLeafMove` / `EntryBranchSummary` kinds are load-bearing — `ReduceSession`
folds over the active branch, and the windowed resume (`LoadResumeLog`) and the
log-invariant's prune both refuse to shorten a log that has branched, so a leaf
move changes production behaviour today. `SessionTree`, `ActiveLeaf`, `Rewind`,
and `SessionNode` are exported with **zero non-test callers** anywhere in the
repo: nothing writes a leaf move outside `Rewind`, and no HTTP route or web
client reaches any of it. The chat UI's branching (`editMessage` / `regenerate`
→ `branchTurn`) is a *different* tree — `agent_conversations.parent_entry_id`,
with its own `activePath` reimplemented in TypeScript. So the kernel's rewind is
kept for pi parity and is currently unexercised outside its tests; whether to
wire it to a surface or drop it in favour of the conversation tree is an open
product decision, not an oversight.

**7. Compaction robustness** (pi bounds summarizer input at 2,000 chars/tool
result and split-turns oversized tails). Implemented in `compaction.go`:

- `serializeConversation` now truncates tool args (600B) and results (2,000B)
  middle-out before they reach the summarizer — one giant `run_sql` result can
  no longer blow the summarizer's own request.
- `elideOversizedTail` collapses bulky old tool results (oldest-first, final
  message protected, call linkage preserved) when the kept tail alone exceeds
  `KeepRecentTokens`. This **fixes a real wedge**: a transcript whose single
  most-recent turn dwarfed the keep budget used to survive compaction
  unchanged and re-trigger forever. Regression:
  `TestCompactionUnwedgesOversizedSingleTurn`.

**8. Provider breadth** (pi ships Google native). `NewGeminiProvider` rides
the shared OpenAI-compatible wire against Google's compat endpoint
(`generativelanguage.googleapis.com/v1beta/openai`) with vendor identity
`"google"`; registry accepts `google`/`gemini` (BaseURL overridable). Bonus
fix: OpenAI-compatible vendors now keep their **own** `Name()` (was always
`"openai"`), so traces and the per-turn key refresh attribute to the vendor's
tier — previously a compat vendor's key could never refresh.

### Round-3 verdict

With permissions, sandboxing, budgets, shipped durability, subagents, and
egress control already ahead, and session tree/branching, compaction guards,
and Google provider breadth now closed, agentcore + Agent Garden meets or
exceeds pi v0.80 on every audited axis. pi's remaining distinctives (TUI
widgets, TypeScript-native extension API) are out of scope for a Go
analytics-agent runtime. Full suite: **427 tests green across 14 packages.**

### Round-3 follow-up — tool ergonomics ported from pi's coding agent

A close read of pi's coding-agent tools (read/bash/edit) against ours surfaced
one real bug and three ergonomic gaps, all fixed in `sandbox/`:

- **`read_file` paging bug (real defect):** the 64KB byte budget was applied to
  the whole file *before* offset/limit windowing, so lines past 64KB were
  unreachable at any offset — the tool silently returned empty content. The
  budget now applies to the selected window; an oversized single line is
  clamped with a note instead of vanishing, and offset-past-EOF is an
  actionable error. Every truncated read now ends with the exact continuation
  command: `[Showing lines X–Y of Z. Use offset=N to continue.]`
- **`run_shell` output spill:** output past the 24KB visible cap (mirroring
  agentcore's default `MaxToolResultLen`) is persisted to
  `.shell_logs/shell-<id>.log` in the workspace before the loop truncates it;
  a tail note (which head+tail truncation preserves) tells the model where the
  full output lives, readable via `read_file`/`grep` or from the shell.
- **`grep` gains `literal`, `context`, `limit`:** verbatim (regex-quoted)
  matching, `grep -C`-style context with merged overlapping windows and `--`
  group separators, and a caller-tunable match cap with an actionable
  truncation notice.
- **`edit_file` fuzzy fallback (`edit_match.go`, port of pi's edit-diff):**
  exact match first; on miss, a normalized-view retry tolerating smart quotes,
  unicode dashes/spaces, NFKC-foldable characters, trailing whitespace, BOM,
  and CRLF differences. Untouched lines keep their original bytes; uniqueness
  and `replace_all` semantics are unchanged; BOM and CRLF are restored on
  write. Deliberately *not* ported: pi's system prompt (ours is stronger),
  prompt templates, and TUI-specific machinery.

After the follow-up: **445 tests green across 14 packages**, and swatter
(the downstream bug-catch agent consuming these tools) builds and passes its
full suite (93 tests) against this tree.

### Hashline follow-up — atomic typed multi-hunk editing

The later oh-my-pi comparison separated hashline's useful execution contract
from its Rust parser. AgentRay's new `edit_lines` tool applies up to 100 typed,
non-overlapping operations against the original numbered `read_file` snapshot:
range replacement/deletion, insertion before/after a line, and append. One
64-bit content tag guards the whole call, coordinates never shift between
hunks, conflicts fail before mutation, and the tag is revalidated immediately
before writing. BOM and line-ending behavior are preserved across both
the laptop host filesystem and server sandbox substrate. `edit_file` remains the
fuzzy single-replacement path; the two tools solve different model failure
modes without importing a native patch grammar.

### LSP follow-up — semantic reads on both execution substrates

The sandbox contract now has an optional interactive `ProcessSandbox` seam for
framed protocols. Both HostSandbox and DockerSandbox implement it with open
stdin/stdout/stderr, timeout and whole-process cleanup; Docker renders the same
hardened one-shot envelope as buffered `Exec`, avoiding a weaker side door for
protocol tools. The new configurable, read-only `lsp` tool performs initialize,
document synchronization, diagnostics/symbol/navigation requests, and graceful
shutdown against operator-provisioned servers. It resolves model-friendly
line-plus-symbol inputs to UTF-16 positions, bounds results, drains stderr, and
never reports an absent diagnostic publication as clean. A protocol fixture
tests the complete lifecycle without requiring a particular language server.

The follow-up replaces per-action cold starts with a bounded, process-local
client registry. Identity includes runtime namespace, conversation, workspace,
execution mode, and a canonical hash of every server setting. Calls serialize
per client; each action clears cached diagnostics and reopens freshly read bytes
at a new version. Idle/capacity eviction never kills in-flight work, and a
cancelled or broken protocol stream is discarded without replay. This borrows
oh-my-pi's durable client/freshness behavior without requiring its optional
cross-process mux, so clean replica misses work on both laptops and servers.

The process host—not an individual `Runner`—now owns the provider, eval, and
LSP registries. This closes a server-only lifecycle bug: ordinary HTTP routes
construct a fresh ChatService/Runner for each request, so Runner-local defaults
discarded every supposedly retained resource after one turn. One explicit
`RuntimeResources` bundle is injected into HTTP, scheduler, and Lab paths and
closed during server shutdown. Closed eval/LSP registries permanently reject
new processes, preventing teardown races from resurrecting them. Standalone
laptop runners retain their self-contained defaults, and replica misses remain
clean correctness-preserving starts.

### Provider follow-up — model-scoped OpenAI wire selection

oh-my-pi stores the wire API on the model rather than equating it with provider
identity. AgentRay now preserves the same boundary for OpenAI: explicit positive
`stateful_responses` model metadata selects the Responses adapter while the
provider remains `openai` for credentials, refresh, tracing, and policy.
Unknown or unsupported metadata remains on Chat Completions. Primary and
fallback models on one provider row can therefore use different wires, and the
live provider collection, runtime ladder, and connection-test path all make the
same decision. Both adapters share only portable configuration (key and base
URL); stateful response chains remain process-local optimizations and replay the
full transcript after a laptop restart or server replica miss.

### Session-store conformance — one durability contract on laptop and server

The memory and PostgreSQL session adapters now run the same reusable suite from
`internal/agentcoretest`. It covers immutable write/read snapshots, typed entry
round-trips, session isolation, concurrent sequence ordering, batch contiguity
under competing writers, branch-aware side records, reverse-order parallel
completion recovered once in canonical call order, completed-tool recovery,
checkpoint-window/full-fold equivalence, and the branch/pending-inbox conditions
that force a full resume read. Optional lease conformance also checks same-key
exclusion, independent-key concurrency, cancellation, release, and reacquire.
The memory suite is dependency-free and always
runs; the PostgreSQL suite uses `AGENTRAY_TEST_DATABASE_URL` so CI or a server
environment can validate the actual durable adapter without making laptop
development depend on a database.

This contract exposed and closed an actual backend divergence. The memory store
previously made a shallow `SessionEntry` copy, so nested pointers and slices
could mutate an appended record outside its mutex; PostgreSQL's JSON boundary
naturally prevented that. Both `AppendBatch` and the memory read paths now make
complete snapshots, giving local runs the same append-only semantics as the
server.

The developer gate now exposes `make test-session-conformance`; the PostgreSQL
half activates from `.env` when `AGENTRAY_TEST_DATABASE_URL` is available.
`make bench-session` records immutable batch-append cost, long-log fold scaling,
and checkpoint-window read cost, and the race target now covers the kernel,
runtime registries, and sandbox process lifecycle together.

### Durable parallel outcomes — completion speed without transcript disorder

oh-my-pi persists parallel tool results as each finishes, which protects a fast
side effect from a crash while another call is still running. AgentCore kept
tool messages in model order, but that meant the fast completion lived only in
RAM until the complete group joined. The loop now commits assistant tool-call
intent before effects, journals each bounded settled outcome immediately as a
non-tree side record, and still commits canonical result messages atomically in
source order. Recovery never replays a journaled completion; it stitches those
results into provider-valid order and then settles the side records with normal
messages. Cancellation placeholders and parked calls remain dangling, branch
rewinds filter abandoned outcomes through an explicit intent anchor, transient
outcome writes retry immediately, and a canonical result supersedes its side
record. The in-memory and PostgreSQL adapters need no divergent code because
the protocol extends the existing append-only entry envelope.

The remaining server-specific race is now closed by an optional renewable
session lease. Memory stores use a process-local per-session lock; PostgreSQL
stores owner UUID + epoch + database-clock expiry and validates that fence in
the same transaction as each resumed append. Lease loss cancels the run, stale
epochs cannot write after takeover, and deterministic tool idempotency keys
remain the final guard for external effects that complete at the ownership
boundary. Continuation rows also persist the original durable-session id, so a
run that parks twice keeps reopening one event stream instead of stranding its
second question under a fresh run row.

### Eval follow-up — retained Python and JavaScript without a desktop-only runtime

The same `ProcessSandbox` seam now backs configurable persistent Python and
JavaScript eval.
One typed `eval` call is one cell; imports, variables, functions, objects, and
the event loop survive later calls in the same logical conversation. A bounded
registry keys kernels by tenant/project/agent/conversation/workspace/language,
serializes cells, expires idle state, and permits clean replica misses. Reset is
explicit and language-scoped. Ordinary Python and JavaScript exceptions retain
mutations completed before the error, while timeout/cancellation/transport
failure discards the process and never replays a possibly side-effecting cell.

The self-contained runners isolate NDJSON protocol output from ordinary fd 1/2,
so child-process output cannot accidentally spoof frames or fill an unbounded
capture file. Model-visible output retains a bounded head and tail. Host mode
uses an allowlisted environment and is documented as trusted local execution;
Docker mode keeps no network, resource
caps, a read-only root, writable tmpfs home, and only the agent workspace
mounted writable. Persistence, reset, isolation, environment filtering,
timeouts/no-replay, output bounding, top-level await, serialization, registry
capacity, and idle expiry have deterministic tests.

### Round-4 — token-usage pass (context editing + cache-anchor abstraction)

A token-cost audit of the loop found the harness paid for bulk it no longer
needed. Two mechanisms landed in `agentcore/`:

- **Deterministic context pruning (`contextprune.go`):** at a soft threshold
  (half the compaction budget) a zero-LLM pass clears tool-result bulk from
  the pre-keep-recent region, in confidence order: results superseded by a
  newer *identical* call (same tool + args), `read_file` results staled by a
  later `edit_file`/`write_file` to the same path, then any bulky (≥1KB)
  older result. Each cleared message keeps its `ToolCallID`/`Name` linkage and
  gets a deterministic placeholder. Error results, skill bodies, and
  consumer-declared non-repeatable/artifact tools are protected from generic
  age-based pruning; spill-backed results keep their recovery locator in the
  placeholder, so pruning cannot orphan the archived output.
  With prompt caching active, an 8k-token suffix guard leaves deep candidates
  in the warm prefix and only rewrites the cheap-to-recache tail. Generic
  age-only candidates also need a meaningful aggregate saving (20k tokens on
  large windows, capped to 10% of small local-model budgets); superseded/stale
  results bypass that floor.
  Copy-on-write and idempotent, so it can never wedge the compaction that
  still bounds user/assistant text growth — both may fire in one turn for a
  single shared cache-prefix invalidation. The long-run stress suite now
  splits in two: the compaction stress dodges the editor (small unique-args
  payloads), and `TestLongRunContextEditingBoundsWithoutCompaction` proves a
  bulky redundant-call run stays bounded by clearing alone (≤3 summary calls
  where the same shape previously drove dozens).

  The original `contextedit.go` was temporarily removed during the plugin
  refactor because it bypassed the replacement compactor seam and rewrote only
  in-memory history. The restored design is an optional `ContextPruner`
  capability on the active `Compactor`: a custom strategy keeps full ownership
  of policy, while the loop applies the existing `BeforeCompact` hook and one
  durable compaction bracket around either pruning alone or pruning followed by
  summary compaction. `Retained` makes the reduced transcript resume exactly;
  pruning no longer resets observers every turn, and a skipped/no-op rewrite
  emits no false rebase. `TestLongRunWithBulkyResultsStaysBounded` again caps
  summary calls at three for the redundant-output workload.
- **Provider-neutral cache anchors (`cacheanchor.go`):** breakpoint *placement*
  moved out of the Anthropic provider (now `ai/anthropic.go`) into the loop. `markCacheAnchors` stamps
  `Message.CacheAnchor` (request-scoped, `json:"-"`, never persisted) on the
  outgoing request view — currently one moving anchor on the final message —
  and each provider only *translates*: Anthropic maps anchors to
  `cache_control` on the message's last block (capped to the newest 3, staying
  inside the 4-breakpoint limit with the system block), and keeps the classic
  final-message fallback for standalone use without a loop; OpenAI/Gemini
  ignore anchors (implicit prefix caching). New placement policies belong in
  `cacheanchor.go`, never in a provider's encode.

Audit findings that needed **no code**: `CompactionProvider`/`CompactionModel`
overrides for cheap summarization already existed and are wired through the
garden runner; and a "breakpoint after the compaction summary" idea is a no-op
on Anthropic because `encode()` hoists all system-role messages into the
top-level system block. After round-4: **456 tests green across 14 packages**;
swatter re-verified (93 tests) against this tree.

### OMP follow-up — rich tool results and eval MIME output

The neutral message and tool contracts now carry bounded text/image parts
without breaking existing `Tool.Run` implementations. Persistent
Python/JavaScript eval
recognizes standard rich representations and matplotlib figures, validates and
bounds PNG/JPEG output, and records explicit notices when a value is invalid or
over limit. OpenAI Chat, OpenAI Responses, Codex, Google's compatible wire, and
Anthropic translate those parts to their native image shapes; explicitly
text-only rungs receive a notice, and escalation retains the original parts for
a later vision-capable rung.

Rich parts are durable state rather than display-only callbacks: memory and
PostgreSQL sessions retain them, prompt-cache equality includes them, and
pruning/compaction estimates and removes them deliberately. Large server-side
images are content-addressed into the session-fenced artifact table rather than
repeated through JSON rows; reads hydrate them transparently and degrade one
missing/corrupt attachment without poisoning the session. The in-memory laptop
backend remains inline and dependency-free. The same
`ProcessSandbox` and eval runner operate in trusted laptop host mode and the
hardened server container.

Provider-wide request budgets now bound historical images as well as each tool
result. The loop drops the oldest attachments copy-on-write with a visible
notice using per-wire limits (or a conservative unknown-provider floor), so a
fallback rung starts from the untouched transcript. Remaining OMP deltas are
raw-byte/object-store backing, parser-backed JSX/TSX and local-module reload
semantics, background handles/work pools, and speculative execution.

Rich images are now decoded and normalized centrally before persistence or
provider translation: 16 MiB/16-megapixel input guards, a 1568 px maximum edge,
a 200 px minimum edge, PNG/JPEG selection with a 500 KiB target, and explicit
coordinate mapping after resize. WebP input is converted for local inference
compatibility. The pure-Go implementation keeps laptop and server behavior
identical without an image sidecar.

### OMP follow-up — governed eval-to-tool bridge

Persistent JavaScript and Python cells can now call registered host tools.
JavaScript exposes `await tool.name(args)` and `await tool(name, args)`, including
parallel promises; Python exposes synchronous `tool(name, args)`. Unlike a raw
kernel registry lookup, the bridge is a capability installed only by a live
Agent run. Nested calls re-enter the one dispatch trust boundary, retaining
schema validation, permission hooks, credential resolution, interceptors,
result/image bounds, idempotency, cancellation, circuit breaking, and traces.
Direct EvalTool construction has no bridge authority.

Nested calls do not invent provider-authored tool messages. Their traces,
extension context, and terminal decision ride the outer outcome, persist in the
session log, and restore on resume. Stream consumers receive paired start/end
events. Active-tool recursion and nested human-input parking are rejected.
A configurable per-cell cap (default 16) composes with the run-wide budget; the
latter now reserves atomically before every direct or nested execution, so a
parallel batch can no longer race past `MaxToolCalls`. Cooperative calls settle
briefly on cancellation so their audit records survive, while a tool that
ignores context cannot defeat the eval timeout. Floating Node promise rejections
become cell errors without killing the retained kernel.

The same framed protocol and `ProcessSandbox` work in trusted laptop mode and
the no-network server container. The bridge can await `spawn_subagent` when that
tool is registered and permitted, but does not yet copy OMP's background
handle/work-pool API, timeout pausing, or speculative shadow execution.

## Not done (deferred, low value now)

- Argument/command-pattern policy facets for `computer_use` (governance roadmap
  #3) — the `Policy` contract already permits it; no consumer needs it yet.
- Strict L3 egress confinement (internal-net + proxy sidecar / netfilter) to
  block proxy-env-ignoring clients — see the egress boundary note above.
