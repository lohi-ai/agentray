// Product information architecture mapped onto the four backend layers:
//   Channel (chat/mcp/schedule/webhook/lab) → Workloads (Garden packs)
//   → Runtime (conversation / run) → Data (dataplane views).
// Pure functions so unit tests can call the same helpers the shell uses.

// Group headings are the layers. Chat is the runtime front door. Operations
// are channels. Agents are workloads. Prototypes are a Product child, not a
// peer of Operations.
export type NavGroupId = 'Runtime' | 'Channels' | 'Workloads' | 'Data' | 'Workspace';

export type NavItemDef = {
  href: string;
  label: string;
  group: NavGroupId;
  // Extra path prefixes that light this item (nested or leftover routes).
  aliases?: readonly string[];
  // Rendered only on the managed cloud. Self-host never sees it.
  hostedOnly?: boolean;
};

export const NAV_ITEMS: readonly NavItemDef[] = [
  // Chat is the landing. Set up is the job index — it used to alias Chat and
  // hide behind a ghost header button, which made the clearest screen the
  // hardest to find.
  { href: '/chat', label: 'Chat', group: 'Runtime' },
  { href: '/start', label: 'Set up', group: 'Runtime' },
  { href: '/operations', label: 'Operations', group: 'Channels' },
  { href: '/agents', label: 'Agents', group: 'Workloads', aliases: ['/teams', '/marketplace', '/monitor', '/agent'] },
  { href: '/dashboard', label: 'Dashboards', group: 'Data', aliases: ['/dashboards', '/templates', '/sql'] },
  { href: '/web-analytics', label: 'Traffic', group: 'Data', aliases: ['/traffic'] },
  { href: '/product', label: 'Product', group: 'Data', aliases: ['/prototypes'] },
  { href: '/persons', label: 'People', group: 'Data', aliases: ['/cohorts'] },
  { href: '/events', label: 'Events', group: 'Data', aliases: ['/replay'] },
  { href: '/settings', label: 'Settings', group: 'Workspace', aliases: ['/alerts'] },
  { href: '/pricing', label: 'Plans', group: 'Workspace', hostedOnly: true },
];

// navItemsFor drops hosted-only surfaces on a self-hosted instance. A
// `docker compose up` operator must never be shown a pricing page or a plan
// ceiling they cannot buy past — see AGENTRAY_HOSTED / config.Hosted.
export function navItemsFor(opts: { hosted?: boolean }, items: readonly NavItemDef[] = NAV_ITEMS): NavItemDef[] {
  return items.filter((item) => !item.hostedOnly || !!opts.hosted);
}

// Web mirror of internal/channels. Shipped kinds are reachable; reserved
// kinds are listed so the UI can say “not yet” instead of inventing a surface.
export type ChannelInfo = {
  kind: 'chat' | 'mcp' | 'schedule' | 'webhook' | 'lab' | 'support_widget' | 'voice';
  label: string;
  shipped: boolean;
  href: string;
};

export const CHANNEL_CATALOG: readonly ChannelInfo[] = [
  { kind: 'chat', label: 'Chat', shipped: true, href: '/chat' },
  { kind: 'mcp', label: 'MCP', shipped: true, href: '/settings' },
  // Both point at /operations, not /agents: a schedule and a webhook are the two
  // things that start work without a human, and that is now a surface rather
  // than tab four of one agent's setup page.
  { kind: 'schedule', label: 'Schedule', shipped: true, href: '/operations' },
  { kind: 'webhook', label: 'Webhook', shipped: true, href: '/operations' },
  { kind: 'lab', label: 'Lab', shipped: true, href: '/agents' },
  { kind: 'support_widget', label: 'Support', shipped: false, href: '' },
  { kind: 'voice', label: 'Voice', shipped: false, href: '' },
];

export type WorkloadCategory = {
  id: 'validate' | 'growth' | 'marketing' | 'data' | 'operator' | 'support';
  label: string;
  reserved: boolean;
};

