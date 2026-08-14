// The plan ladder and the meter math behind it. Pure functions only, so the
// pricing page, the settings meter and the upgrade moment all read the same
// numbers and the unit tests can call exactly what the UI calls.
//
// Nothing here enforces anything: the backend has no plan gate (see
// internal/dataplane/store/workspace_plan.go). A workspace over its ceiling keeps
// ingesting — the meter informs, it never blocks. Copy in this file must not
// promise otherwise.

export type PlanID = 'free' | 'solo' | 'team';

export type Plan = {
  id: PlanID;
  name: string;
  // USD is the billing currency. VND is display-only for the VI locale, priced
  // *under* spot so it reads as a native VN price point rather than a
  // conversion — which also means a rate move can never become a pricing bug.
  usdPerMonth: number;
  vndPerMonth: number;
  tagline: string;
  // The metered axis. Events are cheap for us and legible to the user; agent
  // runs are BYO-AI-key, so the user already pays their own token cost and runs
  // stay unlimited on every tier.
  eventsPerMonth: number;
  projects: number | 'unlimited';
  agents: number | 'unlimited';
  historyDays: number;
  features: string[];
  support: string;
};

// 1,000,000 free events is a competitive floor, not a preference: PostHog and
// Mixpanel both train this market on 1M free, so a smaller free tier reads as
// unserious against the products AgentRay is compared to.
export const FREE_EVENT_CEILING = 1_000_000;

export const PLANS: readonly Plan[] = [
  {
    id: 'free',
    name: 'Free',
    usdPerMonth: 0,
    vndPerMonth: 0,
    tagline: 'Enough to find your weakest step and fix it.',
    eventsPerMonth: FREE_EVENT_CEILING,
    projects: 1,
    agents: 1,
    historyDays: 30,
    features: ['Unlimited asks (bring your own AI key)', '1 project', '1 agent', '30 days of history'],
    support: 'Community',
  },
  {
    id: 'solo',
    name: 'Solo',
    usdPerMonth: 19,
    vndPerMonth: 499_000,
    tagline: 'For the founder who checks the numbers every morning.',
    eventsPerMonth: 3_000_000,
    projects: 3,
    agents: 'unlimited',
    historyDays: 365,
    features: ['Everything in Free', '3 projects', 'Unlimited agents', 'Scheduled runs + daily readout', '12 months of history'],
    support: 'Email',
  },
  {
    id: 'team',
    name: 'Team',
    usdPerMonth: 79,
    vndPerMonth: 1_990_000,
    tagline: 'When more than one person is asking.',
    eventsPerMonth: 15_000_000,
    projects: 'unlimited',
    agents: 'unlimited',
    historyDays: 1095,
    features: ['Everything in Solo', 'Unlimited projects', 'Teams, roles, audit log', '3 years of history'],
    support: 'Priority',
  },
];

export function planByID(id: string | undefined | null): Plan {
  return PLANS.find((plan) => plan.id === id) ?? PLANS[0];
}

// The next rung up, or null at the top. Drives "which plan does Upgrade mean".
export function nextPlan(id: string | undefined | null): Plan | null {
  const index = PLANS.findIndex((plan) => plan.id === planByID(id).id);
  return index >= 0 && index < PLANS.length - 1 ? PLANS[index + 1] : null;
}

// The single nag in the product. It fires once, at 80% of the month's ceiling —
// with a 1M free tier that is genuinely rare, which is the point: it only
// reaches someone whose product is actually working, and it arrives as
// information rather than a wall. There is no persistent upgrade bar anywhere.
export const UPGRADE_THRESHOLD = 0.8;

export type UsageMeter = {
  used: number;
  ceiling: number;
  // 0..1, clamped — a workspace over its ceiling shows a full bar, never a bar
  // that overflows its track.
  ratio: number;
  overCeiling: boolean;
  // true past UPGRADE_THRESHOLD. The card turns --warning and states the
  // projected date; ingestion is explicitly promised to continue.
  nearCeiling: boolean;
  // Day of the month the ceiling is projected to be hit at the current rate,
  // or null when the run rate does not reach it this month.
  projectedFullOn: Date | null;
};

// usageMeter turns a raw event count into everything the three meter surfaces
// need. `now` and `used` are passed in (never read from a clock inside) so the
// projection is testable and the same numbers render on every surface.
export function usageMeter(used: number, plan: Plan, now = new Date()): UsageMeter {
  const ceiling = plan.eventsPerMonth;
  const safeUsed = Number.isFinite(used) && used > 0 ? used : 0;
  const ratio = ceiling > 0 ? Math.min(1, safeUsed / ceiling) : 0;
  const overCeiling = ceiling > 0 && safeUsed >= ceiling;
  return {
    used: safeUsed,
    ceiling,
    ratio,
    overCeiling,
    nearCeiling: ratio >= UPGRADE_THRESHOLD,
    projectedFullOn: projectFull(safeUsed, ceiling, now),
  };
}

// projectFull extrapolates the month-to-date rate to the day the ceiling lands
// on. Returns null when the rate does not reach the ceiling before the month
// ends — the upgrade card then has no date to promise and says so.
function projectFull(used: number, ceiling: number, now: Date): Date | null {
  if (ceiling <= 0 || used <= 0) return null;
  const dayOfMonth = now.getUTCDate();
  const daysInMonth = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 0)).getUTCDate();
  const perDay = used / dayOfMonth;
  if (perDay <= 0) return null;
  const daysToFull = Math.ceil(ceiling / perDay);
  if (daysToFull > daysInMonth) return null;
  return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), Math.max(daysToFull, dayOfMonth)));
}

// Seven-figure counts are the common case on a 1M ceiling, so the meter label
// is compact ("1.2M") with the exact number kept for the title attribute.
export function formatEvents(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '0';
  if (n >= 1_000_000) return `${trim(n / 1_000_000)}M`;
  if (n >= 1_000) return `${trim(n / 1_000)}k`;
  return String(Math.round(n));
}

function trim(n: number): string {
  return n >= 10 ? String(Math.round(n)) : String(Math.round(n * 10) / 10);
}

export function formatPrice(plan: Plan, locale: 'en' | 'vi'): string {
  if (plan.usdPerMonth === 0) return locale === 'vi' ? 'Miễn phí' : 'Free';
  if (locale === 'vi') return `₫${plan.vndPerMonth.toLocaleString('vi-VN')}`;
  return `$${plan.usdPerMonth}`;
}
