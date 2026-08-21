import { describe, expect, it } from 'vitest';
import {
  CHANNEL_CATALOG,
  CHILD_SURFACES,
  FUTURE_CHANNELS,
  NAV_ITEMS,
  WORKLOAD_CATEGORIES,
  childSurfacesFor,
  firstSessionNotice,
  firstValuePath,
  formatAgentError,
  instantReply,
  isRunError,
  needsKeyRecovery,
  writtenOpinion,
  recoveryAction,
  settingsPath,
  projectDetailRoot,
  threadNeedsRecovery,
  weakestLink,
  funnelStepNames,
  settingsTabFromQuery,
  matchActiveHref,
  navItemsFor,
  shouldStartDocksOpen,
  navGroupForPath,
  navGroups,
  shouldShowFirstEventGuide,
  signedInLandingTarget,
  formatRate,
  projectAccess,
  tourSteps,
  nextTourStep,
  tourProgress,
  tourComplete,
  showTour,
  tourHandoff,
} from './ia';

// A workspace part-way through the tour: demo configured, the reader is inside
// it, their own project exists and has nothing in it.
const IN_DEMO = {
  ready: true,
  hasDemo: true,
  inDemo: true,
  demoName: 'Kiem Lai',
  ownProjectName: 'My product',
  ownProjectCount: 1,
  ownEventNameCount: 0,
  ownScheduled: false,
};

