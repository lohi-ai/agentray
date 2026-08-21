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
};

export type FirstValuePath = {
  showFirstEvent: boolean;
  showFirstAsk: boolean;
};

// Empty catalog (zero event names, catalog has loaded) turns on the guided
// first-event + first-ask path.
export function firstValuePath(input: FirstRunInput): FirstValuePath {
  const empty = input.catalogReady && (input.eventNames?.length ?? 0) === 0;
  return { showFirstEvent: empty, showFirstAsk: empty };
}

export function shouldShowFirstEventGuide(input: FirstRunInput): boolean {
  return firstValuePath(input).showFirstEvent;
}

// ---- who may write here ----------------------------------------------------
//
// The shared demo (internal/dataplane/store/demo.go) puts every signed-up
// visitor inside a workspace somebody else owns, as a 'viewer'. The API refuses
// their writes at one choke point (internal/app/demo_guard.go), which is the
// correct place for the SECURITY decision and the wrong place for the UI one: a
// button that is present, clicked, and then answered 403 has already wasted the
// person's attention and taught them the product is broken.
//
// So the affordance decision is made here, from the two read-only facts the API
// already hands back on every project — the caller's `role` and whether the
// project lives in the demo workspace (`is_demo`). Never from the project's
// NAME: the demo is a real project on a real site and can be called anything.

// Mirror of writeRoles in internal/dataplane/store/auth.go. Anything absent —
// 'viewer', an empty role, a role a later release adds — reads.
const WRITE_ROLES = new Set(['owner', 'admin', 'member']);

export type ProjectLike = { role?: string; is_demo?: boolean; name?: string } | null | undefined;

export type ProjectAccess = {
  isDemo: boolean;
  role: string;
  canWrite: boolean;
  // Why not, in the user's words. Empty string when they can write.
  reason: string;
};

export function projectAccess(project: ProjectLike): ProjectAccess {
  const role = (project?.role ?? '').trim().toLowerCase();
  const isDemo = !!project?.is_demo;
  // No project resolved yet, or an API old enough not to send a role: allow.
  // The alternative is every control on every page flickering disabled on each
  // navigation, and the API is still the one that actually decides.
  const canWrite = !project || !role || WRITE_ROLES.has(role);
  const reason = canWrite
    ? ''
    : isDemo
      ? 'This is the shared demo — someone else’s site. Switch to your own project to change anything.'
      : 'You’re a viewer in this workspace. An owner can give you access.';
  return { isDemo, role, canWrite, reason };
}

// ---- the guided tour -------------------------------------------------------
//
// What a new account actually has on its first session:
//
//   * a read-only 'viewer' membership in ONE shared demo workspace, holding a
//     real project fed by a real website that somebody else runs;
//   * its own workspace, holding exactly one project, with nothing in it.
//
// The tour walks that: look around a working product, ask it something, then go
// and make the empty one yours. It is six steps when the instance has a demo
// and three when it does not (a self-hosted `docker compose up` has none), and
// the three-step version has to read as a whole tour rather than as a six-step
// one with holes in it.
//
// EVERY tick is derived from state the server can be asked for again — does
// their own workspace hold a project, does that project have events, is
// anything armed on a schedule. Nothing here reads a local flag: a checklist
// stored in this browser lies the moment the person opens the product on their
// laptop, and "you already did this" is the single worst thing an onboarding
// surface can be wrong about.
//
// The three exploring steps are the exception, and they are marked as one. We
// cannot see which dashboards somebody read, and the demo's agent runs are
// project-scoped — they are every visitor's runs, not this visitor's — so there
// is no honest per-person answer to "did you ask it something". Those steps
// carry `observable: false`, are rendered without a checkbox, and are left out
// of the progress count entirely. A tour that claims to know what it cannot see
// is worse than one that admits the gap.

export type TourStepId = 'demo' | 'explore' | 'ask' | 'project' | 'connect' | 'schedule';

export const EXPLORE_STEPS: readonly TourStepId[] = ['demo', 'explore', 'ask'];
export const CONNECT_STEPS: readonly TourStepId[] = ['project', 'connect', 'schedule'];

// The seeded question for step 3. Phrased about *this* product, not "my" —
// the funnel on screen belongs to the site the demo runs, and calling it the
// visitor's is the exact lie the old synthetic Demo project told.
export const DEMO_ASK_PROMPT = 'What is the weakest step in this product’s activation funnel?';
// The same question once the data is theirs.
export const OWN_ASK_PROMPT = 'What is the weakest step in my activation funnel?';

