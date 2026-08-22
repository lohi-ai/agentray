// Brand strings, in one place, because the whole point of ticket bs-pumdwuri is
// that the front door and the metadata stopped agreeing with the product.
//
// The positioning, in one line: **seeing is the commodity, direction is the
// product.** Every analytics tool can tell you what happened (README:5), so any
// string that promises *seeing* sells the half a competitor also sells. The
// promise position — headline, subhead, claim titles, <title>, meta
// description, OG — belongs to what happens next and without you.
//
// Two rules that are easy to break and expensive to notice:
//
//  1. NO SURFACE EXPLAINS THE NAME. No "AgentRay = Agent + Ray", no geometry,
//     no tagline defining the suffix. Explaining your own name is a confession;
//     the page earns the meaning by what it claims. The mark already carries it
//     — the logo is lucide `Waypoints`, a routed path with a direction, never a
//     lens or an eye.
//  2. Every claim is something the shipped product does. No price, no trial, no
//     customer logo, no testimonial, no demo promise: this instance may be
//     self-hosted, where all five would be a lie.
//
// `PROMISE_BANNED_VERBS` is enforced by lib/brand.test.ts over the strings in
// this file, so a future edit that drifts back to "see what moved" fails a test
// instead of shipping.

// Verbs that sell the commodity half. Legal in the *proof* position (the answer
// mock, the tool-call rows) where the product shows its work — that demotion
// from promise to evidence is the entire brand fix — and banned everywhere a
// stranger reads a promise.
export const PROMISE_BANNED_VERBS = [
  'see',
  'watch',
  'monitor',
  'observe',
  'track',
  'surface',
  'reveal',
  'uncover',
  'spot',
  'discover',
  'illuminate',
  'x-ray',
  'visibility',
  'insight',
  'dashboards?',
] as const;

export const BRAND_NAME = 'AgentRay';

// ≤ 6 words, never a question, never an "Ask…". What happens without you.
export const HEADLINE = 'Your growth loop, running without you.';

// The four shipped beats in order: read → name → design → return.
export const SUBHEAD =
  'AgentRay reads your product data, names the step costing you the most, designs the smallest test that would move it, and brings the result back next cycle.';

// act · act · remember. The third is the sharpest claim available and is the
// one a dashboard structurally cannot make.
export const CLAIMS = [
  {
    id: 'names',
    title: 'It names one step, not ten charts.',
    detail: 'You get “activation → conversion is your widest gap”, not a board to interpret.',
  },
  {
    id: 'designs',
    title: 'It designs the test.',
    detail: 'The smallest experiment that would move that step, and what to measure.',
  },
  {
    id: 'remembers',
    title: 'It remembers what you learned.',
    detail: 'Last cycle’s result is in next cycle’s reasoning. A dashboard starts over every time.',
  },
] as const;

// SEO strings. `TITLE_TEMPLATE` keeps the brand as the suffix so a deep page
// reads "Chat · AgentRay" rather than burying its own subject.
export const TITLE_DEFAULT = 'AgentRay — analytics that runs the loop';
export const TITLE_TEMPLATE = '%s · AgentRay';
export const META_DESCRIPTION =
  'AgentRay names the step costing you the most, designs the test that would move it, and brings the result back next cycle. Open source, self-hostable.';

// siteOrigin is the absolute origin this instance is served from — needed by
// metadataBase, canonical, robots.txt and sitemap.xml, none of which can be
// relative.
//
// There is no dedicated origin variable on most instances, and there does not
// need to be: web and the API share one hostname wherever this is deployed
// (`_WEB_API_URL` is the deploy host, infra/gce/deploy.sh), so the API URL *is*
// the site origin in dev and prod alike. `NEXT_PUBLIC_AGENTRAY_SITE_URL` exists
// for the split-origin case — including local dev, where web is :3200 and the
// API is :8088 and the fallback would otherwise name the wrong port.
export function siteOrigin(): string {
  const explicit = process.env.NEXT_PUBLIC_AGENTRAY_SITE_URL;
  if (explicit) return stripTrailingSlash(explicit);
  const api = process.env.NEXT_PUBLIC_AGENTRAY_API_URL;
  if (api && !isLocalhost(api)) return stripTrailingSlash(api);
  return 'http://localhost:3200';
}

function stripTrailingSlash(value: string): string {
  return value.endsWith('/') ? value.slice(0, -1) : value;
}

function isLocalhost(value: string): boolean {
  return /^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(:|\/|$)/i.test(value);
}

// Every route other than `/` is behind AuthGate and renders a session check to
// anyone without a cookie, so listing them would hand a crawler a page of
// spinners under a dozen URLs. `/` is the only indexable surface, and after the
// crawlability fix it is the only one with content on it.
export const INDEXABLE_PATHS = ['/'] as const;

// Paths a crawler must not follow. `/pricing` is hostedOnly (lib/ia.ts) and
// does not exist on a self-hosted instance at all.
export const CRAWLER_DISALLOW = [
  '/agent',
  '/agents',
  '/alerts',
  '/chat',
  '/cohorts',
  '/dashboard',
  '/dashboards',
  '/events',
  '/marketplace',
  '/monitor',
  '/operations',
  '/persons',
  '/pricing',
  '/product',
  '/prototypes',
  '/replay',
  '/settings',
  '/sql',
  '/start',
  '/teams',
  '/templates',
  '/traffic',
  '/web-analytics',
] as const;

// JSON-LD. `SoftwareApplication` is the honest type — this is an app you run,
// not an article. No `offers` block: pricing is hosted-only and a self-hosted
// instance advertising a price would be a lie (same standing ban as the door).
export function softwareApplicationJsonLd(origin: string): Record<string, unknown> {
  return {
    '@context': 'https://schema.org',
    '@type': 'SoftwareApplication',
    name: BRAND_NAME,
    url: origin,
    applicationCategory: 'BusinessApplication',
    operatingSystem: 'Web',
    description: META_DESCRIPTION,
    isAccessibleForFree: true,
    softwareHelp: 'https://github.com/lohi-ai/agentray',
  };
}
