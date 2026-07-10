# AgentRay docs site

Static documentation site (Astro + Starlight) for AgentRay, deployed behind the
existing GCE Caddy at `agentray.lohi2.com/docs` — **no new infra service**.

## Content sources

Pages are sourced from the repo docs so there is one source of truth:

| Site page | Source |
| --- | --- |
| Quickstart | [`../docs/QUICKSTART.md`](../docs/QUICKSTART.md) |
| Install & instrument | `../README.md` SDK section + `../sdk/*/README.md` |
| First dashboard / first agent | `../docs/QUICKSTART.md` §3 |
| MCP connect | `../README.md` (AI Agents & MCP) |
| Self-host ops | `../README.md` (Local Development, Deploy) |
| Roadmap | [`../ROADMAP.md`](../ROADMAP.md) |

## Develop

```bash
cd website
npm install
npm run dev      # local preview
npm run build    # → dist/ static output
```

## Deploy

Build to `dist/` and publish under the Caddy static root that already serves
`agentray.lohi2.com` (route `/docs/*` → this `dist/`). The CI job that builds the
web app can run `npm run build` here and copy `dist/` alongside it — no separate
container or load balancer.

> Status: scaffold + content map in place. Wiring the Starlight content
> collection to import the `docs/` markdown verbatim and adding the Caddy `/docs`
> route are the remaining steps (tracked under IMPLEMENTATION-PLAN #3c).