// Mirror of internal/workloads categories. `reserved` means the category ships
// no pack — the UI says "not yet" rather than inventing a surface. Operator
// stopped being reserved when ops-watch shipped; validate is the pre-product
// category, the one whose pack runs with an empty event store.
export const WORKLOAD_CATEGORIES: readonly WorkloadCategory[] = [
  { id: 'validate', label: 'Validate', reserved: false },
  { id: 'growth', label: 'Growth', reserved: false },
  { id: 'marketing', label: 'Marketing', reserved: false },
  { id: 'data', label: 'Data', reserved: false },
  { id: 'operator', label: 'Operator', reserved: false },
  { id: 'support', label: 'Support', reserved: true },
];

// The four layers used to be a static strip here, labelled with our own words
// ("Channel", "Workload", "Runtime", "Data") and rendered nowhere. They are now
// jobLayers() in ./jobs, which states each layer as what it does for the job on
// screen — a layer name on its own teaches a product owner nothing.

export type ChildSurface = { href: string; label: string; parentHref: string };

// Surfaces that used to sit as peer nav items. They stay reachable from a
// parent Main/Explore screen instead of competing with Chat / Agents / etc.
export const CHILD_SURFACES: readonly ChildSurface[] = [
  // Data-only: Chat is bleed. The rendered door is the header Set up control.
  { href: '/start', label: 'Set up', parentHref: '/chat' },
  { href: '/marketplace', label: 'Hire a teammate', parentHref: '/agents' },
  { href: '/teams', label: 'Teams', parentHref: '/agents' },
  { href: '/agents/monitor', label: 'Monitor', parentHref: '/agents' },
  { href: '/templates', label: 'Templates', parentHref: '/dashboard' },
  { href: '/sql', label: 'SQL', parentHref: '/dashboard' },
  { href: '/prototypes', label: 'Prototypes', parentHref: '/product' },
  { href: '/alerts', label: 'Alerts', parentHref: '/settings' },
  { href: '/cohorts', label: 'Cohorts', parentHref: '/persons' },
  { href: '/replay', label: 'Replay', parentHref: '/events' },
];

// Third-party channels the product will grow. Not CHANNEL_CATALOG kinds —
// those mirror internal/channels, and slack/discord/telegram are not registered.
export type FutureChannel = { kind: 'slack' | 'discord' | 'telegram'; label: string; detail: string };

export const FUTURE_CHANNELS: readonly FutureChannel[] = [
  { kind: 'slack', label: 'Slack', detail: 'Talk to the same teammate from Slack.' },
  { kind: 'discord', label: 'Discord', detail: 'Same runtime, from a Discord channel.' },
  { kind: 'telegram', label: 'Telegram', detail: 'Ask on the go. The thread still lands here.' },
];

export function navPrefixes(item: NavItemDef): string[] {
  return [item.href, ...(item.aliases ?? [])];
}

export function navGroups(items: readonly NavItemDef[] = NAV_ITEMS): Array<{ id: NavGroupId; label: NavGroupId; items: NavItemDef[] }> {
  const order: NavGroupId[] = ['Runtime', 'Channels', 'Workloads', 'Data', 'Workspace'];
  return order.map((id) => ({ id, label: id, items: items.filter((item) => item.group === id) }));
}

// Longest matching prefix across each item's href and aliases. Nested lab and
// monitor routes therefore light Agents (`/agents`) rather than a peer item.
export function matchActiveHref(pathname: string, items: readonly NavItemDef[] = NAV_ITEMS): string {
  const path = pathname || '/';
  let best = '';
  let bestLen = -1;
  for (const item of items) {
    for (const prefix of navPrefixes(item)) {
      if (path === prefix || path.startsWith(`${prefix}/`)) {
        if (prefix.length > bestLen) {
          best = item.href;
          bestLen = prefix.length;
        }
      }
    }
  }
  return best;
}

export function navGroupForPath(pathname: string, items: readonly NavItemDef[] = NAV_ITEMS): NavGroupId | '' {
  const href = matchActiveHref(pathname, items);
  return items.find((item) => item.href === href)?.group ?? '';
}

export function childSurfacesFor(parentHref: string, surfaces: readonly ChildSurface[] = CHILD_SURFACES): ChildSurface[] {
  return surfaces.filter((surface) => surface.parentHref === parentHref);
}

export const SIGNED_IN_LANDING = '/chat';

