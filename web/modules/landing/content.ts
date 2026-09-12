import { CLAIMS } from '@/lib/brand';

// Every string a stranger reads on the door, in one file, for the same reason
// lib/brand.ts exists: the page and the metadata stopped agreeing with the
// product once before. The promise position (headline, subhead, claim titles)
// stays in lib/brand.ts, where brand.test.ts guards it against
// PROMISE_BANNED_VERBS — this file must never restate one of those strings in
// its own words. Where a section says something a claim already says, it
// *imports the claim* rather than paraphrasing it, so a future edit to
// brand.ts moves the page too and the test still covers the wording.
//
// Section copy that is not in brand.ts is lifted from shipped sources, never
// written for the page:
//   contrast lines      → README.md (the product's own opening argument)
//   the answer mock     → `writtenOpinion` in web/lib/ia.ts
//   channels            → the /operations screen canon in agentray/DESIGN.md
//   self-host facts     → README.md
//
// The standing bans hold here exactly as they do on brand.ts: no price, no
// trial, no customer logo, no testimonial, no demo promise (this instance may
// be self-hosted, where all five would be a lie), and no surface explains the
// name.

// ---------------------------------------------------------------- section 2

// Verbatim the argument README.md opens with. Line one is the commodity every
// competitor also ships; line two is the product. The banned verbs are legal
// here because "what happened" is being named as the *foil* — the same
// exemption brand.test.ts already grants claim details.
export const CONTRAST = {
  commodity: 'Every analytics tool can tell you what happened.',
  product: 'AgentRay ships the rest of the loop as the product.',
} as const;

// ---------------------------------------------------------------- section 3

export type LoopStepId = 'measure' | 'diagnose' | 'design' | 'learn';

export type LoopStep = {
  id: LoopStepId;
  index: string;
  phase: string;
  title: string;
  detail: string;
};

// measure → diagnose → test → learn: the four beats SUBHEAD lists, and the
// four the README names as the loop. Three of the four titles ARE the shipped
// claims — imported, not re-typed, so brand.test.ts keeps guarding them.
export const LOOP_STEPS: readonly LoopStep[] = [
  {
    id: 'measure',
    index: '01',
    phase: 'Measure',
    title: 'It starts from the events you already send.',
    detail:
      'The same store that powers your charts is the one the agent reads. Nothing new to instrument first.',
  },
  { id: 'diagnose', index: '02', phase: 'Diagnose', title: CLAIMS[0].title, detail: CLAIMS[0].detail },
  { id: 'design', index: '03', phase: 'Test', title: CLAIMS[1].title, detail: CLAIMS[1].detail },
  { id: 'learn', index: '04', phase: 'Learn', title: CLAIMS[2].title, detail: CLAIMS[2].detail },
] as const;

// ---------------------------------------------------------------- section 4

export const CHANNELS_SECTION = {
  label: 'Without you',
  title: 'Nobody has to open the chat.',
  detail:
    'A schedule or a webhook starts the same run a conversation would. The loop keeps its cycle whether or not anyone asks.',
} as const;

export type ChannelRow = {
  id: string;
  icon: 'schedule' | 'webhook' | 'chat';
  title: string;
  detail: string;
  status: string;
  tone: 'success' | 'neutral';
};

// Shape follows the /operations canon in DESIGN.md: schedules and webhooks that
// start a run without a conversation, with Chat named as the other channel.
export const CHANNELS: readonly ChannelRow[] = [
  {
    id: 'weekly',
    icon: 'schedule',
    title: 'Every Monday, 09:00',
    detail: 'Run the growth loop and post the result.',
    status: 'Healthy',
    tone: 'success',
  },
  {
    id: 'monthly',
    icon: 'schedule',
    title: 'First of the month',
    detail: 'Re-check the funnel after last month’s test.',
    status: 'Healthy',
    tone: 'success',
  },
  {
    id: 'deploy',
    icon: 'webhook',
    title: 'POST /hooks/deploy',
    detail: 'Starts a run when a release ships.',
    status: 'Healthy',
    tone: 'success',
  },
  {
    id: 'chat',
    icon: 'chat',
    title: 'Chat',
    detail: 'The other channel — for when you do want to ask.',
    status: 'Open',
    tone: 'neutral',
  },
] as const;

// ---------------------------------------------------------------- section 5

export const OSS_SECTION = {
  label: 'Open source',
  title: 'Run the whole thing on your own box.',
  command: 'docker compose up',
  repo: 'https://github.com/lohi-ai/agentray',
} as const;

