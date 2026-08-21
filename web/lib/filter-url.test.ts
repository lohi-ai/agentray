import { describe, expect, it } from 'vitest';
import { defaultFilters } from '@/lib/api';
import { filtersFromQuery, filtersToQuery } from '@/lib/filter-url';

const query = (filters = defaultFilters, base?: string) =>
  filtersToQuery(filters, base ? new URLSearchParams(base) : undefined).toString();

describe('filtersToQuery', () => {
  it('writes nothing for an untouched page', () => {
    expect(query()).toBe('');
  });

  it('writes only what departs from the default', () => {
    expect(query({ ...defaultFilters, platform: 'ios' })).toBe('platform=ios');
    expect(query({ ...defaultFilters, hours: 168, platform: 'ios' })).toBe('hours=168&platform=ios');
  });

  it('leaves the page its own params', () => {
    // /settings?tab=keys and /chat?agent=<id> live in the same query string.
    expect(query({ ...defaultFilters, platform: 'web' }, 'tab=keys')).toBe('tab=keys&platform=web');
  });

  it('clears a filter that returned to its default instead of leaving it stale', () => {
    expect(query(defaultFilters, 'tab=keys&platform=ios&error_only=true')).toBe('tab=keys');
  });

  it('carries error_only only when it is on', () => {
    expect(query({ ...defaultFilters, error_only: true })).toBe('error_only=true');
    expect(query({ ...defaultFilters, error_only: false })).toBe('');
  });
});

describe('filtersFromQuery', () => {
  it('returns null when the URL carries no filter at all', () => {
    // A page param must not be read as "the reader asked for defaults" — that
    // would wipe filters they set on the previous surface.
    expect(filtersFromQuery(new URLSearchParams('tab=keys'))).toBeNull();
    expect(filtersFromQuery(new URLSearchParams(''))).toBeNull();
  });

  it('reads a shared link back', () => {
    const filters = filtersFromQuery(new URLSearchParams('hours=168&platform=ios&search=chapter'));
    expect(filters).not.toBeNull();
    expect(filters!.hours).toBe(168);
    expect(filters!.platform).toBe('ios');
    expect(filters!.search).toBe('chapter');
    expect(filters!.event_type).toBe('');
  });

  it('falls back to the default window rather than asking for NaN hours', () => {
    // A truncated or hand-edited value used to reach the API as `hours=NaN`,
    // which returns nothing and reads on screen as "you have no data".
    for (const bad of ['hours=', 'hours=abc', 'hours=0', 'hours=-5']) {
      const filters = filtersFromQuery(new URLSearchParams(`${bad}&platform=web`));
      expect(filters!.hours).toBe(defaultFilters.hours);
    }
  });

  it('round-trips', () => {
    const original = { ...defaultFilters, hours: 720, platform: 'android', event_name: 'user.pageview', error_only: true };
    const parsed = filtersFromQuery(filtersToQuery(original));
    expect(parsed).toEqual(original);
  });
});
