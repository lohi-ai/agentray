import { describe, expect, it } from 'vitest';
import {
  CHANNEL_CATALOG,
  CHILD_SURFACES,
  FUTURE_CHANNELS,
  NAV_ITEMS,
  WORKLOAD_CATEGORIES,
  childSurfacesFor,
  firstRunHandoff,
  firstSessionNotice,
  firstValuePath,
  isSampleProject,
  formatAgentError,
  instantReply,
  needsKeyRecovery,
  writtenOpinion,
  recoveryAction,
  settingsPath,
  projectDetailRoot,
  threadNeedsRecovery,
  weakestLink,
  settingsTabFromQuery,
  isFirstRun,
  matchActiveHref,
  navItemsFor,
  shouldStartDocksOpen,
  navGroupForPath,
  navGroups,
  shouldShowFirstEventGuide,
  signedInLandingTarget,
} from './ia';

describe('nav grouping', () => {
  it('groups the shell by layer: Runtime → Channels → Workloads → Data → Workspace', () => {
    const groups = navGroups();
    expect(groups.map((g) => g.id)).toEqual(['Runtime', 'Channels', 'Workloads', 'Data', 'Workspace']);
    expect(groups[0].items.map((i) => i.label)).toEqual(['Chat']);
    expect(groups[1].items.map((i) => i.label)).toEqual(['Operations']);
    expect(groups[2].items.map((i) => i.label)).toEqual(['Agents']);
    expect(groups[3].items.map((i) => i.label)).toEqual([
      'Dashboards', 'Traffic', 'Product', 'People', 'Events',
    ]);
    expect(groups[4].items.map((i) => i.label)).toEqual(['Settings', 'Plans']);
    expect(NAV_ITEMS.some((i) => i.href === '/prototypes')).toBe(false);
  });

  it('drops the Plans item from the Workspace group on self-host', () => {
    const groups = navGroups(navItemsFor({ hosted: false }));
    expect(groups[4].items.map((i) => i.label)).toEqual(['Settings']);
  });

  it('does not list lab or agent monitor as peer Workloads items', () => {
    const peerHrefs = NAV_ITEMS.filter((i) => i.group === 'Workloads').map((i) => i.href);
    expect(peerHrefs).toEqual(['/agents']);
    expect(peerHrefs).not.toContain('/agents/monitor');
    expect(peerHrefs.some((href) => href.includes('/lab'))).toBe(false);
  });

  it('keeps previously shipped surfaces reachable from a parent', () => {
    const hrefs = CHILD_SURFACES.map((s) => s.href);
    expect(hrefs).toEqual(expect.arrayContaining([
      '/alerts', '/teams', '/marketplace', '/cohorts', '/agents/monitor',
    ]));
    expect(childSurfacesFor('/agents').map((s) => s.label)).toEqual(
      expect.arrayContaining(['Hire a teammate', 'Teams', 'Monitor']),
    );
    expect(childSurfacesFor('/agents').map((s) => s.href)).toEqual(
      expect.arrayContaining(['/teams', '/marketplace', '/agents/monitor']),
    );
    expect(childSurfacesFor('/settings').map((s) => s.href)).toContain('/alerts');
    expect(childSurfacesFor('/persons').map((s) => s.href)).toContain('/cohorts');
    expect(childSurfacesFor('/product').map((s) => s.href)).toContain('/prototypes');
    expect(childSurfacesFor('/dashboard').map((s) => s.href)).toEqual(
      expect.arrayContaining(['/templates', '/sql']),
    );
    expect(childSurfacesFor('/events').map((s) => s.href)).toContain('/replay');
    expect(childSurfacesFor('/chat').map((s) => s.href)).toContain('/start');
  });
});

