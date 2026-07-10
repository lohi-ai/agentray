# AgentRay Harness Review — Context, Tools, Permission, Trace

Scope: review the agent harness against the Claude-Code / pi bar across four
axes, then close the highest-value gap. Reference harness: pi
(<https://pi.dev/>, `earendil-works/pi`).

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

- Core: `agentcore/sandbox.go` (Session + Image fields, `SessionSandbox`,
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
| Fetch web | `httptool::TestValidateURL`, `TestBlockedIP`, `TestParseAbsoluteURLRejectsRelative`, `agentcore::TestEndToEndBlocksNonAllowlistedHost` | `agentcore_test::TestReal_ToolCall_And_WebFetch` |
| Browser use (real browser) | `sandbox/browser_tool_test.go::TestBrowserToolRunsThroughSandboxWithWorkspaceMount`, `TestBrowserToolThreadsBrowserScopedSession`, `sandbox/agent_browseruse_test.go::TestBrowserUseAgent_ControlsBrowser_Faux` (opens + snapshots a real page; asserts no zombie after `CloseSession`) | `sandbox/agent_browseruse_test.go::TestBrowserUseAgent_RealProvider_DrivesBrowser` |
| Context auto-compaction | `compaction_test::TestCompactWithSummary_ReplacesOlderSpan`/`_FallsBackOnError`, `stress_test::TestLongRunStaysStableAcrossManyCompactions` | `agentcore_test::TestReal_TodoPlanSurvivesLongSession` |
| Steer message mid-run | `steering_test::TestSteeringInjectedBeforeNextTurn`, `TestFollowUpRestartsLoop` | `agentcore_test::TestReal_SteeringMidRun` |
| Todo/plan + keep across long session | `todo_test::TestTodoSurvivesCompaction`, `todo_budget_test::TestPlanUpdatesDoNotStarveTurnBudget`, `goalpin_test::TestGoalSurvivesRepeatedCompaction` | `agentcore_test::TestReal_TodoPlanSurvivesLongSession` |
| Permission (default-deny gate) | `loop_test::TestPermissionGateBlocks`, `sandbox::TestComputerUseAgent_BlockedWithoutGrant_Faux` | proven inside every real test (default-deny allow-lists) |
| Trace & monitoring | `tracing_test::TestTracingProviderChat`/`EndToEnd`/`Stream`, `TestPricingCost` | trace records emitted on every real run |
| Skill use (progressive disclosure) | `skill_loading_test.go` (3 tests) | `agentcore_test::TestReal_SkillUse` |
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
context-isolated task delegation. `spawn_subagent` (built-in, `agentcore/subagent.go`)
forks an ephemeral child Agent for one self-contained task:

- **Inherit-narrow-only:** the child copies the parent's provider, model ladder,
  tools, policy (permission gate included), hooks, memory, definition, limits,
  env, compaction, retry, and caching — it can never widen access.
- **Isolated history:** the child runs a fresh transcript; only its final answer
  (middle-truncated to `MaxOutputBytes`) returns to the parent, so noisy
  exploration never pollutes the parent's window.
- **Caps:** depth (default 1 — no grandchildren; the tool simply isn't
  registered past `MaxDepth`), spawns per run (default 8, atomic counter),
  answer size (default 48 KB). Parent cancellation cancels children (shared ctx).
- **Accounting:** child usage/cost folds into the parent `RunResult` on every
  exit path (`addChildUsage`/`takeChildUsage`); child LLM calls are traced under
  the parent run id via the shared TracingProvider + ctx.
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

## Not done (deferred, low value now)

- Argument/command-pattern policy facets for `computer_use` (governance roadmap
  #3) — the `Policy` contract already permits it; no consumer needs it yet.
- Strict L3 egress confinement (internal-net + proxy sidecar / netfilter) to
  block proxy-env-ignoring clients — see the egress boundary note above.