// The starter chips while a visitor is reading the demo. The product's usual
// starters are written in the first person ("my traffic", "my agents"), which
// is wrong here: every answer would be about someone else's site. These say
// whose product is being asked about.
export const DEMO_STARTERS: readonly string[] = [
  'Where is this site’s traffic coming from?',
  'Which events stopped firing this week?',
  'Which feature keeps people coming back?',
  'Is anything broken right now?',
];

// The standing work step 6 arms: one run a week, Monday morning, before the
// owner opens the tab. Weekly rather than daily because it spends their model
// key every time it fires and a week is the cadence the answer is useful at.
export const TOUR_SCHEDULE_CRON = '0 9 * * 1';
export const TOUR_SCHEDULE_NAME = 'Monday morning check';
export const TOUR_SCHEDULE_PROMPT = 'Read the last 7 days. Say what moved, what the weakest step is now, and the one thing to do about it this week.';

export type TourInput = {
  // false while the reads behind the ticks are still in flight. Nothing renders
  // until this is true, so no step ever flashes done and then undone.
  ready: boolean;
  // Does this instance have a shared demo at all?
  hasDemo: boolean;
  // Is the project the app is pointed at right now the demo?
  inDemo: boolean;
  demoName: string;
  ownProjectName: string;
  // Projects in the workspaces the user actually owns.
  ownProjectCount: number;
  // Distinct event names in their own project. 0 = nothing has ever arrived.
  ownEventNameCount: number;
  // Anything armed on a schedule in their own project.
  ownScheduled: boolean;
  // The teammate that would run on that schedule, for the copy.
  agentName?: string;
};

export type TourAction = {
  label: string;
  href?: string;
  // Handled by the panel rather than by navigation: switching the active
  // project, seeding the question, arming the schedule.
  act?: 'open-demo' | 'open-own' | 'ask' | 'arm';
};

export type TourStep = {
  id: TourStepId;
  // 1-based position AS SHOWN, so the no-demo tour numbers 1-2-3.
  n: number;
  label: string;
  detail: string;
  done: boolean;
  // false = no query can ever answer this. Rendered without a checkbox and left
  // out of the progress count.
  observable: boolean;
  action: TourAction;
  // Extra doors for a step that is about looking around rather than doing one
  // thing.
  links?: ReadonlyArray<{ label: string; href: string }>;
};

export function tourSteps(input: TourInput): TourStep[] {
  const demoName = input.demoName || 'the demo';
  const ownName = input.ownProjectName || 'your project';
  const agent = input.agentName || 'your teammate';
  const events = input.ownEventNameCount;
  const steps: Array<Omit<TourStep, 'n'>> = [];

  if (input.hasDemo) {
    steps.push({
      id: 'demo',
      label: 'Look around a working product',
      detail: input.inDemo
        ? `You’re in ${demoName}. It is a real site someone else runs, wired to AgentRay, and you joined it as a viewer — you can read all of it and change none of it.`
        : `${demoName} is a real site someone else runs, wired to AgentRay. Open it as a viewer and see the product with data already in it.`,
      done: input.inDemo,
      observable: false,
      action: input.inDemo ? { label: 'You’re here' } : { label: `Open ${demoName}`, act: 'open-demo' },
    });
    steps.push({
      id: 'explore',
      label: 'See what it already knows',
      detail: 'The dashboards are built and the teammates are hired. Open them — this is what the product looks like once your own events are arriving.',
      done: input.inDemo,
      observable: false,
      action: { label: 'Open the dashboards', href: '/dashboard' },
      links: [
        { label: 'Dashboards', href: '/dashboard' },
        { label: 'Agents', href: '/agents' },
      ],
    });
    steps.push({
      id: 'ask',
      label: 'Ask it a question',
      detail: 'Ask about the funnel and watch it read the events and answer. Questions here are capped per day — the answer is billed to whoever runs this instance, not to you.',
      done: false,
      observable: false,
      action: { label: 'Ask about the weakest step', act: 'ask' },
    });
  }

  steps.push({
    id: 'project',
    label: input.hasDemo ? 'Move to your own project' : 'Your project',
    detail: input.ownProjectCount > 0
      ? `${ownName} is yours. Everything from here happens there.`
      : 'You have no project of your own yet. Make one and the rest of this list runs against it.',
    done: input.ownProjectCount > 0,
    observable: true,
    // Already standing in it: say so rather than offering a switch that does
    // nothing. Same rule as step 1 inside the demo.
    action: input.ownProjectCount === 0
      ? { label: 'Create a project', href: settingsPath('projects') }
      : input.inDemo || !input.hasDemo
        ? { label: `Open ${ownName}`, act: 'open-own' }
        : { label: 'You’re here' },
  });

  steps.push({
    id: 'connect',
    label: 'Send your first event',
    detail: events > 0
      ? `${events} event ${events === 1 ? 'name' : 'names'} arriving in ${ownName}.`
      : 'Copy the snippet into your site. Nothing to install, no build step. This ticks when the first event lands.',
    done: events > 0,
    observable: true,
    action: { label: 'Get my API key', href: settingsPath('keys') },
  });

  steps.push({
    id: 'schedule',
    label: 'Let it work without you',
    detail: input.ownScheduled
      ? 'Something is armed. It runs whether or not you open this tab.'
      : `Right now ${agent} only works when you message it. A weekly schedule means the answer is waiting on Monday. It spends your model key every time it fires, so it stays off until you turn it on.`,
    done: input.ownScheduled,
    observable: true,
    action: { label: 'Put it on a Monday schedule', act: 'arm' },
  });

  return steps.map((step, i) => ({ ...step, n: i + 1 }));
}