describe('matchActiveHref', () => {
  const cases: Array<[string, string, 'Runtime' | 'Channels' | 'Workloads' | 'Data' | 'Workspace' | '']> = [
    ['/', '', ''],
    ['/start', '/chat', 'Runtime'],
    ['/chat', '/chat', 'Runtime'],
    ['/prototypes', '/product', 'Data'],
    ['/prototypes/abc-123', '/product', 'Data'],
    ['/operations', '/operations', 'Channels'],
    ['/operations/config%3Aproj-1', '/operations', 'Channels'],
    ['/agents', '/agents', 'Workloads'],
    ['/agents/grow-1/lab', '/agents', 'Workloads'],
    ['/agents/monitor', '/agents', 'Workloads'],
    ['/agents/grow-1/monitor', '/agents', 'Workloads'],
    ['/teams', '/agents', 'Workloads'],
    ['/marketplace', '/agents', 'Workloads'],
    ['/agent', '/agents', 'Workloads'],
    ['/dashboard', '/dashboard', 'Data'],
    ['/web-analytics', '/web-analytics', 'Data'],
    ['/product', '/product', 'Data'],
    ['/settings', '/settings', 'Workspace'],
    ['/alerts', '/settings', 'Workspace'],
    ['/persons', '/persons', 'Data'],
    ['/cohorts', '/persons', 'Data'],
    ['/events', '/events', 'Data'],
    ['/replay', '/events', 'Data'],
    ['/sql', '/dashboard', 'Data'],
    ['/templates', '/dashboard', 'Data'],
  ];

  it.each(cases)('%s → href %s in %s', (pathname, href, group) => {
    expect(matchActiveHref(pathname)).toBe(href);
    expect(navGroupForPath(pathname)).toBe(group);
  });
});

describe('architecture catalogs', () => {
  it('mirrors shipped channels and reserved support/voice', () => {
    expect(CHANNEL_CATALOG.filter((c) => c.shipped).map((c) => c.kind)).toEqual([
      'chat', 'mcp', 'schedule', 'webhook', 'lab',
    ]);
    expect(CHANNEL_CATALOG.filter((c) => !c.shipped).map((c) => c.kind)).toEqual([
      'support_widget', 'voice',
    ]);
    const kinds = CHANNEL_CATALOG.map((c) => c.kind);
    expect(kinds).not.toContain('slack');
    expect(kinds).not.toContain('discord');
    expect(kinds).not.toContain('telegram');
    expect(FUTURE_CHANNELS.map((c) => c.kind)).toEqual(['slack', 'discord', 'telegram']);
  });

  // Reserved means "ships no pack". Operator stopped being reserved when
  // ops-watch shipped and validate arrived with product-scout; support is the
  // one still empty, because a support agent has no inbound channel to read.
  it('mirrors workload categories with only support reserved', () => {
    expect(WORKLOAD_CATEGORIES.filter((c) => !c.reserved).map((c) => c.id)).toEqual([
      'validate', 'growth', 'marketing', 'data', 'operator',
    ]);
    expect(WORKLOAD_CATEGORIES.filter((c) => c.reserved).map((c) => c.id)).toEqual([
      'support',
    ]);
  });
});

describe('signedInLandingTarget', () => {
  it('sends an authenticated session to the conversation front door', () => {
    expect(signedInLandingTarget()).toBe('/chat');
    expect(signedInLandingTarget()).not.toBe('/dashboard');
  });
});

describe('firstValuePath', () => {
  it('turns on first-event + first-ask when the catalog has never seen an event', () => {
    const path = firstValuePath({ eventNames: [], catalogReady: true });
    expect(path).toEqual({ showFirstEvent: true, showFirstAsk: true });
    expect(shouldShowFirstEventGuide({ eventNames: [], catalogReady: true })).toBe(true);
  });

  it('hides the first-event card once any event name is in the catalog', () => {
    const path = firstValuePath({ eventNames: ['signup'], catalogReady: true });
    expect(path.showFirstEvent).toBe(false);
    expect(shouldShowFirstEventGuide({ eventNames: ['signup'], catalogReady: true })).toBe(false);
    expect(shouldShowFirstEventGuide({
      eventNames: [{ name: 'signup' }],
      catalogReady: true,
    })).toBe(false);
  });

  it('stays off while the catalog has not loaded', () => {
    expect(firstValuePath({ eventNames: [], catalogReady: false })).toEqual({
      showFirstEvent: false,
      showFirstAsk: false,
    });
  });

  it('keeps the connect guide on a published Demo workspace', () => {
    expect(isSampleProject({ name: 'Demo' })).toBe(true);
    expect(firstValuePath({
      eventNames: ['pageview', 'signup'],
      catalogReady: true,
      sample: true,
    })).toEqual({ showFirstEvent: true, showFirstAsk: false });
  });
});

