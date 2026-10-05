import { createElement, type ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AgentSetupPage } from './page';

const roster = vi.hoisted(() => ({
  agents: [] as { id: string; name: string }[],
  agentsReady: false,
  agentsError: null as Error | null,
  reloadAgents: vi.fn(),
}));

vi.mock('next/navigation', () => ({
  useParams: () => ({ agentId: 'installed-id' }),
  useRouter: () => ({ push: vi.fn() }),
}));
vi.mock('@/modules/agent/hooks', () => ({
  useAgents: () => roster,
  useAgentAuthoring: () => ({ definitionLoading: true }),
}));
// Any accidental return to the monitor dependency fails this regression.
vi.mock('@/modules/agent-monitor/hooks', () => ({
  useAgentMonitorDetail: () => { throw new Error('monitor unavailable'); },
}));
vi.mock('@/modules/shared/components/app-shell', () => ({
  AppShell: ({ title, children }: { title: ReactNode; children: ReactNode }) => createElement('main', null, title, children),
}));
vi.mock('@/modules/shared/components/page-shell', () => ({ PageTabs: () => null }));
vi.mock('@/modules/shared/components/signal-primitives', () => ({
  Loading: ({ label }: { label: string }) => createElement('span', null, label),
  Panel: ({ children }: { children: ReactNode }) => createElement('section', null, children),
  EmptyState: ({ title, detail, action }: { title: string; detail: string; action: ReactNode }) => createElement('div', null, title, detail, action),
  Button: ({ children }: { children: ReactNode }) => createElement('button', null, children),
}));

describe('installed agent setup', () => {
  beforeEach(() => {
    roster.agents = [];
    roster.agentsReady = false;
    roster.agentsError = null;
  });

  it('resolves an installed agent even when monitoring is unavailable', () => {
    roster.agents = [{ id: 'installed-id', name: 'Insight Digest' }];
    roster.agentsReady = true;
    const html = renderToStaticMarkup(createElement(AgentSetupPage));
    expect(html).toContain('Insight Digest');
    expect(html).toContain('Loading persona');
    expect(html).not.toContain('Agent not found');
  });

  it('waits for project selection, initial loading, and install cache refresh', () => {
    expect(renderToStaticMarkup(createElement(AgentSetupPage))).toContain('Loading agent');
  });

  it('shows a retryable lookup failure without claiming the agent was removed', () => {
    roster.agentsError = new Error('Internal Server Error');
    const html = renderToStaticMarkup(createElement(AgentSetupPage));
    expect(html).toContain('Unable to load agent');
    expect(html).toContain('Try again');
    expect(html).not.toContain('Agent not found');
  });

  it('reports missing only after a successful completed roster lookup', () => {
    roster.agentsReady = true;
    expect(renderToStaticMarkup(createElement(AgentSetupPage))).toContain('Agent not found');
  });
});
