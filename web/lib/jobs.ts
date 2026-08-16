// Jobs are the product's front door, expressed in the shape of the backend:
//
//   channel  →  workload  →  runtime  →  dataplane
//
// The nav is organised by *artifact* (Chat, Dashboards, Events, SQL…), which
// only helps someone who already knows which artifact serves their job. A job
// is the other index: an owner says where their product is, and this module
// resolves that into the pack to hire, the channel it runs in, and the evidence
// surfaces that hold the answer.
//
// The three jobs are the three phases of a product's life, and the line between
// them is the dataplane: `validate` runs with an empty event store by design,
// the other two are defined by reading a full one. Everything else in AgentRay
// assumed the store was full, which is why an owner with only an idea had
// nowhere to start.
//
// Pure module — no React, no fetch — so the same helpers back the page, the
// chat front door, and the unit tests.

import { CHANNEL_CATALOG, type ChannelInfo, type WorkloadCategory } from './ia';

export type JobId = 'validate' | 'grow' | 'operate';

export type LayerId = 'channel' | 'workload' | 'runtime' | 'data';

export type JobDef = {
  id: JobId;
  // The owner's own words for where they are. This is what they pick from —
  // never the layer names, which are ours.
  situation: string;
  label: string;
  // What they walk away holding. One sentence, concrete, no capability talk.
  outcome: string;
  // ---- the four layers, bound for this job ----
  channels: ReadonlyArray<ChannelInfo['kind']>;
  category: WorkloadCategory['id'];
  // Pack slugs, primary hire first. These are workloads.Pack slugs — the same
  // strings /api/marketplace/agents returns. Any one of them satisfies "hire the
  // teammate" — they are alternatives, not a checklist.
  packs: readonly string[];
  // campaignPack is a SECOND teammate the job needs as well as one of `packs`,
  // not instead of one. It exists for marketing-first: validating an idea means
  // putting the message in front of real people, and the teammate that does the
  // research is not the teammate that runs the campaign. Kept out of `packs`
  // deliberately — listing it there would let hiring the marketer alone mark
  // "hire the teammate" done, and would move the pack's marketplace label off
  // the job it primarily serves.
  campaignPack?: string;
  // What the runtime does for this job, in the owner's language.
  runtime: string;
  // needsEvents is the whole distinction between phase one and the rest. For
  // `validate` the event stream is the job's *output*, not its input, so an
  // empty store must never read as "not ready".
  needsEvents: boolean;
  surfaces: ReadonlyArray<{ href: string; label: string }>;
  prompts: readonly string[];
};

export const JOBS: readonly JobDef[] = [
  {
    id: 'validate',
    situation: 'I have an idea, not a product yet',
    label: 'Prove the idea',
    outcome: 'Evidence that someone wants this, the sentence that sells it, and a marketing test that decides whether to build.',
    channels: ['chat'],
    category: 'validate',
    packs: ['product-scout'],
    campaignPack: 'marketing-lead',
    runtime: 'Reads the open web for real demand, designs a test that can prove you wrong, then markets it before you build it.',
    needsEvents: false,
    surfaces: [
      { href: '/settings?tab=keys', label: 'Instrument the test' },
      { href: '/events', label: 'Watch the first events land' },
    ],
    prompts: [
      'Is anyone already paying to solve this problem?',
      'Write the one sentence that sells this to my customer.',
      'Design the cheapest test that could prove this idea wrong.',
      'Draft the Reddit and TikTok posts that put this in front of real people.',
      'How is my test doing against the number I committed to?',
    ],
  },
  {
    id: 'grow',
    situation: 'My product is live and I want it to grow',
    label: 'Grow what’s running',
    outcome: 'The weakest step in your funnel, who your customers are, and what to do about it this week.',
    channels: ['chat', 'schedule'],
    category: 'growth',
    packs: ['growth-lead', 'marketing-lead', 'data-analyst', 'insight-digest'],
    runtime: 'Reads your events on demand and on a schedule, then writes the next move.',
    needsEvents: true,
    surfaces: [
      { href: '/dashboard', label: 'Dashboards' },
      { href: '/persons', label: 'People' },
      { href: '/web-analytics', label: 'Traffic' },
      { href: '/product', label: 'Product' },
    ],
    prompts: [
      'What is the single weakest step in my activation funnel?',
      'Where is my best traffic coming from?',
      'Draft this week’s content plan from my product data.',
      'Which feature drives retention?',
    ],
  },
  {
    id: 'operate',
    situation: 'My product is live and I need it to stay up',
    label: 'Keep it healthy',
    outcome: 'What broke, for whom, since when — and the fix filed before you find out from a customer.',
    channels: ['chat', 'schedule', 'webhook'],
    category: 'operator',
    packs: ['ops-watch', 'tracking-steward'],
    runtime: 'Sweeps errors, latency and spend on a schedule, escalates once, files the fix.',
    needsEvents: true,
    surfaces: [
      { href: '/alerts', label: 'Alerts' },
      { href: '/monitor', label: 'Agent health' },
      { href: '/events', label: 'Events' },
      { href: '/sql', label: 'SQL' },
    ],
    prompts: [
      'Is anything broken right now?',
      'Where is my AI spend going this week?',
      'Which events stopped firing?',
      'What did my agents do today?',
    ],
  },
];