export function signedInLandingTarget(): string {
  return SIGNED_IN_LANDING;
}

// Starter prompts used to live here as a flat catalog keyed by capability
// (growth / marketing / data / ops / runtime), which asked a new owner to know
// which of our capabilities their problem belonged to. They now live on the
// job that owns them — see JOBS in ./jobs — so the front door offers the same
// prompts grouped by the situation the owner is actually in.

export type FirstRunInput = {
  // Catalog rows or bare names — only length matters for first-run gating.
  eventNames: { readonly length: number } | null | undefined;
  catalogReady: boolean;
  // false = we know there is no workspace model key. undefined = still loading.
  hasModelKey?: boolean;
  // Published sample workspace — still show connect, and label the opinion.
  sample?: boolean;
};

export function isSampleProject(project?: { name?: string } | null): boolean {
  return /^demo$/i.test(project?.name ?? '');
}

export type FirstValuePath = {
  showFirstEvent: boolean;
  showFirstAsk: boolean;
};

// FIRST_RUN_PROMPT is the one seeded question the first-run panel fires at the
// already-hired agent. It is phrased as the job the product exists to do, not as
// a demo script, so the answer the stranger watches arrive is a real one.
export const FIRST_RUN_PROMPT = 'What is the single weakest step in my activation funnel?';

export type FirstRunGate = {
  // Agent runs for the active project. Only the count matters — a run that
  // errored still means the user has pressed the button and seen the runtime.
  runs: { readonly length: number } | null | undefined;
  // false while the runs query is in flight. The panel must never flash for a
  // workspace that has already run, so an unresolved gate reads as "not first".
  runsReady: boolean;
  // Turns already in the open thread. A thread mid-conversation is not a first
  // session even when the workspace has no persisted run yet.
  turnCount?: number;
};

// isFirstRun gates the first-session panel. It keys off *agent runs*, never off
// event emptiness: signup seeds a populated Demo project, so an events-based
// check would leave the panel showing forever (own project) or never (Demo).
export function isFirstRun(gate: FirstRunGate): boolean {
  if (!gate.runsReady) return false;
  if ((gate.turnCount ?? 0) > 0) return false;
  return (gate.runs?.length ?? 0) === 0;
}

export type FirstRunHandoff = {
  dashboard: { label: string; title: string; detail: string; action: string; href: string };
  connect: { label: string; title: string; detail: string; action: string; href: string };
};

// firstRunHandoff is the end of the first run: the payoff (the dashboard the
// agent just read) and the next commitment (point it at your own product). Two
// callouts, in that order — the payoff is never withheld behind the upsell.
// Returns null until the seeded turn has actually settled, and stays null when
// it failed: a failed run gets the error surface, not a handover.
//
// turnCount scopes it to the seeded exchange: it counts what the *user* asked
// (not messages — a ChatMsg is one message, so an exchange is two), so the first
// run is turnCount 1 and anything above that is a later question. A user who
// steers their first run counts 2 and loses the handoff — the conservative miss:
// showing it twice is worse than showing it once, and they have already engaged.
// The "started" flag is session-sticky,
// so without this bound the
// handoff re-renders under every later answer in the session, and under any older
// thread the user clicks into — a "your dashboard is ready" attached to a turn
// that built nothing.
export function firstRunHandoff(input: { started: boolean; settled: boolean; failed: boolean; turnCount?: number }): FirstRunHandoff | null {
  if (!input.started || !input.settled || input.failed) return null;
  if ((input.turnCount ?? 1) > 1) return null;
  return {
    dashboard: {
      label: 'Your dashboard',
      title: 'Product Overview is ready',
      detail: 'I read the funnel and pinned what moved. Open it — you watched it get built.',
      action: 'Open your dashboard',
      href: '/dashboard',
    },
    connect: {
      label: 'Next',
      title: 'That was sample data. Point me at your product.',
      detail: 'Send one event from your own app and I will do that again on numbers you care about.',
      action: 'Connect my product',
      href: '/settings?tab=keys',
    },
  };
}

