# AgentRay Web Architecture

Next.js 16 (App Router) dashboard for AgentRay analytics. All pages are client-rendered — the app is a pure SPA mounted inside the `(analytics)` route group.

## Stack

| Layer | Technology |
|---|---|
| Framework | Next.js 16, React 19, TypeScript |
| State | Zustand 5 (client state), TanStack Query 5 (server state) |
| Styling | Tailwind CSS v4, CSS tokens in `globals.css` |
| Charts | Apache ECharts (via `echarts`) |
| HTTP | `lib/api.ts` — typed `AgentRayAPI` class, cookie credentials |

## Directory Layout

```
web/
  app/
    layout.tsx                        — root HTML shell
    (analytics)/
      layout.tsx                      — authenticated shell: nav, filter bar, alerts
      page.tsx                        — / Overview
      agent/page.tsx                  — /agent workspace (chat)
      agents/page.tsx                 — /agents roster
      agents/monitoring/page.tsx     — /agents/monitoring fleet health
      agents/[agentId]/monitoring/page.tsx
      agents/[agentId]/lab/page.tsx
      chat/page.tsx                   — /chat (alias to /agent)
      dashboard/ events/ persons/ ...

  components/ui/                      — shadcn primitives (Button, Card, …)
  lib/
    api.ts                            — AgentRayAPI class + all TypeScript types
    utils.ts                          — cn() helper

  modules/                            — feature modules (see Module Layout below)
```

## Module Layout

Each route-backed module folder mirrors URL segments exactly. Files stay ≤ 200 lines.

```
modules/
  shared/                             — cross-module components and utilities
  app/                                — global auth, project, and hook layer

  <route-segment>/                    — /<route-segment>
    index.ts                          — exports the route section component
    models/                           — TypeScript types for this domain
    hooks/                            — data-fetching and state hooks
    lib/                              — route-local helpers and API functions
      utils/                          — pure helpers (no hooks, no JSX)
      api/                            — raw API calls (optional, if heavy)
    page.tsx                          — state container; wires data to child components
    components/                       — route-specific leaf components
    <query-ui>/                       — query-param UI state: sheet, drawer, modal, panel
      index.ts
      page.tsx                        — container for that UI state
      tabs/
        <tab-name>.tsx                — tab content, if needed
    <child-route>/                    — /<route-segment>/<child-route>
      index.ts
      page.tsx
    [id]/                             — /<route-segment>/[id]
      <child-route>/                  — /<route-segment>/[id]/<child-route>
        index.ts
        page.tsx
```

> **Path mapping rule:** route-backed module folders mirror URL segments exactly.
> Each route folder owns a `page.tsx` section component exported through `index.ts`.
> Query-param UI states, such as sheets, tabs, filters, and modals, live under the owning route folder instead of pretending to be URL segments.

## File-size Targets

| File type | Target |
|---|---|
| Route `page.tsx` | ≤ 150 lines (state wiring only) |
| Leaf component | ≤ 150 lines |
| `lib/utils/*` / `models/*` | ≤ 100 lines per file |
| `hooks/*` | ≤ 200 lines per file |

Large components should be split into named child folders, `tabs/`, or sibling files.

## State Management

| State type | Tool |
|---|---|
| Server state (agents, runs, config) | TanStack Query via `hooks.ts` |
| Global auth / project | Zustand `useAuthStore` |
| UI state (open sheet, active tab) | `useState` in `screens/page.tsx` |
| Chat threads | `localStorage` per project |

## Design System

All visual work follows `DESIGN.md` at the repo root. Key rules:

- Use `Panel`, `StatStrip`, `Stat`, `Field`, `EmptyState` from `modules/shared/ui.tsx`
- Use `AgentRaySheet` from `modules/shared/sheet.tsx` for slide-overs
- Use shadcn primitives from `components/ui/` — `Button`, `Badge`, `Dialog`, `Tabs`, `Select`, `Input`, `Textarea`
- Reference CSS tokens from `globals.css` — never hardcode hex/radius/font
- Status labels: "Healthy", "Needs attention", "Working now", "Set up next" — not jargon

## Public Module API

Each `modules/<name>/index.tsx` exports only what `app/` pages need:

| Module | Exported |
|---|---|
| `@/modules/agent` | `AgentSection`, `AgentSettings` |
| `@/modules/agents` | `AgentsSection` |
| `@/modules/agent-monitor` | `AgentMonitorSection`, `AgentMonitorDetailSection` |
| `@/modules/shared/ui` | `Panel`, `StatStrip`, `Stat`, `Field`, `EmptyState`, `ConfirmDeleteButton` |
| `@/modules/shared/sheet` | `AgentRaySheet` |
| `@/modules/agent/chat-link` | `agentChatHref`, `normalizeChatRouteAgent` |
| `@/modules/agent/hooks` | `useAgent`, `useAgents`, `useAgentRun`, … |
