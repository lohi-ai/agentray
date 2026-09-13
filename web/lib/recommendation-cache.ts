import type { QueryClient } from '@tanstack/react-query';

// Every surface that can settle agent_recommendations uses this one invalidation
// contract. The same row is rendered by Plans, Chat, DailyReadout, and the
// Overview's top-open-finding panel; invalidating only the writer's own query
// leaves another client-side route free to show a settled row as open.
const RECOMMENDATION_QUERY_KEYS = [
  'findings',
  'overview-findings',
  'agent-recs',
  'daily-readout',
] as const;

export function invalidateRecommendationQueries(queryClient: QueryClient, projectID?: string) {
  for (const key of RECOMMENDATION_QUERY_KEYS) {
    void queryClient.invalidateQueries({ queryKey: [key, projectID] });
  }
}
