import { readdirSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  CHANNEL_CATALOG,
  CHILD_SURFACES,
  NAV_ITEMS,
  firstSessionNotice,
  isLinkedSurface,
  projectLanding,
  recoveryAction,
  settingsPath,
  signedInLandingTarget,
  tourSteps,
  type TourInput,
} from './ia';
import { JOBS, jobSteps, type JobState } from './jobs';

// The spec's sentence — "an unimplemented destination renders as a non-linked
// affordance, never a link to a route that does not exist" — is only kept by a
// test that walks the real route table. A hand-written list of expected routes
// goes stale the day somebody renames a directory; this one reads the app
// router's own page.tsx files, so a route that ships is a route that matches
// and a route that is deleted turns every href pointing at it red.

const APP_DIR = path.resolve(__dirname, '../app');
// Every page.tsx is a route; the file's directory IS the URL. Dynamic
// segments ([id], [...rest]) match any concrete segment(s).
function shippedRoutes(): string[] {
  const routes: string[] = [];
  const walk = (dir: string, route: string) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (entry.isDirectory()) walk(path.join(dir, entry.name), `${route}/${entry.name}`);
      else if (entry.name === 'page.tsx') routes.push(route || '/');
    }
  };
  walk(APP_DIR, '');
  return routes;
}


function resolvesToRoute(href: string, routes: string[]): boolean {
  const pathname = href.split('?')[0].split('#')[0];
  if (!pathname.startsWith('/')) return false;
  const segments = pathname.split('/').filter(Boolean);
  return routes.some((route) => {
    const pattern = route.split('/').filter(Boolean);
    let i = 0;
    for (const part of pattern) {
      if (part.startsWith('[...') || part.startsWith('[[...')) return true;
      if (i >= segments.length) return false;
      if (!part.startsWith('[') && part !== segments[i]) return false;
      i += 1;
    }
    return i === segments.length;
  });
}

// Every href the IA can produce, labelled so a failure names its source.
function iaHrefs(): Array<{ from: string; href: string }> {
  const out: Array<{ from: string; href: string }> = [];
  const push = (from: string, href: string | undefined | null) => {
    if (href !== undefined && href !== null) out.push({ from, href });
  };

  for (const item of NAV_ITEMS) {
    push(`NAV_ITEMS ${item.label}`, item.href);
    for (const alias of item.aliases ?? []) push(`NAV_ITEMS ${item.label} alias`, alias);
  }

  for (const surface of CHILD_SURFACES) {
    push(`CHILD_SURFACES ${surface.label}`, surface.href);
  }

  // Reserved channels are listed so the UI can say "not yet" — but a catalog
  // entry that still carries an href is a link the router cannot serve, so
  // every declared href ships or the row is a bug.
  for (const channel of CHANNEL_CATALOG) {
    push(`CHANNEL_CATALOG ${channel.kind}`, channel.href);
  }

  for (const tab of [undefined, 'ai', 'keys', 'connectors', 'plan', 'projects'] as const) {
    push(`settingsPath(${tab ?? ''})`, settingsPath(tab));
  }

  push('recoveryAction(paused)', recoveryAction('error: agent is disabled').href);
  push('recoveryAction(no key)', recoveryAction('error: no workspace model key').href);
  push('signedInLandingTarget', signedInLandingTarget());

  for (const from of ['/agents/abc/setup', '/teams/t1', '/operations/op1', '/prototypes/p1', '/plans/p1', '/chat']) {
    push(`projectLanding(${from})`, projectLanding(from));
    push(`projectLanding(${from}, created)`, projectLanding(from, { created: true }));
  }

  for (const job of JOBS) {
    for (const surface of job.surfaces) push(`JOBS ${job.id} surface`, surface.href);
    const states: JobState[] = [
      { installedPacks: [], eventNameCount: 0 },
      { installedPacks: [], eventNameCount: 3, hasModelKey: true, scheduled: false },
      { installedPacks: [], eventNameCount: 0, testProposed: true, testID: 'test-1' },
      { installedPacks: [], eventNameCount: 0, testProposed: true },
    ];
    for (const state of states) {
      for (const step of jobSteps(job, state)) push(`jobSteps ${job.id}.${step.id}`, step.action.href);
    }
  }

  const tours: TourInput[] = [
    { ready: true, hasDemo: true, inDemo: false, demoName: 'Demo', ownProjectName: 'Mine', ownProjectCount: 0, ownEventNameCount: 0, ownScheduled: false },
    { ready: true, hasDemo: true, inDemo: true, demoName: 'Demo', ownProjectName: 'Mine', ownProjectCount: 1, ownEventNameCount: 0, ownScheduled: false },
    { ready: true, hasDemo: false, inDemo: false, demoName: '', ownProjectName: 'Mine', ownProjectCount: 0, ownEventNameCount: 0, ownScheduled: false },
  ];
  for (const input of tours) {
    for (const step of tourSteps(input)) {
      push(`tourSteps ${step.id} action`, step.action.href);
      for (const link of step.links ?? []) push(`tourSteps ${step.id} link`, link.href);
    }
  }

  push(
    'firstSessionNotice(no key)',
    firstSessionNotice({ eventNames: ['user.pageview'], catalogReady: true, hasModelKey: false })?.href,
  );

  return out;
}

describe('IA route reachability', () => {
  const routes = shippedRoutes();

  it('reads the real route table off app/**/page.tsx', () => {
    // Guards against the test itself going blind: if the walk ever returns
    // nothing (moved app dir, renamed pages) every assertion below would pass
    // vacuously.
    expect(routes).toContain('/overview');
    expect(routes).toContain('/dashboard');
    expect(routes.length).toBeGreaterThan(20);
  });

  it('resolves every href the IA can produce to a shipped route', () => {
    const dead = iaHrefs().filter(({ href }) => !resolvesToRoute(href, routes));
    expect(dead).toEqual([]);
  });

  it('never hands a named-but-unbuilt destination an href', () => {
    // The other half of the contract: "Coming soon" surfaces and reserved
    // channels must not carry a placeholder URL at all — an empty string is
    // still a link the router cannot serve.
    for (const surface of CHILD_SURFACES) {
      if (!isLinkedSurface(surface)) expect(surface.href).toBeUndefined();
    }
    for (const channel of CHANNEL_CATALOG) {
      if (!channel.shipped) expect(channel.href).toBeUndefined();
    }
  });
});
