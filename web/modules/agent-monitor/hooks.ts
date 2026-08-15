'use client';

import { useQuery } from '@tanstack/react-query';
import { AgentRayAPI } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';

// useAgentMonitor fetches the project-wide rollup — every agent with its run
// counts, token/cost totals, and last activity. It powers agent health overview
// surfaces and the agents workspace rollup without needing per-agent code.
export function useAgentMonitor() {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['agent-monitor', projectID],
    queryFn: () => new AgentRayAPI(projectID!).agentMonitor(),
    enabled: !!projectID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });
  return {
    agents: query.data?.agents ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
  };
}

// useAgentMonitorDetail fetches one agent's rollup plus its recent runs for the
// detailed health view shown inside the agent sheet. The per-run loop trace
// (tool + LLM calls) is fetched lazily on drill-in via useAgentRun.
export function useAgentMonitorDetail(agentID: string | null) {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['agent-monitor-detail', projectID, agentID],
    queryFn: () => new AgentRayAPI(projectID!).agentMonitorDetail(agentID!),
    enabled: !!projectID && !!agentID,
    staleTime: 30 * 1000,
    refetchOnWindowFocus: false,
  });
  return {
    agent: query.data?.agent,
    runs: query.data?.runs ?? [],
    isLoading: query.isLoading,
    error: query.error as Error | null,
  };
}

// useRunSteps loads one run's table of contents and ONE window of its steps.
//
// The split is the whole point. Chapters are small and always complete, so the
// reader can see the shape of a run of any length at once; steps are large (each
// carries the context that entered it) and only worth fetching for the span the
// reader actually opened. Changing `offset` refetches just the window — the
// chapter list comes back from cache under the same key shape, so navigating a
// long run does not re-download its outline on every jump.
export function useRunSteps(runID: string | null, agentID: string, offset: number, limit = 50) {
  const projectID = useAuthStore((s) => s.project?.id);
  const query = useQuery({
    queryKey: ['run-steps', projectID, agentID, runID, offset, limit],
    queryFn: () => new AgentRayAPI(projectID!).labReplaySteps(runID!, agentID, offset, limit),
    enabled: !!projectID && !!runID,
    // A finished run's fold never changes, so nothing here goes stale. A live
    // one is watched on the monitor page, not read step-by-step here.
    staleTime: 5 * 60 * 1000,
    refetchOnWindowFocus: false,
    // Keep the previous window on screen while the next one loads, so jumping
    // between chapters does not blank the page it is replacing.
    placeholderData: (prev) => prev,
  });
  return {
    steps: query.data?.steps ?? [],
    chapters: query.data?.chapters ?? [],
    total: query.data?.total ?? 0,
    windowOffset: query.data?.offset ?? 0,
    isLoading: query.isLoading,
    isFetching: query.isFetching,
    error: query.error as Error | null,
  };
}
