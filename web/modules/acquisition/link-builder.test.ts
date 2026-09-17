import { describe, expect, it } from 'vitest';
import { buildUTMURL } from './link-builder';

describe('buildUTMURL', () => {
  it('returns empty string when url is empty or blank', () => {
    expect(buildUTMURL('', { source: 'google' })).toBe('');
    expect(buildUTMURL('   ', { source: 'google' })).toBe('');
  });

  it('returns original url when no utm params are provided', () => {
    expect(buildUTMURL('https://example.com', {})).toBe('https://example.com');
    expect(buildUTMURL('https://example.com', { source: '  ' })).toBe('https://example.com');
  });

  it('attaches utm_source and utm_campaign to clean url', () => {
    const result = buildUTMURL('https://example.com/landing', {
      source: 'newsletter',
      campaign: 'launch-week',
    });
    expect(result).toBe('https://example.com/landing?utm_source=newsletter&utm_campaign=launch-week');
  });

  it('attaches all five UTM parameters', () => {
    const result = buildUTMURL('https://example.com', {
      source: 'google',
      medium: 'cpc',
      campaign: 'brand',
      term: 'novel reader',
      content: 'hero-button',
    });
    expect(result).toBe(
      'https://example.com/?utm_source=google&utm_medium=cpc&utm_campaign=brand&utm_term=novel+reader&utm_content=hero-button',
    );
  });

  it('preserves existing query parameters on base url', () => {
    const result = buildUTMURL('https://example.com/page?ref=home', {
      source: 'x',
      medium: 'social',
    });
    expect(result).toBe('https://example.com/page?ref=home&utm_source=x&utm_medium=social');
  });

  it('preserves hash fragments after query parameters', () => {
    const result = buildUTMURL('https://example.com/page#pricing', {
      source: 'reddit',
      campaign: 'ama',
    });
    expect(result).toBe('https://example.com/page?utm_source=reddit&utm_campaign=ama#pricing');
  });

  it('handles relative or schemeless urls', () => {
    const result = buildUTMURL('/pricing', {
      source: 'partner',
    });
    expect(result).toBe('/pricing?utm_source=partner');
  });
});
