import { describe, expect, it } from 'vitest';
import {
  JOBS,
  jobById,
  jobChannels,
  jobForPack,
  jobLayers,
  jobPackName,
  jobProgress,
  jobSteps,
  nextStep,
  primaryPack,
  suggestedJob,
  type JobState,
} from './jobs';
import { WORKLOAD_CATEGORIES } from './ia';

const EMPTY: JobState = { installedPacks: [], eventNameCount: 0 };

describe('the job catalog', () => {
  it('covers the three phases of a product, each with a hire and a starting question', () => {
    expect(JOBS.map((j) => j.id)).toEqual(['validate', 'grow', 'operate']);
    for (const job of JOBS) {
      expect(job.situation.length).toBeGreaterThan(0);
      expect(job.outcome.length).toBeGreaterThan(0);
      expect(job.packs.length).toBeGreaterThan(0);
      expect(job.prompts.length).toBeGreaterThan(0);
      expect(job.surfaces.length).toBeGreaterThan(0);
      expect(job.channels).toContain('chat');
    }
  });

  it('binds every job to a real, non-reserved workload category', () => {
    const shipped = new Set(WORKLOAD_CATEGORIES.filter((c) => !c.reserved).map((c) => c.id));
    for (const job of JOBS) {
      expect(shipped.has(job.category)).toBe(true);
    }
  });

  // The whole reason `validate` is its own job: it is the one that must work
  // with an empty dataplane. If this ever flips, an owner with an idea gets
  // told to go send an event they have no product to send.
  it('makes validate the only job that does not need an event stream', () => {
    expect(JOBS.filter((j) => !j.needsEvents).map((j) => j.id)).toEqual(['validate']);
  });

  it('gives the standing jobs a channel beyond chat', () => {
    expect(jobById('grow')!.channels).toContain('schedule');
    expect(jobById('operate')!.channels).toContain('schedule');
    expect(jobById('validate')!.channels).not.toContain('schedule');
  });

  it('indexes packs back to their job, and names them', () => {
    expect(jobForPack('ops-watch')?.id).toBe('operate');
    expect(jobForPack('product-scout')?.id).toBe('validate');
    expect(jobForPack('growth-lead')?.id).toBe('grow');
    expect(jobForPack('nope')).toBeNull();
    expect(primaryPack(jobById('validate')!)).toBe('product-scout');
    expect(jobPackName('ops-watch')).toBe('Ops Watch');
    // Unknown slug degrades to the slug rather than rendering "undefined".
    expect(jobPackName('mystery-pack')).toBe('mystery-pack');
  });

  it('misses cleanly on an unknown job id', () => {
    expect(jobById('nope')).toBeNull();
  });
});

describe('jobLayers', () => {
  it('names the four layers in order for every job', () => {
    for (const job of JOBS) {
      expect(jobLayers(job).map((l) => l.id)).toEqual(['channel', 'workload', 'runtime', 'data']);
    }
  });

  // A job must never advertise a channel the backend has not shipped — the
  // catalog lists support_widget and voice precisely so the UI can say "not
  // yet" rather than render a surface that does not exist.
  it('resolves channels against the shipped catalog only', () => {
    for (const job of JOBS) {
      const resolved = jobChannels(job);
      expect(resolved.length).toBeGreaterThan(0);
      expect(resolved.every((c) => c.shipped)).toBe(true);
      expect(resolved.map((c) => c.kind).sort()).toEqual([...job.channels].sort());
    }
    expect(jobChannels({ ...jobById('grow')!, channels: ['chat', 'voice'] }).map((c) => c.kind))
      .toEqual(['chat']);
  });

  it('names the job’s channels in the layer it describes', () => {
    expect(jobLayers(jobById('operate')!)[0].detail).toMatch(/Chat · Schedule · Webhook/);
    expect(jobLayers(jobById('validate')!)[0].detail).toMatch(/^Chat —/);
  });

  it('tells the truth about what each job answers from', () => {
    expect(jobLayers(jobById('validate')!)[3].detail).toMatch(/no product data yet/i);
    expect(jobLayers(jobById('grow')!)[3].detail).toMatch(/your own event data/i);
    expect(jobLayers(jobById('operate')!)[0].detail).toMatch(/schedule/i);
    expect(jobLayers(jobById('validate')!)[0].detail).not.toMatch(/schedule/i);
  });
});