// Empty catalog (zero event names, catalog has loaded) turns on the guided
// first-event + first-ask path. A published Demo workspace keeps the connect
// guide on so sample data never hides "bring your product."
export function firstValuePath(input: FirstRunInput): FirstValuePath {
  const empty = input.catalogReady && (input.eventNames?.length ?? 0) === 0;
  const connect = empty || !!input.sample;
  return { showFirstEvent: connect, showFirstAsk: empty };
}

export function shouldShowFirstEventGuide(input: FirstRunInput): boolean {
  return firstValuePath(input).showFirstEvent;
}

type CatalogEvent = { name: string; count: number };

function catalogEvents(names: FirstRunInput['eventNames']): CatalogEvent[] {
  if (!names) return [];
  const out: CatalogEvent[] = [];
  for (let i = 0; i < names.length; i += 1) {
    const row = (names as readonly unknown[])[i];
    if (typeof row === 'string' && row) out.push({ name: row, count: 0 });
    else if (row && typeof row === 'object') {
      const rec = row as { name?: unknown; event_name?: unknown; count?: unknown };
      const name = rec.event_name ?? rec.name;
      const count = typeof rec.count === 'number' && rec.count > 0 ? rec.count : 0;
      if (typeof name === 'string' && name) out.push({ name, count });
    }
  }
  return out;
}

function catalogLabels(names: FirstRunInput['eventNames']): string[] {
  return catalogEvents(names).map((e) => e.name);
}

const FUNNEL_STAGES = [
  { id: 'visit', label: 'visit', match: /page_?view|visit|session_start|app_open|^loaded$/i },
  { id: 'signup', label: 'signup', match: /sign[_\s-]?up|register|account_created|signed_up/i },
  { id: 'activation', label: 'activation', match: /activat|onboard|first_value|aha|setup_complete|completed_setup|first_(action|event|project)|value_realized/i },
  { id: 'retention', label: 'return', match: /return|repeat|day_?[27]|habit|engaged/i },
  { id: 'revenue', label: 'purchase', match: /purchase|paid|checkout|subscri|upgrade|payment|invoice|conversion/i },
] as const;

export const DEFAULT_FUNNEL_STEPS = ['user.pageview', 'user.signup', 'user.conversion'] as const;

type FunnelMatch = { id: string; label: string; event: string; count: number; order: number };

function stageOfName(name: string): number {
  let claimed = -1;
  FUNNEL_STAGES.forEach((stage, order) => {
    if (stage.match.test(name)) claimed = order;
  });
  return claimed;
}

function matchedFunnelSteps(names: FirstRunInput['eventNames']): FunnelMatch[] {
  const events = catalogEvents(names);
  const steps: FunnelMatch[] = [];
  FUNNEL_STAGES.forEach((stage, order) => {
    const matched = events.filter((e) => stageOfName(e.name) === order);
    if (matched.length === 0) return;
    steps.push({
      id: stage.id,
      label: stage.label,
      event: matched[0].name,
      count: matched.reduce((sum, e) => sum + e.count, 0),
      order,
    });
  });
  return steps;
}

// funnelStepNames is the honest Product-page funnel: matched activation stages
// in order, or the default contract when the catalog has fewer than two.
export function funnelStepNames(names: FirstRunInput['eventNames']): string[] {
  const steps = matchedFunnelSteps(names);
  if (steps.length >= 2) return steps.map((s) => s.event);
  return [...DEFAULT_FUNNEL_STEPS];
}

export type WeakestLink = {
  from: string;
  to: string;
  // Plain-language stage names ("visit", "purchase"). `from`/`to` carry the raw
  // event name, which is the developer's word for the step, not the owner's —
  // the headline reads with these and keeps the event name as the evidence.
  fromLabel: string;
  toLabel: string;
  fromCount: number;
  toCount: number;
  rate: number;
  missing: boolean;
  // How many funnel stages sit untracked between the two we matched. Anything
  // above zero means we are NOT looking at a step, we are looking across a hole,
  // and the honest headline is the hole rather than a conversion rate.
  stagesSkipped: number;
};

