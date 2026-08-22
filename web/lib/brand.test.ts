import { describe, expect, it } from 'vitest';
import {
  CLAIMS,
  CRAWLER_DISALLOW,
  HEADLINE,
  INDEXABLE_PATHS,
  META_DESCRIPTION,
  PROMISE_BANNED_VERBS,
  SUBHEAD,
  TITLE_DEFAULT,
  TITLE_TEMPLATE,
  softwareApplicationJsonLd,
} from './brand';

// The promise position, exhaustively: the strings a stranger reads before they
// know anything. Claim *details* are deliberately absent — a detail is allowed
// to name the commodity as the foil ("A dashboard starts over every time"),
// which is the opposite of promising it.
const PROMISE_STRINGS: Record<string, string> = {
  headline: HEADLINE,
  subhead: SUBHEAD,
  title: TITLE_DEFAULT,
  description: META_DESCRIPTION,
  ...Object.fromEntries(CLAIMS.map((claim) => [`claim:${claim.id}`, claim.title])),
};

describe('promise-position copy', () => {
  // The regression this guards is the one the whole ticket exists to undo: the
  // door used to lead with "Ask which step is losing people." and the metadata
  // with "see what moved", on a product whose own pitch is that seeing is the
  // part every competitor already ships.
  it.each(Object.entries(PROMISE_STRINGS))('%s sells no commodity verb', (_name, value) => {
    for (const verb of PROMISE_BANNED_VERBS) {
      expect(value).not.toMatch(new RegExp(`\\b${verb}\\b`, 'i'));
    }
  });

  it('never asks a question in the headline', () => {
    expect(HEADLINE).not.toContain('?');
    // ≤ 6 words is the slot spec: a headline that needs a seventh is
    // explaining rather than promising.
    expect(HEADLINE.split(/\s+/).length).toBeLessThanOrEqual(6);
  });

  // No surface explains the name. A page that has to define its own suffix has
  // already lost the argument the suffix was supposed to win.
  it.each(Object.entries(PROMISE_STRINGS))('%s does not explain the name', (_name, value) => {
    expect(value).not.toMatch(/agent\s*\+\s*ray|geometr|the name/i);
  });

  it('keeps the meta description inside a search snippet', () => {
    expect(META_DESCRIPTION.length).toBeLessThanOrEqual(165);
  });

  it('keeps the brand as the title suffix, not the subject', () => {
    expect(TITLE_TEMPLATE).toBe('%s · AgentRay');
  });
});

describe('crawler surface', () => {
  it('indexes only the door', () => {
    expect(INDEXABLE_PATHS).toEqual(['/']);
  });

  // The gated routes are the ones a crawler must not spend an impression on.
  // /pricing is hostedOnly (lib/ia.ts) and does not exist at all on a
  // self-hosted instance.
  it('disallows every gated route, pricing included', () => {
    expect(CRAWLER_DISALLOW).toContain('/chat');
    expect(CRAWLER_DISALLOW).toContain('/settings');
    expect(CRAWLER_DISALLOW).toContain('/pricing');
    expect(CRAWLER_DISALLOW).not.toContain('/');
  });
});

describe('softwareApplicationJsonLd', () => {
  const jsonLd = softwareApplicationJsonLd('https://agentray.example.com');

  it('describes the app, not an article', () => {
    expect(jsonLd['@type']).toBe('SoftwareApplication');
    expect(jsonLd.url).toBe('https://agentray.example.com');
    expect(jsonLd.description).toBe(META_DESCRIPTION);
  });

  // Same standing ban as the door: a self-hosted instance advertising a price
  // would be a lie, and pricing is hosted-only.
  it('advertises no price', () => {
    expect(jsonLd).not.toHaveProperty('offers');
    expect(JSON.stringify(jsonLd)).not.toMatch(/price/i);
  });
});