// nextTourStep is where the pointer sits. While the user is inside the demo it
// stays in the exploring half: sending somebody who is reading someone else's
// site off to "connect your product" is how a tour loses people two steps in.
// Once they are looking at their own project the exploring half is behind them
// for good — pointing back at the demo would be a tour that never ends.
export function nextTourStep(input: TourInput, steps: TourStep[] = tourSteps(input)): TourStep | null {
  const half = input.hasDemo && input.inDemo ? EXPLORE_STEPS : CONNECT_STEPS;
  return steps.find((step) => half.includes(step.id) && !step.done)
    ?? steps.find((step) => CONNECT_STEPS.includes(step.id) && !step.done)
    ?? null;
}

// tourProgress counts only what a query can answer. The exploring steps are
// excluded, so the strip can actually reach its total.
export function tourProgress(steps: TourStep[]): { done: number; total: number } {
  const observable = steps.filter((step) => step.observable);
  return { done: observable.filter((step) => step.done).length, total: observable.length };
}

// The tour is over when the three observable steps are done: their own project
// exists, it has events, and something runs without them.
export function tourComplete(input: TourInput): boolean {
  return input.ownProjectCount > 0 && input.ownEventNameCount > 0 && input.ownScheduled;
}

export function showTour(input: TourInput): boolean {
  return input.ready && !tourComplete(input);
}

// tourHandoff is what goes under the answer once the demo has finished
// answering: the honest next move, which is that this was someone else's data.
// Null until the turn has actually settled, and null when it failed — a failed
// run gets the error surface, not a handover.
export function tourHandoff(
  input: TourInput,
  turn: { settled: boolean; failed: boolean },
): TourStep | null {
  if (!input.ready || !input.hasDemo || !input.inDemo) return null;
  if (!turn.settled || turn.failed) return null;
  const steps = tourSteps(input);
  return steps.find((step) => CONNECT_STEPS.includes(step.id) && !step.done) ?? null;
}

// count is event volume, users is distinct people. Keeping both named apart is
// the whole point: the written opinion speaks in people, and for months it was
// printing volume under a "people" noun because the catalog only carried one
// number. user.pageview on a real project is 5,128 events and 835 people.
type CatalogEvent = { name: string; count: number; users: number };