describe('firstSessionNotice', () => {
  it('coaches an empty project to send a signal or ask what to track', () => {
    const notice = firstSessionNotice({ eventNames: [], catalogReady: true });
    expect(notice?.kind).toBe('empty');
    expect(notice?.ask.length).toBeGreaterThan(0);
  });

  it('still writes the weakest-link when there is no AI key', () => {
    const notice = firstSessionNotice({
      eventNames: [{ event_name: 'signup', count: 12 }],
      catalogReady: true,
      hasModelKey: false,
    });
    expect(notice?.kind).toBe('noticed');
    expect(notice?.title).toMatch(/12/);
    expect(notice?.detail).toMatch(/12 people/);
    expect(notice?.detail).toMatch(/activation/i);
    expect(notice?.href).toBe(settingsPath('ai'));
    expect(settingsTabFromQuery('?tab=ai')).toBe('AI Provider');
  });

  it('names the events it can already see', () => {
    const notice = firstSessionNotice({ eventNames: ['signup'], catalogReady: true });
    expect(notice?.kind).toBe('noticed');
    expect(notice?.title).toMatch(/signup/);
    expect(notice?.ask).toMatch(/activation|signup/i);
    expect(firstSessionNotice({
      eventNames: [{ event_name: 'signup' }],
      catalogReady: true,
    })?.detail).toMatch(/activation/i);
  });
});

describe('weakestLink', () => {
  it('computes the biggest drop from catalog counts', () => {
    const link = weakestLink([
      { event_name: 'pageview', count: 1000 },
      { event_name: 'signup', count: 80 },
      { event_name: 'purchase', count: 10 },
    ]);
    expect(link).toEqual(expect.objectContaining({
      from: 'pageview',
      to: 'signup',
      fromCount: 1000,
      toCount: 80,
      missing: false,
    }));
    expect(link?.rate).toBeCloseTo(0.08);
  });

  it('treats a missing next stage as 0% conversion', () => {
    const link = weakestLink([{ event_name: 'signup', count: 12 }]);
    expect(link).toEqual(expect.objectContaining({
      from: 'signup',
      to: 'activation',
      fromCount: 12,
      toCount: 0,
      rate: 0,
      missing: true,
    }));
  });
});

describe('formatAgentError', () => {
  it('turns setup failures into a next step', () => {
    expect(formatAgentError('agent is disabled for this project')).toMatch(/Set up/i);
    expect(formatAgentError('no workspace model key configured')).toMatch(/Settings/i);
  });

  it('leaves a real answer alone when it merely mentions a key or a pause', () => {
    const answer = 'Signups paused after the campaign ended. The connector has no api key set, so warehouse rows stopped.';
    expect(formatAgentError(answer)).toBe(answer);
    expect(needsKeyRecovery(answer)).toBe(false);
  });
});

describe('needsKeyRecovery', () => {
  it('matches raw stored errors and formatted copy', () => {
    expect(needsKeyRecovery('no workspace model key configured')).toBe(true);
    expect(needsKeyRecovery(formatAgentError('no workspace model key configured'))).toBe(true);
    expect(needsKeyRecovery('agent is disabled for this project')).toBe(true);
    expect(needsKeyRecovery(formatAgentError('agent is disabled for this project'))).toBe(true);
    expect(needsKeyRecovery('Here is your funnel.')).toBe(false);
    expect(threadNeedsRecovery([
      { text: 'What is the weakest step?' },
      { text: 'no workspace model key configured' },
    ])).toBe(true);
    expect(recoveryAction('agent is disabled for this project').href).toBe('/agents');
    expect(recoveryAction(formatAgentError('no workspace model key configured')).href).toBe(settingsPath('ai'));
    expect(threadNeedsRecovery([
      { text: formatAgentError('no workspace model key configured') },
    ], { hasModelKey: true })).toBe(false);
  });
});

describe('settingsPath', () => {
  it('writes the Projects deep link', () => {
    expect(settingsPath('projects')).toBe('/settings?tab=projects');
    expect(settingsTabFromQuery('?tab=projects')).toBe('Projects');
  });
});

describe('projectDetailRoot', () => {
  it('sends agent / team / ops / prototype detail to the list root', () => {
    expect(projectDetailRoot('/agents/abc/setup')).toBe('/agents');
    expect(projectDetailRoot('/agents/abc/monitor')).toBe('/agents');
    expect(projectDetailRoot('/teams/t1')).toBe('/teams');
    expect(projectDetailRoot('/operations/op1')).toBe('/operations');
    expect(projectDetailRoot('/prototypes/p1')).toBe('/prototypes');
  });

  it('leaves list pages, chat, sql, and /agents/monitor alone', () => {
    expect(projectDetailRoot('/agents')).toBeNull();
    expect(projectDetailRoot('/agents/monitor')).toBeNull();
    expect(projectDetailRoot('/chat')).toBeNull();
    expect(projectDetailRoot('/sql')).toBeNull();
    expect(projectDetailRoot('/settings?tab=projects')).toBeNull();
  });
});