// startersByJob is the chat front door's chip wall. It used to be grouped by
// capability (Growth / Marketing / Data / Operations / Your team), which asked
// the reader to know which of our capabilities their problem belonged to.
// Grouping by job asks them only where their product is.
export function startersByJob(jobs: readonly JobDef[] = JOBS): Array<{ id: JobId; label: string; prompts: readonly string[] }> {
  return jobs.map((job) => ({ id: job.id, label: job.label, prompts: job.prompts }));
}

export function jobById(id: string): JobDef | null {
  return JOBS.find((job) => job.id === id) ?? null;
}

export function primaryPack(job: JobDef): string {
  return job.packs[0] ?? '';
}

// jobPacks is every pack a job's readiness depends on, including the campaign
// teammate. Callers resolving JobState must use this rather than `packs`, or the
// marketing step can never read as done.
export function jobPacks(job: JobDef): string[] {
  return job.campaignPack ? [...job.packs, job.campaignPack] : [...job.packs];
}

// jobLayers renders one job as the four architecture rows. It is the only place
// the layer names are shown to a user, and each row is stated as what the layer
// does for *this* job — a label like "Dataplane" on its own teaches nothing.
export type JobLayer = { id: LayerId; label: string; detail: string };

// jobChannels resolves a job's channel kinds against the shipped catalog, so a
// job can never advertise a channel the backend has not shipped (support and
// voice are reserved kinds — listed there so the UI says "not yet" instead of
// inventing a surface).
export function jobChannels(job: JobDef, catalog: readonly ChannelInfo[] = CHANNEL_CATALOG): ChannelInfo[] {
  return catalog.filter((channel) => channel.shipped && job.channels.includes(channel.kind));
}

export function jobLayers(job: JobDef): JobLayer[] {
  const channels = jobChannels(job);
  return [
    {
      id: 'channel',
      label: 'How you reach it',
      detail: job.channels.includes('schedule')
        ? `${channels.map((c) => c.label).join(' · ')} — ask it, or give it a schedule and it works while you sleep.`
        : `${channels.map((c) => c.label).join(' · ')} — ask it a question.`,
    },
    { id: 'workload', label: 'Who does the work', detail: job.runtime },
    {
      id: 'runtime',
      label: 'How it stays safe',
      detail: 'One runtime, one set of guardrails: it reads what you granted, and never states a number it did not check.',
    },
    {
      id: 'data',
      label: 'What it answers from',
      detail: job.needsEvents
        ? 'Your own event data — every claim opens to the rows behind it.'
        : 'Real pages it fetched and what you tell it. No product data yet, and it will say so rather than invent any.',
    },
  ];
}

// ---- readiness -------------------------------------------------------------

