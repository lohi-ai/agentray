import { describe, expect, it } from 'vitest';
import { cn } from './utils';

describe('vendored Lohi class merging', () => {
  it('keeps clsx inputs and resolves Tailwind conflicts', () => {
    expect(cn('px-2 text-sm', false, ['font-medium', { hidden: false }], 'px-4')).toBe('text-sm font-medium px-4');
  });
});