describe('jobSteps', () => {
  it('walks a cold workspace to the key first, then the hire', () => {
    const job = jobById('grow')!;
    const steps = jobSteps(job, { ...EMPTY, hasModelKey: false });
    expect(steps.map((s) => s.id)).toEqual(['key', 'hire', 'data', 'standing']);
    expect(nextStep(job, { ...EMPTY, hasModelKey: false })?.id).toBe('key');
    expect(steps[0].action.href).toBe('/settings?tab=ai');
  });

  // A workspace whose key query has not resolved must not flash a blocker at
  // an owner who already has one.
  it('treats an unresolved model key as satisfied', () => {
    const job = jobById('grow')!;
    expect(nextStep(job, EMPTY)?.id).toBe('hire');
    expect(jobSteps(job, EMPTY)[0].done).toBe(true);
  });

  it('offers the job’s own pack as a one-click install', () => {
    const job = jobById('operate')!;
    const hire = jobSteps(job, EMPTY).find((s) => s.id === 'hire')!;
    expect(hire.done).toBe(false);
    expect(hire.action.install).toBe('ops-watch');
    expect(hire.detail).toMatch(/Ops Watch/);
  });

  it('counts any of the job’s packs as hired, not just the primary one', () => {
    const job = jobById('grow')!;
    const hire = jobSteps(job, { ...EMPTY, installedPacks: ['data-analyst'] })
      .find((s) => s.id === 'hire')!;
    expect(hire.done).toBe(true);
  });

  // The one place the three stories genuinely diverge.
  it('treats an empty event store as a blocker to grow and as the deliverable of validate', () => {
    const grow = jobSteps(jobById('grow')!, { ...EMPTY, installedPacks: ['growth-lead'] })
      .find((s) => s.id === 'data')!;
    expect(grow.label).toBe('Point it at your product');
    expect(grow.action.href).toBe('/settings?tab=keys');

    const validate = jobSteps(jobById('validate')!, { ...EMPTY, installedPacks: ['product-scout'] })
      .find((s) => s.id === 'data')!;
    expect(validate.label).toBe('Instrument the page');
    // The on-ramp, not a conversation: the owner of a Framer page needs a
    // snippet with their key in it, and telling them to npm install is telling
    // them to come back next week.
    expect(validate.detail).toMatch(/no build step|no npm|snippet/i);
    expect(validate.action.href).toBe('/settings?tab=keys');
  });

  it('pluralises the event-name count it reports back', () => {
    const one = jobSteps(jobById('grow')!, { ...EMPTY, eventNameCount: 1 }).find((s) => s.id === 'data')!;
    const many = jobSteps(jobById('grow')!, { ...EMPTY, eventNameCount: 4 }).find((s) => s.id === 'data')!;
    expect(one.detail).toMatch(/1 event name/);
    expect(many.detail).toMatch(/4 event names/);
    expect(one.done).toBe(true);
  });

  it('closes out a fully set-up grow workspace', () => {
    const job = jobById('grow')!;
    const state: JobState = {
      installedPacks: ['growth-lead'],
      eventNameCount: 6,
      hasModelKey: true,
      scheduled: true,
    };
    expect(nextStep(job, state)).toBeNull();
    expect(jobProgress(job, state)).toEqual({ done: 4, total: 4 });
  });

  // Marketing-first: validate ends on putting the message in front of real
  // people, because the response to that is what decides whether anything gets
  // built. Research and instrumentation are the setup for it, not the point.
  it('ends validate on the marketing test, not on a schedule', () => {
    const steps = jobSteps(jobById('validate')!, {
      installedPacks: ['product-scout'],
      eventNameCount: 3,
      hasModelKey: true,
      testCommitted: true,
    });
    const last = steps[steps.length - 1];
    expect(last.id).toBe('market');
    expect(last.detail).toMatch(/can’t market it, don’t build it/i);
    expect(last.action.install).toBe('marketing-lead');
    expect(steps.some((s) => s.id === 'standing')).toBe(false);
  });

  // The threshold used to be unobservable — a number agreed in chat left no row,
  // so the step could never check off and was excluded from the count to stop
  // the card reading "3 of 4" forever. It is a committed row now, so it counts
  // like every other step and the checklist can genuinely finish.
  it('counts the committed threshold as a real step', () => {
    const job = jobById('validate')!;
    const uncommitted: JobState = { installedPacks: ['product-scout'], eventNameCount: 3, hasModelKey: true };
    expect(nextStep(job, uncommitted)?.id).toBe('threshold');
    expect(jobSteps(job, uncommitted).find((s) => s.id === 'threshold')!.observable).toBe(true);
    expect(jobProgress(job, uncommitted)).toEqual({ done: 3, total: 5 });

    const done: JobState = { ...uncommitted, testCommitted: true, installedPacks: ['product-scout', 'marketing-lead'] };
    expect(nextStep(job, done)).toBeNull();
    expect(jobProgress(job, done)).toEqual({ done: 5, total: 5 });
  });

  // A proposal binds nobody. The two states need different next actions —
  // "design a test" versus "agree to this one" — and collapsing them would send
  // an owner who already has a proposal back to write another.
  it('distinguishes a proposed test from a committed one', () => {
    const job = jobById('validate')!;
    const none = jobSteps(job, { ...EMPTY, installedPacks: ['product-scout'] }).find((s) => s.id === 'threshold')!;
    expect(none.action.label).toBe('Design the test');

    const proposed = jobSteps(job, { ...EMPTY, installedPacks: ['product-scout'], testProposed: true })
      .find((s) => s.id === 'threshold')!;
    expect(proposed.done).toBe(false);
    expect(proposed.action.label).toBe('Review it');
    expect(proposed.detail).toMatch(/commit/i);
  });

  // The campaign teammate is an ADDITIONAL hire, not an alternative one. If it
  // satisfied the "hire the teammate" step, an owner could hire the marketer,
  // see the checklist advance, and never get the research the job is named for.
  it('does not let the campaign teammate stand in for the primary hire', () => {
    const job = jobById('validate')!;
    const steps = jobSteps(job, { ...EMPTY, installedPacks: ['marketing-lead'] });
    expect(steps.find((s) => s.id === 'hire')!.done).toBe(false);
    expect(steps.find((s) => s.id === 'market')!.done).toBe(true);
  });
});