// weakestLink finds the biggest drop in a recognised activation funnel from
// catalog names + counts. No model required — this is the written opinion the
// empty-state used to fake with a "track activation" hint.
export function weakestLink(names: FirstRunInput['eventNames']): WeakestLink | null {
  const steps = matchedFunnelSteps(names);
  if (steps.length === 0) return null;

  if (steps.length >= 2) {
    let worst: WeakestLink | null = null;
    for (let i = 0; i < steps.length - 1; i += 1) {
      const from = steps[i];
      const to = steps[i + 1];
      const rate = from.count > 0 ? to.count / from.count : 0;
      const cand: WeakestLink = {
        from: from.event,
        to: to.event,
        fromLabel: from.label,
        toLabel: to.label,
        fromCount: from.count,
        toCount: to.count,
        rate,
        missing: false,
        stagesSkipped: to.order - from.order - 1,
      };
      if (!worst || rate < worst.rate) worst = cand;
    }
    return worst;
  }

  const idx = FUNNEL_STAGES.findIndex((s) => s.id === steps[0].id);
  const next = FUNNEL_STAGES[idx + 1];
  if (next) {
    return {
      from: steps[0].event,
      to: next.label,
      fromLabel: steps[0].label,
      toLabel: next.label,
      fromCount: steps[0].count,
      toCount: 0,
      rate: 0,
      missing: true,
      stagesSkipped: 0,
    };
  }
  return null;
}

// formatRate never rounds a real conversion down to "0%". A funnel that
// converts 16 of 4,783 is 0.3%, not zero — and printing "0%" next to a
// non-zero count is the fastest way to make an owner stop believing the
// numbers. Below 0.1% we say so rather than inventing precision.
export function formatRate(rate: number): string {
  const pct = rate * 100;
  if (pct <= 0) return '0%';
  if (pct < 0.1) return '<0.1%';
  if (pct < 10) return `${Number(pct.toFixed(1))}%`;
  return `${Math.round(pct)}%`;
}

function countLabel(n: number, noun = 'people'): string {
  if (n === 1 && noun === 'people') return '1 person';
  return `${n.toLocaleString()} ${noun}`;
}

function weakestNotice(link: WeakestLink): FirstSessionNotice {
  const fromN = link.fromCount > 0 ? countLabel(link.fromCount) : '';
  const toN = link.toCount.toLocaleString();
  const pct = formatRate(link.rate);
  if (link.missing) {
    return {
      kind: 'noticed',
      title: fromN ? `${fromN} hit ${link.fromLabel}, then nothing` : `I can see ${link.fromLabel}`,
      detail: fromN
        ? `${fromN} hit ${link.fromLabel} (${link.from}). I don’t see ${link.toLabel} at all — so the funnel stops there.`
        : `I only see ${link.fromLabel} (${link.from}). The next step — ${link.toLabel} — isn’t tracked, so the funnel stops here.`,
      ask: `What should we track for ${link.toLabel} after ${link.fromLabel}?`,
    };
  }
  // A gap between the two matched stages means the stages in between are
  // untracked. Reporting that span as "the biggest drop" invites the owner to
  // go optimise a step, when the real finding is that they cannot see the
  // steps. Name the hole instead — it is both truer and more actionable.
  if (link.stagesSkipped > 0) {
    return {
      kind: 'noticed',
      title: `Nothing is tracked between ${link.fromLabel} and ${link.toLabel}`,
      detail: fromN
        ? `${toN} of ${fromN} reach ${link.toLabel} (${pct}), but the steps in between aren’t tracked — so I can’t tell you where the rest go.`
        : `${link.fromLabel} and ${link.toLabel} are tracked; the steps in between aren’t, so the drop-off is invisible.`,
      ask: `What should we track between ${link.fromLabel} and ${link.toLabel} to see where people drop off?`,
    };
  }
  return {
    kind: 'noticed',
    title: `${link.fromLabel} → ${link.toLabel} is the weakest step`,
    detail: fromN
      ? `${toN} of ${fromN} (${pct}) make it from ${link.fromLabel} to ${link.toLabel}. That’s the biggest drop.`
      : `The biggest drop is ${link.fromLabel} → ${link.toLabel}.`,
    ask: `What should we test this week to lift ${link.fromLabel} → ${link.toLabel}?`,
  };
}

export type FirstSessionNotice = {
  kind: 'empty' | 'noticed' | 'setup';
  title: string;
  detail: string;
  ask: string;
  href?: string;
};

