import { describe, expect, it } from 'vitest';
import type { QueryClient } from '@tanstack/react-query';
import { invalidateRecommendationQueries } from './recommendation-cache';

describe('invalidateRecommendationQueries', () => {
  it('invalidates every reader of an acknowledged recommendation', () => {
    const calls: Array<{ queryKey: readonly unknown[] }> = [];
    const queryClient = {
      invalidateQueries: (filters: { queryKey: readonly unknown[] }) => {
        calls.push(filters);
        return Promise.resolve();
      },
    } as unknown as QueryClient;

    invalidateRecommendationQueries(queryClient, 'project-1');

    expect(calls.map((call) => call.queryKey)).toEqual([
      ['findings', 'project-1'],
      ['overview-findings', 'project-1'],
      ['agent-recs', 'project-1'],
      ['daily-readout', 'project-1'],
    ]);
  });
});
