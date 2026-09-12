import { describe, expect, it } from 'vitest';
import { verificationSnippet } from './first-event-quickstart';

describe('verificationSnippet', () => {
  it('sends an authenticated, platform-stamped verification receipt for every manual source', () => {
    const key = 'capture-key';
    const cases = [
      { source: 'website' as const, lang: 'js' as const, platform: 'web' },
      { source: 'app' as const, lang: 'curl' as const, platform: 'server' },
      { source: 'app' as const, lang: 'js' as const, platform: 'server' },
      { source: 'app' as const, lang: 'python' as const, platform: 'server' },
      { source: 'ios' as const, lang: 'js' as const, platform: 'ios' },
    ];

    for (const tc of cases) {
      const snippet = verificationSnippet(tc.source, tc.lang, 'https://api.example.test', key);
      expect(snippet).toContain('onboarding_verified');
      expect(snippet).toContain(tc.platform);
      expect(snippet).toContain(key);
    }
  });
});