export function settingsPath(tab?: 'ai' | 'keys' | 'connectors' | 'plan' | 'projects'): string {
  if (tab === 'ai') return '/settings?tab=ai';
  if (tab === 'keys') return '/settings?tab=keys';
  if (tab === 'connectors') return '/settings?tab=connectors';
  if (tab === 'plan') return '/settings?tab=plan';
  if (tab === 'projects') return '/settings?tab=projects';
  return '/settings';
}

// List root to land on after a project switch, or null when the current route
// is already safe (chat, list pages, settings, sql). `/agents/monitor` is a
// fleet page, not an agent id.
export function projectDetailRoot(pathname: string): string | null {
  const path = pathname.split('?')[0] || '';
  if (path === '/agents/monitor' || path.startsWith('/agents/monitor/')) return null;
  if (/^\/agents\/[^/]+/.test(path)) return '/agents';
  if (/^\/teams\/[^/]+/.test(path)) return '/teams';
  if (/^\/operations\/[^/]+/.test(path)) return '/operations';
  if (/^\/prototypes\/[^/]+/.test(path)) return '/prototypes';
  return null;
}

export function settingsTabFromQuery(search: string): string {
  const raw = search.startsWith('?') ? search.slice(1) : search;
  const tab = new URLSearchParams(raw).get('tab');
  if (tab === 'ai') return 'AI Provider';
  if (tab === 'keys') return 'API keys';
  if (tab === 'connectors') return 'Data connectors';
  if (tab === 'members') return 'Members';
  if (tab === 'projects') return 'Projects';
  if (tab === 'activity') return 'Activity';
  if (tab === 'plan') return 'Plan & usage';
  return 'Workspace';
}

// The paid moment: after the first event lands, the product should sound like
// an analyst who noticed something — not like an empty chart board. The
// weakest-link is computed from catalog counts here; it must not wait on a model.
export function firstSessionNotice(input: FirstRunInput): FirstSessionNotice | null {
  if (!input.catalogReady) return null;
  const names = catalogLabels(input.eventNames);
  if (names.length === 0) {
    return {
      kind: 'empty',
      title: 'Send one event, then ask me what to do',
      detail: 'I can’t coach growth without a signal. Drop a snippet, or ask me what to track first.',
      ask: 'What should we track first to see if people activate?',
    };
  }
  const link = weakestLink(input.eventNames);
  const notice = link ? weakestNotice(link) : namesNotice(names);
  if (input.sample) {
    notice.detail = `Sample workspace. ${notice.detail} Connect your website, app, or warehouse to see YOUR drop.`;
  }
  if (input.hasModelKey === false) {
    notice.detail = `${notice.detail} Add an AI key in Settings to ask me to go deeper.`;
    notice.href = settingsPath('ai');
  }
  return notice;
}

function namesNotice(names: string[]): FirstSessionNotice {
  const shown = names.slice(0, 3).join(', ');
  const extra = names.length > 3 ? ` +${names.length - 3} more` : '';
  return {
    kind: 'noticed',
    title: `I can see ${shown}${extra}`,
    detail: 'Ask me the weakest step, or what to measure next. I’ll use these events — not a blank board.',
    ask: `What should we do next after ${names[0]}?`,
  };
}

// Matches the two formatAgentError sentences, and the raw errors behind them
// only when they ARE the message. An answer that says "signups paused" or
// "the connector has no api key" is prose, not a broken run, and must not pin
// a "turn the agent on" bar over the whole thread.
export function needsKeyRecovery(text: string): boolean {
  const raw = (text ?? '').trim();
  return DISABLED_ERROR.test(raw)
    || NO_KEY_ERROR.test(raw)
    || /add an ai key in settings|growth lead is paused/i.test(raw);
}

export function threadNeedsRecovery(
  messages: ReadonlyArray<{ text?: string }>,
  opts?: { hasModelKey?: boolean },
): boolean {
  return messages.some((m) => {
    if (!needsKeyRecovery(m.text ?? '')) return false;
    const action = recoveryAction(m.text ?? '');
    // Stale "add a key" turns stay in the transcript; don't block a workspace
    // that already has a hosted or pasted key.
    if (action.href.includes('settings') && opts?.hasModelKey) return false;
    return true;
  });
}