export type JobState = {
  // Pack slugs already installed in this project (from the agents roster).
  installedPacks: readonly string[];
  // Distinct event names seen. 0 = nothing has ever arrived.
  eventNameCount: number;
  // false = we know there is no workspace model key. undefined = still loading,
  // so a step must not flash as blocked.
  hasModelKey?: boolean;
  // true once the job's agent has a schedule trigger armed.
  scheduled?: boolean;
  // A validation test exists and the owner has COMMITTED to its number. Not
  // "a test was proposed" — an agent's proposal binds nobody, and the whole
  // value of the row is that the owner agreed to it before seeing the result.
  testCommitted?: boolean;
  // A test is on the board but still waiting for that agreement. Distinct from
  // no test at all, because the next action is completely different: one is
  // "design a test", the other is "agree to this number".
  testProposed?: boolean;
  // Addresses collected so far. Reported in the step's detail because a count
  // the owner can see is the difference between a checklist and a scoreboard.
  waitlistCount?: number;
};

export type JobStep = {
  id: 'key' | 'hire' | 'data' | 'standing' | 'threshold' | 'market';
  label: string;
  detail: string;
  done: boolean;
  // observable=false means no query can ever mark this done — it is a promise
  // the owner makes, not a switch we can read. Such a step still renders and
  // still carries its control, but it is left out of the progress count:
  // a checklist that can never reach its total reads as broken, and the owner
  // who *has* done it comes back to the same unmoved "3 of 4".
  observable: boolean;
  // The one control that advances this step.
  action: { label: string; href?: string; install?: string };
};

// jobSteps is the whole UX engine: the steps for a job, in dependency order,
// each either done or carrying exactly one control. The caller renders them and
// takes the first not-done one as the single next action.
//
// The two shapes differ because the phases genuinely do. For grow/operate an
// empty event store is a blocker — there is nothing to answer from. For validate
// the events are the *deliverable*, and the sequence is marketing-first: agree
// the number, instrument the page, then put the message in front of real people
// and let their response decide whether anything gets built.
export function jobSteps(job: JobDef, state: JobState): JobStep[] {
  const installed = new Set(state.installedPacks);
  const hired = job.packs.some((slug) => installed.has(slug));
  const hasData = state.eventNameCount > 0;
  const primary = primaryPack(job);

  const steps: JobStep[] = [
    {
      id: 'key',
      label: 'Give it a brain',
      detail: 'One AI key. You pay your model provider directly — we never mark it up.',
      // undefined means still loading; treat as done so the page does not flash
      // a blocker at a workspace that has a key.
      done: state.hasModelKey !== false,
      observable: true,
      action: { label: 'Add AI key', href: '/settings?tab=ai' },
    },
    {
      id: 'hire',
      label: 'Hire the teammate',
      detail: hired
        ? 'Hired and ready.'
        : `${jobPackName(primary)} does this job. One click, no configuration.`,
      done: hired,
      observable: true,
      action: { label: 'Hire', install: primary },
    },
  ];

  if (job.needsEvents) {
    steps.push({
      id: 'data',
      label: 'Point it at your product',
      detail: hasData
        ? `${state.eventNameCount} event ${state.eventNameCount === 1 ? 'name' : 'names'} arriving.`
        : 'It answers from your own events. Send one and it has something to read.',
      done: hasData,
      observable: true,
      action: { label: 'Connect my product', href: '/settings?tab=keys' },
    });
    steps.push({
      id: 'standing',
      label: 'Let it run without you',
      detail: state.scheduled
        ? 'Running on a schedule.'
        : 'Give it a schedule and the work happens whether or not you open this tab.',
      done: !!state.scheduled,
      observable: true,
      action: { label: 'Set a schedule', href: '/agents' },
    });
    return steps;
  }

  // Validate. The threshold comes before the instrumentation on purpose: a
  // number agreed after the first numbers arrive is not a threshold, and this is
  // the step where founders lie to themselves.
  steps.push({
    id: 'threshold',
    label: 'Agree the number that decides',
    detail: state.testCommitted
      ? 'Committed. The scoreboard below reads against it — you can no longer move it.'
      : state.testProposed
        ? 'Your teammate proposed a test. Commit to it and the number is settled before the data can argue with it.'
        : 'Ask for a test, then commit to its kill/keep number. A threshold picked after the result is not a threshold.',
    done: !!state.testCommitted,
    // Finally observable: the commitment is a row, not a promise made in chat
    // that nothing could ever read back.
    observable: true,
    action: state.testProposed
      ? { label: 'Review it', href: '/start?job=validate' }
      : { label: 'Design the test', href: '/chat' },
  });

  steps.push({
    id: 'data',
    label: 'Instrument the page',
    detail: hasData
      ? `${state.eventNameCount} event ${state.eventNameCount === 1 ? 'name' : 'names'} arriving — the test can be read.`
      : 'Paste one snippet into your landing page. No build step, no npm — it works on Framer, Carrd, or plain HTML.',
    done: hasData,
    observable: true,
    action: { label: 'Get the snippet', href: '/settings?tab=keys' },
  });

  const marketer = job.campaignPack ?? '';
  const marketing = !!marketer && installed.has(marketer);
  steps.push({
    id: 'market',
    label: 'Market it before you build it',
    detail: marketing
      ? 'Hired. Ask it for this week’s posts — the traffic they bring is what the number is made of.'
      : `${jobPackName(marketer)} writes the posts that put this in front of real people. If you can’t market it, don’t build it.`,
    done: marketing,
    observable: true,
    action: { label: 'Hire', install: marketer },
  });

  return steps;
}