describe('suggestedJob', () => {
  it('opens on validate for a workspace with no teammate and no events', () => {
    expect(suggestedJob(EMPTY)).toBe('validate');
    expect(suggestedJob({ installedPacks: ['product-scout'], eventNameCount: 0 })).toBe('validate');
  });

  // A hire outranks an empty event store: the owner told us what they are here
  // for, and greeting them with "I have an idea, not a product yet" throws that
  // away. Their real next step is grow's own data step.
  it('keeps a hired owner on their job even before any event arrives', () => {
    expect(suggestedJob({ installedPacks: ['growth-lead'], eventNameCount: 0 })).toBe('grow');
    expect(suggestedJob({ installedPacks: ['ops-watch'], eventNameCount: 0 })).toBe('operate');
  });

  it('opens on operate once an operations teammate is hired', () => {
    expect(suggestedJob({ installedPacks: ['ops-watch'], eventNameCount: 9 })).toBe('operate');
    expect(suggestedJob({ installedPacks: ['tracking-steward'], eventNameCount: 9 })).toBe('operate');
  });

  it('otherwise opens on grow', () => {
    expect(suggestedJob({ installedPacks: ['growth-lead'], eventNameCount: 9 })).toBe('grow');
    expect(suggestedJob({ installedPacks: [], eventNameCount: 9 })).toBe('grow');
  });
});