// Straight from README.md's "Underneath is a complete product-analytics base"
// paragraph. No claim here is one the shipped product does not make already.
export const OSS_FACTS = [
  {
    id: 'base',
    title: 'A complete analytics base',
    detail: 'Go ingestion, event storage in embedded DuckDB, PostgreSQL metadata. The charts are not a separate product.',
  },
  {
    id: 'posthog',
    title: 'PostHog-compatible events',
    detail: 'Instrumentation you already ship migrates by changing only the host.',
  },
  {
    id: 'mcp',
    title: 'MCP server and agent skills',
    detail: 'Claude Code or Codex works your real event data — one-off questions, or a scheduled loop.',
  },
] as const;

// ---------------------------------------------------------------- section 6

export const DOOR_SECTION = {
  title: 'Point it at your events.',
  titleSecondLine: 'Read what it says on Monday.',
  detail:
    'The event model is PostHog-compatible, so instrumentation you already ship migrates by changing only the host.',
} as const;

// ------------------------------------------------------- the product stages
//
// Numbers below are ONE example carried consistently through every surface on
// the page: 28 people fired `activation`, 7 fired `user.conversion`, a 25% gap.
// They are the figures `writtenOpinion` (web/lib/ia.ts) formats, so the mock
// funnel, the mock answer and the mock memory all agree with each other and
// with the shipped code that would produce them. Every stage renders labelled
// "Example" and aria-hidden — it is what an answer looks like, not a number
// from this instance.

export const EXAMPLE_EVENTS = [
  { name: 'session_start', count: '1,204' },
  { name: 'pageview', count: '3,918' },
  { name: 'signup', count: '41' },
  { name: 'activation', count: '28' },
  { name: 'prototype.waitlist', count: '19' },
  { name: 'user.conversion', count: '7' },
] as const;

export const EXAMPLE_FUNNEL = [
  { name: 'session_start', people: 1204, width: 100, weakest: false },
  { name: 'signup', people: 41, width: 34, weakest: false },
  { name: 'activation', people: 28, width: 23, weakest: false },
  { name: 'user.conversion', people: 7, width: 6, weakest: true },
] as const;

export const EXAMPLE_GAP = {
  title: 'activation → user.conversion — the widest gap, 25%.',
  detail: '28 people in, 7 out. Every other step keeps more than it loses.',
} as const;

// Shape and wording follow `writtenOpinion` (web/lib/ia.ts): the widest gap as
// a percentage, the two people counts it compared, the caveat naming what the
// comparison does NOT prove, and the closing instruction it actually emits —
// the single most differentiated sentence the product ships, which is why it
// is never truncated away.
export const EXAMPLE_ANSWER = [
  '**activation → user.conversion is the widest gap (25%).**',
  '',
  '28 people fired `activation`; 7 people fired `user.conversion`. That’s the widest gap in your catalog.',
  '',
  'I’m comparing two people counts, not tracing one cohort — open the funnel to confirm the same people did both steps in that order.',
  '',
  'This week: one test on that step. Don’t add a new dashboard — change the product so more people who hit `activation` also hit `user.conversion`.',
].join('\n');

export const EXAMPLE_BRIEF = [
  { id: 'change', label: 'Change', detail: 'Move the value moment ahead of the paywall for new signups.' },
  { id: 'step', label: 'Step it should move', detail: 'activation → user.conversion' },
  { id: 'metric', label: 'How we’ll know', detail: 'user.conversion per person who fired activation' },
  { id: 'window', label: 'Run for', detail: 'One cycle — 7 days, or 200 people through the step.' },
] as const;

export const EXAMPLE_BRIEF_NOTE =
  'One test, not a backlog. Anything larger is a plan, not an experiment.';

export const EXAMPLE_MEMORY = [
  {
    id: 'c1',
    when: 'Cycle 1',
    detail: 'Named activation → user.conversion as the widest gap (25%).',
    state: 'Learned',
    current: false,
  },
  {
    id: 'c2',
    when: 'Cycle 2',
    detail: 'Test shipped: value moment moved ahead of the paywall. Conversion 6% → 11%.',
    state: 'Learned',
    current: false,
  },
  {
    id: 'c3',
    when: 'Cycle 3',
    detail: 'Carrying that forward: the paywall is no longer the weakest link. Next widest gap is signup → activation.',
    state: 'Reasoning now',
    current: true,
  },
] as const;

// The one label every mock surface carries. A stranger must never be able to
// mistake an illustration for this workspace's data.
export const EXAMPLE_BADGE = 'Example';