describe('nav grouping', () => {
  it('groups the shell by layer: Runtime → Channels → Workloads → Data → Workspace', () => {
    const groups = navGroups();
    expect(groups.map((g) => g.id)).toEqual(['Runtime', 'Channels', 'Workloads', 'Data', 'Workspace']);
    expect(groups[0].items.map((i) => i.label)).toEqual(['Chat', 'Set up']);
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
    ['/start', '/start', 'Runtime'],
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

});

describe('firstSessionNotice', () => {
  it('coaches an empty project to send a signal or ask what to track', () => {
    const notice = firstSessionNotice({ eventNames: [], catalogReady: true });
    expect(notice?.kind).toBe('empty');
    expect(notice?.ask.length).toBeGreaterThan(0);
  });

  it('still writes the weakest-link when there is no AI key', () => {
    const notice = firstSessionNotice({
      eventNames: [{ event_name: 'signup', count: 40, users: 12 }],
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
      { event_name: 'pageview', count: 6000, users: 1000 },
      { event_name: 'signup', count: 300, users: 80 },
      { event_name: 'purchase', count: 25, users: 10 },
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

  it('never rounds a real conversion down to zero percent', () => {
    // 16 of 4,783 is 0.3%, not 0%. Printing "0%" beside a non-zero count is
    // how an owner learns to stop trusting the numbers.
    expect(formatRate(16 / 4783)).toBe('0.3%');
    expect(formatRate(0.0004)).toBe('<0.1%');
    expect(formatRate(0)).toBe('0%');
    expect(formatRate(0.08)).toBe('8%');
    expect(formatRate(0.5)).toBe('50%');
  });

  it('carries plain-language stage labels alongside the raw event name', () => {
    const link = weakestLink([
      { event_name: 'user.pageview', count: 28000, users: 4783 },
      { event_name: 'subscription_activated', count: 20, users: 16 },
    ]);
    expect(link?.from).toBe('user.pageview');
    expect(link?.fromLabel).toBe('visit');
    expect(link?.toLabel).toBe('purchase');
  });

  it('counts the untracked stages between the two it matched', () => {
    // visit -> purchase skips signup, activation and return.
    const gap = weakestLink([
      { event_name: 'user.pageview', count: 28000, users: 4783 },
      { event_name: 'subscription_activated', count: 20, users: 16 },
    ]);
    expect(gap?.stagesSkipped).toBe(3);
    const adjacent = weakestLink([
      { event_name: 'pageview', count: 6000, users: 1000 },
      { event_name: 'signup', count: 300, users: 80 },
    ]);
    expect(adjacent?.stagesSkipped).toBe(0);
  });

  // The defect this pins: the catalog carries event volume AND people, and the
  // written opinion speaks in people. Reading `count` printed 5,128 pageviews as
  // "5,128 people" on a project with 835 real visitors.
  it('counts people, not event volume', () => {
    const link = weakestLink([
      { event_name: 'user.pageview', count: 5128, users: 835 },
      { event_name: 'signup', count: 620, users: 105 },
    ]);
    expect(link?.fromCount).toBe(835);
    expect(link?.toCount).toBe(105);
    expect(link?.rate).toBeCloseTo(105 / 835);
  });

  // A stage can match several events; the funnel only ever queries the busiest
  // one. Summing across them described a step the funnel never ran, and summing
  // uniques would double-count anyone who fired two of them.
  it('describes the event the funnel actually queries', () => {
    const names = [
      { event_name: 'pageview', count: 6000, users: 1000 },
      { event_name: 'app_open', count: 900, users: 400 },
      { event_name: 'signup', count: 300, users: 80 },
    ];
    expect(funnelStepNames(names)[0]).toBe('pageview');
    expect(weakestLink(names)?.fromCount).toBe(1000);
  });

  // The catalog gives two independent people counts. It cannot establish that
  // the `to` people are a subset of the `from` people — only the server-side
  // windowFunnel can. So the number is a gap, and a gap can never exceed 100%.
  it('never reports passage above 100%', () => {
    const link = weakestLink([
      { event_name: 'signup', count: 2, users: 2 },
      { event_name: 'subscription_activated', count: 4, users: 4 },
    ]);
    expect(link?.rate).toBeLessThanOrEqual(1);
  });

  it('degrades to no number rather than a wrong one when users is absent', () => {
    const link = weakestLink([
      { event_name: 'pageview', count: 6000 },
      { event_name: 'signup', count: 300 },
    ]);
    expect(link?.fromCount).toBe(0);
    expect(writtenOpinion({ eventNames: [{ event_name: 'pageview', count: 6000 }], catalogReady: true }))
      .not.toMatch(/6,000 people/);
  });

  it('treats a missing next stage as 0% conversion', () => {
    const link = weakestLink([{ event_name: 'signup', count: 40, users: 12 }]);
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

describe('funnelStepNames', () => {
  it('returns matched catalog events in stage order', () => {
    expect(funnelStepNames([
      { event_name: 'user.conversion', count: 2 },
      { event_name: 'user.pageview', count: 10 },
      { event_name: 'user.signup', count: 4 },
    ])).toEqual(['user.pageview', 'user.signup', 'user.conversion']);
  });

  it('falls back to the contract when fewer than two stages match', () => {
    expect(funnelStepNames([{ event_name: 'hello_agentray', count: 1 }])).toEqual([
      'user.pageview', 'user.signup', 'user.conversion',
    ]);
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
      eventNames: [{ event_name: 'signup', count: 40, users: 12 }],
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

describe('projectAccess', () => {
  it('drives affordances off the role and the demo mark, never off the name', () => {
    // The demo is a REAL project on a real site and can be called anything —
    // the old check was `/^demo$/i.test(project.name)`, which both missed the
    // real one and would have locked a customer's own project called "Demo".
    expect(projectAccess({ name: 'Demo', role: 'owner' }).canWrite).toBe(true);
    expect(projectAccess({ name: 'Kiem Lai', role: 'viewer', is_demo: true }).canWrite).toBe(false);
  });

  it('says why, in the reader’s words, and distinguishes the demo from a plain viewer', () => {
    expect(projectAccess({ role: 'viewer', is_demo: true }).reason).toMatch(/shared demo/i);
    expect(projectAccess({ role: 'viewer' }).reason).toMatch(/viewer in this workspace/i);
    expect(projectAccess({ role: 'admin' }).reason).toBe('');
  });

  it('mirrors the API’s writing roles, and treats an unresolved project as writable', () => {
    for (const role of ['owner', 'admin', 'member']) {
      expect(projectAccess({ role }).canWrite).toBe(true);
    }
    // Nothing loaded yet: disabling every control on every page for one frame of
    // each navigation is worse than letting the API make the real decision.
    expect(projectAccess(null).canWrite).toBe(true);
    expect(projectAccess({ name: 'x' }).canWrite).toBe(true);
  });
});

describe('tourSteps', () => {
  it('walks demo → explore → ask → project → connect → schedule', () => {
    expect(tourSteps(IN_DEMO).map((s) => s.id)).toEqual([
      'demo', 'explore', 'ask', 'project', 'connect', 'schedule',
    ]);
  });

  it('reads as a whole three-step tour on an instance with no demo', () => {
    // A self-hosted `docker compose up` has no demo. The tour must not be a
    // six-step one with its first three missing.
    const steps = tourSteps({ ...IN_DEMO, hasDemo: false, inDemo: false });
    expect(steps.map((s) => s.id)).toEqual(['project', 'connect', 'schedule']);
    expect(steps.map((s) => s.n)).toEqual([1, 2, 3]);
  });

  it('never calls the demo the reader’s own, and never calls it a sample', () => {
    const detail = tourSteps(IN_DEMO).map((s) => `${s.label} ${s.detail}`).join(' ');
    expect(detail).not.toMatch(/sample/i);
    expect(detail).toMatch(/someone else runs/i);
    expect(detail).toMatch(/viewer/i);
  });

  it('ticks connect and schedule from real workspace state, not from a flag', () => {
    const connected = tourSteps({ ...IN_DEMO, ownEventNameCount: 3, ownScheduled: true });
    expect(connected.find((s) => s.id === 'connect')?.done).toBe(true);
    expect(connected.find((s) => s.id === 'connect')?.detail).toMatch(/3 event names/);
    expect(connected.find((s) => s.id === 'schedule')?.done).toBe(true);
  });

  it('says the schedule spends the reader’s own key, so arming stays a choice', () => {
    const step = tourSteps(IN_DEMO).find((s) => s.id === 'schedule');
    expect(step?.done).toBe(false);
    expect(step?.detail).toMatch(/spends your model key/i);
    expect(step?.action.act).toBe('arm');
  });
});

describe('tourProgress', () => {
  it('counts only what a query can answer, so the strip can reach its total', () => {
    // Nobody can read back which dashboards someone opened, or whether THIS
    // visitor asked the demo anything — the demo's runs belong to every
    // visitor. Those three steps are out of the count rather than stuck at
    // unticked forever.
    expect(tourProgress(tourSteps(IN_DEMO))).toEqual({ done: 1, total: 3 });
    expect(tourProgress(tourSteps({ ...IN_DEMO, ownEventNameCount: 2 }))).toEqual({ done: 2, total: 3 });
    expect(tourProgress(tourSteps({ ...IN_DEMO, ownProjectCount: 0 }))).toEqual({ done: 0, total: 3 });
  });
});

describe('nextTourStep', () => {
  it('keeps the pointer in the demo while the reader is in the demo', () => {
    expect(nextTourStep(IN_DEMO)?.id).toBe('ask');
  });

  it('resumes on the right step once they are in their own project', () => {
    // Second visit, own project active, nothing connected: the pointer must not
    // send them back to someone else's site.
    expect(nextTourStep({ ...IN_DEMO, inDemo: false })?.id).toBe('connect');
    expect(nextTourStep({ ...IN_DEMO, inDemo: false, ownEventNameCount: 4 })?.id).toBe('schedule');
    expect(nextTourStep({ ...IN_DEMO, inDemo: false, ownProjectCount: 0 })?.id).toBe('project');
  });

  it('starts at the connect half on an instance with no demo', () => {
    expect(nextTourStep({ ...IN_DEMO, hasDemo: false, inDemo: false })?.id).toBe('connect');
  });

  it('has nothing left once the three observable steps are done', () => {
    const done = { ...IN_DEMO, inDemo: false, ownEventNameCount: 5, ownScheduled: true };
    expect(nextTourStep(done)).toBeNull();
    expect(tourComplete(done)).toBe(true);
    expect(showTour(done)).toBe(false);
  });
});

describe('showTour', () => {
  it('renders nothing until the reads behind the ticks have settled', () => {
    // A step that flashes done and then undone is the same lie as a wrong tick.
    expect(showTour({ ...IN_DEMO, ready: false })).toBe(false);
    expect(showTour(IN_DEMO)).toBe(true);
  });
});

describe('tourHandoff', () => {
  it('stays quiet until the demo’s answer settles, and when it failed', () => {
    expect(tourHandoff(IN_DEMO, { settled: false, failed: false })).toBeNull();
    expect(tourHandoff(IN_DEMO, { settled: true, failed: true })).toBeNull();
  });

  it('hands the reader the next connect step after the demo answers', () => {
    expect(tourHandoff(IN_DEMO, { settled: true, failed: false })?.id).toBe('connect');
  });

  it('is derived, so it never appears outside the demo', () => {
    // The old handoff hung off a session-sticky flag and re-rendered under any
    // later thread. This one cannot: it reads where the reader actually is.
    expect(tourHandoff({ ...IN_DEMO, inDemo: false }, { settled: true, failed: false })).toBeNull();
    expect(tourHandoff({ ...IN_DEMO, hasDemo: false }, { settled: true, failed: false })).toBeNull();
  });
});

describe('tourSteps project step', () => {
  it('offers the switch from the demo and says “you’re here” once they are out of it', () => {
    const fromDemo = tourSteps(IN_DEMO).find((s) => s.id === 'project');
    expect(fromDemo?.action.act).toBe('open-own');
    const inOwn = tourSteps({ ...IN_DEMO, inDemo: false }).find((s) => s.id === 'project');
    expect(inOwn?.action.act).toBeUndefined();
    expect(inOwn?.action.label).toBe('You’re here');
  });

  it('offers creation when there is no project of their own', () => {
    const none = tourSteps({ ...IN_DEMO, ownProjectCount: 0 }).find((s) => s.id === 'project');
    expect(none?.action.href).toContain('projects');
    expect(none?.done).toBe(false);
  });
});

describe('isRunError', () => {
  it('calls a provider transport failure what it is', () => {
    expect(isRunError('provider chat (turn 1): 9router: unexpected response (status 429): {"error":…}')).toBe(true);
    expect(isRunError('error: run aborted')).toBe(true);
    expect(isRunError('')).toBe(true);
    expect(isRunError('no workspace model key')).toBe(true);
  });

  it('leaves a real answer alone, including one that talks about errors', () => {
    expect(isRunError('The weakest step is signup → first event: 46 of 744 get through.')).toBe(false);
    expect(isRunError('Nothing is broken — no error events in the last 24 hours.')).toBe(false);
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