// writtenOpinion is the sentence a customer would pay for: one weakest step,
// grounded in catalog counts, no model required.
export function writtenOpinion(input: FirstRunInput): string {
  const link = weakestLink(input.eventNames);
  if (link?.missing) {
    const who = link.fromCount > 0 ? countLabel(link.fromCount) : 'People';
    return [
      `**${link.from} → ${link.to} is the weakest step — 0%.**`,
      '',
      `${who} hit \`${link.from}\`. I don’t see ${link.to} at all.`,
      '',
      `Track one ${link.to} event this week: the first time a person gets value after ${link.from}. Until that event exists I can’t tell you which step to fix — the funnel stops here.`,
      '',
      `Suggested name: \`${link.to === 'activation' ? 'first_value' : link.to}\`. Send it once, then ask me again.`,
    ].join('\n');
  }
  if (link) {
    const pct = Math.round(link.rate * 100);
    const who = link.fromCount > 0 ? countLabel(link.fromCount) : 'people';
    return [
      `**${link.from} → ${link.to} is the weakest step (${pct}%).**`,
      '',
      `${link.toCount.toLocaleString()} of ${who} make it from \`${link.from}\` to \`${link.to}\`. That’s the biggest drop I can see.`,
      '',
      `This week: one test on that step. Don’t add a new dashboard — change the product so more people who hit ${link.from} also hit ${link.to}.`,
    ].join('\n');
  }
  const names = catalogLabels(input.eventNames);
  if (names.length === 0) {
    return [
      '**I can’t see a funnel yet.**',
      '',
      'Send one event this week: the moment a person first gets value. Name it something you can see in the product (`first_value`, `setup_complete`). Then ask me again.',
    ].join('\n');
  }
  return `I can see ${names.slice(0, 3).join(', ')}. Ask me the weakest step and I’ll compute the drop — I won’t guess.`;
}

// instantReply answers the product's own growth questions from the catalog so
// the first ask is never a "great question to think through" hedge.
export function instantReply(input: FirstRunInput, prompt: string): string | null {
  if (!input.catalogReady) return null;
  const trimmed = prompt.trim();
  if (!trimmed) return null;
  const notice = firstSessionNotice(input);
  const recommended = !!notice?.ask && trimmed === notice.ask.trim();
  const funnelAsk = /weakest|activation|what should we track|funnel|drop.?off|what should we (do|test|measure)/i.test(trimmed);
  if (!recommended && !funnelAsk) return null;
  return writtenOpinion(input);
}

export function recoveryAction(text: string): { label: string; href: string; detail: string } {
  if (/growth lead is paused|agent is disabled/i.test(text) && !/ai key|model key|api key/i.test(text)) {
    return {
      label: 'Open Team',
      href: '/agents',
      detail: 'Growth Lead is paused. Turn the agent on in Team → Set up.',
    };
  }
  return {
    label: 'Add AI key',
    href: settingsPath('ai'),
    detail: 'Growth Lead can’t answer until you add an AI key.',
  };
}

export function shouldStartDocksOpen(input: { threadCount: number; recommendationCount: number }): boolean {
  return input.threadCount > 0 || input.recommendationCount > 0;
}

// formatAgentError runs over EVERY assistant turn (live finals and replayed
// history alike), so it only rewrites text that IS a bare runtime error — the
// whole message, optionally prefixed with "error:". A real answer that merely
// mentions an API key mid-sentence must survive untouched.
const DISABLED_ERROR = /^(?:error:\s*)?agent is disabled\b/i;
const NO_KEY_ERROR = /^(?:error:\s*)?(?:no workspace model key|no api key)\b/i;

export function formatAgentError(message: string): string {
  const raw = message.trim();
  if (DISABLED_ERROR.test(raw)) {
    return 'Growth Lead is paused. Open Team → Set up and turn the agent on.';
  }
  if (NO_KEY_ERROR.test(raw)) {
    return 'Add an AI key in Settings so I can answer. One key is enough.';
  }
  return raw || 'Something went wrong. Try again.';
}