function catalogEvents(names: FirstRunInput['eventNames']): CatalogEvent[] {
  if (!names) return [];
  const out: CatalogEvent[] = [];
  for (let i = 0; i < names.length; i += 1) {
    const row = (names as readonly unknown[])[i];
    if (typeof row === 'string' && row) out.push({ name: row, count: 0, users: 0 });
    else if (row && typeof row === 'object') {
      const rec = row as { name?: unknown; event_name?: unknown; count?: unknown; users?: unknown };
      const name = rec.event_name ?? rec.name;
      const count = typeof rec.count === 'number' && rec.count > 0 ? rec.count : 0;
      const users = typeof rec.users === 'number' && rec.users > 0 ? rec.users : 0;
      if (typeof name === 'string' && name) out.push({ name, count, users });
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

type FunnelMatch = { id: string; label: string; event: string; count: number; users: number; order: number };

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
    // The catalog arrives volume-descending, so matched[0] is the busiest event
    // for this stage — and it is the one `funnelStepNames` actually queries. The
    // counts must therefore describe *that* event, not a sum across every event
    // that matched the stage: summing described a step the funnel never ran, and
    // summing `users` across events would double-count anyone in two of them.
    steps.push({
      id: stage.id,
      label: stage.label,
      event: matched[0].name,
      count: matched[0].count,
      users: matched[0].users,
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

// retentionAnchorEvent is the event a retention cohort is defined by: "people who
// first did THIS". The first matched funnel stage is the right anchor — it is the
// entry step, so the cohort is people who arrived, not people who already
// converted. Falls back to the busiest event in the catalog, then to the default
// contract, so this never resolves to a name nothing emits.
export function retentionAnchorEvent(names: FirstRunInput['eventNames']): string {
  const steps = matchedFunnelSteps(names);
  if (steps.length > 0) return steps[0].event;
  const events = catalogEvents(names);
  if (events.length > 0) return events[0].name;
  return DEFAULT_FUNNEL_STEPS[0];
}

export type WeakestLink = {
  from: string;
  to: string;
  // Plain-language stage names ("visit", "purchase"). `from`/`to` carry the raw
  // event name, which is the developer's word for the step, not the owner's —
  // the headline reads with these and keeps the event name as the evidence.
  fromLabel: string;
  toLabel: string;
  // People, not events — the number of distinct stitched, human identities that
  // fired each stage's event.
  fromCount: number;
  toCount: number;
  // toCount / fromCount. This is a ratio of two independently-measured people
  // counts, NOT a measured passage rate: the catalog cannot tell us whether the
  // `to` people are the same people as the `from` people, or whether they did
  // the steps in that order. Only the funnel query (windowFunnel, server-side)
  // establishes passage. Word it as a gap, never as "conversion".
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
      const rate = from.users > 0 ? Math.min(to.users / from.users, 1) : 0;
      const cand: WeakestLink = {
        from: from.event,
        to: to.event,
        fromLabel: from.label,
        toLabel: to.label,
        fromCount: from.users,
        toCount: to.users,
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
      fromCount: steps[0].users,
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
        ? `${fromN} hit ${link.fromLabel} and ${toN} hit ${link.toLabel} (${pct}), but the steps in between aren’t tracked — so I can’t tell you where the rest go.`
        : `${link.fromLabel} and ${link.toLabel} are tracked; the steps in between aren’t, so the drop-off is invisible.`,
      ask: `What should we track between ${link.fromLabel} and ${link.toLabel} to see where people drop off?`,
    };
  }
  return {
    kind: 'noticed',
    title: `${link.fromLabel} → ${link.toLabel} is the weakest step`,
    detail: fromN
      ? `${fromN} hit ${link.fromLabel}, ${toN} hit ${link.toLabel} — a ${pct} gap, the widest I can see. Run the funnel to confirm they’re the same people, in that order.`
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

// isRunError says whether an assistant turn came back as a failure rather than
// as an answer. The transcript keeps the text either way — that IS what
// happened — but nothing that celebrates a result may treat it as one, the
// tour's handoff above all: "that answer was about someone else's product" is
// absurd printed under a provider timeout. Matched on the shapes a failed run
// actually produces: the runtime's own `error:` prefix, the key/paused cases,
// and the provider transport lines the runtime passes through verbatim.
export function isRunError(text: string): boolean {
  const raw = (text ?? '').trim();
  if (!raw) return true;
  if (needsKeyRecovery(raw)) return true;
  return /^error:/i.test(raw)
    || /^provider chat \(turn \d+\)/i.test(raw)
    || /unexpected response \(status \d{3}\)/i.test(raw);
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
      `**${link.from} → ${link.to} is the widest gap (${pct}%).**`,
      '',
      `${who} fired \`${link.from}\`; ${countLabel(link.toCount)} fired \`${link.to}\`. That’s the widest gap in this catalog.`,
      '',
      `I’m comparing two people counts, not tracing one cohort — open the funnel to confirm the same people did both steps in that order.`,
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
