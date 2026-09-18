import { describe, expect, it } from 'vitest';
import { isAPIKeyOptionalVendor, isOAuthVendor, vendorLabel } from './provider-form';

describe('provider-form vendor metadata', () => {
  it('exposes the public Responses wire as an explicit storage-aware choice', () => {
    expect(vendorLabel('openai-responses')).toBe('OpenAI Responses (stored context)');
    expect(isOAuthVendor('openai-responses')).toBe(false);
    expect(isAPIKeyOptionalVendor('openai-responses')).toBe(false);
  });
});