describe('instantReply', () => {
  it('writes the computed drop without calling a model', () => {
    const input = {
      eventNames: [{ event_name: 'signup', count: 12 }],
      catalogReady: true,
    };
    const reply = instantReply(input, 'What should we track for activation after signup?');
    expect(reply).toMatch(/0%/);
    expect(reply).toMatch(/signup/);
    expect(reply).toMatch(/activation/);
    expect(writtenOpinion(input)).toMatch(/first_value/);
  });

  it('does not intercept unrelated questions', () => {
    expect(instantReply({
      eventNames: [{ event_name: 'signup', count: 1 }],
      catalogReady: true,
    }, 'Who is on my team?')).toBeNull();
  });
});

describe('shouldStartDocksOpen', () => {
  it('keeps the first session full-width until there is work to show', () => {
    expect(shouldStartDocksOpen({ threadCount: 0, recommendationCount: 0 })).toBe(false);
    expect(shouldStartDocksOpen({ threadCount: 1, recommendationCount: 0 })).toBe(true);
  });
});

describe('isFirstRun', () => {
  it('never shows the panel while the runs query is still in flight', () => {
    // A flash of "your data is already here" on a workspace with 200 runs is
    // worse than a beat of nothing, so an unresolved gate reads as not-first.
    expect(isFirstRun({ runs: [], runsReady: false })).toBe(false);
    expect(isFirstRun({ runs: undefined, runsReady: false })).toBe(false);
  });

  it('shows the panel only when the workspace has never run an agent', () => {
    expect(isFirstRun({ runs: [], runsReady: true })).toBe(true);
    expect(isFirstRun({ runs: [{}], runsReady: true })).toBe(false);
  });

  it('does not key off event emptiness — the seeded Demo project always has events', () => {
    // The gate takes runs, not eventNames. A populated Demo workspace with no
    // run is still a first run; an empty own-project with a run is not.
    expect(isFirstRun({ runs: [], runsReady: true })).toBe(true);
    expect(isFirstRun({ runs: [{}, {}], runsReady: true })).toBe(false);
  });

  it('stands down inside a thread that already has turns', () => {
    expect(isFirstRun({ runs: [], runsReady: true, turnCount: 1 })).toBe(false);
    expect(isFirstRun({ runs: [], runsReady: true, turnCount: 0 })).toBe(true);
  });
});

describe('firstRunHandoff', () => {
  it('stays quiet until the seeded turn settles', () => {
    expect(firstRunHandoff({ started: false, settled: false, failed: false })).toBeNull();
    expect(firstRunHandoff({ started: true, settled: false, failed: false })).toBeNull();
  });

  it('withholds the handover when the run failed', () => {
    // A failed run gets the error surface (retry + simplify), never a
    // "your dashboard is ready" that points at nothing.
    expect(firstRunHandoff({ started: true, settled: true, failed: true })).toBeNull();
  });

  it('belongs to the seeded exchange, not to every later turn', () => {
    // `started` is session-sticky, so without the turn bound the handoff would
    // render again under the answer to every follow-up question.
    // One ChatMsg is one exchange (it carries both the ask and the answer), so
    // the seeded run is turnCount 1 and the first follow-up is 2.
    expect(firstRunHandoff({ started: true, settled: true, failed: false, turnCount: 1 })).not.toBeNull();
    expect(firstRunHandoff({ started: true, settled: true, failed: false, turnCount: 2 })).toBeNull();
  });

  it('leads with the payoff, then the next commitment', () => {
    const handoff = firstRunHandoff({ started: true, settled: true, failed: false });
    expect(handoff?.dashboard.href).toBe('/dashboard');
    expect(handoff?.connect.detail).toMatch(/your own app/i);
    // The sample-data admission is in the connect callout, never omitted.
    expect(handoff?.connect.title).toMatch(/sample data/i);
  });
});

describe('navItemsFor', () => {
  it('hides pricing on a self-hosted instance', () => {
    const selfHost = navItemsFor({ hosted: false }).map((item) => item.href);
    expect(selfHost).not.toContain('/pricing');
    expect(selfHost).toContain('/settings');
    expect(navItemsFor({}).map((item) => item.href)).not.toContain('/pricing');
  });

  it('shows pricing on the managed cloud', () => {
    expect(navItemsFor({ hosted: true }).map((item) => item.href)).toContain('/pricing');
  });
});