// nextStep takes the already-built list rather than rebuilding it, so the panel
// that renders the checklist and the rule that picks its next action are looking
// at the same objects.
export function nextStep(steps: JobStep[]): JobStep | null {
  return steps.find((step) => !step.done) ?? null;
}

// jobProgress is done-count / total, for the one-glance strip on the picker.
// Only observable steps count, so every job's checklist can actually finish.
export function jobProgress(job: JobDef, state: JobState): { done: number; total: number } {
  const steps = jobSteps(job, state).filter((s) => s.observable);
  return { done: steps.filter((s) => s.done).length, total: steps.length };
}

// suggestedJob picks the job to open first from what the workspace actually
// looks like.
//
// A hire is read before the event count, because hiring is something the owner
// deliberately did and an empty store is only something we infer. Someone who
// hired Growth Lead yesterday and has not instrumented yet must not be greeted
// with "I have an idea, not a product yet" — their next step belongs to `grow`
// ("point it at your product"), which is exactly what that job's plan opens on.
export function suggestedJob(state: JobState): JobId {
  const installed = new Set(state.installedPacks);
  const hiredFor = (id: JobId) => jobById(id)!.packs.some((slug) => installed.has(slug));
  if (hiredFor('operate')) return 'operate';
  if (hiredFor('grow')) return 'grow';
  // No standing teammate: nothing has ever arrived means pre-product.
  if (state.eventNameCount === 0) return 'validate';
  return 'grow';
}

// Display names for the packs a job hires. Kept here (not fetched) so the
// picker can name the teammate before the marketplace request resolves —
// the catalog is the source of truth and overrides this at render time.
const PACK_NAMES: Record<string, string> = {
  'product-scout': 'Product Scout',
  'growth-lead': 'Growth Lead',
  'marketing-lead': 'Marketing Lead',
  'marketing-strategist': 'Marketing Strategist',
  'data-analyst': 'Data Analyst',
  'insight-digest': 'Insight Digest',
  'ops-watch': 'Ops Watch',
  'tracking-steward': 'Tracking Steward',
};

export function jobPackName(slug: string): string {
  return PACK_NAMES[slug] ?? slug;
}

// jobForPack is the reverse index: which job does this pack serve? Used to
// label a marketplace card with the job it belongs to.
export function jobForPack(slug: string): JobDef | null {
  return JOBS.find((job) => job.packs.includes(slug)) ?? null;
}
