import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Docs site served under agentray.lohi2.com/docs via the existing GCE Caddy.
// Content lives in src/content/docs; the top-level repo markdown (QUICKSTART,
// README sections, ROADMAP) is the source of truth mirrored into those pages.
export default defineConfig({
  base: '/docs',
  integrations: [
    starlight({
      title: 'AgentRay',
      description: 'Open-source analytics that closes the growth loop.',
      social: {
        github: 'https://github.com/lohi-ai/agentray',
      },
      sidebar: [
        { label: 'Start here', items: [
          { label: 'Quickstart', link: '/quickstart/' },
          { label: 'Why AgentRay', link: '/why/' },
        ]},
        { label: 'Instrument', items: [
          { label: 'Browser SDK', link: '/instrument/browser/' },
          { label: 'Python SDK', link: '/instrument/python/' },
          { label: 'Migrating from PostHog', link: '/instrument/posthog/' },
        ]},
        { label: 'Agents', items: [
          { label: 'First agent answer', link: '/agents/first-answer/' },
          { label: 'Connect over MCP', link: '/agents/mcp/' },
          { label: 'Growth Autopilot', link: '/agents/autopilot/' },
        ]},
        { label: 'Operate', items: [
          { label: 'Self-host', link: '/operate/self-host/' },
          { label: 'Alerts & budgets', link: '/operate/alerts-budgets/' },
        ]},
      ],
    }),
  ],
});
